package metrics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type deferredTestSetup struct {
	providers *Metrics
	meter     Meter
	exporter  *tracetest.InMemoryExporter
	reader    *sdkmetric.ManualReader
}

// deferredTestProviders builds the providers NewMetrics would, on an
// in-memory exporter and a manual reader. A negative queue size gives the
// inline path, as in production.
func deferredTestProviders(t testing.TB, cfg Config, options ...sdktrace.TracerProviderOption) *deferredTestSetup {
	t.Helper()
	setup := &deferredTestSetup{exporter: tracetest.NewInMemoryExporter(), reader: sdkmetric.NewManualReader()}
	options = append(options, sdktrace.WithIDGenerator(presetIDGenerator{}))
	if _, benchmark := t.(*testing.B); !benchmark {
		// Benchmarks bring their own exporter; this one keeps every span.
		options = append(options, sdktrace.WithSyncer(setup.exporter))
	}
	setup.providers = &Metrics{cfg: cfg, sampleLimit: transactionSampleLimit(cfg.TraceSampleRate)}
	if cfg.MetricsEnabled {
		setup.providers.meterProvider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(setup.reader))
	}
	if cfg.TracingEnabled {
		setup.providers.tracerProvider = sdktrace.NewTracerProvider(options...)
	}
	setup.providers.deferred = newDeferredPipeline(&setup.providers.cfg)
	t.Cleanup(func() { _ = setup.providers.Shutdown() })

	meter, err := setup.providers.NewMeter("app", Tag("chain", "test"))
	if err != nil {
		t.Fatal(err)
	}
	setup.meter = meter
	return setup
}

func deferredTestConfig(queueSize int) Config {
	return Config{MetricsEnabled: true, TracingEnabled: true, TelemetryQueueSize: queueSize, TelemetryWorkers: 2}
}

func (setup *deferredTestSetup) flush(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := setup.providers.FlushTraces(ctx); err != nil {
		t.Fatal(err)
	}
}

func (setup *deferredTestSetup) span(t testing.TB, name string) tracetest.SpanStub {
	t.Helper()
	for _, span := range setup.exporter.GetSpans() {
		if span.Name == name {
			return span
		}
	}
	t.Fatalf("span %q not exported", name)
	return tracetest.SpanStub{}
}

// describeSpans renders every exported span without what differs between two
// runs: identifiers and times. Parents are named.
func (setup *deferredTestSetup) describeSpans() []string {
	spans := setup.exporter.GetSpans()
	names := map[trace.SpanID]string{}
	for _, span := range spans {
		names[span.SpanContext.SpanID()] = span.Name
	}
	lines := make([]string, 0, len(spans))
	for _, span := range spans {
		attributes := []string{}
		for _, keyValue := range span.Attributes {
			attributes = append(attributes, string(keyValue.Key)+"="+keyValue.Value.Emit())
		}
		sort.Strings(attributes)
		events := []string{}
		for _, event := range span.Events {
			eventAttributes := []string{}
			for _, keyValue := range event.Attributes {
				if keyValue.Key == "exception.stacktrace" {
					continue
				}
				eventAttributes = append(eventAttributes, string(keyValue.Key)+"="+keyValue.Value.Emit())
			}
			events = append(events, event.Name+strings.Join(eventAttributes, ","))
		}
		parent := "-"
		if span.Parent.IsValid() {
			parent = names[span.Parent.SpanID()]
		}
		lines = append(lines, fmt.Sprintf("%s parent=%s scope=%s sampled=%t status=%v/%q attributes=%v events=%v zero=%t",
			span.Name, parent, span.InstrumentationScope.Name, span.SpanContext.IsSampled(),
			span.Status.Code, span.Status.Description, attributes, events,
			strings.HasPrefix(span.Name, "tx.block.") && span.StartTime.Equal(span.EndTime)))
	}
	sort.Strings(lines)
	return lines
}

// describeMetrics renders every data point: counter values and histogram
// counts, by scope, instrument and attributes.
func (setup *deferredTestSetup) describeMetrics(t testing.TB) []string {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := setup.reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	lines := []string{}
	for _, scope := range collected.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if strings.HasPrefix(instrument.Name, "telemetry.") {
				continue
			}
			switch data := instrument.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					lines = append(lines, fmt.Sprintf("%s %s %s = %d", scope.Scope.Name, instrument.Name, point.Attributes.Encoded(attribute.DefaultEncoder()), point.Value))
				}
			case metricdata.Histogram[int64]:
				for _, point := range data.DataPoints {
					lines = append(lines, fmt.Sprintf("%s %s %s count %d", scope.Scope.Name, instrument.Name, point.Attributes.Encoded(attribute.DefaultEncoder()), point.Count))
				}
			}
		}
	}
	sort.Strings(lines)
	return lines
}

// deferredTestScenario exercises every kind of call the library offers the
// way the node makes them.
func deferredTestScenario(root Meter) {
	bank := root.SubMeter("bank", Tag("svc", "bank"))
	failure := errors.New("insufficient funds")
	transaction := []byte("transaction-bytes")
	observedAt := time.Unix(1700000000, 123456789)

	// A transaction stage span with children, attributes and a status.
	ctx, stage := StartTransactionSpan(context.Background(), root, transaction, "tx.mempool.check", TraceTag("peer", "p1"))
	stage.SetAttributes(attribute.String("outcome", "accepted"))

	holder := &deferredTestContext{ctx: ctx}
	func() {
		defer bank.FuncTiming(holder, "SendCoins", Tag("denom", "inj"), TraceTag("height", int64(7)), MetricTag("lane", "fast"))()
		func() {
			inner := &deferredTestContext{ctx: holder.ctx}
			defer bank.FuncTiming(inner, "SubtractCoins")()
		}()
		func() {
			var err error
			inner := &deferredTestContext{ctx: holder.ctx}
			defer bank.FuncTiming(inner, "AddCoins")(&err)
			err = failure
		}()
	}()

	// FuncTimingCtx with an explicit FuncError before the stop.
	explicitCtx, stop := bank.FuncTimingCtx(ctx, "Explicit")
	bank.FuncError(explicitCtx, "Explicit", failure)
	stop()

	// A span changed through the context, as other instrumentation would.
	changedCtx, stopChanged := root.FuncTimingCtx(ctx, "Changed")
	changed := trace.SpanFromContext(changedCtx)
	changed.SetAttributes(attribute.Int64("gas", 21000))
	changed.AddEvent("checkpoint", trace.WithAttributes(attribute.Bool("cached", true)))
	changed.RecordError(failure)
	changed.SetStatus(codes.Error, "failed before the end")
	stopChanged()

	stage.SetStatus(codes.Ok, "")
	stage.End()

	// Stages that reuse the hash of the context, and point events.
	_, recheck := StartTransactionSpan(ctx, root, nil, "tx.mempool.recheck")
	recheck.SetStatus(codes.Error, "evicted")
	recheck.End()
	RecordTransactionEvent(context.Background(), root, transaction, "tx.block.delivered", observedAt, TraceTag("height", int64(8)))
	RecordTransactionEvent(ctx, root, nil, "tx.block.committed", observedAt)

	// A call outside any trace starts one.
	lone := &deferredTestContext{ctx: context.Background()}
	func() { defer root.FuncTiming(lone, "EndBlocker")() }()
}

// The deferred pipeline must export what the inline path exports.
func TestDeferredTelemetryMatchesInline(t *testing.T) {
	inline := deferredTestProviders(t, deferredTestConfig(-1))
	if inline.providers.deferred != nil {
		t.Fatal("a negative queue size must turn the pipeline off")
	}
	deferredTestScenario(inline.meter)

	deferred := deferredTestProviders(t, deferredTestConfig(0))
	deferredTestScenario(deferred.meter)
	deferred.flush(t)

	wantSpans, gotSpans := inline.describeSpans(), deferred.describeSpans()
	if len(wantSpans) != 10 {
		t.Fatalf("the scenario should export 10 spans inline, got %d:\n%s", len(wantSpans), strings.Join(wantSpans, "\n"))
	}
	if strings.Join(gotSpans, "\n") != strings.Join(wantSpans, "\n") {
		t.Fatalf("spans differ\ninline:\n%s\ndeferred:\n%s", strings.Join(wantSpans, "\n"), strings.Join(gotSpans, "\n"))
	}

	wantMetrics, gotMetrics := inline.describeMetrics(t), deferred.describeMetrics(t)
	if len(wantMetrics) == 0 {
		t.Fatal("the scenario should record metrics")
	}
	if strings.Join(gotMetrics, "\n") != strings.Join(wantMetrics, "\n") {
		t.Fatalf("metrics differ\ninline:\n%s\ndeferred:\n%s", strings.Join(wantMetrics, "\n"), strings.Join(gotMetrics, "\n"))
	}

	stats := deferred.providers.TelemetryQueueStats()
	if stats.Enqueued == 0 || stats.Processed != stats.Enqueued || stats.Dropped != 0 || stats.Failed != 0 {
		t.Fatalf("unexpected queue stats: %+v", stats)
	}
}

// What the instrumented code reads from the context while a function runs
// is what the exported span carries.
func TestDeferredSpanIdentityAndTimes(t *testing.T) {
	setup := deferredTestProviders(t, deferredTestConfig(0))
	if otelSpanKey == nil {
		t.Fatal("the OpenTelemetry span key was not captured; the span is no longer its own context")
	}

	type valueKey struct{}
	deadline := time.Now().Add(time.Hour)
	base, cancel := context.WithDeadline(context.WithValue(context.Background(), valueKey{}, "kept"), deadline)
	defer cancel()

	before := time.Now()
	parentCtx, stopParent := setup.meter.FuncTimingCtx(base, "Parent")
	parent := trace.SpanContextFromContext(parentCtx)
	childCtx, stopChild := setup.meter.FuncTimingCtx(parentCtx, "Child")
	child := trace.SpanContextFromContext(childCtx)
	time.Sleep(2 * time.Millisecond)
	stopChild()
	stopParent()
	after := time.Now()

	if !parent.IsValid() || !parent.IsSampled() || !child.IsSampled() {
		t.Fatalf("span contexts: parent %v, child %v", parent, child)
	}
	if child.TraceID() != parent.TraceID() || child.SpanID() == parent.SpanID() {
		t.Fatalf("child %v is not in the trace of %v", child, parent)
	}
	if trace.SpanFromContext(childCtx).IsRecording() {
		t.Fatal("a span must not be recording after its stop function ran")
	}

	// The context still is the caller's context.
	if got, _ := childCtx.Deadline(); !got.Equal(deadline) {
		t.Fatalf("deadline lost: %v", got)
	}
	if childCtx.Value(valueKey{}) != "kept" || childCtx.Err() != nil || childCtx.Done() != base.Done() {
		t.Fatal("the span context does not behave as its parent context")
	}
	derived, cancelDerived := context.WithCancel(childCtx)
	cancel()
	select {
	case <-derived.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation does not reach a context derived from the span context")
	}
	cancelDerived()

	setup.flush(t)
	exportedParent, exportedChild := setup.span(t, "Parent"), setup.span(t, "Child")
	if exportedParent.SpanContext.SpanID() != parent.SpanID() || exportedParent.SpanContext.TraceID() != parent.TraceID() {
		t.Fatalf("parent exported as %v, was %v", exportedParent.SpanContext, parent)
	}
	if exportedChild.SpanContext.SpanID() != child.SpanID() || exportedChild.Parent.SpanID() != parent.SpanID() {
		t.Fatalf("child exported as %v under %v, was %v under %v", exportedChild.SpanContext, exportedChild.Parent, child, parent)
	}
	if exportedParent.Parent.IsValid() {
		t.Fatal("the first span of a trace has no parent")
	}
	for _, span := range []tracetest.SpanStub{exportedParent, exportedChild} {
		if span.StartTime.Before(before.Add(-time.Millisecond)) || span.EndTime.After(after.Add(time.Millisecond)) || span.EndTime.Before(span.StartTime) {
			t.Fatalf("%s: times %v..%v outside the call %v..%v", span.Name, span.StartTime, span.EndTime, before, after)
		}
	}
	if duration := exportedChild.EndTime.Sub(exportedChild.StartTime); duration < 2*time.Millisecond || duration > time.Second {
		t.Fatalf("child duration %v", duration)
	}
}

func TestDeferredEventKeepsGivenTimestamp(t *testing.T) {
	setup := deferredTestProviders(t, deferredTestConfig(0))
	observedAt := time.Unix(1700000000, 123456789)
	transaction := []byte("transaction")
	RecordTransactionEvent(context.Background(), setup.meter, transaction, "tx.block.delivered", observedAt)
	setup.flush(t)

	span := setup.span(t, "tx.block.delivered")
	if !span.StartTime.Equal(observedAt) || !span.EndTime.Equal(observedAt) {
		t.Fatalf("times %v..%v, want %v", span.StartTime, span.EndTime, observedAt)
	}
	if got := transactionTestAttributes(span)["tx_hash"].AsString(); got != transactionTestHash(transaction) {
		t.Fatalf("tx_hash %q", got)
	}
}

// A full queue costs records, never time.
func TestDeferredQueueFullDropsAndCounts(t *testing.T) {
	setup := deferredTestProviders(t, deferredTestConfig(-1))
	// A pipeline nobody drains.
	pipeline := &deferredPipeline{queue: newSpanQueue(1), deferRoots: true, stop: make(chan struct{})}
	setup.providers.deferred = pipeline
	capacity := len(pipeline.queue.cells)

	holder := &deferredTestContext{ctx: context.Background()}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range capacity + 10 {
			deferredBenchmarkCall(setup.meter, holder, "Busy")
		}
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("calls block on a full queue")
	}

	stats := setup.providers.TelemetryQueueStats()
	if stats.Enqueued != uint64(capacity) || stats.Dropped != 10 || stats.Processed != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	setup.flush(t)
	if spans := len(setup.exporter.GetSpans()); spans != capacity {
		t.Fatalf("flush exported %d spans, want %d", spans, capacity)
	}
	if lines := setup.describeMetrics(t); len(lines) != 2 || !strings.HasSuffix(lines[0], fmt.Sprintf("= %d", capacity)) {
		t.Fatalf("metrics of the accepted records: %v", lines)
	}
}

func TestDeferredQueueStatsExportedAsMetrics(t *testing.T) {
	setup := deferredTestProviders(t, deferredTestConfig(0))
	if err := setup.providers.registerTelemetryQueueMetrics(); err != nil {
		t.Fatal(err)
	}
	holder := &deferredTestContext{ctx: context.Background()}
	for range 5 {
		deferredBenchmarkCall(setup.meter, holder, "Call")
	}
	setup.flush(t)

	var collected metricdata.ResourceMetrics
	if err := setup.reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	values := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			if sum, isSum := instrument.Data.(metricdata.Sum[int64]); isSum && strings.HasPrefix(instrument.Name, "telemetry.") {
				values[instrument.Name] = sum.DataPoints[0].Value
			}
		}
	}
	want := map[string]int64{
		TelemetryRecordsEnqueuedMetric: 5, TelemetryRecordsProcessedMetric: 5,
		TelemetryRecordsDroppedMetric: 0, TelemetryRecordsFailedMetric: 0,
	}
	if fmt.Sprint(values) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", values, want)
	}
}

// Shutdown exports what the queue still holds.
func TestDeferredShutdownDrainsQueue(t *testing.T) {
	setup := deferredTestProviders(t, deferredTestConfig(0))
	holder := &deferredTestContext{ctx: context.Background()}
	for range 1000 {
		deferredBenchmarkCall(setup.meter, holder, "Call")
	}
	exporter := setup.exporter
	// InMemoryExporter forgets its spans on Shutdown; read them through a
	// flush of the queue first, then make sure Shutdown leaves nothing.
	setup.providers.deferred.close()
	if spans := len(exporter.GetSpans()); spans != 1000 {
		t.Fatalf("%d spans after close, want 1000", spans)
	}
	stats := setup.providers.TelemetryQueueStats()
	if stats.Processed != 1000 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	// Calls after the shutdown are harmless.
	deferredBenchmarkCall(setup.meter, holder, "Late")
	if after := setup.providers.TelemetryQueueStats(); after.Enqueued != stats.Enqueued || after.Dropped != 0 {
		t.Fatalf("a record was taken after close: %+v", after)
	}
}

// Below a parent that is not sampled no span is exported, the metrics are
// still recorded, and the decision reaches the next level.
func TestDeferredUnsampledParent(t *testing.T) {
	setup := deferredTestProviders(t, deferredTestConfig(0))
	unsampled := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), unsampled)

	outerCtx, stopOuter := setup.meter.FuncTimingCtx(ctx, "Outer")
	outer := trace.SpanFromContext(outerCtx)
	innerCtx, stopInner := setup.meter.FuncTimingCtx(outerCtx, "Inner")
	inner := trace.SpanContextFromContext(innerCtx)
	stopInner()
	stopOuter()
	RecordTransactionEvent(outerCtx, setup.meter, []byte("tx"), "tx.event", time.Time{})
	setup.flush(t)

	if outer.IsRecording() || outer.SpanContext().IsSampled() || inner.IsSampled() {
		t.Fatal("spans below an unsampled parent must not be sampled")
	}
	if outer.SpanContext().TraceID() != unsampled.TraceID() || inner.TraceID() != unsampled.TraceID() {
		t.Fatal("the trace ID must still propagate")
	}
	if spans := setup.exporter.GetSpans(); len(spans) != 0 {
		t.Fatalf("exported %d spans of an unsampled trace", len(spans))
	}
	if lines := setup.describeMetrics(t); len(lines) != 4 {
		t.Fatalf("func metrics must not depend on sampling: %v", lines)
	}
}

// With a sampler that may reject traces, the first span of a trace starts
// in the SDK and the spans below it stay deferred.
func TestDeferredRootsFollowSampler(t *testing.T) {
	for _, test := range []struct {
		sampler sdktrace.Sampler
		spans   int
	}{
		{sdktrace.ParentBased(sdktrace.AlwaysSample()), 2},
		{sdktrace.ParentBased(sdktrace.NeverSample()), 0},
	} {
		setup := deferredTestProviders(t, deferredTestConfig(0), sdktrace.WithSampler(test.sampler))
		setup.providers.deferred.deferRoots = false

		rootCtx, stopRoot := setup.meter.FuncTimingCtx(context.Background(), "Root")
		if _, deferred := trace.SpanFromContext(rootCtx).(*deferredSpan); deferred {
			t.Fatal("the first span of a trace must go through the sampler")
		}
		childCtx, stopChild := setup.meter.FuncTimingCtx(rootCtx, "Child")
		if _, deferred := trace.SpanFromContext(childCtx).(*deferredSpan); !deferred {
			t.Fatal("spans below the first one must be deferred")
		}
		stopChild()
		stopRoot()
		setup.flush(t)

		if spans := len(setup.exporter.GetSpans()); spans != test.spans {
			t.Fatalf("%s: %d spans, want %d", test.sampler.Description(), spans, test.spans)
		}
		if test.spans == 2 {
			if root, child := setup.span(t, "Root"), setup.span(t, "Child"); child.Parent.SpanID() != root.SpanContext.SpanID() {
				t.Fatal("the deferred child lost its parent")
			}
		}
		if lines := setup.describeMetrics(t); len(lines) != 4 {
			t.Fatalf("metrics: %v", lines)
		}
	}
}

func TestRootSpansAlwaysSampledReadsEnvironment(t *testing.T) {
	for value, want := range map[string]bool{
		"": true, "always_on": true, "parentbased_always_on": true, " ParentBased_Always_On ": true,
		"always_off": false, "traceidratio": false, "parentbased_traceidratio": false, "unknown": false,
	} {
		t.Setenv("OTEL_TRACES_SAMPLER", value)
		if got := rootSpansAlwaysSampled(); got != want {
			t.Fatalf("%q: got %t", value, got)
		}
	}
}

func TestDeferredStuckFunctionWatchdog(t *testing.T) {
	cfg := deferredTestConfig(0)
	cfg.StuckFuncTimeout = 20 * time.Millisecond
	setup := deferredTestProviders(t, cfg)

	_, stopFast := setup.meter.FuncTimingCtx(context.Background(), "Fast")
	stopFast()
	_, stopStuck := setup.meter.FuncTimingCtx(context.Background(), "Stuck")
	deadline := time.Now().Add(10 * time.Second)
	for len(setup.exporter.GetSpans()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the stuck span was not exported while the function runs")
		}
		time.Sleep(time.Millisecond)
	}
	stopStuck()
	setup.flush(t)

	stuck := setup.span(t, "Stuck")
	if stuck.Status.Code != codes.Error || stuck.Status.Description != "stuck" || len(stuck.Events) != 1 {
		t.Fatalf("stuck span: status %+v, %d events", stuck.Status, len(stuck.Events))
	}
	if fast := setup.span(t, "Fast"); fast.Status.Code != codes.Ok {
		t.Fatalf("fast span: %+v", fast.Status)
	}
	metrics := strings.Join(setup.describeMetrics(t), "\n")
	for _, want := range []string{"func.timing.timeout", "func.called chain=test,func_name=Stuck = 1", "func.timing chain=test,func_name=Stuck count 1"} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("missing %q in:\n%s", want, metrics)
		}
	}
}

type deferredPanickingError struct{}

func (deferredPanickingError) Error() string { panic("error text unavailable") }

// A record that cannot be processed is counted and the worker goes on.
func TestDeferredWorkerSurvivesPanic(t *testing.T) {
	setup := deferredTestProviders(t, deferredTestConfig(0))
	func() {
		var err error
		_, stop := setup.meter.FuncTimingCtx(context.Background(), "Broken")
		defer stop(&err)
		err = deferredPanickingError{}
	}()
	_, stop := setup.meter.FuncTimingCtx(context.Background(), "Healthy")
	stop()
	setup.flush(t)

	stats := setup.providers.TelemetryQueueStats()
	if stats.Failed != 1 || stats.Processed != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
	setup.span(t, "Healthy")
}

func TestDeferredMetricsOnly(t *testing.T) {
	cfg := deferredTestConfig(0)
	cfg.TracingEnabled = false
	setup := deferredTestProviders(t, cfg)

	base := context.Background()
	ctx, stop := setup.meter.FuncTimingCtx(base, "Call", Tag("kind", "a"))
	if ctx != base {
		t.Fatal("without tracing the context must come back unchanged")
	}
	failure := errors.New("failed")
	stop(&failure)
	if err := setup.providers.deferred.flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"app func.called chain=test,func_name=Call,kind=a = 1",
		"app func.error chain=test,func_name=Call,kind=a = 1",
		"app func.timing chain=test,func_name=Call,kind=a count 1",
	}
	if got := setup.describeMetrics(t); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %v", got)
	}
}

// Many goroutines, nested calls, every record accounted for. Meant for the
// race detector.
func TestDeferredConcurrentCalls(t *testing.T) {
	setup := deferredTestProviders(t, deferredTestConfig(0))
	const goroutines, calls = 8, 500

	var group sync.WaitGroup
	for range goroutines {
		group.Add(1)
		go func() {
			defer group.Done()
			for range calls {
				outer := &deferredTestContext{ctx: context.Background()}
				func() {
					defer setup.meter.FuncTiming(outer, "Outer")()
					span := trace.SpanFromContext(outer.ctx)
					var inner sync.WaitGroup
					inner.Add(1)
					go func() { // another goroutine touches the span through the context
						defer inner.Done()
						span.SetAttributes(attribute.Bool("touched", true))
						_ = span.IsRecording()
					}()
					nested := &deferredTestContext{ctx: outer.ctx}
					func() { defer setup.meter.FuncTiming(nested, "Inner")() }()
					inner.Wait()
				}()
			}
		}()
	}
	group.Wait()
	setup.flush(t)

	if spans := len(setup.exporter.GetSpans()); spans != 2*goroutines*calls {
		t.Fatalf("%d spans, want %d", spans, 2*goroutines*calls)
	}
	stats := setup.providers.TelemetryQueueStats()
	if stats.Enqueued != 2*goroutines*calls || stats.Processed != stats.Enqueued || stats.Dropped != 0 || stats.Failed != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestSpanQueueManyProducersManyConsumers(t *testing.T) {
	queue := newSpanQueue(64)
	const producers, consumers, perProducer = 4, 3, 20000

	var produced, consumed sync.WaitGroup
	seen := make([][]int64, consumers)
	done := make(chan struct{})
	for consumer := range consumers {
		consumed.Add(1)
		go func() {
			defer consumed.Done()
			for {
				if span := queue.pop(); span != nil {
					seen[consumer] = append(seen[consumer], span.start)
					continue
				}
				select {
				case <-done:
					if queue.pop() == nil {
						return
					}
					t.Error("queue not empty after the producers finished and a pop returned nil")
					return
				default:
				}
			}
		}()
	}
	for producer := range producers {
		produced.Add(1)
		go func() {
			defer produced.Done()
			for index := range perProducer {
				span := &deferredSpan{start: int64(producer*perProducer + index)}
				for !queue.push(span) {
					time.Sleep(time.Microsecond)
				}
			}
		}()
	}
	produced.Wait()
	for queue.head.Load() != queue.tail.Load() {
		time.Sleep(time.Millisecond)
	}
	close(done)
	consumed.Wait()

	all := map[int64]bool{}
	for _, values := range seen {
		for _, value := range values {
			if all[value] {
				t.Fatalf("record %d delivered twice", value)
			}
			all[value] = true
		}
	}
	if len(all) != producers*perProducer {
		t.Fatalf("%d records delivered, want %d", len(all), producers*perProducer)
	}
}

func TestSpanQueueCapacity(t *testing.T) {
	for size, want := range map[int]int{1: 64, 64: 64, 65: 128, 1000: 1024, 1 << 18: 1 << 18} {
		if got := len(newSpanQueue(size).cells); got != want {
			t.Fatalf("size %d: capacity %d, want %d", size, got, want)
		}
	}
	queue := newSpanQueue(64)
	for index := range 64 {
		if !queue.push(&deferredSpan{}) {
			t.Fatalf("push %d refused", index)
		}
	}
	if queue.push(&deferredSpan{}) {
		t.Fatal("push into a full queue accepted")
	}
	if queue.pop() == nil || !queue.push(&deferredSpan{}) {
		t.Fatal("a pop must make room for a push")
	}
}

func TestDeferredClockFollowsWallClock(t *testing.T) {
	for range 100 {
		before := time.Now()
		got := wallTime(monotonicNow())
		after := time.Now()
		if got.Before(before.Add(-clockStepThresholdDuration)) || got.After(after.Add(clockStepThresholdDuration)) {
			t.Fatalf("%v outside %v..%v", got, before, after)
		}
	}
	// A stepped wall clock is followed; reading noise is not.
	saved := clockOffset.Load()
	defer clockOffset.Store(saved)
	clockOffset.Store(saved + int64(time.Hour))
	refreshClockOffset()
	if drift := clockOffset.Load() - saved; drift > clockStepThreshold || drift < -clockStepThreshold {
		t.Fatalf("offset not corrected, off by %v", time.Duration(drift))
	}
	settled := clockOffset.Load()
	refreshClockOffset()
	if clockOffset.Load() != settled {
		t.Fatal("offset must not follow reading noise")
	}
}

const clockStepThresholdDuration = time.Duration(clockStepThreshold)

// The record is sized to stay in the 128-byte allocation class.
func TestDeferredSpanSize(t *testing.T) {
	if size := unsafe.Sizeof(deferredSpan{}); size > 128 {
		t.Fatalf("deferredSpan is %d bytes", size)
	}
}

// BenchmarkFuncTimingDeferred measures what a timed call costs the calling
// goroutine with the pipeline on, next to BenchmarkFuncTiming/timed for the
// inline path. dropped/op above zero means the workers of this machine did
// not keep up with a loop that does nothing but call.
func BenchmarkFuncTimingDeferred(b *testing.B) {
	run := func(b *testing.B, cfg Config, tags ...TagAttribute) {
		setup := deferredTestProviders(b, cfg, sdktrace.WithBatcher(deferredDiscardExporter{}))
		meter := setup.meter.SubMeter("bank", Tag("svc", "bank"))
		parentCtx, stop := meter.FuncTimingCtx(context.Background(), "Parent")
		defer stop()
		holder := &deferredTestContext{ctx: parentCtx}
		call := func() {
			saved := holder.ctx
			defer meter.FuncTiming(holder, "SendCoins", tags...)()
			holder.ctx = saved
		}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			call()
		}
		b.StopTimer()
		b.ReportMetric(float64(setup.providers.TelemetryQueueStats().Dropped)/float64(b.N), "dropped/op")
	}
	// The queue takes every record and nothing drains it: the cost of the
	// calling goroutine alone.
	b.Run("caller-only", func(b *testing.B) {
		cfg := deferredTestConfig(-1)
		setup := deferredTestProviders(b, cfg, sdktrace.WithBatcher(deferredDiscardExporter{}))
		setup.providers.deferred = &deferredPipeline{queue: newSpanQueue(b.N + 2), deferRoots: true, stop: make(chan struct{})}
		setup.providers.deferred.closed.Store(false)
		meter := setup.meter.SubMeter("bank", Tag("svc", "bank"))
		parentCtx, _ := meter.FuncTimingCtx(context.Background(), "Parent")
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
	})
	b.Run("traces-and-metrics", func(b *testing.B) { run(b, deferredTestConfig(0)) })
	b.Run("with-tag", func(b *testing.B) { run(b, deferredTestConfig(0), Tag("denom", "inj")) })
	b.Run("metrics-only", func(b *testing.B) {
		cfg := deferredTestConfig(0)
		cfg.TracingEnabled = false
		run(b, cfg)
	})
	b.Run("inline", func(b *testing.B) { run(b, deferredTestConfig(-1)) })
}

// BenchmarkTelemetryWorker measures what one record costs a worker: the SDK
// span and the three metric updates.
func BenchmarkTelemetryWorker(b *testing.B) {
	setup := deferredTestProviders(b, deferredTestConfig(-1), sdktrace.WithBatcher(deferredDiscardExporter{}))
	pipeline := &deferredPipeline{queue: newSpanQueue(b.N + 1), deferRoots: true, stop: make(chan struct{})}
	setup.providers.deferred = pipeline
	meter := setup.meter.SubMeter("bank", Tag("svc", "bank"))
	parentCtx, stop := meter.FuncTimingCtx(context.Background(), "Parent")
	for range b.N {
		_, stopCall := meter.FuncTimingCtx(parentCtx, "SendCoins")
		stopCall()
	}

	b.ReportAllocs()
	b.ResetTimer()
	for span := pipeline.queue.pop(); span != nil; span = pipeline.queue.pop() {
		pipeline.process(span)
	}
	b.StopTimer()
	stop()
}

type deferredTestContext struct{ ctx context.Context }

func (c *deferredTestContext) ContextPtr() *context.Context { return &c.ctx }

type deferredDiscardExporter struct{}

func (deferredDiscardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}
func (deferredDiscardExporter) Shutdown(context.Context) error { return nil }

// deferredBenchmarkCall makes one instrumented call the way a keeper does.
//
//go:noinline
func deferredBenchmarkCall(meter Meter, ctx *deferredTestContext, fn string) {
	saved := ctx.ctx
	defer meter.FuncTiming(ctx, fn)()
	ctx.ctx = saved
}
