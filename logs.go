package metrics

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

const (
	trace_id_key = "trace_id"
	span_id_key  = "span_id"
)

type logger[T any] interface {
	With(keyVals ...any) T
}

// AddLogTraceIDs adds "trace_id" and "span_id" string keys to the logger instance (using .With() method)
// if the ctx has tracing span inside
func AddLogTraceIDs[L logger[L]](ctx context.Context, logger L) L {
	spanCtx := trace.SpanContextFromContext(ctx)

	if spanCtx.HasTraceID() {
		logger = logger.With(trace_id_key, spanCtx.TraceID().String())
	}

	if spanCtx.HasSpanID() {
		logger = logger.With(span_id_key, spanCtx.SpanID().String())
	}

	return logger
}
