package metrics

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

type funcSkipTestContext struct{ ctx context.Context }

func (c *funcSkipTestContext) ContextPtr() *context.Context { return &c.ctx }

func funcSkipTestMeter(t testing.TB, skip []string, processor sdktrace.TracerProviderOption) (Meter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	providers := &Metrics{
		cfg:            Config{MetricsEnabled: true, TracingEnabled: true, FuncTimingSkip: skip},
		meterProvider:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		tracerProvider: sdktrace.NewTracerProvider(processor),
	}
	t.Cleanup(func() {
		_ = providers.meterProvider.Shutdown(context.Background())
		_ = providers.tracerProvider.Shutdown(context.Background())
	})
	meter, err := providers.NewMeter("app")
	if err != nil {
		t.Fatal(err)
	}
	return meter, reader
}

func funcSkipTestSpanNames(exporter *tracetest.InMemoryExporter) []string {
	names := []string{}
	for _, span := range exporter.GetSpans() {
		names = append(names, span.Name)
	}
	sort.Strings(names)
	return names
}

// funcSkipTestFuncNames returns the func_name values of every data point the
// reader collected, across counters and histograms.
func funcSkipTestFuncNames(t *testing.T, reader *sdkmetric.ManualReader) map[string]int {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	count := func(set attribute.Set) {
		if name, ok := set.Value("func_name"); ok {
			names[name.AsString()]++
		}
	}
	for _, scope := range data.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			switch points := instrument.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range points.DataPoints {
					count(point.Attributes)
				}
			case metricdata.Histogram[int64]:
				for _, point := range points.DataPoints {
					count(point.Attributes)
				}
			}
		}
	}
	return names
}

func TestFuncSkipSet(t *testing.T) {
	t.Parallel()
	entries := []string{"GetParams", " bank.GetBalance ", "evm.GetState", "Engine.Evict", "exchange_risk.Engine.Evict", ""}
	cases := map[string][]string{
		"bank":          {"Engine.Evict", "GetBalance", "GetParams", "bank.GetBalance", "evm.GetState", "exchange_risk.Engine.Evict"},
		"evm":           {"Engine.Evict", "GetParams", "GetState", "bank.GetBalance", "evm.GetState", "exchange_risk.Engine.Evict"},
		"exchange_risk": {"Engine.Evict", "GetParams", "bank.GetBalance", "evm.GetState", "exchange_risk.Engine.Evict"},
	}
	for scope, want := range cases {
		got := []string{}
		for name := range funcSkipSet(scope, entries) {
			got = append(got, name)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("scope %q: got %v, want %v", scope, got, want)
		}
	}
	if set := funcSkipSet("bank", nil); set != nil {
		t.Fatalf("empty list must give a nil set, got %v", set)
	}
}

func TestFuncTimingSkipsListedFunctions(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	root, reader := funcSkipTestMeter(t, []string{"GetParams", "bank.GetBalance"}, sdktrace.WithSyncer(exporter))
	bank := root.SubMeter("bank")
	evm := root.SubMeter("evm")

	// Skipped by scope: the context keeps no span and nothing is recorded,
	// not even for a failing call.
	failure := errors.New("failed")
	ctx, stop := bank.FuncTimingCtx(context.Background(), "GetBalance")
	if trace.SpanFromContext(ctx).SpanContext().IsValid() {
		t.Fatal("skipped function started a span")
	}
	stop(&failure)

	wrapped := &funcSkipTestContext{ctx: context.Background()}
	bank.FuncTiming(wrapped, "GetBalance")()
	if trace.SpanFromContext(wrapped.ctx).SpanContext().IsValid() {
		t.Fatal("skipped function replaced the wrapped context")
	}

	// Skipped in every scope.
	bank.FuncTiming(wrapped, "GetParams")()
	_, stop = evm.FuncTimingCtx(context.Background(), "GetParams")
	stop()

	// Same name, other scope: still timed.
	ctx, stop = evm.FuncTimingCtx(context.Background(), "GetBalance")
	if !trace.SpanFromContext(ctx).SpanContext().IsValid() {
		t.Fatal("function outside the list did not start a span")
	}
	stop()
	bank.FuncTiming(wrapped, "SendCoins")()
	if !trace.SpanFromContext(wrapped.ctx).SpanContext().IsValid() {
		t.Fatal("function outside the list did not replace the wrapped context")
	}

	if got, want := funcSkipTestSpanNames(exporter), []string{"GetBalance", "SendCoins"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans: got %v, want %v", got, want)
	}
	// One call counter and one timing histogram point per timed function.
	if got, want := funcSkipTestFuncNames(t, reader), map[string]int{"GetBalance": 2, "SendCoins": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("metric points by func_name: got %v, want %v", got, want)
	}
}

func TestFuncTimingWithoutSkipList(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	root, _ := funcSkipTestMeter(t, nil, sdktrace.WithSyncer(exporter))
	_, stop := root.SubMeter("bank").FuncTimingCtx(context.Background(), "GetBalance")
	stop()
	if got, want := funcSkipTestSpanNames(exporter), []string{"GetBalance"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans: got %v, want %v", got, want)
	}
}

type funcSkipDiscardExporter struct{}

func (funcSkipDiscardExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}
func (funcSkipDiscardExporter) Shutdown(context.Context) error { return nil }

//go:noinline
func funcSkipBenchmarkCall(meter Meter, ctx *funcSkipTestContext, fn string) {
	saved := ctx.ctx
	defer meter.FuncTiming(ctx, fn)()
	ctx.ctx = saved
}

// BenchmarkFuncTiming compares one instrumented call, as a keeper makes it,
// when the function is timed, when it is on the skip list, and when the
// meter is disabled.
func BenchmarkFuncTiming(b *testing.B) {
	run := func(b *testing.B, meter Meter, fn string) {
		ctx := &funcSkipTestContext{ctx: context.Background()}
		b.ReportAllocs()
		b.ResetTimer()
		for index := 0; index < b.N; index++ {
			funcSkipBenchmarkCall(meter, ctx, fn)
		}
	}
	batcher := sdktrace.WithBatcher(funcSkipDiscardExporter{})

	b.Run("timed", func(b *testing.B) {
		root, _ := funcSkipTestMeter(b, []string{"bank.GetBalance"}, batcher)
		run(b, root.SubMeter("bank", Tag("svc", "bank")), "SendCoins")
	})
	b.Run("skipped", func(b *testing.B) {
		root, _ := funcSkipTestMeter(b, []string{"bank.GetBalance"}, batcher)
		run(b, root.SubMeter("bank", Tag("svc", "bank")), "GetBalance")
	})
	b.Run("skipped-list-of-40", func(b *testing.B) {
		skip := []string{"bank.GetBalance"}
		for index := 0; index < 39; index++ {
			skip = append(skip, "bank.Function"+string(rune('A'+index)))
		}
		root, _ := funcSkipTestMeter(b, skip, batcher)
		run(b, root.SubMeter("bank", Tag("svc", "bank")), "GetBalance")
	})
	b.Run("nil-meter", func(b *testing.B) {
		run(b, NewNilMeter(), "GetBalance")
	})
}

// A meter decorated with trace tags, as a node does with its root meter at
// startup, keeps the skip rules of its scope.
func TestFuncTimingSkipSurvivesWithTraceTags(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	meter, _ := funcSkipTestMeter(t, []string{"CheckTxApp", "app.Scoped"}, sdktrace.WithSyncer(exporter))
	decorated := WithTraceTags(meter, TraceTag("node_id", "node"))

	for _, name := range []string{"CheckTxApp", "Scoped", "Timed"} {
		_, stop := decorated.FuncTimingCtx(context.Background(), name)
		stop()
	}
	sub := decorated.SubMeter("bank")
	_, stop := sub.FuncTimingCtx(context.Background(), "CheckTxApp")
	stop()

	if names := funcSkipTestSpanNames(exporter); !reflect.DeepEqual(names, []string{"Timed"}) {
		t.Fatalf("spans of a decorated meter: %v, expected only Timed", names)
	}
}

// BenchmarkSubMeterWithSkipList measures a SubMeter call with a skip list the
// size of a node's default: keepers whose meter cache does not survive a
// value receiver create a sub-meter per call.
func BenchmarkSubMeterWithSkipList(b *testing.B) {
	skip := make([]string, 0, 40)
	for index := range 40 {
		skip = append(skip, "scope"+string(rune('a'+index%20))+".Function"+string(rune('A'+index%26)))
	}
	meter, _ := funcSkipTestMeter(b, skip, sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = meter.SubMeter("bank", Tag("svc", "bank"))
	}
}

// The skip sets are cached per scope. Nothing invalidates them, so the list
// they come from must be fixed when the providers are created: a caller that
// changes its slice afterwards changes nothing, for scopes seen before the
// change and after it alike.
func TestFuncTimingSkipListIsFixedAtCreation(t *testing.T) {
	list := []string{"bank.GetBalance", "GetParams"}
	providers, err := NewMetrics(Config{
		Endpoint: "localhost:1", InsecureEndpoint: true, TracingEnabled: true, FuncTimingSkip: list,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = providers.Shutdown() })

	root, err := providers.NewMeter("app")
	if err != nil {
		t.Fatal(err)
	}
	early := root.SubMeter("bank").(*meter)
	list[0], list[1] = "bank.SendCoins", "auth.GetAccount"
	late := root.SubMeter("auth").(*meter)
	again := root.SubMeter("bank").(*meter)

	for name, scoped := range map[string]*meter{"before the change": early, "after it": again} {
		if !scoped.skipsFuncTiming("GetBalance") || !scoped.skipsFuncTiming("GetParams") || scoped.skipsFuncTiming("SendCoins") {
			t.Errorf("bank meter created %s does not follow the list given at creation", name)
		}
	}
	if late.skipsFuncTiming("GetAccount") || !late.skipsFuncTiming("GetParams") {
		t.Error("a scope first seen after the change follows the changed slice")
	}
}

// Meters of one scope created from many goroutines get the same set, and
// meters of different scopes get their own.
func TestFuncTimingSkipSetsUnderConcurrentMeterCreation(t *testing.T) {
	root, _ := funcSkipTestMeter(t, []string{"bank.GetBalance", "auth.GetAccount", "GetParams"}, sdktrace.WithSyncer(tracetest.NewInMemoryExporter()))

	scopes := map[string][2]string{"bank": {"GetBalance", "GetAccount"}, "auth": {"GetAccount", "GetBalance"}}
	failures := make(chan string, 64)
	var group sync.WaitGroup
	for range 8 {
		for scope, functions := range scopes {
			group.Add(1)
			go func() {
				defer group.Done()
				for range 200 {
					scoped := root.SubMeter(constantName(scope)).(*meter)
					if !scoped.skipsFuncTiming(functions[0]) || scoped.skipsFuncTiming(functions[1]) || !scoped.skipsFuncTiming("GetParams") {
						select {
						case failures <- scope:
						default:
						}
						return
					}
				}
			}()
		}
	}
	group.Wait()
	close(failures)
	for scope := range failures {
		t.Errorf("a %s meter got another scope's skip set", scope)
	}

	// One cached set per scope, however many meters were created.
	cached := 0
	root.(*meter).metrics.skipSets.Range(func(_, _ any) bool { cached++; return true })
	if cached != 3 { // app, bank, auth
		t.Errorf("%d cached skip sets, expected 3", cached)
	}
}
