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
	// untraced marks a transaction outside the trace sample: no span is
	// recorded with this context, whatever sampler the SDK runs with.
	untraced bool
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

// gossipTraceSwitches is implemented by the library's meter. Custom Meter
// implementations keep gossip tracing disabled.
type gossipTraceSwitches interface {
	GossipReceiveTracingEnabled() bool
	GossipSendTracingEnabled() bool
}

// GossipReceiveTracingEnabled reports whether the receive side of transaction
// gossip should be traced: tracing is on and trace-gossip-receive is set.
func (m *meter) GossipReceiveTracingEnabled() bool {
	return m.TransactionTracer() != nil && m.metrics.cfg.TraceGossipReceive
}

// GossipSendTracingEnabled reports whether the send side of transaction gossip
// should be traced: tracing is on and trace-gossip-send is set.
func (m *meter) GossipSendTracingEnabled() bool {
	return m.TransactionTracer() != nil && m.metrics.cfg.TraceGossipSend
}

// GossipReceiveTracingEnabled lets a reactor skip hashing and span creation
// for received transactions unless the operator asked for them.
func GossipReceiveTracingEnabled(meter Meter) bool {
	switches, supported := meter.(gossipTraceSwitches)
	return supported && switches.GossipReceiveTracingEnabled()
}

// GossipSendTracingEnabled is the send-side counterpart of
// GossipReceiveTracingEnabled.
func GossipSendTracingEnabled(meter Meter) bool {
	switches, supported := meter.(gossipTraceSwitches)
	return supported && switches.GossipSendTracingEnabled()
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
//
// With Config.TraceSampleRate above 1, a transaction outside the sample gets
// a span that records nothing and a context under which nothing is recorded.
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
	if recordDeferredTransactionEvent(ctx, meter, transaction, name, observedAt, tags) {
		return
	}
	_, span := startTransactionSpan(ctx, meter, transaction, name, observedAt, tags...)
	span.End(trace.WithTimestamp(observedAt))
}

// deferredTransactionMeter returns the library meter behind instrument when
// its transaction spans go through the deferred pipeline.
func deferredTransactionMeter(instrument Meter, tags []TagAttribute) *meter {
	scoped, supported := instrument.(*meter)
	if !supported || scoped.TransactionTracer() == nil || scoped.metrics.deferred == nil || len(tags) > maxDeferredTags {
		return nil
	}
	return scoped
}

// recordDeferredTransactionEvent queues a point observation: the worker
// hashes the transaction and builds the zero-duration span. The transaction
// bytes are kept until then and must not change, which holds for the
// immutable transactions of a block or a mempool. It reports false when the
// event has to be recorded on the calling goroutine. With trace sampling the
// worker also makes the choice, so the calling goroutine never hashes.
func recordDeferredTransactionEvent(ctx context.Context, instrument Meter, transaction []byte, name string, observedAt time.Time, tags []TagAttribute) bool {
	scoped := deferredTransactionMeter(instrument, tags)
	if scoped == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	span := &deferredSpan{m: scoped, name: name, parent: ctx, kind: kindTransactionStage}
	span.setTags(tags)
	if !span.inherit(ctx) {
		if !scoped.metrics.deferred.deferRoots {
			return false
		}
		span.traceID = randomTraceID()
	}
	if span.kind&kindSampled == 0 {
		return true // the trace is not recorded
	}
	span.spanID = randomSpanID()
	span.extra = &spanExtra{startTime: observedAt, endTime: observedAt, transaction: transaction}
	span.state.Store(spanEnded)
	scoped.metrics.deferred.enqueue(span)
	return true
}

// startDeferredTransactionSpan starts a transaction span whose SDK span a
// worker builds after End. It returns a nil span when the span has to start
// on the calling goroutine.
func startDeferredTransactionSpan(ctx context.Context, instrument Meter, transaction []byte, name string, observedAt time.Time, tags []TagAttribute) (context.Context, trace.Span) {
	scoped := deferredTransactionMeter(instrument, tags)
	if scoped == nil {
		return ctx, nil
	}

	span := &deferredSpan{m: scoped, name: name, kind: kindTransactionStage}
	span.setTags(tags)
	if !span.inherit(ctx) {
		if !scoped.metrics.deferred.deferRoots {
			return ctx, nil
		}
		span.traceID = randomTraceID()
	}
	span.spanID = randomSpanID()

	// The returned context carries the hash for the spans below this one,
	// so it is computed here.
	if transaction != nil {
		ctx = withTransactionHash(ctx, transaction)
	}
	span.parent = ctx
	if observedAt.IsZero() {
		span.start = monotonicNow()
	} else {
		span.extras().startTime = observedAt
	}
	return span.asContext(ctx), span
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

	ctx, transaction, sampled := sampleTransaction(ctx, meter, transaction)
	if !sampled {
		return ctx, inertTransactionSpan
	}

	if deferredCtx, span := startDeferredTransactionSpan(ctx, meter, transaction, name, observedAt, tags); span != nil {
		return deferredCtx, span
	}

	if transaction != nil {
		ctx = withTransactionHash(ctx, transaction)
	}
	attributes := transactionSpanAttributes(ctx, meter, tags)
	attributes = append(attributes, attribute.String("tx_stage", name))
	options := []trace.SpanStartOption{trace.WithAttributes(attributes...)}
	if !observedAt.IsZero() {
		options = append(options, trace.WithTimestamp(observedAt))
	}
	return tracer.Start(ctx, name, options...)
}

// TransactionTraceContext returns ctx carrying the canonical uppercase SHA256
// tx_hash of transaction, for callers that start several spans for one
// transaction: hash once here, then start the spans with nil transaction
// bytes. A span started with transaction bytes replaces the hash. With
// tracing disabled, ctx is returned as is, without hashing. For a
// transaction outside the trace sample the returned context is marked
// instead, and the spans started with it are not recorded.
func TransactionTraceContext(ctx context.Context, meter Meter, transaction []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if !TransactionTracingEnabled(meter) {
		return ctx
	}
	ctx, transaction, sampled := sampleTransaction(ctx, meter, transaction)
	if !sampled || transaction == nil {
		return ctx
	}
	return withTransactionHash(ctx, transaction)
}

func withTransactionHash(ctx context.Context, transaction []byte) context.Context {
	hash := sha256.Sum256(transaction)
	return withTransactionHashSum(ctx, &hash)
}

func withTransactionHashSum(ctx context.Context, hash *[sha256.Size]byte) context.Context {
	metadata, _ := ctx.Value(transactionTraceKey{}).(transactionTraceMetadata)
	const hexadecimal = "0123456789ABCDEF"
	var encodedHash [sha256.Size * 2]byte
	for index, value := range hash {
		encodedHash[index*2], encodedHash[index*2+1] = hexadecimal[value>>4], hexadecimal[value&15]
	}
	metadata.hash = string(encodedHash[:])
	return context.WithValue(ctx, transactionTraceKey{}, metadata)
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
