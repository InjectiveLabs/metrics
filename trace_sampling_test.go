package metrics

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func samplingTestConfig(queueSize, rate int) Config {
	cfg := deferredTestConfig(queueSize)
	cfg.TraceSampleRate = rate
	return cfg
}

func samplingTestTransaction(index int) []byte {
	return []byte(fmt.Sprintf("transaction-%d", index))
}

func TestTransactionSampledIsAboutOneInRate(t *testing.T) {
	const transactions = 50_000
	for _, rate := range []int{2, 10, 100} {
		sampled := 0
		for index := range transactions {
			hash := sha256.Sum256(samplingTestTransaction(index))
			if transactionSampled(hash[:], transactionSampleLimit(rate)) {
				sampled++
			}
		}
		expected := transactions / rate
		if sampled < expected*8/10 || sampled > expected*12/10 {
			t.Errorf("rate %d: %d of %d transactions sampled, expected about %d", rate, sampled, transactions, expected)
		}
	}
	hash := sha256.Sum256(samplingTestTransaction(0))
	for _, rate := range []int{-1, 0, 1} {
		if !transactionSampled(hash[:], transactionSampleLimit(rate)) {
			t.Errorf("rate %d must trace every transaction", rate)
		}
	}
}

// samplingTestScenario runs what a node does with the transactions of one
// block: the entry path with its own trace per transaction, then the block
// with its events and one runTx per transaction.
func samplingTestScenario(setup *deferredTestSetup, transactions int) {
	meter := setup.meter
	for index := range transactions {
		transaction := samplingTestTransaction(index)

		// Entry: a span with the bytes, spans without, a timed function,
		// and a span from the detached context kept in the mempool.
		checkCtx, checkSpan := StartTransactionSpan(context.Background(), meter, transaction, "tx.mempool.check")
		_, waitSpan := StartTransactionSpan(checkCtx, meter, nil, "tx.mempool.update_lock_wait")
		waitSpan.End()
		_, stopAnte := meter.FuncTimingCtx(checkCtx, "AnteHandle")
		stopAnte()
		kept := DetachTransactionContext(checkCtx)
		checkSpan.End()
		_, removedSpan := StartTransactionSpan(kept, meter, nil, "tx.mempool.removed")
		removedSpan.End()

		// Gossip: the hash is established without a span.
		gossipCtx := TransactionTraceContext(context.Background(), meter, transaction)
		_, sendSpan := StartTransactionSpan(gossipCtx, meter, nil, "tx.gossip.enqueue")
		sendSpan.End()
		RecordTransactionEvent(gossipCtx, meter, nil, "tx.gossip.skipped", time.Now())
	}

	blockCtx, stopBlock := meter.FuncTimingCtx(context.Background(), "internalFinalizeBlock")
	for index := range transactions {
		transaction := samplingTestTransaction(index)
		RecordTransactionEvent(blockCtx, meter, transaction, "tx.block.finalize_started", time.Now())

		hash := sha256.Sum256(transaction)
		holder := &deferredTestContext{ctx: SampleTransactionContext(blockCtx, meter, hash[:])}
		func() {
			defer meter.FuncTiming(holder, "runTx")()
			func() {
				inner := &deferredTestContext{ctx: holder.ctx}
				defer meter.FuncTiming(inner, "GetAccount")()
			}()
		}()
	}
	// A block-level call after the transactions: no mark may reach it.
	_, stopEnd := meter.FuncTimingCtx(blockCtx, "EndBlocker")
	stopEnd()
	stopBlock()
}

func TestTransactionSamplingTracesWholeTransactionsOnly(t *testing.T) {
	const (
		transactions = 400
		rate         = 4
	)
	sampledHashes := map[string]bool{}
	for index := range transactions {
		transaction := samplingTestTransaction(index)
		hash := sha256.Sum256(transaction)
		if transactionSampled(hash[:], transactionSampleLimit(rate)) {
			sampledHashes[transactionTestHash(transaction)] = true
		}
	}
	sampled := len(sampledHashes)
	if sampled == 0 || sampled == transactions {
		t.Fatalf("%d of %d transactions sampled: the test needs both kinds", sampled, transactions)
	}

	perTransaction := []string{
		"tx.mempool.check", "tx.mempool.update_lock_wait", "AnteHandle", "tx.mempool.removed",
		"tx.gossip.enqueue", "tx.gossip.skipped", "tx.block.finalize_started", "runTx", "GetAccount",
	}
	withHash := map[string]bool{
		"tx.mempool.check": true, "tx.mempool.update_lock_wait": true, "AnteHandle": true, "tx.mempool.removed": true,
		"tx.gossip.enqueue": true, "tx.gossip.skipped": true, "tx.block.finalize_started": true,
	}

	described := map[string][]string{}
	for path, queueSize := range map[string]int{"deferred": 0, "inline": -1} {
		t.Run(path, func(t *testing.T) {
			setup := deferredTestProviders(t, samplingTestConfig(queueSize, rate))
			samplingTestScenario(setup, transactions)
			setup.flush(t)

			counts := map[string]int{}
			for _, span := range setup.exporter.GetSpans() {
				counts[span.Name]++
				if !withHash[span.Name] {
					continue
				}
				hash := transactionTestAttributes(span)["tx_hash"].AsString()
				if !sampledHashes[hash] {
					t.Errorf("span %s recorded for transaction %s, which is outside the sample", span.Name, hash)
				}
			}
			for _, name := range perTransaction {
				if counts[name] != sampled {
					t.Errorf("%d %s spans, expected one per sampled transaction: %d", counts[name], name, sampled)
				}
			}
			for _, name := range []string{"internalFinalizeBlock", "EndBlocker"} {
				if counts[name] != 1 {
					t.Errorf("%d %s spans, expected 1: block spans are not sampled", counts[name], name)
				}
			}

			// Metrics are exact: every call is counted.
			metrics := strings.Join(setup.describeMetrics(t), "\n")
			for _, name := range []string{"runTx", "GetAccount", "AnteHandle"} {
				expected := fmt.Sprintf("func_name=%s = %d", name, transactions)
				if !strings.Contains(metrics, expected) {
					t.Errorf("func.called of %s is not %d:\n%s", name, transactions, metrics)
				}
			}
			described[path] = setup.describeSpans()
		})
	}
	deferred, inline := strings.Join(described["deferred"], "\n"), strings.Join(described["inline"], "\n")
	if deferred != inline {
		t.Errorf("the deferred and the inline path export different spans under sampling")
	}
}

func TestTransactionSamplingOffTracesEverything(t *testing.T) {
	for _, rate := range []int{0, 1} {
		setup := deferredTestProviders(t, samplingTestConfig(0, rate))
		samplingTestScenario(setup, 20)
		setup.flush(t)
		runs := 0
		for _, span := range setup.exporter.GetSpans() {
			if span.Name == "runTx" {
				runs++
			}
		}
		if runs != 20 {
			t.Errorf("rate %d: %d runTx spans, expected 20", rate, runs)
		}
	}
}

func TestUntracedContextKeepsParentIdentity(t *testing.T) {
	// Without a parent: a valid identity that is not sampled.
	root := trace.SpanContextFromContext(untracedContext(context.Background()))
	if !root.IsValid() || root.IsSampled() {
		t.Fatalf("mark without a parent: valid %v, sampled %v", root.IsValid(), root.IsSampled())
	}

	// Under a parent: same trace, flag dropped, and the parent's own
	// context untouched.
	setup := deferredTestProviders(t, samplingTestConfig(0, 2))
	blockCtx, stop := setup.meter.FuncTimingCtx(context.Background(), "block")
	defer stop()
	block := trace.SpanContextFromContext(blockCtx)
	marked := trace.SpanContextFromContext(untracedContext(blockCtx))
	if marked.TraceID() != block.TraceID() || marked.IsSampled() {
		t.Errorf("mark under a block: trace %s (block %s), sampled %v", marked.TraceID(), block.TraceID(), marked.IsSampled())
	}
	if !trace.SpanContextFromContext(blockCtx).IsSampled() {
		t.Error("marking a transaction changed the block's context")
	}

	// A mark is kept by a detached context.
	detached := trace.SpanContextFromContext(DetachTransactionContext(untracedContext(context.Background())))
	if !detached.IsValid() || detached.IsSampled() {
		t.Error("the detached context lost the mark")
	}
}

func TestSampleTransactionContextNeedsTracingAndRate(t *testing.T) {
	// A transaction outside a sample of one in two.
	var hash [sha256.Size]byte
	for index := 0; ; index++ {
		hash = sha256.Sum256(samplingTestTransaction(index))
		if !transactionSampled(hash[:], transactionSampleLimit(2)) {
			break
		}
	}
	ctx := context.Background()

	sampling := deferredTestProviders(t, samplingTestConfig(0, 2))
	if marked := SampleTransactionContext(ctx, sampling.meter, hash[:]); marked == ctx {
		t.Error("a transaction outside the sample was not marked")
	}

	everything := deferredTestProviders(t, samplingTestConfig(0, 1))
	if SampleTransactionContext(ctx, everything.meter, hash[:]) != ctx {
		t.Error("rate 1 marked a transaction")
	}

	cfg := samplingTestConfig(0, 2)
	cfg.TracingEnabled = false
	untraced := deferredTestProviders(t, cfg)
	if SampleTransactionContext(ctx, untraced.meter, hash[:]) != ctx {
		t.Error("a context was marked with tracing off")
	}

	if SampleTransactionContext(ctx, NewNilMeter(), hash[:]) != ctx {
		t.Error("a context was marked for a meter without sampling")
	}
}

// BenchmarkFuncTimingUntracedTransaction measures what a timed call costs the
// calling goroutine inside a transaction outside the sample: the record for
// the metrics is still handed to the queue, no span is built.
func BenchmarkFuncTimingUntracedTransaction(b *testing.B) {
	setup := deferredTestProviders(b, samplingTestConfig(-1, 100), sdktrace.WithBatcher(deferredDiscardExporter{}))
	setup.providers.deferred = &deferredPipeline{queue: newSpanQueue(b.N + 2), deferRoots: true, stop: make(chan struct{})}
	meter := setup.meter.SubMeter("bank", Tag("svc", "bank"))
	parentCtx, _ := meter.FuncTimingCtx(untracedContext(context.Background()), "runTx")
	holder := &deferredTestContext{ctx: parentCtx}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		deferredBenchmarkCall(meter, holder, "SendCoins")
	}
	b.StopTimer()
	if dropped := setup.providers.TelemetryQueueStats().Dropped; dropped != 0 {
		b.Fatalf("%d records dropped", dropped)
	}
	setup.providers.deferred.closed.Store(true) // nothing to export at cleanup
}
