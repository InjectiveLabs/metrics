package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// flushTestMeter returns a meter whose batch processor only exports on
// flush or shutdown, so what reaches the exporter is what FlushTraces did.
func flushTestMeter(t *testing.T, exporter sdktrace.SpanExporter) Meter {
	t.Helper()
	counters := &traceExportCounters{}
	providers := &Metrics{
		cfg:            Config{TracingEnabled: true},
		tracerProvider: newTracerProviderWithExporter(exporter, resource.Empty(), counters, sdktrace.WithBatchTimeout(time.Hour)),
		traceExport:    counters,
	}
	t.Cleanup(func() { _ = providers.tracerProvider.Shutdown(context.Background()) })
	meter, err := providers.NewMeter("flush-test")
	if err != nil {
		t.Fatal(err)
	}
	return meter
}

func TestFlushTracesExportsQueuedSpans(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	meter := flushTestMeter(t, exporter)
	for range 3 {
		_, span := StartTransactionSpan(context.Background(), meter, []byte("tx"), "tx.stage")
		span.End()
	}
	if got := len(exporter.GetSpans()); got != 0 {
		t.Fatalf("expected the batcher to hold the spans, exporter got %d", got)
	}
	if err := FlushTraces(context.Background(), meter); err != nil {
		t.Fatal(err)
	}
	if got := len(exporter.GetSpans()); got != 3 {
		t.Fatalf("expected 3 flushed spans, got %d", got)
	}

	// The processor keeps working after a flush.
	_, span := StartTransactionSpan(context.Background(), meter, []byte("tx"), "tx.stage")
	span.End()
	if err := FlushTraces(context.Background(), meter); err != nil {
		t.Fatal(err)
	}
	if got := len(exporter.GetSpans()); got != 4 {
		t.Fatalf("expected 4 flushed spans, got %d", got)
	}
}

// A receiver that stopped reading must not hold a crashing process for
// longer than the caller allowed.
func TestFlushTracesBoundedByContext(t *testing.T) {
	t.Parallel()
	exporter := &blockingExporter{release: make(chan struct{})}
	meter := flushTestMeter(t, exporter)
	t.Cleanup(func() { close(exporter.release) }) // runs before the provider shutdown
	_, span := StartTransactionSpan(context.Background(), meter, []byte("tx"), "tx.stage")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := FlushTraces(ctx, meter)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the flush to give up on the deadline, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("flush held the caller for %s", elapsed)
	}
}

func TestFlushTracesNoopWithoutTracing(t *testing.T) {
	t.Parallel()
	if err := FlushTraces(context.Background(), NewNilMeter()); err != nil {
		t.Fatalf("nil meter: %v", err)
	}
	if err := FlushTraces(context.Background(), nil); err != nil {
		t.Fatalf("nil interface: %v", err)
	}
	providers := &Metrics{cfg: Config{}} // tracing off: no tracer provider at all
	meter, err := providers.NewMeter("flush-off")
	if err != nil {
		t.Fatal(err)
	}
	if err := FlushTraces(context.Background(), meter); err != nil {
		t.Fatalf("tracing off: %v", err)
	}
	if err := providers.FlushTraces(context.Background()); err != nil {
		t.Fatalf("provider with tracing off: %v", err)
	}
}
