package metrics

import (
	"context"
	"crypto/sha256"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type transactionTraceKey struct{}

// transactionTraceMetadata is fixed-size correlation metadata. It deliberately
// does not retain transaction bytes, request values or application payloads.
type transactionTraceMetadata struct {
	hash string
}

type transactionTracerProvider interface {
	TransactionTracer() trace.Tracer
}

var inertTransactionSpan = trace.SpanFromContext(context.Background())

// TransactionTracer exposes the existing tracer without changing the Meter
// interface. An absent tracer keeps transaction instrumentation disabled.
func (m *meter) TransactionTracer() trace.Tracer {
	if m == nil || m.metrics == nil || m.metrics.disabled || !m.metrics.cfg.TracingEnabled {
		return nil
	}
	return m.tracer
}

// TransactionTracingEnabled allows callers to skip transaction hashing, tag
// construction and timestamps when tracing is disabled. Unsupported custom
// Meter implementations remain compatible and are treated as disabled.
func TransactionTracingEnabled(meter Meter) bool {
	provider, supported := meter.(transactionTracerProvider)
	return supported && provider.TransactionTracer() != nil
}

// StartTransactionSpan starts a span without creating metric instruments or
// timeout goroutines. Non-nil transaction bytes establish the canonical uppercase
// SHA256 tx_hash; nil reuses correlation metadata already carried by ctx.
// tx_stage is the span name. Other tags describe only this stage and do not
// become inherited metadata.
//
// The transaction hash is a correlation attribute, not an OpenTelemetry trace
// ID. Independent nodes can join observations by chain ID and hash without
// manufacturing a causal parent relationship across the P2P protocol.
func StartTransactionSpan(ctx context.Context, meter Meter, transaction []byte, name string, tags ...TagAttribute) (context.Context, trace.Span) {
	return startTransactionSpan(ctx, meter, transaction, name, time.Time{}, tags...)
}

// RecordTransactionEvent records a point observation as a zero-duration span at
// an already captured boundary. Capturing once before a block's transaction loop
// prevents hashing and export bookkeeping from shifting each transaction's time.
func RecordTransactionEvent(ctx context.Context, meter Meter, transaction []byte, name string, observedAt time.Time, tags ...TagAttribute) {
	if !TransactionTracingEnabled(meter) {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	_, span := startTransactionSpan(ctx, meter, transaction, name, observedAt, tags...)
	span.End(trace.WithTimestamp(observedAt))
}

func startTransactionSpan(ctx context.Context, meter Meter, transaction []byte, name string, observedAt time.Time, tags ...TagAttribute) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	provider, supported := meter.(transactionTracerProvider)
	var tracer trace.Tracer
	if supported {
		tracer = provider.TransactionTracer()
	}
	if tracer == nil {
		// Preserve context for other enabled components. Callers must mutate
		// the returned span, not a potentially recording incoming parent.
		return ctx, inertTransactionSpan
	}

	metadata, _ := ctx.Value(transactionTraceKey{}).(transactionTraceMetadata)
	if transaction != nil {
		hash := sha256.Sum256(transaction)
		const hexadecimal = "0123456789ABCDEF"
		var encodedHash [sha256.Size * 2]byte
		for index, value := range hash {
			encodedHash[index*2], encodedHash[index*2+1] = hexadecimal[value>>4], hexadecimal[value&15]
		}
		metadata.hash = string(encodedHash[:])
		ctx = context.WithValue(ctx, transactionTraceKey{}, metadata)
	}
	attributes := transactionSpanAttributes(ctx, meter, tags)
	attributes = append(attributes, attribute.String("tx_stage", name))
	options := []trace.SpanStartOption{trace.WithAttributes(attributes...)}
	if !observedAt.IsZero() {
		options = append(options, trace.WithTimestamp(observedAt))
	}
	return tracer.Start(ctx, name, options...)
}

func transactionSpanAttributes(ctx context.Context, instrument Meter, tags []TagAttribute) []attribute.KeyValue {
	if scopedMeter, supported := instrument.(*meter); supported {
		tags = scopedMeter.getMergedTraceTags(tags...)
	}
	attributes := make([]attribute.KeyValue, 0, len(tags)+2)
	for _, tag := range tags {
		switch tag := tag.(type) {
		case TagAttr:
			attributes = append(attributes, toAttr(tag))
		case TagAttrTrace:
			attributes = append(attributes, toAttr(tag))
		}
	}
	metadata, _ := ctx.Value(transactionTraceKey{}).(transactionTraceMetadata)
	if metadata.hash != "" {
		// The computed hash wins over a conflicting caller-provided attribute.
		attributes = append(attributes, attribute.String("tx_hash", metadata.hash))
	}
	return attributes
}

// DetachTransactionContext preserves only span identity and fixed transaction
// correlation metadata for asynchronous work. It drops cancellation, deadlines,
// arbitrary request values and references to the recording parent span.
func DetachTransactionContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	spanContext := trace.SpanContextFromContext(ctx)
	metadata, hasMetadata := ctx.Value(transactionTraceKey{}).(transactionTraceMetadata)
	if !spanContext.IsValid() && !hasMetadata {
		return context.Background()
	}
	detached := trace.ContextWithSpanContext(context.Background(), spanContext)
	if hasMetadata {
		detached = context.WithValue(detached, transactionTraceKey{}, metadata)
	}
	return detached
}
