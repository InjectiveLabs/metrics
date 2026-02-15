package metrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ContextPointer is needed to accomodate for any context.Context wrapper (mainly sdk.Context)
// that can-not be directly referenced by this package
type ContextPointer interface {
	ContextPtr() *context.Context
}

func newTracerProvider(cfg *Config) (*sdktrace.TracerProvider, error) {
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
// Usage: defer metrics.FuncTiming(&sdkCtx, "EndBlocker")()
//
// This function overwrites the context.COntext inside of ctx with a copy with attached tracing span value
// using ContextPtr() method
func (m *meter) FuncTiming(ctx ContextPointer, fn string, tags ...TagAttr) StopFn {
	if m == nil || m.Meter == nil {
		return func() {}
	}

	return m.FuncTimingCtx(ctx.ContextPtr(), fn, tags...)
}

// FuncTimingCtx reports function call and execution time in ms.
// Fucntion name is stored as "func_name" tag.
// Uses "func.timing" histogram instrument.
// Usage: defer metrics.FuncTimingCtx(&ctx, "EndBlocker")()
//
// This function overwrites the ctx with a copy of it with trace span attached.
func (m *meter) FuncTimingCtx(ctx *context.Context, fn string, tags ...TagAttr) StopFn {
	if m == nil || m.Meter == nil {
		return func() {}
	}

	m.Func(fn, tags...)
	t := time.Now()

	var (
		traceSpan trace.Span
		spanCtx   context.Context
	)

	if m.tracer != nil {
		spanCtx, traceSpan = m.tracer.Start(*ctx, fn, trace.WithAttributes(m.getMergedTags(tags...)...))
		*ctx = spanCtx
	}

	return func() {
		d := time.Since(t)

		if traceSpan != nil {
			traceSpan.End()
		}

		m.Histogram("func.timing", d.Milliseconds(), append([]TagAttr{Tag("func_name", fn)}, tags...)...) //nolint:errcheck
	}
}
