package metrics

import "time"

type Config struct {
	Endpoint         string        // gRPC OTEL receiver endpoint, e.g. localhost:4317 or set to empty to disable metrics completely
	InsecureEndpoint bool          // whether to use TLS during Endpoint connection
	ExportInterval   time.Duration // time interval between metric exports, default: 10s
	StuckFuncTimeout time.Duration // timeout before FuncTiming marks the function as stuck and ends the span. Set to 0 (default) to disable timeouts completely.

	MetricsEnabled bool // whether metrics collections should be enabled
	TracingEnabled bool // whether tracing should be enabled
}

const (
	defaultExportInterval = 10 * time.Second
)

func (cfg *Config) setDefaults() {
	if cfg.ExportInterval == 0 {
		cfg.ExportInterval = defaultExportInterval
	}
}
