package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// blockingExporter holds every export until released, as a receiver that
// stopped reading would.
type blockingExporter struct {
	tracetest.InMemoryExporter
	release chan struct{}
}

func (exporter *blockingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	<-exporter.release
	return exporter.InMemoryExporter.ExportSpans(ctx, spans)
}

type failingExporter struct{ tracetest.InMemoryExporter }

func (*failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("receiver unavailable")
}

func countedProvider(t *testing.T, exporter sdktrace.SpanExporter, options ...sdktrace.BatchSpanProcessorOption) (*sdktrace.TracerProvider, *traceExportCounters) {
	t.Helper()
	counters := &traceExportCounters{}
	provider := newTracerProviderWithExporter(exporter, resource.Empty(), counters, options...)
	return provider, counters
}

func endSpans(provider *sdktrace.TracerProvider, count int) {
	tracer := provider.Tracer("trace-export-test")
	for range count {
		_, span := tracer.Start(context.Background(), "span")
		span.End()
	}
}

func TestTraceExportStatsCountExportedSpans(t *testing.T) {
	t.Parallel()
	provider, counters := countedProvider(t, tracetest.NewInMemoryExporter())
	endSpans(provider, 3)
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := counters.stats(); stats != (TraceExportStats{Ended: 3, Exported: 3}) {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestTraceExportStatsCountFailedExports(t *testing.T) {
	t.Parallel()
	provider, counters := countedProvider(t, &failingExporter{})
	endSpans(provider, 3)
	_ = provider.Shutdown(context.Background()) // reports the export error
	if stats := counters.stats(); stats != (TraceExportStats{Ended: 3, ExportFailed: 3}) {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// The batch processor drops silently when its queue is full; the counters
// show it as ended spans that were neither exported nor failed.
func TestTraceExportStatsRevealQueueDrops(t *testing.T) {
	t.Parallel()
	exporter := &blockingExporter{release: make(chan struct{})}
	provider, counters := countedProvider(t, exporter,
		sdktrace.WithMaxQueueSize(1), sdktrace.WithMaxExportBatchSize(1), sdktrace.WithBatchTimeout(time.Hour))
	endSpans(provider, 10)
	close(exporter.release)
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := counters.stats()
	if stats.Ended != 10 || stats.ExportFailed != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	if dropped := stats.Ended - stats.Exported; dropped == 0 || stats.Exported == 0 {
		t.Fatalf("expected some spans exported and some dropped: %+v", stats)
	}
}

func TestTraceExportStatsZeroWithoutTracing(t *testing.T) {
	t.Parallel()
	var none *Metrics
	if none.TraceExportStats() != (TraceExportStats{}) || (&Metrics{}).TraceExportStats() != (TraceExportStats{}) {
		t.Fatal("expected zero stats")
	}
}

func TestTraceExportMetricsObserveCounters(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	providers := &Metrics{
		cfg:           Config{MetricsEnabled: true, TracingEnabled: true},
		meterProvider: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		traceExport:   &traceExportCounters{},
	}
	t.Cleanup(func() { _ = providers.meterProvider.Shutdown(context.Background()) })
	if err := providers.registerTraceExportMetrics(); err != nil {
		t.Fatal(err)
	}
	providers.traceExport.ended.Add(5)
	providers.traceExport.exported.Add(3)
	providers.traceExport.failed.Add(1)

	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	observed := make(map[string]int64)
	for _, scope := range data.ScopeMetrics {
		for _, item := range scope.Metrics {
			sum, ok := item.Data.(metricdata.Sum[int64])
			if !ok || len(sum.DataPoints) != 1 {
				t.Fatalf("%s: unexpected data %T", item.Name, item.Data)
			}
			observed[item.Name] = sum.DataPoints[0].Value
		}
	}
	expected := map[string]int64{TraceSpansEndedMetric: 5, TraceSpansExportedMetric: 3, TraceSpansExportFailedMetric: 1}
	if len(observed) != len(expected) {
		t.Fatalf("unexpected metrics: %v", observed)
	}
	for name, value := range expected {
		if observed[name] != value {
			t.Fatalf("%s: got %d, want %d", name, observed[name], value)
		}
	}
}
