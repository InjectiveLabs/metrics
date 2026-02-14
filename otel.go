package metrics

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type openTelemetryStatter struct {
	meterProvider *sdkmetric.MeterProvider
	meter         metric.Meter
	baseTags      []attribute.KeyValue

	mu         sync.Mutex
	counters   map[string]metric.Int64Counter
	histograms map[string]metric.Float64Histogram
	gauges     map[string]metric.Float64ObservableGauge

	// store last gauge values
	gaugeValues sync.Map
}

func newOpenTelemetryStatter(addr, appName string, baseTags []string, opts ...metric.MeterOption) (Statter, error) {
	mp, err := initMeterProvider(addr)
	if err != nil {
		return nil, err
	}

	meter := mp.Meter(appName, opts...)

	return &openTelemetryStatter{
		meterProvider: mp,
		meter:         meter,
		baseTags:      parseTags(baseTags),
		counters:      make(map[string]metric.Int64Counter),
		histograms:    make(map[string]metric.Float64Histogram),
		gauges:        make(map[string]metric.Float64ObservableGauge),
	}, nil
}

func initMeterProvider(addr string) (*sdkmetric.MeterProvider, error) {
	ctx := context.Background()
	// Create the OTLP exporter
	// It will automatically use OTEL_EXPORTER_OTLP_METRICS_ENDPOINT and headers from env
	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(addr), otlpmetricgrpc.WithInsecure())
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
	)
	if err != nil {
		return nil, fmt.Errorf("new resource failed: %w", err)
	}

	// Create the MeterProvider with the exporter and resource
	// Set a periodic reader to export metrics every 10 seconds
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(10*time.Second))),
	)

	// Set the global MeterProvider
	otel.SetMeterProvider(mp)

	return mp, nil
}

func newOtelTracer(addr, name string, opts ...trace.TracerOption) (trace.Tracer, *sdktrace.TracerProvider, error) {
	tp, err := initOtelTraceProvider(addr)
	if err != nil {
		return nil, nil, err
	}

	return tp.Tracer(name, opts...), tp, nil
}

func initOtelTraceProvider(addr string) (*sdktrace.TracerProvider, error) {
	ctx := context.Background()
	// Reads OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_HEADERS from environment
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(addr), otlptracegrpc.WithInsecure())
	if err != nil {
		return nil, err
	}

	// Reads OTEL_SERVICE_NAME from environment and adds host/process/OS attributes
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithOS(),
		resource.WithProcess(),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	// Makes the tracer available to instrumentation libraries
	otel.SetTracerProvider(tp)

	return tp, nil
}

func (o *openTelemetryStatter) Count(bucket string, value int64, tags []string, rate float64) error {
	counter, err := o.getCounter(bucket)
	if err != nil {
		return err
	}
	counter.Add(context.Background(), value, metric.WithAttributes(append(o.baseTags, parseTags(tags)...)...))
	return nil
}

func (o *openTelemetryStatter) Incr(bucket string, tags []string, rate float64) error {
	return o.Count(bucket, 1, tags, rate)
}

func (o *openTelemetryStatter) Decr(bucket string, tags []string, rate float64) error {
	return o.Count(bucket, -1, tags, rate)
}

func (o *openTelemetryStatter) Gauge(bucket string, value float64, tags []string, rate float64) error {
	o.gaugeValues.Store(bucket, value)

	gauge, err := o.getGauge(bucket)
	if err != nil {
		return err
	}

	// registration only happens once
	_, err = o.meter.RegisterCallback(
		func(ctx context.Context, obs metric.Observer) error {
			if v, ok := o.gaugeValues.Load(bucket); ok {
				obs.ObserveFloat64(gauge, v.(float64), metric.WithAttributes(append(o.baseTags, parseTags(tags)...)...))
			}
			return nil
		},
		gauge,
	)

	return err
}

func (o *openTelemetryStatter) Timing(bucket string, value time.Duration, tags []string, rate float64) error {
	hist, err := o.getHistogram(bucket)
	if err != nil {
		return err
	}

	ms := float64(value) / float64(time.Millisecond)
	hist.Record(context.Background(), ms, metric.WithAttributes(append(o.baseTags, parseTags(tags)...)...))
	return nil
}

func (o *openTelemetryStatter) Histogram(bucket string, value float64, tags []string, rate float64) error {
	hist, err := o.getHistogram(bucket)
	if err != nil {
		return err
	}

	hist.Record(context.Background(), value, metric.WithAttributes(append(o.baseTags, parseTags(tags)...)...))
	return nil
}

func (o *openTelemetryStatter) Close() error {
	return o.meterProvider.Shutdown(context.Background())
}

func (o *openTelemetryStatter) getCounter(name string) (metric.Int64Counter, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if c, ok := o.counters[name]; ok {
		return c, nil
	}

	c, err := o.meter.Int64Counter(name)
	if err != nil {
		return nil, err
	}

	o.counters[name] = c
	return c, nil
}

func (o *openTelemetryStatter) getHistogram(name string) (metric.Float64Histogram, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if h, ok := o.histograms[name]; ok {
		return h, nil
	}

	h, err := o.meter.Float64Histogram(name)
	if err != nil {
		return nil, err
	}

	o.histograms[name] = h
	return h, nil
}

func (o *openTelemetryStatter) getGauge(name string) (metric.Float64ObservableGauge, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if g, ok := o.gauges[name]; ok {
		return g, nil
	}

	g, err := o.meter.Float64ObservableGauge(name)
	if err != nil {
		return nil, err
	}

	o.gauges[name] = g
	return g, nil
}

func parseTags(tags []string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(tags))

	for _, t := range tags {
		parts := strings.SplitN(t, "=", 2)
		if len(parts) == 2 {
			attrs = append(attrs, attribute.String(parts[0], parts[1]))
		}
	}

	return attrs
}
