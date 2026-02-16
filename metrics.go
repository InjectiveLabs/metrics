package metrics

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	Endpoint         string        // gRPC OTEL receiver endpoint, e.g. localhost:4317 or set to empty to disable metrics completely
	InsecureEndpoint bool          // whether to use TLS during Endpoint connection
	ExportInterval   time.Duration // time interval between metric exports, default: 10s

	MetricsEnabled bool // whether metrics collections should be enabled
	TracingEnabled bool // whether tracing should be enabled
}

// Metrics is a prodiver for telemetry meters.
// Call Meter() to acquire a scoped meter for your instrumented piece of code.
type Metrics struct {
	cfg Config

	meterProvider  *sdkmetric.MeterProvider
	tracerProvider *sdktrace.TracerProvider

	disabled bool
}

// Meter is a scoped metering instance and is used to send metrics
// via different instruments acquired from it.
// Meter has unqiue scope name associated with it and a set of base tags that
// are attached to every metrics being sent through it.
// You can derive SubMeter()-s from it, scope and base tags of one will be merged with the parent,
// use it for sub-modules of your app.
// It also exposes embedded raw OTEL Meter interface so you can use OTEL instruments directly (Int64Counter, etc.)
type Meter interface {
	metric.Meter

	// SubMeter creates a new Meter derived from current MEter instance.
	// Scope name and base tags of sub-meter are then concatenated with parent's ones.
	SubMeter(subScopeName string, subBaseTags ...TagAttr) Meter

	// Count adds an int64 to existing (or new) counter with the name.
	// Appends tags to Meter baseTags to submit them with the counter new value.
	Count(name string, numToAdd int64, tags ...TagAttr) error

	// Gauge records new gauge int64 reading.
	// Appends tags to Meter baseTags to submit them with the gauge new value.
	Gauge(name string, value int64, tags ...TagAttr) error

	// Histogram records new value into set of values distributed over time, e.g. timer value in ms.
	// Appends tags to Meter baseTags to submit them with the gauge new value.
	Histogram(name string, value int64, tags ...TagAttr) error

	// Func inrements number of times function was called as "func.called" counter.
	// Function name is stored in "func_name" tag.
	Func(fn string, tags ...TagAttr)

	// FuncTiming reports function call and execution time in ms.
	// Fucntion name is stored as "func_name" tag.
	// Uses "func.timing" histogram instrument.
	// Usage: defer metrics.FuncTiming(&sdkCtx, "EndBlocker")()
	//
	// This function overwrites the context.COntext inside of ctx with a copy with attached tracing span value
	// using ContextPtr() method
	FuncTiming(ctx ContextPointer, fn string, tags ...TagAttr) StopFn

	// FuncTimingCtx reports function call and execution time in ms.
	// Fucntion name is stored as "func_name" tag.
	// Uses "func.timing" histogram instrument.
	// Usage:
	// spanCtx, stop := metrics.FuncTimingCtx(ctx, "EndBlocker")()
	// defer stop()
	//
	// WARNING: DO NOT USE IT FOR sdk.Context wrapped as context.Context, use FuncTiming() instead
	FuncTimingCtx(ctx context.Context, fn string, tags ...TagAttr) (context.Context, StopFn)
}

type meter struct {
	metric.Meter

	name     string
	metrics  *Metrics
	baseTags []TagAttr

	counters   sync.Map // string => metric.Int64Counter
	gauges     sync.Map // string => metric.Int64Gauge
	histograms sync.Map // string => Int64Histogram

	tracer trace.Tracer
}

type StopFn func()

// NewMetrics creates a metrics provider that can be used to spawn Meters from.
// Usually only one instance4 of Metrics per app is used, and multiple Meters per application scope.
func NewMetrics(cfg Config, resourceAttributes ...TagAttr) (*Metrics, error) {
	ms := &Metrics{
		cfg: cfg,
	}

	if ms.cfg.Endpoint == "" || (!ms.cfg.MetricsEnabled && !ms.cfg.TracingEnabled) {
		ms.disabled = true
		return ms, nil
	}

	var err error

	if ms.cfg.MetricsEnabled {
		ms.meterProvider, err = newMeterProvider(&cfg, resourceAttributes...)
		if err != nil {
			return nil, fmt.Errorf("can't create MetricsProvider: %w", err)
		}
	}

	if ms.cfg.TracingEnabled {
		ms.tracerProvider, err = newTracerProvider(&cfg, resourceAttributes...)
		if err != nil {
			return nil, fmt.Errorf("can't create TracerProvider: %w", err)
		}
	}

	return ms, nil
}

func newMeterProvider(cfg *Config, resourceAttributes ...TagAttr) (*sdkmetric.MeterProvider, error) {
	ctx := context.Background()

	// Create the OTLP exporter
	// It will automatically use OTEL_EXPORTER_OTLP_METRICS_ENDPOINT and headers from env
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.InsecureEndpoint {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	exporter, err := otlpmetricgrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new otlp metric grpc exporter failed: %w", err)
	}

	// Create the resource with attributes from environment (OTEL_RESOURCE_ATTRIBUTES)
	// and default host/process attributes
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithProcess(),
		resource.WithOS(),
		resource.WithContainer(),
		resource.WithAttributes(resourceAttributes...),
	)
	if err != nil {
		return nil, fmt.Errorf("new resource failed: %w", err)
	}

	if cfg.ExportInterval == 0 {
		cfg.ExportInterval = 10 * time.Second
	}

	// Create the MeterProvider with the exporter and resource
	// Set a periodic reader to export metrics every 10 seconds
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(cfg.ExportInterval))),
	)

	return mp, nil
}

// NewMeter return a new meter instance, that has a defined unique scope and is used to provide metrics
func (ms *Metrics) NewMeter(scopeName string, baseTags ...TagAttr) (Meter, error) {
	var m *meter

	if ms.disabled {
		return m, nil
	}

	m = &meter{
		name:     scopeName,
		metrics:  ms,
		baseTags: baseTags,
	}

	if ms.cfg.MetricsEnabled {
		m.Meter = ms.meterProvider.Meter(scopeName)
	}

	if ms.cfg.TracingEnabled {
		m.tracer = ms.tracerProvider.Tracer(scopeName)
	}

	return m, nil
}

func (ms *Metrics) Shutdown() error {
	if ms.cfg.MetricsEnabled {
		if err := ms.meterProvider.Shutdown(context.Background()); err != nil {
			return fmt.Errorf("can't shutdown MeterProvider: %w", err)
		}
	}
	if ms.cfg.TracingEnabled {
		if err := ms.tracerProvider.Shutdown(context.Background()); err != nil {
			return fmt.Errorf("can't shutdown TracerProvider: %w", err)
		}
	}

	return nil
}

// SubMeter creates a new Meter derived from current MEter instance.
// Scope name and base tags of sub-meter are then concatenated with parent's ones.
func (m *meter) SubMeter(subScopeName string, subBaseTags ...TagAttr) Meter {
	if m == nil {
		return nil
	}

	mergedBaseTags := append(append([]TagAttr{}, m.baseTags...), subBaseTags...)

	subMeter := &meter{
		name:     subScopeName,
		metrics:  m.metrics,
		baseTags: mergedBaseTags,
		tracer:   m.tracer, // we re-use parent's tracer to have correct nested spans down from the root span
	}

	if m.metrics.cfg.MetricsEnabled {
		subMeter.Meter = m.metrics.meterProvider.Meter(fmt.Sprintf("%s.%s", m.name, subScopeName))
	}

	return subMeter
}

func (m *meter) getCounter(name string) (metric.Int64Counter, error) {
	c, loaded := m.counters.Load(name)
	if !loaded {
		newC, err := m.Int64Counter(name)
		if err != nil {
			return nil, fmt.Errorf("can't create Int64Counter for Meter: %w", err)
		}
		c, _ = m.counters.LoadOrStore(name, newC)
	}
	return c.(metric.Int64Counter), nil
}

func (m *meter) getGauge(name string) (metric.Int64Gauge, error) {
	g, loaded := m.gauges.Load(name)
	if !loaded {
		newG, err := m.Int64Gauge(name)
		if err != nil {
			return nil, fmt.Errorf("can't create Int64Gauge for Meter: %w", err)
		}
		g, _ = m.gauges.LoadOrStore(name, newG)
	}
	return g.(metric.Int64Gauge), nil
}

func (m *meter) getHistogram(name string) (metric.Int64Histogram, error) {
	h, loaded := m.histograms.Load(name)
	if !loaded {
		newH, err := m.Int64Histogram(name)
		if err != nil {
			return nil, fmt.Errorf("can't create Int64Histogram for Meter: %w", err)
		}
		h, _ = m.gauges.LoadOrStore(name, newH)
	}
	return h.(metric.Int64Histogram), nil
}

// Count adds an int64 to existing (or new) counter with the name.
// Appends tags to Meter baseTags to submit them with the counter new value.
func (m *meter) Count(name string, numToAdd int64, tags ...TagAttr) error {
	if m == nil || m.Meter == nil {
		return nil
	}

	c, err := m.getCounter(name)
	if err != nil {
		return fmt.Errorf("can't get %s counter: %w", name, err)
	}

	c.Add(context.Background(), numToAdd, metric.WithAttributes(m.getMergedTags(tags...)...))

	return nil
}

// Gauge records new gauge int64 reading.
// Appends tags to Meter baseTags to submit them with the gauge new value.
func (m *meter) Gauge(name string, value int64, tags ...TagAttr) error {
	if m == nil || m.Meter == nil {
		return nil
	}

	g, err := m.getGauge(name)
	if err != nil {
		return fmt.Errorf("can't get %s gauge: %w", name, err)
	}

	g.Record(context.Background(), value, metric.WithAttributes(m.getMergedTags(tags...)...))

	return nil
}

// Histogram records new value into set of values distributed over time, e.g. timer value in ms.
// Appends tags to Meter baseTags to submit them with the gauge new value.
func (m *meter) Histogram(name string, value int64, tags ...TagAttr) error {
	if m == nil || m.Meter == nil {
		return nil
	}

	h, err := m.getHistogram(name)
	if err != nil {
		return fmt.Errorf("can't get %s histogram: %w", name, err)
	}

	h.Record(context.Background(), value, metric.WithAttributes(m.getMergedTags(tags...)...))

	return nil
}

// Func inrements number of times function was called as "func.called" counter.
// Function name is stored in "func_name" tag.
func (m *meter) Func(fn string, tags ...TagAttr) {
	if m == nil || m.Meter == nil {
		return
	}

	m.Count("func.called", 1, append([]TagAttr{Tag("func_name", fn)}, tags...)...) //nolint:errcheck
}
