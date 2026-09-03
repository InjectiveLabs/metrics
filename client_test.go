package metrics

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingStatter records that it was closed, so the tests can assert the refresh
// path does not leak the client it replaces.
type countingStatter struct {
	mux    sync.Mutex
	closed int
}

func (s *countingStatter) Count(string, int64, []string, float64) error          { return nil }
func (s *countingStatter) Incr(string, []string, float64) error                  { return nil }
func (s *countingStatter) Decr(string, []string, float64) error                  { return nil }
func (s *countingStatter) Gauge(string, float64, []string, float64) error        { return nil }
func (s *countingStatter) Timing(string, time.Duration, []string, float64) error { return nil }
func (s *countingStatter) Histogram(string, float64, []string, float64) error    { return nil }

func (s *countingStatter) Close() error {
	s.mux.Lock()
	defer s.mux.Unlock()
	s.closed++
	return nil
}

func (s *countingStatter) closeCount() int {
	s.mux.Lock()
	defer s.mux.Unlock()
	return s.closed
}

func currentClient() Statter {
	clientMux.RLock()
	defer clientMux.RUnlock()
	return client
}

func resetClientGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		clientMux.Lock()
		client = nil
		clientMux.Unlock()
		setCurrentConfig(nil)
	})
}

// runRefresh starts a refresh loop for the generation currently installed and, on
// cleanup, cancels it and waits for it to actually exit.
//
// The waiting is the point: cancel() does not abandon a tick the loop has already
// selected, so without the join a swap can land after the test returns and replace
// the package client while the next test is asserting on it. Call it after
// resetClientGlobals so that this cleanup, being registered later, runs first.
func runRefresh(t *testing.T, addr, prefix string, cfg *StatterConfig, interval time.Duration) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	gen := initGeneration.Load()

	go func() {
		startStatterRefresh(ctx, addr, prefix, cfg, interval, gen)
		close(done)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestBaseTags(t *testing.T) {
	tests := []struct {
		name     string
		config   *StatterConfig
		expected []string
	}{
		{
			name: "Datadog agent with default tags",
			config: &StatterConfig{
				Agent:    DatadogAgent,
				EnvName:  "prod",
				HostName: "host1",
				DefaultTags: []interface{}{
					"key1", "value1",
					"key2", "value2",
				},
			},
			expected: []string{"env:prod", "machine:host1", "key1:value1", "key2:value2"},
		},
		{
			name: "Telegraf agent with default tags",
			config: &StatterConfig{
				Agent:    TelegrafAgent,
				EnvName:  "dev",
				HostName: "host2",
				DefaultTags: []interface{}{
					"key1", "value1",
					"key2", "value2",
				},
			},
			expected: []string{"env", "dev", "machine", "host2", "key1", "value1", "key2", "value2"},
		},
		{
			name: "Datadog agent with no default tags",
			config: &StatterConfig{
				Agent:       DatadogAgent,
				EnvName:     "staging",
				HostName:    "host3",
				DefaultTags: nil,
			},
			expected: []string{"env:staging", "machine:host3"},
		},
		{
			name: "Telegraf agent with no default tags",
			config: &StatterConfig{
				Agent:       TelegrafAgent,
				EnvName:     "test",
				HostName:    "host4",
				DefaultTags: nil,
			},
			expected: []string{"env", "test", "machine", "host4"},
		},
		{
			name: "Empty config",
			config: &StatterConfig{
				Agent:       DatadogAgent,
				DefaultTags: nil,
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(func() {
				setCurrentConfig(nil)
			})
			setCurrentConfig(tt.config)
			result := tt.config.BaseTags()
			assert.ElementsMatch(t, tt.expected, result)
		})
	}
}

func TestSwapStatterClosesReplacedClient(t *testing.T) {
	resetClientGlobals(t)

	first := &countingStatter{}
	second := &countingStatter{}

	swapStatter(first, &StatterConfig{Agent: TelegrafAgent})
	assert.Same(t, first, currentClient())
	assert.Equal(t, 0, first.closeCount(), "installing the first client must not close it")

	swapStatter(second, &StatterConfig{Agent: TelegrafAgent})
	assert.Same(t, second, currentClient(), "the new client must be the one in use")
	assert.Equal(t, 1, first.closeCount(), "the replaced client must be closed, or its socket leaks")
	assert.Equal(t, 0, second.closeCount())
}

func TestSwapStatterFromEmpty(t *testing.T) {
	resetClientGlobals(t)

	// The very first swap has nothing to close and must not panic on the nil client.
	assert.NotPanics(t, func() { swapStatter(&countingStatter{}, &StatterConfig{Agent: TelegrafAgent}) })
}

func TestStatterNeedsRefresh(t *testing.T) {
	tests := []struct {
		agent    string
		expected bool
		why      string
	}{
		{DatadogAgent, true, "UDP socket, dialed once, cannot detect that delivery stopped"},
		{TelegrafAgent, true, "same connectionless UDP path as datadog"},
		{OTELAgent, false, "gRPC/HTTP exporter redials on its own; rebuilding it would drop batches"},
		{"", false, "unknown agents are left alone"},
	}

	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			assert.Equal(t, tt.expected, statterNeedsRefresh(tt.agent), tt.why)
		})
	}
}

func TestServiceConfigRefreshInterval(t *testing.T) {
	tests := []struct {
		name     string
		set      time.Duration
		expected time.Duration
	}{
		{"unset falls back to the default", 0, DefaultRefreshInterval},
		{"explicit interval is honoured", 30 * time.Second, 30 * time.Second},
		{"negative disables the refresh", -1, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, ServiceConfig{RefreshInterval: tt.set}.refreshInterval())
		})
	}
}

func TestStartStatterRefreshRebuildsAndClosesTheOldClient(t *testing.T) {
	resetClientGlobals(t)

	cfg := &StatterConfig{Agent: DatadogAgent, EnvName: "test", HostName: "host"}

	initial := &countingStatter{}
	swapStatter(initial, cfg)

	runRefresh(t, "127.0.0.1:8125", "test.", cfg, 20*time.Millisecond)

	// The refresh must replace the client it found in place, and close it on the way
	// out - that swap is the whole point: a socket that no longer reaches the agent
	// is indistinguishable, from inside the process, from one that does.
	require.Eventually(t, func() bool {
		return currentClient() != Statter(initial)
	}, 3*time.Second, 10*time.Millisecond, "refresh loop never replaced the client")

	assert.Equal(t, 1, initial.closeCount(), "the replaced client must be closed")
}

func TestStartStatterRefreshStopsOnContextCancel(t *testing.T) {
	resetClientGlobals(t)

	cfg := &StatterConfig{Agent: DatadogAgent, EnvName: "test", HostName: "host"}
	setCurrentConfig(cfg)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		startStatterRefresh(ctx, "127.0.0.1:8125", "test.", cfg, time.Hour, initGeneration.Load())
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh loop outlived its context")
	}
}

func TestStartStatterRefreshKeepsClientWhenRebuildFails(t *testing.T) {
	resetClientGlobals(t)

	// An unsupported agent makes newStatter fail on every tick. The existing client
	// has to survive that: a client that might still work beats a nil one, which
	// drops every metric silently.
	cfg := &StatterConfig{Agent: "nonexistent-agent"}

	initial := &countingStatter{}
	swapStatter(initial, cfg)

	runRefresh(t, "127.0.0.1:8125", "test.", cfg, 10*time.Millisecond)

	time.Sleep(150 * time.Millisecond)

	assert.Same(t, initial, currentClient(), "a failed rebuild must leave the working client in place")
	assert.Equal(t, 0, initial.closeCount(), "a failed rebuild must not close the client still in use")
}

func TestStartRefreshIsANoOpWhenItCannotHelp(t *testing.T) {
	tests := []struct {
		name string
		cfg  *StatterConfig
	}{
		{"Init has not run", nil},
		{"metrics are mocked", &StatterConfig{Agent: DatadogAgent, MockingEnabled: true}},
		{"agent manages its own connection", &StatterConfig{Agent: OTELAgent}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetClientGlobals(t)

			initial := &countingStatter{}
			swapStatter(initial, tt.cfg)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			StartRefresh(ctx, "127.0.0.1:8125", "test.", 10*time.Millisecond)
			time.Sleep(120 * time.Millisecond)

			assert.Same(t, initial, currentClient(), "no refresh loop should have started")
			assert.Equal(t, 0, initial.closeCount())
		})
	}
}

func TestStartRefreshRebuildsForUDPAgents(t *testing.T) {
	resetClientGlobals(t)

	initial := &countingStatter{}
	swapStatter(initial, &StatterConfig{Agent: DatadogAgent, EnvName: "test", HostName: "host"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartRefresh(ctx, "127.0.0.1:8125", "test.", 20*time.Millisecond)

	require.Eventually(t, func() bool {
		return currentClient() != Statter(initial)
	}, 3*time.Second, 10*time.Millisecond, "StartRefresh never rebuilt the client")

	assert.Equal(t, 1, initial.closeCount())
}

// Init used to assign the package-level config with no synchronisation while every
// reporting call read it - JoinTags and the stuck-function timers do so on the hot
// path. Any service that initialises metrics on a goroutine, which is the usual shape
// when Init is wrapped in a retry loop, was racing its own first reports.
//
// This reproduces that shape: report continuously while Init runs underneath. It is
// only meaningful under -race, where it fails outright before the fix.
func TestInitConcurrentWithReporting_IsRaceFree(t *testing.T) {
	resetClientGlobals(t)

	// A real socket, so the statter is not logging a write error per report and the
	// test is measuring the config access rather than the failure path.
	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("bind sink: %v", err)
	}
	t.Cleanup(func() {
		if err := sink.Close(); err != nil {
			t.Errorf("close UDP sink: %v", err)
		}
	})
	addr := sink.LocalAddr().String()

	swapStatter(&countingStatter{}, &StatterConfig{Agent: TelegrafAgent, EnvName: "test", HostName: "host"})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers: the reporting path, hammering the config through JoinTags.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				CustomReport(func(s Statter, tagSpec []string) {
					_ = s.Gauge("probe", 1, tagSpec, 1)
				}, Tags{"svc": "test"})
			}
		}()
	}

	// Writer: repeated Init, as a retry loop or a re-init would do.
	for i := 0; i < 50; i++ {
		if err := Init(addr, "racetest.", &StatterConfig{
			Agent: TelegrafAgent, EnvName: "test", HostName: "host",
		}); err != nil {
			t.Fatalf("init: %v", err)
		}
	}

	close(stop)
	wg.Wait()
}

// A refresh loop outlives the Init that started it only until the next Init. Without
// the generation guard, a loop left over from an earlier Init keeps rebuilding from
// its own address, prefix and tags, and the first tick after a re-init hands the
// process a client pointed at the previous destination with the previous tag format.
func TestRefreshFromASupersededInitRetiresItself(t *testing.T) {
	resetClientGlobals(t)

	staleCfg := &StatterConfig{Agent: DatadogAgent, EnvName: "test", HostName: "host"}
	staleGen := swapStatter(&countingStatter{}, staleCfg)

	// A later Init: different destination, different agent, new generation.
	currentCfg := &StatterConfig{Agent: TelegrafAgent, EnvName: "test", HostName: "host"}
	current := &countingStatter{}
	swapStatter(current, currentCfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		startStatterRefresh(ctx, "127.0.0.1:8125", "stale.", staleCfg, 10*time.Millisecond, staleGen)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a superseded refresh loop must stop on its own, not keep swapping")
	}

	assert.Same(t, current, currentClient(), "a stale refresh must not replace what a later Init installed")
	assert.Equal(t, 0, current.closeCount(), "the live client must not be closed by a stale refresh")
	assert.Equal(t, TelegrafAgent, currentConfig().Agent, "a stale refresh must not restore the older config")
}

func TestInitClosesThePreviousClientWhenSwitchingToMocking(t *testing.T) {
	resetClientGlobals(t)

	previous := &countingStatter{}
	swapStatter(previous, &StatterConfig{Agent: TelegrafAgent})

	require.NoError(t, Init("127.0.0.1:8125", "test.", &StatterConfig{
		Agent: TelegrafAgent, MockingEnabled: true,
	}))

	assert.NotSame(t, previous, currentClient(), "mocking must take over the client")
	assert.Equal(t, 1, previous.closeCount(), "switching to mocking must close the real client, or its socket leaks")
}

// The config is what JoinTags reads to pick the tag format, so publishing it before
// the client it belongs to would have reports formatting tags for a client that does
// not exist yet - and would leave that mismatch in place for good if the build failed.
func TestInitLeavesTheWorkingClientAndItsConfigWhenConstructionFails(t *testing.T) {
	resetClientGlobals(t)

	working := &countingStatter{}
	swapStatter(working, &StatterConfig{Agent: TelegrafAgent, EnvName: "test", HostName: "host"})

	require.Error(t, Init("127.0.0.1:8125", "test.", &StatterConfig{Agent: "nonexistent-agent"}))

	assert.Same(t, working, currentClient(), "a failed Init must leave the working client in place")
	assert.Equal(t, 0, working.closeCount())
	assert.Equal(t, TelegrafAgent, currentConfig().Agent, "a failed Init must not publish the config it could not build")
}
