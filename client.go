package metrics

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dogstatsd "github.com/DataDog/datadog-go/v5/statsd"
	"github.com/alexcesaro/statsd"
	"github.com/mixpanel/mixpanel-go"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	ddotel "gopkg.in/DataDog/dd-trace-go.v1/ddtrace/opentelemetry"
	"gopkg.in/DataDog/dd-trace-go.v1/profiler"

	log "github.com/InjectiveLabs/suplog"
)

const (
	DatadogAgent  = "datadog"
	TelegrafAgent = "telegraf"
	OTELAgent     = "otel"
)

var (
	ErrUnsupportedAgent = errors.New("unsupported agent type")

	client    Statter
	clientMux = new(sync.RWMutex)

	// configPtr holds the active StatterConfig. Init used to assign a package-level
	// variable with no synchronisation at all, while every reporting call read it -
	// JoinTags and the stuck-function timers do so on the hot path. Services that
	// initialise metrics on a goroutine (a retry loop around Init is the common
	// shape) therefore raced Init against their own first reports; `go test -race`
	// catches it immediately once a test does the same thing.
	configPtr atomic.Pointer[StatterConfig]

	// initGeneration counts the configurations Init has installed. A refresh loop is
	// bound to the generation it was started for and retires itself as soon as a
	// later Init installs another one - otherwise a loop left over from an earlier
	// Init would keep rebuilding from its own address, prefix and tags and overwrite
	// the client the current Init put in place.
	initGeneration atomic.Uint64

	// generationSuperseded is closed when a later Init installs the next generation.
	// The generation number alone is only checked once a rebuild is already in hand,
	// so a superseded loop would otherwise sit on its ticker for a full interval -
	// five minutes by default - and then dial the old destination once more before
	// noticing it has been replaced. Closing the channel retires it at once. Guarded
	// by clientMux together with the counter it tracks.
	generationSuperseded = make(chan struct{})

	traceProviderShutdownFn func(ctx context.Context) error
	tracer                  trace.Tracer
	mixPanelClient          *mixpanel.ApiClient
)

// currentConfig returns the active config, or an empty one when Init has not run.
// Returning a zero value rather than nil keeps a report that arrives before Init
// harmless, which is the same thing the nil-client check does one level up.
func currentConfig() *StatterConfig {
	if cfg := configPtr.Load(); cfg != nil {
		return cfg
	}
	return &StatterConfig{}
}

func setCurrentConfig(cfg *StatterConfig) {
	configPtr.Store(cfg)
}

type StatterConfig struct {
	Addr                   string            // localhost:8125
	Prefix                 string            // metrics prefix
	Agent                  string            // telegraf/datadog
	EnvName                string            // dev/test/staging/prod
	HostName               string            // hostname
	Version                string            // version
	DefaultTags            []interface{}     // default tags for all metrics
	StuckFunctionTimeout   time.Duration     // stuck time
	MockingThreshold       time.Duration     // mocking threshold
	MockingEnabled         bool              // whether to enable mock statter, which only produce logs
	Disabled               bool              // whether to disable metrics completely
	TracingEnabled         bool              // whether tracing should be enabled
	ProfilingEnabled       bool              // whether Datadog profiling should be enabled
	MixPanelEnabled        bool              // whether MixPanel should be enabled
	MixPanelProjectToken   string            // MixPanel project token
	OTELInsecure           bool              // disable TLS (use for self-hosted SigNoz without TLS)
	OTELHeaders            map[string]string // extra headers, e.g. {"signoz-access-token": "<token>"} for SigNoz Cloud
	OTELUseCounterForCount bool              // use monotonic OTel counters for Count/Incr instead of UpDownCounters
}

func (m *StatterConfig) BaseTags() []string {
	defaultTags := Combine(m.DefaultTags...)
	var baseTags []string

	switch m.Agent {

	case DatadogAgent:
		if len(m.EnvName) > 0 {
			baseTags = append(baseTags, "env:"+m.EnvName)
		}
		if len(m.HostName) > 0 {
			baseTags = append(baseTags, "machine:"+m.HostName)
		}
		for k, v := range defaultTags {
			baseTags = append(baseTags, k+":"+v)
		}
	case OTELAgent:
		if len(m.EnvName) > 0 {
			baseTags = append(baseTags, "env="+m.EnvName)
		}
		if len(m.HostName) > 0 {
			baseTags = append(baseTags, "machine="+m.HostName)
		}
		for k, v := range defaultTags {
			baseTags = append(baseTags, k+"="+v)
		}
	// telegraf by default
	default:
		if len(m.EnvName) > 0 {
			baseTags = append(baseTags, "env", m.EnvName)
		}
		if len(m.HostName) > 0 {
			baseTags = append(baseTags, "machine", m.HostName)
		}
		for k, v := range defaultTags {
			baseTags = append(baseTags, k, v)
		}
	}

	return baseTags
}

type Statter interface {
	Count(name string, value int64, tags []string, rate float64) error
	Incr(name string, tags []string, rate float64) error
	Decr(name string, tags []string, rate float64) error
	Gauge(name string, value float64, tags []string, rate float64) error
	Timing(name string, value time.Duration, tags []string, rate float64) error
	Histogram(name string, value float64, tags []string, rate float64) error
	Close() error
}

type timedCloser interface {
	CloseCtx(ctx context.Context) error
}

func CloseWithTimeout(timeout time.Duration) {
	clientMux.Lock()
	defer clientMux.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if client != nil {
		if ct, ok := client.(timedCloser); ok {
			_ = ct.CloseCtx(ctx)
		} else {
			_ = client.Close()
		}
	}

	if traceProviderShutdownFn != nil {
		_ = traceProviderShutdownFn(ctx)
	}
}

func Close() {
	CloseWithTimeout(DefaultCloseTimeout)
}

func Init(addr string, prefix string, cfg *StatterConfig) error {
	cfg = checkConfig(cfg)

	if cfg.MockingEnabled {
		// init a mock statter instead of real statsd client
		swapStatter(newMockStatter(cfg), cfg)
		return nil
	}

	statter, err := newStatter(addr, prefix, cfg)
	if err != nil {
		return err
	}

	// Tracing and profiling can both fail, and a failed Init must leave the process
	// reporting through whatever it was already using. So they run before the swap:
	// until every step that can return an error has passed, the candidate statter is
	// not published, and on failure it is closed rather than left holding a socket
	// nobody will ever read from again.
	if err := setupTracingFn(addr, prefix, cfg); err != nil {
		_ = statter.Close()
		return err
	}

	if cfg.Agent == DatadogAgent && cfg.ProfilingEnabled {
		if err := setupProfiler(cfg); err != nil {
			_ = statter.Close()
			return err
		}
	}

	swapStatter(statter, cfg)

	if cfg.MixPanelEnabled {
		StartMixPanel(cfg.MixPanelProjectToken)
	}

	return nil
}

// setupTracingFn is the tracing step Init runs. Indirected only so a test can fail
// the step that comes after a successful statter build, which is the case that used
// to throw away a working client.
var setupTracingFn = setupTracing

// setupTracing wires the process-global tracer provider for the configured agent.
// Split out of Init so that everything able to fail sits in one place ahead of the
// client swap; it is a no-op unless tracing is on for an agent that supports it.
func setupTracing(addr, prefix string, cfg *StatterConfig) error {
	if !cfg.TracingEnabled {
		return nil
	}

	switch cfg.Agent {
	// OpenTelemetry tracing via DataDog provider
	case DatadogAgent:
		traceProvider := ddotel.NewTracerProvider()
		otel.SetTracerProvider(traceProvider)
		tracer = otel.Tracer("")
		traceProviderShutdownFn = func(_ context.Context) error {
			return traceProvider.Shutdown()
		}

	case OTELAgent:
		traceProvider, err := newOTELTracerProvider(addr, cfg.OTELInsecure, cfg.OTELHeaders, cfg.BaseTags())
		if err != nil {
			return errors.Wrap(err, "otel tracer provider init failed")
		}
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		otel.SetTracerProvider(traceProvider)
		tracer = otel.Tracer(prefix)
		traceProviderShutdownFn = func(ctx context.Context) error {
			return traceProvider.Shutdown(ctx)
		}
	}

	return nil
}

// newStatter builds a statter for the configured agent. Split out of Init so the
// refresh loop can rebuild ONLY the statter: Init also wires up tracing, profiling
// and MixPanel, and those are process-global, one-shot setups that must not be
// repeated.
func newStatter(addr, prefix string, cfg *StatterConfig) (Statter, error) {
	var (
		statter Statter
		err     error
	)

	switch cfg.Agent {
	case DatadogAgent:
		statter, err = dogstatsd.New(
			addr,
			dogstatsd.WithNamespace(prefix),
			dogstatsd.WithWriteTimeout(time.Duration(10)*time.Second),
			dogstatsd.WithTags(cfg.BaseTags()),
		)

	case TelegrafAgent:
		statter, err = newTelegrafStatter(
			statsd.Address(addr),
			statsd.Prefix(prefix),
			statsd.ErrorHandler(errHandler),
			statsd.TagsFormat(statsd.InfluxDB),
			statsd.Tags(cfg.BaseTags()...),
		)

	case OTELAgent:
		statter, err = newOTELStatter(
			addr,
			prefix,
			cfg.OTELInsecure,
			cfg.OTELHeaders,
			cfg.BaseTags(),
			cfg.OTELUseCounterForCount,
		)

	default:
		return nil, ErrUnsupportedAgent
	}

	if err != nil {
		return nil, errors.Wrap(err, "statsd init failed")
	}

	return statter, nil
}

// swapStatter installs a statter built by Init, publishing the config it was built
// from in the same critical section and closing the statter it replaces.
//
// Client and config go in together because the reporting path reads both under one
// clientMux.RLock - JoinTags picks the tag format out of the config - so publishing
// the config first would let a report format Telegraf tags for a Datadog client
// that has not been replaced yet, and would leave that mismatch behind for good if
// construction then failed.
//
// Closing the replaced statter matters too: Init used to overwrite the
// package-level client without closing it, which leaked the old client's socket and
// flush goroutine on every re-init - harmless when Init ran once per process, a slow
// leak now that the refresh loop swaps repeatedly.
//
// The returned generation and channel are what the caller's refresh loop runs under:
// the loop stops when the channel closes, which happens on the next swap.
func swapStatter(statter Statter, cfg *StatterConfig) (uint64, <-chan struct{}) {
	clientMux.Lock()
	previous := client
	client = statter
	setCurrentConfig(cfg)
	gen := initGeneration.Add(1)

	// Retire the loops belonging to the generation being replaced, then hand the
	// new one its own signal.
	close(generationSuperseded)
	generationSuperseded = make(chan struct{})
	superseded := generationSuperseded
	clientMux.Unlock()

	if previous != nil {
		_ = previous.Close()
	}

	return gen, superseded
}

// activeInit returns everything a refresh loop needs to be bound to one Init: the
// config it should rebuild from, the generation that config was installed under, and
// the channel closed when a later Init supersedes it. All three come out of a single
// lock, so a loop can never be handed the config of one Init and the generation of
// another - which would let it rebuild a stale destination that the generation check
// then waves through as current.
func activeInit() (*StatterConfig, uint64, <-chan struct{}) {
	clientMux.RLock()
	defer clientMux.RUnlock()
	return currentConfig(), initGeneration.Load(), generationSuperseded
}

// refreshStatter installs a rebuilt statter, but only while gen is still the current
// generation. A refresh loop holds the addr, prefix and config of the Init that
// started it; if a later Init has since installed different ones, letting this swap
// through would put the older Init's destination and tag format back in place. It
// reports false when that has happened, which is the loop's signal to stop.
func refreshStatter(statter Statter, gen uint64) bool {
	clientMux.Lock()
	if initGeneration.Load() != gen {
		clientMux.Unlock()
		_ = statter.Close()
		return false
	}

	previous := client
	client = statter
	clientMux.Unlock()

	if previous != nil {
		_ = previous.Close()
	}

	return true
}

// statterNeedsRefresh reports whether this agent talks over a connectionless
// socket that can silently stop being delivered. See startStatterRefresh.
//
// OTEL is excluded on purpose: it exports over gRPC/HTTP, which detects a broken
// connection and redials on its own, and rebuilding its exporter on a timer would
// throw away batched points for no benefit.
func statterNeedsRefresh(agent string) bool {
	return agent == DatadogAgent || agent == TelegrafAgent
}

// startStatterRefresh periodically rebuilds the statsd client.
//
// The UDP statters resolve their address and dial ONCE, at construction, and then
// hold that socket for the lifetime of the process. Nothing in the write path can
// notice that the socket has stopped being delivered, because there is nothing to
// notice: sends to a dead peer succeed locally, and a connectionless socket gets
// no signal back. So a process that outlives the agent it was pointed at goes on
// "reporting" into a hole, silently, forever.
//
// That is not hypothetical. On 2026-09-02 every one of the 46 statsd-emitting pods
// on ovh-prod-mainnet-eu that had started before its node's otel-agent was replaced
// was found to be reporting nothing - the agents' statsd receivers had accepted zero
// points in 18h, while the pods logged no errors and stayed healthy. The metrics only
// came back when the pods were restarted, by hand, one deployment at a time.
//
// Re-resolving DNS would not have caught it: the Service ClusterIP never changed,
// only the pod behind it. Rebuilding the socket is what recovers, so that is what
// this does. It is cheap - a UDP dial, no handshake - and the old client is closed,
// which flushes whatever it had buffered.
//
// The loop belongs to the Init generation named by gen and stops as soon as a later
// Init supersedes it - immediately, on the superseded signal, rather than at the next
// tick - so it can never put an earlier Init's destination or tag format back in
// place, nor dial the old destination once more on its way out.
func startStatterRefresh(ctx context.Context, addr, prefix string, cfg *StatterConfig, interval time.Duration, gen uint64, superseded <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-superseded:
			// A later Init owns the client now and started its own refresh for it.
			return
		case <-t.C:
			statter, err := newStatter(addr, prefix, cfg)
			if err != nil {
				// Keep serving with the existing client. A refresh that cannot build a
				// replacement is strictly better off leaving the old one in place: it
				// might still work, and a nil client silently drops every metric.
				log.WithError(err).Warningln("metrics client refresh failed, keeping the current one")
				continue
			}

			// The generation check is still needed: an Init can land between the
			// tick and the swap, after this loop has read the signal as open.
			if !refreshStatter(statter, gen) {
				return
			}
		}
	}
}

// StartRefresh starts the periodic statsd client rebuild for callers that use Init
// directly instead of InitService, which starts it for them. Call it once, after a
// successful Init, with the same addr and prefix; it returns immediately and the
// loop stops when ctx is done.
//
// This exists because most services still on the plain Init path predate
// InitService, and they are exactly the ones that were found reporting into dead
// sockets. Passing interval <= 0 uses DefaultRefreshInterval.
//
// It is a no-op when Init has not run, when metrics are mocked, and for agents that
// manage their own connection - see statterNeedsRefresh.
func StartRefresh(ctx context.Context, addr, prefix string, interval time.Duration) {
	startRefresh(ctx, addr, prefix, interval)
}

// startRefresh is StartRefresh with a handle on the loop it starts: it returns a
// channel closed once the loop has exited, or nil when there was no loop to start.
// The tests join on it, so a rebuild can never land after the test that started it
// has already reset the package globals.
func startRefresh(ctx context.Context, addr, prefix string, interval time.Duration) <-chan struct{} {
	cfg, gen, superseded := activeInit()

	if cfg.Agent == "" || cfg.MockingEnabled || !statterNeedsRefresh(cfg.Agent) {
		return nil
	}

	if interval <= 0 {
		interval = DefaultRefreshInterval
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		startStatterRefresh(ctx, addr, prefix, cfg, interval, gen, superseded)
	}()

	return done
}

type ServiceConfig struct {
	Disabled             bool
	ServiceName          string
	AgentID              string
	AgentAddress         string
	MetricsPrefix        string
	EnvName              string
	MockingEnabled       bool
	MockingThreshold     time.Duration
	MixPanelEnabled      bool
	MixPanelProjectToken string
	OTelInsecure         bool
	OTelUseCounters      bool
	TracingEnabled       bool
	RetryInitialInterval time.Duration

	// RefreshInterval is how often the UDP statsd client is rebuilt so it cannot be
	// left holding a socket that no longer reaches the agent. See startStatterRefresh
	// for why that happens and why nothing in the write path can detect it.
	//
	// Defaults to DefaultRefreshInterval. Set to a negative value to switch the
	// refresh off entirely; it is on by default because the failure it prevents is
	// silent, and a service that has stopped reporting looks exactly like a service
	// with nothing to report.
	RefreshInterval time.Duration
}

func (c ServiceConfig) normalizePrefix() string {
	return strings.TrimRight(c.MetricsPrefix, ".") + "."
}

func InitService(ctx context.Context, cfg ServiceConfig) (func(timeout time.Duration), error) {
	closeFn := func(time.Duration) {}
	if err := cfg.Validate(); err != nil {
		return closeFn, err
	}
	if cfg.Disabled {
		return closeFn, nil
	}

	if cfg.RetryInitialInterval <= 0 {
		cfg.RetryInitialInterval = 10 * time.Second
	}

	for {
		hostname, _ := os.Hostname()
		err := Init(cfg.AgentAddress, cfg.normalizePrefix(), &StatterConfig{
			Agent:                  cfg.AgentID,
			EnvName:                cfg.EnvName,
			HostName:               hostname,
			MockingEnabled:         cfg.MockingEnabled,
			MockingThreshold:       cfg.MockingThreshold,
			MixPanelEnabled:        cfg.MixPanelEnabled,
			MixPanelProjectToken:   cfg.MixPanelProjectToken,
			OTELInsecure:           cfg.OTelInsecure,
			OTELUseCounterForCount: cfg.OTelUseCounters,
			TracingEnabled:         cfg.TracingEnabled,
			DefaultTags:            []interface{}{"service.name", cfg.ServiceName},
		})
		if err != nil {
			log.WithError(err).Warningf("metrics init failed, will retry in %s seconds", cfg.RetryInitialInterval)
			select {
			case <-ctx.Done():
				return closeFn, nil
			case <-time.After(cfg.RetryInitialInterval):
			}
			continue
		}
		log.Debugf("metrics %s client initialized at %s with prefix %s (mocking %v)",
			cfg.AgentID, cfg.AgentAddress, cfg.normalizePrefix(), cfg.MockingEnabled)
		break
	}

	if refresh := cfg.refreshInterval(); refresh > 0 {
		StartRefresh(ctx, cfg.AgentAddress, cfg.normalizePrefix(), refresh)
	}

	return CloseWithTimeout, nil
}

// refreshInterval resolves the configured refresh cadence: unset means the default,
// negative means the caller has deliberately turned it off.
func (c ServiceConfig) refreshInterval() time.Duration {
	if c.RefreshInterval == 0 {
		return DefaultRefreshInterval
	}
	return c.RefreshInterval
}

func StartMixPanel(projectToken string) {
	clientMux.Lock()
	defer clientMux.Unlock()
	mixPanelClient = mixpanel.NewApiClient(projectToken)
}

func setupProfiler(cfg *StatterConfig) error {
	runtime.SetMutexProfileFraction(5)
	runtime.SetBlockProfileRate(5)

	err := profiler.Start(
		profiler.WithService(cfg.Prefix),
		profiler.WithEnv(cfg.EnvName),
		profiler.WithHostname(cfg.HostName),
		profiler.WithVersion(cfg.Version),
		profiler.WithProfileTypes(
			profiler.CPUProfile,
			profiler.HeapProfile,
			profiler.BlockProfile,
			profiler.MutexProfile,
		),
	)
	if err != nil {
		return errors.Wrap(err, "profiler start failed")
	}
	return nil
}

func checkConfig(cfg *StatterConfig) *StatterConfig {
	if cfg == nil {
		cfg = &StatterConfig{}
	}
	if cfg.StuckFunctionTimeout < time.Second {
		cfg.StuckFunctionTimeout = 5 * time.Minute
	}
	if len(cfg.EnvName) == 0 {
		cfg.EnvName = "local"
	}
	return cfg
}

func errHandler(err error) {
	log.WithError(err).Errorln("statsd error")
}

func newMockStatter(cfg *StatterConfig) Statter {
	return &mockStatter{
		l: log.WithFields(log.Fields{
			"module": "mock_statter",
		}),
		threshold: cfg.MockingThreshold,
	}
}

type mockStatter struct {
	l         log.Logger
	threshold time.Duration
}

func (s *mockStatter) Count(name string, value int64, tags []string, rate float64) error {
	s.l.WithFields(s.withTagFields(tags)).Debugf("Count %s: %v", name, value)
	return nil
}

func (s *mockStatter) Incr(name string, tags []string, rate float64) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.WithFields(s.withTagFields(tags)).Debugf("Incr %s", name)
	return nil
}

func (s *mockStatter) Decr(name string, tags []string, rate float64) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.WithFields(s.withTagFields(tags)).Debugf("Decr %s", name)
	return nil
}

func (s *mockStatter) Gauge(name string, value float64, tags []string, rate float64) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.WithFields(s.withTagFields(tags)).Debugf("Gauge %s: %v", name, value)
	return nil
}

func (s *mockStatter) Timing(name string, value time.Duration, tags []string, rate float64) error {
	if value > s.threshold {
		s.l.WithFields(s.withTagFields(tags)).Debugf("Timing %s: %v", name, value)
	}
	return nil
}

func (s *mockStatter) Histogram(name string, value float64, tags []string, rate float64) error {
	if value > float64(s.threshold.Milliseconds()) {
		s.l.WithFields(s.withTagFields(tags)).Debugf("Histogram %s: %v", name, value)
	}
	return nil
}

func (s *mockStatter) Unique(bucket string, value string) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.Debugf("Unique %s: %v", bucket, value)
	return nil
}

func (s *mockStatter) Close() error {
	s.l.Debugf("closed at %s", time.Now())
	return nil
}

func (s *mockStatter) withTagFields(tags []string) log.Fields {
	fields := make(log.Fields)
	for i := 0; i < len(tags); i += 2 {
		if i+1 >= len(tags) { // protect against odd number of tags
			break
		}
		fields[tags[i]] = tags[i+1]
	}
	return fields
}
