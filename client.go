package metrics

import (
	"context"
	"runtime"
	"time"

	"github.com/alexcesaro/statsd"
	"github.com/mixpanel/mixpanel-go"
	"github.com/pkg/errors"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const (
	DatadogAgent       = "datadog"
	TelegrafAgent      = "telegraf"
	OpenTelemetryAgent = "opentelemetry"
)

var (
	ErrUnsupportedAgent = errors.New("unsupported agent type")

	client Statter
	config *StatterConfig

	traceProvider  *sdktrace.TracerProvider
	tracer         trace.Tracer
	mixPanelClient *mixpanel.ApiClient
)

type Statter interface {
	Count(name string, value int64, tags []string, rate float64) error
	Incr(name string, tags []string, rate float64) error
	Decr(name string, tags []string, rate float64) error
	Gauge(name string, value float64, tags []string, rate float64) error
	Timing(name string, value time.Duration, tags []string, rate float64) error
	Histogram(name string, value float64, tags []string, rate float64) error
	Close() error
}

type StatterConfig struct {
	Addr                 string        // localhost:8125
	Prefix               string        // metrics prefix
	Agent                string        // telegraf/datadog
	EnvName              string        // dev/test/staging/prod
	HostName             string        // hostname
	Version              string        // version
	DefaultTags          []interface{} // default tags for all metrics
	StuckFunctionTimeout time.Duration // stuck time
	MockingThreshold     time.Duration // mocking threshold
	MockingEnabled       bool          // whether to enable mock statter, which only produce logs
	Disabled             bool          // whether to disable metrics completely
	TracingEnabled       bool          // whether DataDog tracing should be enabled (via OpenTelemetry)
	ProfilingEnabled     bool          // whether Datadog profiling should be enabled
	MixPanelEnabled      bool          // whether MixPanel should be enabled
	MixPanelProjectToken string        // MixPanel project token
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

	case OpenTelemetryAgent:
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

func Close() {
	if traceProvider != nil {
		traceProvider.Shutdown(context.Background())
	}

	if client != nil {
		client.Close()
	}
}

func Disable() {
	config = checkConfig(nil)
	client = newMockStatter(true)
	tracer = nil
}

func InitWithConfig(cfg *StatterConfig) error {
	return Init(cfg.Addr, cfg.Prefix, cfg)
}

func Init(addr string, prefix string, cfg *StatterConfig) error {
	config = checkConfig(cfg)
	if config.MockingEnabled {
		// init a mock statter instead of real statsd client
		client = newMockStatter(cfg)
		return nil
	}

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

	case OpenTelemetryAgent:
		statter, err = newOpenTelemetryStatter(addr, prefix, config.BaseTags())

	default:
		return ErrUnsupportedAgent
	}

	if err != nil {
		err = errors.Wrap(err, "metrics init failed")
		return err
	}
	client = statter

	// OpenTelemetry tracing
	if cfg.TracingEnabled {
		tracer, traceProvider, err = newOtelTracer(addr, prefix)
		if err != nil {
			return errors.Wrap(err, "tracing init failed")
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
