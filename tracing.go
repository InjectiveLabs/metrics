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
// Usage: defer metrics.FuncTiming(ctx, "EndBlocker")()
//
// This function overwrites the ctx with a copy with attached tracing span value.
func (m *Meter) FuncTiming(ctx *context.Context, fn string, tags ...TagAttr) StopFn {
	m.Func(fn, tags...)
	t := time.Now()

	var traceSpan trace.Span
	if m.tracer != nil {
		spanCtx, span := m.tracer.Start(*ctx, fn, trace.WithAttributes(m.getMergedTags(tags...)...))
		traceSpan = span
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
