package metrics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func transactionTestMeter(t *testing.T, tracingEnabled bool) (Meter, *tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	providers := &Metrics{
		cfg:            Config{MetricsEnabled: true, TracingEnabled: tracingEnabled},
		meterProvider:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		tracerProvider: sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)),
	}
	t.Cleanup(func() {
		_ = providers.meterProvider.Shutdown(context.Background())
		_ = providers.tracerProvider.Shutdown(context.Background())
	})
	meter, err := providers.NewMeter("transaction-test")
	if err != nil {
		t.Fatal(err)
	}
	return meter, exporter, reader
}

func transactionTestHash(transaction []byte) string {
	hash := sha256.Sum256(transaction)
	return strings.ToUpper(hex.EncodeToString(hash[:]))
}

func transactionTestAttributes(span tracetest.SpanStub) map[string]attribute.Value {
	attributes := make(map[string]attribute.Value)
	for _, attribute := range span.Attributes {
		attributes[string(attribute.Key)] = attribute.Value
	}
	return attributes
}

func TestTransactionSpanCorrelationAndTraceOnlyMetadata(t *testing.T) {
	t.Parallel()
	meter, exporter, reader := transactionTestMeter(t, true)
	transaction := []byte("signed transaction")
	ctx, span := StartTransactionSpan(context.Background(), meter, transaction, "tx.api.received",
		TraceTag("tx_hash", "incorrect"), TraceTag("tx_stage", "incorrect"), MetricTag("metric_only", "exclude"))
	childContext, child := StartTransactionSpan(ctx, meter, nil, "tx.mempool.check")
	child.End()
	_, stop := meter.FuncTimingCtx(childContext, "application_check")
	stop()
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	wantHash := transactionTestHash(transaction)
	if err := meter.Count("transaction-test.count", 1, TraceTag("tx_hash", wantHash)); err != nil {
		t.Fatal(err)
	}
	for _, observed := range spans {
		attributes := transactionTestAttributes(observed)
		if attributes["tx_hash"].AsString() != wantHash {
			t.Fatalf("%s hash = %q, want %q", observed.Name, attributes["tx_hash"].AsString(), wantHash)
		}
		if _, present := attributes["metric_only"]; present {
			t.Fatalf("metric-only tag leaked into %s", observed.Name)
		}
		if observed.Name != "application_check" && attributes["tx_stage"].AsString() != observed.Name {
			t.Fatalf("%s has incorrect lifecycle stage %q", observed.Name, attributes["tx_stage"].AsString())
		}
		if observed.SpanContext.TraceID() != span.SpanContext().TraceID() {
			t.Fatalf("%s lost local trace context", observed.Name)
		}
	}
	if spans[0].Parent.SpanID() != span.SpanContext().SpanID() {
		t.Fatal("transaction child lost parent span")
	}

	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			switch observed := metric.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range observed.DataPoints {
					if _, present := point.Attributes.Value("tx_hash"); present {
						t.Fatalf("transaction hash leaked into metric %s", metric.Name)
					}
				}
			case metricdata.Histogram[int64]:
				for _, point := range observed.DataPoints {
					if _, present := point.Attributes.Value("tx_hash"); present {
						t.Fatalf("transaction hash leaked into histogram %s", metric.Name)
					}
				}
			}
		}
	}
}

func TestTransactionSpanDoesNotCreateMetrics(t *testing.T) {
	t.Parallel()
	meter, _, reader := transactionTestMeter(t, true)
	_, span := StartTransactionSpan(context.Background(), meter, []byte("one"), "tx.mempool.check")
	span.End()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 0 {
		t.Fatalf("span-only helper created metrics: %+v", data.ScopeMetrics)
	}
}

func TestTransactionSpanDisabledProtectsParent(t *testing.T) {
	t.Parallel()
	disabledMeter, _, _ := transactionTestMeter(t, false)
	parentMeter, exporter, _ := transactionTestMeter(t, true)
	parentContext, parent := StartTransactionSpan(context.Background(), parentMeter, []byte("parent"), "parent")
	for name, meter := range map[string]Meter{"nil": nil, "typed-nil": NewNilMeter(), "tracing-disabled": disabledMeter} {
		t.Run(name, func(t *testing.T) {
			if TransactionTracingEnabled(meter) {
				t.Fatal("disabled meter reports tracing enabled")
			}
			ctx, span := StartTransactionSpan(parentContext, meter, []byte("ignored"), "ignored")
			if span.IsRecording() {
				t.Fatal("disabled helper returned recording span")
			}
			if ctx != parentContext {
				t.Fatal("disabled helper replaced incoming context")
			}
			span.SetAttributes(attribute.Bool("unwanted", true))
			span.End()
			RecordTransactionEvent(parentContext, meter, []byte("ignored"), "ignored-event", time.Now())
		})
	}
	parent.End()
	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d parent spans, want 1", len(spans))
	}
	attributes := transactionTestAttributes(spans[0])
	if _, present := attributes["unwanted"]; present {
		t.Fatal("disabled span modified parent")
	}
}

func TestDisabledTransactionSpanPreservesOtherComponentsContext(t *testing.T) {
	t.Parallel()
	meter, exporter, _ := transactionTestMeter(t, true)
	ctx, parent := StartTransactionSpan(context.Background(), meter, []byte("transaction"), "parent")
	continued, disabled := StartTransactionSpan(ctx, NewNilMeter(), []byte("ignored"), "disabled")
	disabled.End()
	_, child := StartTransactionSpan(continued, meter, nil, "enabled-child")
	child.End()
	parent.End()
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[0].Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Fatal("disabled instrumentation broke another component's trace ancestry")
	}
	if got := transactionTestAttributes(spans[0])["tx_hash"].AsString(); got != transactionTestHash([]byte("transaction")) {
		t.Fatalf("disabled instrumentation changed transaction correlation: %q", got)
	}
}

func TestDetachTransactionContextRetainsOnlyCorrelation(t *testing.T) {
	t.Parallel()
	meter, exporter, _ := transactionTestMeter(t, true)
	type requestValueKey struct{}
	requestContext, cancel := context.WithTimeout(context.WithValue(context.Background(), requestValueKey{}, "request"), time.Minute)
	ctx, parent := StartTransactionSpan(requestContext, meter, []byte("signed transaction"), "request")
	detached := DetachTransactionContext(ctx)
	cancel()
	parent.End()
	if detached.Err() != nil {
		t.Fatal("detached context retained cancellation")
	}
	if _, present := detached.Deadline(); present {
		t.Fatal("detached context retained deadline")
	}
	if detached.Value(requestValueKey{}) != nil {
		t.Fatal("detached context retained request values")
	}
	_, child := StartTransactionSpan(detached, meter, nil, "included")
	child.End()
	spans := exporter.GetSpans()
	if len(spans) != 2 || spans[1].Parent.SpanID() != spans[0].SpanContext.SpanID() {
		t.Fatal("detached context lost parent identity")
	}
	if got := transactionTestAttributes(spans[1])["tx_hash"].AsString(); got != transactionTestHash([]byte("signed transaction")) {
		t.Fatalf("detached hash = %q", got)
	}
}

func TestRecordTransactionEventUsesCapturedTimestamp(t *testing.T) {
	t.Parallel()
	meter, exporter, _ := transactionTestMeter(t, true)
	observedAt := time.Unix(1_700_000_000, 123456789)
	RecordTransactionEvent(context.Background(), meter, []byte("signed transaction"), "tx.block.committed", observedAt,
		TraceTag("height", int64(123)))
	spans := exporter.GetSpans()
	if len(spans) != 1 || !spans[0].StartTime.Equal(observedAt) || !spans[0].EndTime.Equal(observedAt) {
		t.Fatalf("event did not retain exact boundary time: %+v", spans)
	}
}

func TestRecordTransactionEventCapturesMissingTimestampOnce(t *testing.T) {
	t.Parallel()
	meter, exporter, _ := transactionTestMeter(t, true)
	RecordTransactionEvent(context.Background(), meter, []byte("transaction"), "tx.event", time.Time{})
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].StartTime.IsZero() || !spans[0].StartTime.Equal(spans[0].EndTime) {
		t.Fatalf("event without supplied time is not a point observation: %+v", spans)
	}
}

func TestTransactionHashChangesWithTransactionBytes(t *testing.T) {
	t.Parallel()
	meter, exporter, _ := transactionTestMeter(t, true)
	ctx, parent := StartTransactionSpan(context.Background(), meter, []byte("first"), "first")
	_, child := StartTransactionSpan(ctx, meter, []byte("second"), "second")
	child.End()
	parent.End()
	spans := exporter.GetSpans()
	if got := transactionTestAttributes(spans[0])["tx_hash"].AsString(); got != transactionTestHash([]byte("second")) {
		t.Fatalf("nested transaction reused incorrect hash: %q", got)
	}
}

func TestTransactionTraceContextHashesOnceForSeveralSpans(t *testing.T) {
	t.Parallel()
	meter, exporter, _ := transactionTestMeter(t, true)
	ctx := TransactionTraceContext(context.Background(), meter, []byte("hashed once"))
	_, first := StartTransactionSpan(ctx, meter, nil, "first")
	first.End()
	_, second := StartTransactionSpan(ctx, meter, nil, "second")
	second.End()
	_, replaced := StartTransactionSpan(ctx, meter, []byte("other"), "replaced")
	replaced.End()

	spans := exporter.GetSpans()
	if len(spans) != 3 {
		t.Fatalf("expected 3 spans, got %d", len(spans))
	}
	for _, span := range spans[:2] {
		if got := transactionTestAttributes(span)["tx_hash"].AsString(); got != transactionTestHash([]byte("hashed once")) {
			t.Fatalf("span %q did not inherit the context hash: %q", span.Name, got)
		}
	}
	if got := transactionTestAttributes(spans[2])["tx_hash"].AsString(); got != transactionTestHash([]byte("other")) {
		t.Fatalf("transaction bytes did not replace the context hash: %q", got)
	}
}

func TestTransactionTraceContextDisabledReturnsContextUnchanged(t *testing.T) {
	t.Parallel()
	disabledMeter, _, _ := transactionTestMeter(t, false)
	type requestValueKey struct{}
	ctx := context.WithValue(context.Background(), requestValueKey{}, "request")
	for name, meter := range map[string]Meter{"disabled": disabledMeter, "nil": NewNilMeter(), "none": nil} {
		if got := TransactionTraceContext(ctx, meter, []byte("transaction")); got != ctx {
			t.Fatalf("%s meter: context was changed", name)
		}
	}
	if got := TransactionTraceContext(nil, disabledMeter, []byte("transaction")); got == nil { //nolint:staticcheck // nil ctx is the case under test
		t.Fatal("nil context must be replaced")
	}
}

func gossipSwitchTestMeter(t *testing.T, cfg Config) Meter {
	t.Helper()
	providers := &Metrics{
		cfg:            cfg,
		tracerProvider: sdktrace.NewTracerProvider(),
	}
	t.Cleanup(func() { _ = providers.tracerProvider.Shutdown(context.Background()) })
	meter, err := providers.NewMeter("gossip-switch-test")
	if err != nil {
		t.Fatal(err)
	}
	return meter
}

func TestGossipTracingSwitches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		cfg           Config
		expectReceive bool
		expectSend    bool
	}{
		{name: "tracing off", cfg: Config{TraceGossipReceive: true, TraceGossipSend: true}},
		{name: "tracing on, switches off", cfg: Config{TracingEnabled: true}},
		{name: "receive only", cfg: Config{TracingEnabled: true, TraceGossipReceive: true}, expectReceive: true},
		{name: "send only", cfg: Config{TracingEnabled: true, TraceGossipSend: true}, expectSend: true},
		{name: "both", cfg: Config{TracingEnabled: true, TraceGossipReceive: true, TraceGossipSend: true}, expectReceive: true, expectSend: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meter := gossipSwitchTestMeter(t, tc.cfg)
			if got := GossipReceiveTracingEnabled(meter); got != tc.expectReceive {
				t.Fatalf("receive: got %v, want %v", got, tc.expectReceive)
			}
			if got := GossipSendTracingEnabled(meter); got != tc.expectSend {
				t.Fatalf("send: got %v, want %v", got, tc.expectSend)
			}
			// A sub-meter shares the parent's configuration.
			sub := meter.SubMeter("mempool")
			if got := GossipReceiveTracingEnabled(sub); got != tc.expectReceive {
				t.Fatalf("sub-meter receive: got %v, want %v", got, tc.expectReceive)
			}
			if got := GossipSendTracingEnabled(sub); got != tc.expectSend {
				t.Fatalf("sub-meter send: got %v, want %v", got, tc.expectSend)
			}
		})
	}
}

func TestGossipTracingSwitchesOffForNilAndCustomMeters(t *testing.T) {
	t.Parallel()
	type customMeter struct{ Meter }
	for name, meter := range map[string]Meter{"nil": NewNilMeter(), "custom": customMeter{}} {
		if GossipReceiveTracingEnabled(meter) || GossipSendTracingEnabled(meter) {
			t.Fatalf("%s meter should keep gossip tracing disabled", name)
		}
	}
}

func TestTransactionTracingUnsupportedCustomMeter(t *testing.T) {
	t.Parallel()
	// Embedding preserves compatibility with existing custom Meter wrappers.
	type customMeter struct{ Meter }
	if TransactionTracingEnabled(customMeter{}) {
		t.Fatal("unsupported custom meter should remain disabled")
	}
}

func TestTransactionSpansKeepIndependentNodeTraces(t *testing.T) {
	t.Parallel()
	meter, exporter, _ := transactionTestMeter(t, true)
	transaction := []byte("same signed transaction on independent nodes")
	_, first := StartTransactionSpan(context.Background(), meter, transaction, "first-node")
	_, second := StartTransactionSpan(context.Background(), meter, transaction, "second-node")
	first.End()
	second.End()
	if first.SpanContext().TraceID() == second.SpanContext().TraceID() {
		t.Fatal("transaction hash manufactured a shared trace ID")
	}
	spans := exporter.GetSpans()
	if transactionTestAttributes(spans[0])["tx_hash"] != transactionTestAttributes(spans[1])["tx_hash"] {
		t.Fatal("independent node traces cannot correlate by transaction hash")
	}
}

func TestTransactionSpanPreservesScopeTags(t *testing.T) {
	t.Parallel()
	meter, exporter, _ := transactionTestMeter(t, true)
	meter = meter.SubMeter("mempool", Tag("chain_id", "injective-test"), TraceTag("node_id", "sentry-one"), MetricTag("metric_only", "exclude"))
	_, span := StartTransactionSpan(context.Background(), meter, []byte("signed transaction"), "tx.mempool.received")
	span.End()
	attributes := transactionTestAttributes(exporter.GetSpans()[0])
	if attributes["chain_id"].AsString() != "injective-test" || attributes["node_id"].AsString() != "sentry-one" {
		t.Fatalf("transaction span lost scope tags: %v", attributes)
	}
	if _, present := attributes["metric_only"]; present {
		t.Fatal("scope metric-only tag leaked into span")
	}
}

func BenchmarkTransactionSpanDisabled(b *testing.B) {
	transaction := make([]byte, 1024*1024)
	ctx := context.Background()
	meter := NewNilMeter()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if TransactionTracingEnabled(meter) {
			_, span := StartTransactionSpan(ctx, meter, transaction, "disabled")
			span.End()
		}
	}
}

func BenchmarkTransactionSpanDisabledDirect(b *testing.B) {
	transaction := make([]byte, 1024*1024)
	ctx := context.Background()
	meter := NewNilMeter()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		_, span := StartTransactionSpan(ctx, meter, transaction, "disabled")
		span.End()
	}
}

type transactionUnavailableExporter struct {
	started  chan struct{}
	unblock  chan struct{}
	exported atomic.Int64
}

func (exporter *transactionUnavailableExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	exporter.exported.Add(int64(len(spans)))
	select {
	case exporter.started <- struct{}{}:
	default:
	}
	<-exporter.unblock
	return errors.New("collector unavailable")
}

func (*transactionUnavailableExporter) Shutdown(context.Context) error { return nil }

func TestTransactionSpansDoNotBlockOnUnavailableExporter(t *testing.T) {
	t.Parallel()
	exporter := &transactionUnavailableExporter{started: make(chan struct{}, 1), unblock: make(chan struct{})}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter,
		sdktrace.WithMaxQueueSize(1), sdktrace.WithMaxExportBatchSize(1)))
	providers := &Metrics{cfg: Config{TracingEnabled: true}, tracerProvider: provider}
	meter, err := providers.NewMeter("transaction-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(exporter.unblock)
		_ = provider.Shutdown(context.Background())
	})
	_, first := StartTransactionSpan(context.Background(), meter, []byte("transaction"), "first")
	first.End()
	select {
	case <-exporter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("exporter did not receive the first batch")
	}
	completed := make(chan struct{})
	go func() {
		defer close(completed)
		for range 100 {
			_, span := StartTransactionSpan(context.Background(), meter, []byte("transaction"), "during-outage")
			span.End()
		}
	}()
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("span completion blocked behind the unavailable exporter")
	}
	if exporter.exported.Load() != 1 {
		t.Fatal("exporter unexpectedly progressed before outage release")
	}
}
