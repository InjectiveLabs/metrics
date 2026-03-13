package metrics

import "time"

type Config struct {
	Endpoint         string        `mapstructure:"endpoint"`           // gRPC OTEL receiver endpoint, e.g. localhost:4317
	InsecureEndpoint bool          `mapstructure:"insecure-endpoint"`  // whether to use TLS during Endpoint connection
	ExportInterval   time.Duration `mapstructure:"export-interval"`    // time interval between metric exports, default: 10s
	StuckFuncTimeout time.Duration `mapstructure:"stuck-func-timeout"` // timeout before FuncTiming marks the function as stuck and ends the span. Set to 0 (default) to disable timeouts completely.

	MetricsEnabled bool `mapstructure:"metrics-enabled"` // whether metrics collections should be enabled
	TracingEnabled bool `mapstructure:"tracing-enabled"` // whether tracing should be enabled

	// time after which block is considered slow and flight recorder should dump the profile to disk.
	// 0 - disables flight recorder.
	FlightRecorderThreshold time.Duration `mapstructure:"flight-recorder-threshold"`
}

const (
	defaultExportInterval = 10 * time.Second
)

func DefaultConfig() Config {
	cfg := Config{}
	cfg.setDefaults()
	return cfg
}

func (cfg *Config) setDefaults() {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "localhost:4317"
	}
	if cfg.ExportInterval == 0 {
		cfg.ExportInterval = defaultExportInterval
	}
}
