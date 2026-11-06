package metrics

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TraceExportStats counts the spans that ended and what the exporter did with
// them. The SDK's batch processor drops spans silently when its queue is
// full, so Ended minus Exported minus ExportFailed is the drop count plus
// what sits in the queue, at most the queue size. After Shutdown the queue
// is empty and the difference is exact.
type TraceExportStats struct {
	Ended        uint64 // spans ended, before batching
	Exported     uint64 // spans the exporter accepted
	ExportFailed uint64 // spans the exporter returned an error for
}

// Metric names the stats are exported under when metrics are enabled.
const (
	TraceSpansEndedMetric        = "trace.spans.ended"
	TraceSpansExportedMetric     = "trace.spans.exported"
	TraceSpansExportFailedMetric = "trace.spans.export_failed"
)

type traceExportCounters struct {
	ended    atomic.Uint64
	exported atomic.Uint64
	failed   atomic.Uint64
}

func (counters *traceExportCounters) stats() TraceExportStats {
	if counters == nil {
		return TraceExportStats{}
	}
	return TraceExportStats{
		Ended:        counters.ended.Load(),
		Exported:     counters.exported.Load(),
		ExportFailed: counters.failed.Load(),
	}
}

// TraceExportStats returns the span export counters; zero when tracing is
// off.
func (ms *Metrics) TraceExportStats() TraceExportStats {
	if ms == nil {
		return TraceExportStats{}
	}
	return ms.traceExport.stats()
}

// countingSpanProcessor counts the spans handed to the batch processor.
type countingSpanProcessor struct {
	sdktrace.SpanProcessor
	counters *traceExportCounters
}

func (processor countingSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	processor.counters.ended.Add(1)
	processor.SpanProcessor.OnEnd(span)
}

// countingSpanExporter counts the spans the batch processor hands to the
// exporter, by the exporter's verdict.
type countingSpanExporter struct {
	sdktrace.SpanExporter
	counters *traceExportCounters
}

func (exporter countingSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := exporter.SpanExporter.ExportSpans(ctx, spans)
	if err != nil {
		exporter.counters.failed.Add(uint64(len(spans)))
		return err
	}
	exporter.counters.exported.Add(uint64(len(spans)))
	return nil
}

// newTracerProviderWithExporter batches spans to exporter, counting them in
// counters on the way in and out.
func newTracerProviderWithExporter(
	exporter sdktrace.SpanExporter,
	res *resource.Resource,
	counters *traceExportCounters,
	batchOptions ...sdktrace.BatchSpanProcessorOption,
) *sdktrace.TracerProvider {
	batcher := sdktrace.NewBatchSpanProcessor(countingSpanExporter{SpanExporter: exporter, counters: counters}, batchOptions...)
	return sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(countingSpanProcessor{SpanProcessor: batcher, counters: counters}),
		sdktrace.WithResource(res),
	)
}

// registerTraceExportMetrics exports the counters through the metrics
// provider, read on every collection.
func (ms *Metrics) registerTraceExportMetrics() error {
	scope := ms.meterProvider.Meter("metrics")
	ended, err := scope.Int64ObservableCounter(TraceSpansEndedMetric, metric.WithDescription("spans ended, before batching"))
	if err != nil {
		return fmt.Errorf("%s: %w", TraceSpansEndedMetric, err)
	}
	exported, err := scope.Int64ObservableCounter(TraceSpansExportedMetric, metric.WithDescription("spans the exporter accepted"))
	if err != nil {
		return fmt.Errorf("%s: %w", TraceSpansExportedMetric, err)
	}
	failed, err := scope.Int64ObservableCounter(TraceSpansExportFailedMetric, metric.WithDescription("spans the exporter returned an error for"))
	if err != nil {
		return fmt.Errorf("%s: %w", TraceSpansExportFailedMetric, err)
	}
	_, err = scope.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := ms.traceExport.stats()
		observer.ObserveInt64(ended, int64(stats.Ended))         //nolint:gosec // counters fit
		observer.ObserveInt64(exported, int64(stats.Exported))   //nolint:gosec // counters fit
		observer.ObserveInt64(failed, int64(stats.ExportFailed)) //nolint:gosec // counters fit
		return nil
	}, ended, exported, failed)
	if err != nil {
		return fmt.Errorf("trace export metrics callback: %w", err)
	}
	return nil
}
