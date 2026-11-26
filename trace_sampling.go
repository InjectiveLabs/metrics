package metrics

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"

	"go.opentelemetry.io/otel/trace"
)

// Transaction trace sampling.
//
// With Config.TraceSampleRate N above 1, one transaction in N is traced and
// the others leave no spans. The choice comes from the transaction's SHA256
// hash, so it is the same on every node with the same N and for every stage
// of the transaction: a traced transaction is traced from the RPC call to
// the commit, on the sentry and on the validator. Spans that do not belong
// to a transaction, such as those of a block, are not affected. Metrics are
// not sampled: calls made for an untraced transaction are still counted and
// timed.
//
// A transaction outside the sample is marked twice in its context. The mark
// proper is a value of the library, which the inline path checks before it
// starts a span: it holds with every SDK sampler, including always_on and
// traceidratio, which look at the trace ID only. The second is a span context
// without the sampled flag, which the deferred path reads from the parent it
// inherits anyway and which other OpenTelemetry code understands.

// sampleEverything is the limit that every transaction is under.
const sampleEverything = 0

// transactionSampleLimit turns a rate N into the limit the choice compares
// against: the largest 64-bit number divided by N. It is computed once, so
// that a choice is one comparison.
func transactionSampleLimit(rate int) uint64 {
	if rate <= 1 {
		return sampleEverything
	}
	return math.MaxUint64 / uint64(rate)
}

// transactionSampler is implemented by the library's meter. Custom Meter
// implementations trace every transaction.
type transactionSampler interface {
	transactionSampleLimit() uint64
}

func (m *meter) transactionSampleLimit() uint64 {
	if m == nil || m.metrics == nil {
		return sampleEverything
	}
	return m.metrics.sampleLimit
}

func sampleLimitOf(instrument Meter) uint64 {
	if sampler, supported := instrument.(transactionSampler); supported {
		return sampler.transactionSampleLimit()
	}
	return sampleEverything
}

// transactionSampled reports whether the transaction with this SHA256 hash
// is traced: the first eight bytes of the hash, read as a big-endian number,
// are at most limit. A hash is uniform, so with the limit of a rate N this
// holds for one transaction in N.
func transactionSampled(hash []byte, limit uint64) bool {
	if limit == sampleEverything || len(hash) < 8 {
		return true
	}
	return binary.BigEndian.Uint64(hash) <= limit
}

// untracedContext returns ctx marked as belonging to a transaction outside
// the sample. Under a traced parent, a block for instance, the span context
// keeps the parent's identity and drops the sampled flag; without a parent it
// is a new unsampled identity.
func untracedContext(ctx context.Context) context.Context {
	metadata, _ := ctx.Value(transactionTraceKey{}).(transactionTraceMetadata)
	if !metadata.untraced {
		metadata.untraced = true
		ctx = context.WithValue(ctx, transactionTraceKey{}, metadata)
	}

	parent := trace.SpanContextFromContext(ctx)
	switch {
	case !parent.IsValid():
		parent = trace.NewSpanContext(trace.SpanContextConfig{TraceID: randomTraceID(), SpanID: randomSpanID()})
	case !parent.IsSampled():
		return ctx
	default:
		parent = parent.WithTraceFlags(parent.TraceFlags() &^ trace.FlagsSampled)
	}
	return trace.ContextWithSpanContext(ctx, parent)
}

// transactionUntraced reports whether ctx carries the mark of a transaction
// outside the sample.
func transactionUntraced(ctx context.Context) bool {
	metadata, _ := ctx.Value(transactionTraceKey{}).(transactionTraceMetadata)
	return metadata.untraced
}

// SampleTransactionContext applies trace sampling to code that times a
// transaction with FuncTiming instead of StartTransactionSpan: call it with
// the transaction's SHA256 hash before the first FuncTiming of the
// transaction and use the returned context for it. For a transaction outside
// the sample the returned context is marked and the spans below it are not
// recorded; otherwise ctx is returned as is. The context must be the
// transaction's own, or the mark reaches the transactions after it.
func SampleTransactionContext(ctx context.Context, meter Meter, hash []byte) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	limit := sampleLimitOf(meter)
	if limit == sampleEverything || !TransactionTracingEnabled(meter) || transactionSampled(hash, limit) {
		return ctx
	}
	return untracedContext(ctx)
}

// sampleTransaction is the sampling step of a transaction span or event that
// starts on the calling goroutine. It reports false when nothing is to be
// recorded, with the context the caller hands on. When it hashed the
// transaction, the returned context carries the hash and the returned bytes
// are nil, so that the hash is computed once.
func sampleTransaction(ctx context.Context, instrument Meter, transaction []byte) (context.Context, []byte, bool) {
	limit := sampleLimitOf(instrument)
	if limit == sampleEverything {
		return ctx, transaction, true
	}
	if transactionUntraced(ctx) {
		return ctx, transaction, false
	}
	if transaction == nil {
		return ctx, nil, true
	}
	hash := sha256.Sum256(transaction)
	if !transactionSampled(hash[:], limit) {
		return untracedContext(ctx), transaction, false
	}
	return withTransactionHashSum(ctx, &hash), nil, true
}
