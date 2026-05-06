package metrics

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type otelStatter struct {
	meter         otelmetric.Meter
	meterProvider *sdkmetric.MeterProvider
	prefix        string

	mu             sync.RWMutex
	updownCounters map[string]otelmetric.Int64UpDownCounter
	gauges         map[string]otelmetric.Float64Gauge
	histograms     map[string]otelmetric.Float64Histogram
}

func newOTELResource(baseTags []string) *resource.Resource {
	attrs := []attribute.KeyValue{}
	for _, tag := range baseTags {
		if idx := strings.IndexByte(tag, '='); idx > 0 {
			attrs = append(attrs, attribute.String(tag[:idx], tag[idx+1:]))
		}
	}
	res, err := resource.New(context.Background(), resource.WithAttributes(attrs...))
	if err != nil {
		return resource.Default()
	}
	return res
}

func newOTELStatter(endpoint, prefix string, insecure bool, headers map[string]string, baseTags []string) (Statter, error) {
	ctx := context.Background()

	metricOpts := []otlpmetricgrpc.Option{
		otlpmetricgrpc.WithEndpoint(endpoint),
	}
	if insecure {
		//nolint:staticcheck // WithInsecure is the correct option for self-hosted SigNoz without TLS
		metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
	}
	if len(headers) > 0 {
		metricOpts = append(metricOpts, otlpmetricgrpc.WithHeaders(headers))
	}

	exporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
	if err != nil {
		return nil, err
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(newOTELResource(baseTags)),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	otel.SetMeterProvider(mp)

	return &otelStatter{
		meter:          mp.Meter(prefix),
		meterProvider:  mp,
		prefix:         prefix,
		updownCounters: make(map[string]otelmetric.Int64UpDownCounter),
		gauges:         make(map[string]otelmetric.Float64Gauge),
		histograms:     make(map[string]otelmetric.Float64Histogram),
	}, nil
}

func newOTELTracerProvider(endpoint string, insecure bool, headers map[string]string, baseTags []string) (*sdktrace.TracerProvider, error) {
	ctx := context.Background()

	traceOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
	}
	if insecure {
		//nolint:staticcheck
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
	}
	if len(headers) > 0 {
		traceOpts = append(traceOpts, otlptracegrpc.WithHeaders(headers))
	}

	exp, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, err
	}

	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(newOTELResource(baseTags)),
		sdktrace.WithBatcher(exp),
	), nil
}

// tagsToAttrs parses "key=value" tag strings into OTel attributes.
func (s *otelStatter) tagsToAttrs(tags []string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, len(tags))
	for _, tag := range tags {
		idx := strings.IndexByte(tag, '=')
		if idx <= 0 {
			continue
		}
		attrs = append(attrs, attribute.String(tag[:idx], tag[idx+1:]))
	}
	return attrs
}

func (s *otelStatter) getUpDownCounter(name string) (otelmetric.Int64UpDownCounter, error) {
	fullName := s.prefix + name
	s.mu.RLock()
	c, ok := s.updownCounters[fullName]
	s.mu.RUnlock()
	if ok {
		return c, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok = s.updownCounters[fullName]; ok {
		return c, nil
	}
	c, err := s.meter.Int64UpDownCounter(fullName)
	if err != nil {
		return nil, err
	}
	s.updownCounters[fullName] = c
	return c, nil
}

func (s *otelStatter) getGauge(name string) (otelmetric.Float64Gauge, error) {
	fullName := s.prefix + name
	s.mu.RLock()
	g, ok := s.gauges[fullName]
	s.mu.RUnlock()
	if ok {
		return g, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok = s.gauges[fullName]; ok {
		return g, nil
	}
	g, err := s.meter.Float64Gauge(fullName)
	if err != nil {
		return nil, err
	}
	s.gauges[fullName] = g
	return g, nil
}

func (s *otelStatter) getHistogram(name string) (otelmetric.Float64Histogram, error) {
	fullName := s.prefix + name
	s.mu.RLock()
	h, ok := s.histograms[fullName]
	s.mu.RUnlock()
	if ok {
		return h, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok = s.histograms[fullName]; ok {
		return h, nil
	}
	h, err := s.meter.Float64Histogram(fullName)
	if err != nil {
		return nil, err
	}
	s.histograms[fullName] = h
	return h, nil
}

func (s *otelStatter) Count(name string, value int64, tags []string, rate float64) error {
	c, err := s.getUpDownCounter(name)
	if err != nil {
		return err
	}
	c.Add(context.Background(), value, otelmetric.WithAttributes(s.tagsToAttrs(tags)...))
	return nil
}

func (s *otelStatter) Incr(name string, tags []string, rate float64) error {
	c, err := s.getUpDownCounter(name)
	if err != nil {
		return err
	}
	c.Add(context.Background(), 1, otelmetric.WithAttributes(s.tagsToAttrs(tags)...))
	return nil
}

func (s *otelStatter) Decr(name string, tags []string, rate float64) error {
	c, err := s.getUpDownCounter(name)
	if err != nil {
		return err
	}
	c.Add(context.Background(), -1, otelmetric.WithAttributes(s.tagsToAttrs(tags)...))
	return nil
}

func (s *otelStatter) Gauge(name string, value float64, tags []string, rate float64) error {
	g, err := s.getGauge(name)
	if err != nil {
		return err
	}
	g.Record(context.Background(), value, otelmetric.WithAttributes(s.tagsToAttrs(tags)...))
	return nil
}

func (s *otelStatter) Timing(name string, value time.Duration, tags []string, rate float64) error {
	h, err := s.getHistogram(name)
	if err != nil {
		return err
	}
	// OTel convention: duration histograms use seconds
	h.Record(context.Background(), value.Seconds(), otelmetric.WithAttributes(s.tagsToAttrs(tags)...))
	return nil
}

func (s *otelStatter) Histogram(name string, value float64, tags []string, rate float64) error {
	h, err := s.getHistogram(name)
	if err != nil {
		return err
	}
	h.Record(context.Background(), value, otelmetric.WithAttributes(s.tagsToAttrs(tags)...))
	return nil
}

func (s *otelStatter) Close() error {
	return s.meterProvider.Shutdown(context.Background())
}
