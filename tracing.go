package metrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ContextPointer is needed to accomodate for any context.Context wrapper (mainly sdk.Context)
// to pull internally stored context.Context and replace it, since we can not reference sdk types
// and we can not replace sdk.Context itself due to messy wrapping / unwrapping SDK logic.
type ContextPointer interface {
	ContextPtr() *context.Context
}

func newTracerProvider(cfg *Config, counters *traceExportCounters, resourceAttributes ...TagAttribute) (*sdktrace.TracerProvider, error) {
	ctx := context.Background()

	// Reads OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_HEADERS from environment
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.InsecureEndpoint {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("new otlp trace grpc exporter failed: %w", err)
	}

	// Reads OTEL_SERVICE_NAME from environment and adds host/process/OS attributes
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithHost(),
		resource.WithOS(),
		resource.WithProcess(),
		resource.WithContainer(),
		resource.WithAttributes(toAttrs(resourceAttributes)...),
	)
	if err != nil {
		return nil, fmt.Errorf("new resource failed: %w", err)
	}

	return newTracerProviderWithExporter(exporter, res, counters, cfg.batchOptions()...), nil
}

// batchOptions returns the batcher settings the configuration sets; the
// rest stay at the SDK's defaults or environment values.
func (cfg *Config) batchOptions() []sdktrace.BatchSpanProcessorOption {
	var options []sdktrace.BatchSpanProcessorOption
	if cfg.TraceQueueSize > 0 {
		options = append(options, sdktrace.WithMaxQueueSize(cfg.TraceQueueSize))
	}
	if cfg.TraceBatchSize > 0 {
		options = append(options, sdktrace.WithMaxExportBatchSize(cfg.TraceBatchSize))
	}
	return options
}

// FuncTiming reports function call and execution time in ms.
// Fucntion name is stored as "func_name" tag.
// Uses "func.timing" histogram instrument.
//
// Usage: defer metrics.FuncTiming(&sdkCtx, "EndBlocker")()
// Usage to auto-handle error: defer metrics.FuncTiming(&sdkCtx, "EndBlocker")(&err) // err <- here is a named ("err") returned error value of an enclosing fn
//
// This function overwrites the context.COntext inside of ctx with a copy with attached tracing span value
// using ContextPtr() method
func (m *meter) FuncTiming(ctx ContextPointer, fn string, tags ...TagAttribute) StopFn {
	if m == nil {
		return noopStopFn
	}

	ctxPtr := ctx.ContextPtr()
	spanCtx, stop := m.funcTiming(*ctxPtr, fn, tags)
	*ctxPtr = spanCtx

	return stop
}

// FuncTimingCtx reports function call and execution time in ms.
// Fucntion name is stored as "func_name" tag.
// Uses "func.timing" histogram instrument.
//
// Usage:
// spanCtx, stop := metrics.FuncTimingCtx(ctx, "EndBlocker")
// defer stop(err)
//
// Usage to auto-handle error:
// spanCtx, stop := metrics.FuncTimingCtx(ctx, "EndBlocker")
// defer stop(&err) // err <- here is a named ("err") returned error value of an enclosing fn
//
// WARNING: DO NOT USE IT FOR sdk.Context wrapped as context.Context, use FuncTiming() instead
func (m *meter) FuncTimingCtx(ctx context.Context, fn string, tags ...TagAttribute) (context.Context, StopFn) {
	if m == nil {
		return ctx, noopStopFn
	}

	return m.funcTiming(ctx, fn, tags)
}

// funcTiming hands the call to the deferred pipeline when there is one, and
// otherwise builds the span and the metrics on the calling goroutine.
func (m *meter) funcTiming(ctx context.Context, fn string, tags []TagAttribute) (context.Context, StopFn) {
	if pipeline := m.metrics.deferred; pipeline != nil && len(tags) <= maxDeferredTags {
		if spanCtx, stop := pipeline.startFunc(ctx, m, fn, tags); stop != nil {
			return spanCtx, stop
		}
	}
	return m.funcTimingInline(ctx, fn, tags)
}

// startFunc starts a deferred FuncTiming call: it reads the clock, picks the
// span ID and allocates the record. Everything else happens on a worker
// after the stop function ran. It returns a nil StopFn for a call the
// pipeline does not take: one that starts a trace while the sampler may
// reject traces.
func (pipeline *deferredPipeline) startFunc(ctx context.Context, m *meter, fn string, tags []TagAttribute) (context.Context, StopFn) {
	span := &deferredSpan{m: m, name: fn}
	span.setTags(tags)

	if m.tracer != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		if !span.inherit(ctx) {
			if !pipeline.deferRoots {
				return ctx, nil
			}
			span.traceID = randomTraceID()
		}
		span.spanID = randomSpanID()
		span.parent = ctx
		ctx = span.asContext(ctx)
	}

	if timeout := m.metrics.cfg.StuckFuncTimeout; timeout > 0 {
		span.kind |= kindWatchdog
		span.extras().done = make(chan struct{})
		span.start = monotonicNow() // the watchdog reads it
		go span.watchStuck(timeout)
		return ctx, span.stop
	}

	span.start = monotonicNow()
	return ctx, span.stop
}

// watchStuck is the deferred counterpart of the watchdog in
// funcTimingInline: it ends the span of a function that runs longer than
// timeout, marked as stuck.
func (span *deferredSpan) watchStuck(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-span.extra.done:
		return
	case <-timer.C:
	}

	m, fn, tags := span.m, span.name, span.tagSlice()
	d := time.Duration(monotonicNow() - span.start)

	if span.IsRecording() {
		err := fmt.Errorf("detected stuck function: %s stuck for %v", fn, d)
		span.RecordError(err, trace.WithStackTrace(true))
		span.SetAttributes(attribute.String("exception.type", "stuck"))
		span.SetStatus(codes.Error, "stuck")
		span.End()
	}

	m.Histogram("func.timing.timeout", d.Milliseconds(), append([]TagAttribute{Tag("func_name", fn)}, tags...)...) //nolint:errcheck
}

// funcTimingInline is FuncTimingCtx without the deferred pipeline.
func (m *meter) funcTimingInline(ctx context.Context, fn string, tags []TagAttribute) (context.Context, StopFn) {
	m.Func(fn, tags...)
	start := time.Now()

	var (
		span trace.Span
	)

	if m.tracer != nil {
		ctx, span = m.tracer.Start(ctx, fn, trace.WithAttributes(transactionSpanAttributes(ctx, m, tags)...))
	}

	// func timeout watchdog
	doneC := make(chan struct{})

	if timeout := m.metrics.cfg.StuckFuncTimeout; timeout > 0 {
		go func() {
			timer := time.NewTimer(timeout)
			defer timer.Stop()

			select {
			case <-doneC:
				return
			case <-timer.C:
				d := time.Since(start)

				if span != nil && span.IsRecording() {
					err := fmt.Errorf("detected stuck function: %s stuck for %v", fn, d)
					span.RecordError(err, trace.WithStackTrace(true))
					span.SetAttributes(attribute.String("exception.type", "stuck"))
					span.SetStatus(codes.Error, "stuck")
					span.End()
				}

				m.Histogram("func.timing.timeout", d.Milliseconds(), append([]TagAttribute{Tag("func_name", fn)}, tags...)...) //nolint:errcheck
			}
		}()
	}

	return ctx, func(errors ...*error) {
		close(doneC)

		d := time.Since(start)

		var err error

		if len(errors) > 0 {
			err = *errors[0]
		}

		if err != nil {
			m.FuncError(ctx, fn, err, tags...)
		} else if span != nil && span.IsRecording() {
			span.SetStatus(codes.Ok, "")
			span.End()
		}

		m.Histogram("func.timing", d.Milliseconds(), append([]TagAttribute{Tag("func_name", fn)}, tags...)...) //nolint:errcheck
	}
}

// FuncError reports fn error metric and also sets current span in context (if present) to Error status with description err.Error()
func (m *meter) FuncError(ctx context.Context, fn string, err error, tags ...TagAttribute) {
	if m == nil {
		return
	}

	m.Error(fn, err, tags...)

	if m.tracer == nil {
		return
	}

	span := trace.SpanFromContext(ctx)

	if !span.IsRecording() {
		return
	}

	span.SetStatus(codes.Error, err.Error())
	// span.RecordError(err, errorOpts...) // we will not append exception event with stacktrace to the span for performance reasons, status is enough
	span.End()
}
