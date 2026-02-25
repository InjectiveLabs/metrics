package metrics

import (
	"context"
	"fmt"
	"time"

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

func newTracerProvider(cfg *Config, resourceAttributes ...TagAttr) (*sdktrace.TracerProvider, error) {
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
		resource.WithAttributes(resourceAttributes...),
	)
	if err != nil {
		return nil, fmt.Errorf("new resource failed: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	return tp, nil
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
func (m *meter) FuncTiming(ctx ContextPointer, fn string, tags ...TagAttr) StopFn {
	if m == nil {
		return noopStopFn
	}

	ctxPtr := ctx.ContextPtr()
	spanCtx, stop := m.FuncTimingCtx(*ctxPtr, fn, tags...)
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
func (m *meter) FuncTimingCtx(ctx context.Context, fn string, tags ...TagAttr) (context.Context, StopFn) {
	if m == nil {
		return ctx, noopStopFn
	}

	m.Func(fn, tags...)
	start := time.Now()

	var (
		span    trace.Span
		spanCtx context.Context
	)

	if m.tracer != nil {
		spanCtx, span = m.tracer.Start(ctx, fn, trace.WithAttributes(m.getMergedTags(tags...)...))
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
					span.SetAttributes(Tag("exception.type", "stuck"))
					span.SetStatus(codes.Error, "stuck")
					span.End()
				}

				m.Histogram("func.timing.timeout", d.Milliseconds(), append([]TagAttr{Tag("func_name", fn)}, tags...)...) //nolint:errcheck
			}
		}()
	}

	return spanCtx, func(errors ...*error) {
		close(doneC)

		d := time.Since(start)

		var err error

		if len(errors) > 0 {
			err = *errors[0]
		}

		if err != nil {
			m.FuncError(spanCtx, fn, err, tags...)
		} else if span != nil && span.IsRecording() {
			span.SetStatus(codes.Ok, "")
			span.End()
		}

		m.Histogram("func.timing", d.Milliseconds(), append([]TagAttr{Tag("func_name", fn)}, tags...)...) //nolint:errcheck
	}
}

// FuncError reports fn error metric and also sets current span in context (if present) to Error status with description err.Error()
func (m *meter) FuncError(ctx context.Context, fn string, err error, tags ...TagAttr) {
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
