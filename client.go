package metrics

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	dogstatsd "github.com/DataDog/datadog-go/v5/statsd"
	"github.com/alexcesaro/statsd"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	ddotel "gopkg.in/DataDog/dd-trace-go.v1/ddtrace/opentelemetry"
	"gopkg.in/DataDog/dd-trace-go.v1/profiler"
)

const (
	DatadogAgent  = "datadog"
	TelegrafAgent = "telegraf"
)

var (
	ErrUnsupportedAgent = errors.New("unsupported agent type")

	client    Statter
	clientMux = new(sync.RWMutex)
	config    *StatterConfig

	traceProvider *ddotel.TracerProvider
	tracer        trace.Tracer
)

type StatterConfig struct {
	Addr                 string        // localhost:8125
	Prefix               string        // metrics prefix
	Agent                string        // telegraf/datadog
	EnvName              string        // dev/test/staging/prod
	HostName             string        // hostname
	Version              string        // version
	StuckFunctionTimeout time.Duration // stuck time
	MockingEnabled       bool          // whether to enable mock statter, which only produce logs
	Disabled             bool          // whether to disable metrics completely
	TracingEnabled       bool          // whether DataDog tracing should be enabled (via OpenTelemetry)
	ProfilingEnabled     bool          // whether Datadog profiling should be enabled

}

func (m *StatterConfig) BaseTags() []string {
	var baseTags []string

	switch m.Agent {

	case DatadogAgent:
		if len(config.EnvName) > 0 {
			baseTags = append(baseTags, "env:"+config.EnvName)
		}
		if len(config.HostName) > 0 {
			baseTags = append(baseTags, "machine:"+config.HostName)
		}
	// telegraf by default
	default:
		if len(config.EnvName) > 0 {
			baseTags = append(baseTags, "env", config.EnvName)
		}
		if len(config.HostName) > 0 {
			baseTags = append(baseTags, "machine", config.HostName)
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

func Close() {
	clientMux.RLock()
	defer clientMux.RUnlock()
	if client == nil {
		return
	}
	if traceProvider != nil {
		traceProvider.Shutdown()
	}
	client.Close()
}

func Disable() {
	config = checkConfig(nil)
	clientMux.Lock()
	client = newMockStatter(true)
	clientMux.Unlock()
	tracer = nil
}

func InitWithConfig(cfg *StatterConfig) error {
	return Init(cfg.Addr, cfg.Prefix, cfg)
}

func Init(addr string, prefix string, cfg *StatterConfig) error {
	if cfg.Disabled {
		Disable()
		return nil
	}

	config = checkConfig(cfg)
	if config.MockingEnabled {
		// init a mock statter instead of real statsd client
		clientMux.Lock()
		client = newMockStatter(false)
		clientMux.Unlock()
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
	default:
		return ErrUnsupportedAgent
	}

	if err != nil {
		err = errors.Wrap(err, "statsd init failed")
		return err
	}
	clientMux.Lock()
	client = statter
	clientMux.Unlock()

	// OpenTelemetry tracing via DataDog provider
	if cfg.Agent == DatadogAgent && cfg.TracingEnabled {
		traceProvider = ddotel.NewTracerProvider()
		otel.SetTracerProvider(traceProvider)
		tracer = otel.Tracer("")
	}

	if cfg.Agent == DatadogAgent && cfg.ProfilingEnabled {
		err = setupProfiler(cfg)
		if err != nil {
			return err
		}
	}

	return nil
}

func setupProfiler(cfg *StatterConfig) error {
	runtime.SetMutexProfileFraction(5)
	runtime.SetBlockProfileRate(5)

	err := profiler.Start(
		profiler.WithService(cfg.Prefix),
		profiler.WithEnv(cfg.EnvName),
		profiler.WithVersion(cfg.Version),
		profiler.WithTags("hostname:"+cfg.HostName),
		profiler.WithProfileTypes(
			profiler.CPUProfile,
			profiler.HeapProfile,
			profiler.MutexProfile,
			profiler.BlockProfile,
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
	fmt.Printf("statsd error, err: %v\n", err)
}

func newMockStatter(noop bool) Statter {
	return &mockStatter{}
}

type mockStatter struct {
}

func (s *mockStatter) Count(name string, value int64, tags []string, rate float64) error {
	return nil
}

func (s *mockStatter) Incr(name string, tags []string, rate float64) error {
	return nil
}

func (s *mockStatter) Decr(name string, tags []string, rate float64) error {
	return nil
}

func (s *mockStatter) Gauge(name string, value float64, tags []string, rate float64) error {
	return nil
}

func (s *mockStatter) Timing(name string, value time.Duration, tags []string, rate float64) error {
	return nil
}

func (s *mockStatter) Histogram(name string, value float64, tags []string, rate float64) error {
	return nil
}

func (s *mockStatter) Unique(bucket string, value string) error {
	return nil
}

func (s *mockStatter) Close() error {
	return nil
}
