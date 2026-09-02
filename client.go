package metrics

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
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
	config    *StatterConfig

	traceProviderShutdownFn func(ctx context.Context) error
	tracer                  trace.Tracer
	mixPanelClient          *mixpanel.ApiClient
)

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
		if len(config.EnvName) > 0 {
			baseTags = append(baseTags, "env:"+config.EnvName)
		}
		if len(config.HostName) > 0 {
			baseTags = append(baseTags, "machine:"+config.HostName)
		}
		for k, v := range defaultTags {
			baseTags = append(baseTags, k+":"+v)
		}
	case OTELAgent:
		if len(config.EnvName) > 0 {
			baseTags = append(baseTags, "env="+config.EnvName)
		}
		if len(config.HostName) > 0 {
			baseTags = append(baseTags, "machine="+config.HostName)
		}
		for k, v := range defaultTags {
			baseTags = append(baseTags, k+"="+v)
		}
	// telegraf by default
	default:
		if len(config.EnvName) > 0 {
			baseTags = append(baseTags, "env", config.EnvName)
		}
		if len(config.HostName) > 0 {
			baseTags = append(baseTags, "machine", config.HostName)
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
	config = checkConfig(cfg)
	if config.MockingEnabled {
		// init a mock statter instead of real statsd client
		clientMux.Lock()
		client = newMockStatter(cfg)
		clientMux.Unlock()
		return nil
	}

	statter, err := newStatter(addr, prefix, cfg)
	if err != nil {
		return err
	}

	swapStatter(statter)

	// OpenTelemetry tracing via DataDog provider
	if cfg.Agent == DatadogAgent && cfg.TracingEnabled {
		traceProvider := ddotel.NewTracerProvider()
		otel.SetTracerProvider(traceProvider)
		tracer = otel.Tracer("")
		traceProviderShutdownFn = func(_ context.Context) error {
			return traceProvider.Shutdown()
		}
	} else if cfg.Agent == OTELAgent && cfg.TracingEnabled {
		traceProvider, err := newOTELTracerProvider(addr, cfg.OTELInsecure, cfg.OTELHeaders, config.BaseTags())
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

	if cfg.Agent == DatadogAgent && cfg.ProfilingEnabled {
		err = setupProfiler(cfg)
		if err != nil {
			return err
		}
	}

	if cfg.MixPanelEnabled {
		StartMixPanel(cfg.MixPanelProjectToken)
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
			dogstatsd.WithTags(config.BaseTags()),
		)

	case TelegrafAgent:
		statter, err = newTelegrafStatter(
			statsd.Address(addr),
			statsd.Prefix(prefix),
			statsd.ErrorHandler(errHandler),
			statsd.TagsFormat(statsd.InfluxDB),
			statsd.Tags(config.BaseTags()...),
		)

	case OTELAgent:
		statter, err = newOTELStatter(
			addr,
			prefix,
			cfg.OTELInsecure,
			cfg.OTELHeaders,
			config.BaseTags(),
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

// swapStatter installs a new statter and closes the one it replaces. Init used to
// overwrite the package-level client without closing it, which leaked the old
// client's socket and flush goroutine on every re-init - harmless when Init ran
// once per process, a slow leak now that the refresh loop calls it repeatedly.
func swapStatter(statter Statter) {
	clientMux.Lock()
	previous := client
	client = statter
	clientMux.Unlock()

	if previous != nil {
		_ = previous.Close()
	}
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
func startStatterRefresh(ctx context.Context, addr, prefix string, cfg *StatterConfig, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
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

			swapStatter(statter)
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
	// Read straight from the package config, as the rest of the package does. Init
	// writes it unguarded, so this is only safe in the documented order - after Init
	// has returned - which is also the only order in which it makes sense to call.
	cfg := config

	if cfg == nil || cfg.MockingEnabled || !statterNeedsRefresh(cfg.Agent) {
		return
	}

	if interval <= 0 {
		interval = DefaultRefreshInterval
	}

	go startStatterRefresh(ctx, addr, prefix, cfg, interval)
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

	if refresh := cfg.refreshInterval(); refresh > 0 && !cfg.MockingEnabled && statterNeedsRefresh(cfg.AgentID) {
		go startStatterRefresh(ctx, cfg.AgentAddress, cfg.normalizePrefix(), config, refresh)
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
