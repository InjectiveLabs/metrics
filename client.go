package metrics

import (
	"runtime"
	"sync"
	"time"

	dogstatsd "github.com/DataDog/datadog-go/v5/statsd"
	log "github.com/InjectiveLabs/suplog"
	"github.com/alexcesaro/statsd"
	"github.com/pkg/errors"

	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
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
)

type StatterConfig struct {
	Addr                 string        // localhost:8125
	Prefix               string        // metrics prefix
	Agent                string        // telegraf/datadog
	EnvName              string        // dev/test/staging/prod
	HostName             string        // hostname
	Version              string        // version
	StuckFunctionTimeout time.Duration // stuck time
	MockingThreshold     time.Duration // mocking threshold
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
	client.Close()
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
		tracer.Start(
			tracer.WithService(cfg.Prefix),
			tracer.WithEnv(cfg.EnvName),
			tracer.WithHostname(cfg.HostName),
		)
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
