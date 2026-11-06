package metrics

import "context"

// traceFlusher is implemented by the library's meter. Custom Meter
// implementations have nothing to flush.
type traceFlusher interface {
	FlushTraces(ctx context.Context) error
}

// FlushTraces exports every ended span still queued in the batch processor
// and waits for the exporter's verdict, bounded by ctx. It exists for code
// that is about to take the process down: a goroutine that recovers a panic
// can label its span, end it, flush, and re-panic, so the span that describes
// the crash is not lost with the queue. The batch processor keeps running
// afterwards. Nil, disabled and tracing-off meters return nil at once.
func (m *meter) FlushTraces(ctx context.Context) error {
	if m.TransactionTracer() == nil {
		return nil
	}
	return m.metrics.FlushTraces(ctx)
}

// FlushTraces is the provider-level counterpart of Meter.FlushTraces.
func (ms *Metrics) FlushTraces(ctx context.Context) error {
	if ms == nil || ms.disabled || !ms.cfg.TracingEnabled {
		return nil
	}
	return ms.tracerProvider.ForceFlush(ctx)
}

// FlushTraces flushes through any Meter; unsupported implementations are a
// no-op. See meter.FlushTraces for when to call it.
func FlushTraces(ctx context.Context, meter Meter) error {
	flusher, supported := meter.(traceFlusher)
	if !supported {
		return nil
	}
	return flusher.FlushTraces(ctx)
}
