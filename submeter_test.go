package metrics

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type subMeterTestTag struct{ name string }

func (subMeterTestTag) Type() byte { return forBoth }

func subMeterTestRoot(t testing.TB) (*meter, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	root, _ := funcSkipTestMeter(t, nil, sdktrace.WithSyncer(exporter))
	return root.(*meter), exporter
}

func subMeterTestTags(scoped Meter) map[string]string {
	tags := map[string]string{}
	for _, attr := range toAttrs(scoped.(*meter).baseTags) {
		tags[string(attr.Key)] = attr.Value.Emit()
	}
	return tags
}

func TestSubMeterIsKeptPerScopeAndTags(t *testing.T) {
	root, _ := subMeterTestRoot(t)

	bank := root.SubMeter("bank", Tag("svc", "bank"))
	if again := root.SubMeter("bank", Tag("svc", "bank")); again != bank {
		t.Error("the same scope and tags gave another sub-meter")
	}
	if auth := root.SubMeter("auth", Tag("svc", "auth")); auth == bank {
		t.Error("another scope gave the same sub-meter")
	}
	if plain := root.SubMeter("plain"); plain != root.SubMeter("plain") {
		t.Error("a scope without tags is not kept")
	}

	// Every way the tags can differ gives a sub-meter of its own, with its
	// own tags, and leaves the kept one alone.
	differing := map[string][]TagAttribute{
		"value":      {Tag("svc", "other")},
		"key":        {Tag("service", "bank")},
		"value type": {Tag("svc", true)},
		"tag kind":   {TraceTag("svc", "bank")},
		"count":      {Tag("svc", "bank"), Tag("extra", "x")},
		"none":       {},
	}
	for difference, tags := range differing {
		other := root.SubMeter("bank", tags...)
		if other == bank {
			t.Errorf("tags differing in %s gave the kept sub-meter", difference)
		}
		if got, want := len(other.(*meter).baseTags), len(root.baseTags)+len(tags); got != want {
			t.Errorf("tags differing in %s: %d base tags, expected %d", difference, got, want)
		}
	}
	if root.SubMeter("bank", Tag("svc", "bank")) != bank {
		t.Error("a call with other tags replaced the kept sub-meter")
	}
	if kept := len(*root.subMeters.Load()); kept != 3 {
		t.Errorf("%d sub-meters kept, expected one per scope: 3", kept)
	}
}

// The order executors of the exchange ask for one scope with a tag per
// market. Each market must get its own tag, and the number of kept
// sub-meters must not grow with the number of markets.
func TestSubMeterWithAChangingTagIsNeverMixedUpOrAccumulated(t *testing.T) {
	root, _ := subMeterTestRoot(t)
	for round := range 3 {
		for _, market := range []string{"0xaaa", "0xbbb", "0xccc", "0xddd"} {
			executor := root.SubMeter("SpotMarketOrderExecutor", Tag("market_id", market))
			if got := subMeterTestTags(executor)["market_id"]; got != market {
				t.Fatalf("round %d: the executor of market %s reports as market %s", round, market, got)
			}
		}
	}
	if kept := len(*root.subMeters.Load()); kept != 1 {
		t.Errorf("%d sub-meters kept for one scope", kept)
	}
}

func TestSubMeterKeptTagsDoNotFollowTheCallersSlice(t *testing.T) {
	root, _ := subMeterTestRoot(t)
	tags := []TagAttribute{Tag("svc", "bank")}
	bank := root.SubMeter("bank", tags...)
	tags[0] = Tag("svc", "changed")

	if got := subMeterTestTags(bank)["svc"]; got != "bank" {
		t.Errorf("the kept sub-meter's tag became %q", got)
	}
	if root.SubMeter("bank", Tag("svc", "bank")) != bank {
		t.Error("the kept sub-meter is no longer found by its tags")
	}
	if changed := root.SubMeter("bank", tags...); changed == bank || subMeterTestTags(changed)["svc"] != "changed" {
		t.Error("the changed tags gave the kept sub-meter")
	}
}

// Tags the library cannot compare never match, not even themselves: such a
// sub-meter is built for every call and not kept.
func TestSubMeterWithTagsThatCannotBeComparedIsNotKept(t *testing.T) {
	root, _ := subMeterTestRoot(t)
	uncomparable := map[string]TagAttribute{
		"a slice value":   TagAttr(attribute.StringSlice("peers", []string{"a", "b"})),
		"a foreign type":  subMeterTestTag{name: "x"},
		"an unset value":  TagAttr{},
		"a nil interface": nil,
	}
	for kind, tag := range uncomparable {
		first := root.SubMeter("odd", tag)
		if second := root.SubMeter("odd", tag); first == second {
			t.Errorf("a sub-meter with %s was kept", kind)
		}
	}
	if known := root.subMeters.Load(); known != nil && len(*known) != 0 {
		t.Errorf("%d sub-meters kept", len(*known))
	}
}

// A parent and its decorated copy, and two parents, keep their own
// sub-meters: the sub-meter carries the parent's tags.
func TestSubMeterIsKeptPerParent(t *testing.T) {
	root, _ := subMeterTestRoot(t)
	plain := root.SubMeter("bank", Tag("svc", "bank"))

	decorated := WithTraceTags(root, TraceTag("node_id", "node"))
	labelled := decorated.SubMeter("bank", Tag("svc", "bank"))
	if labelled == plain {
		t.Fatal("the decorated parent returned the original parent's sub-meter")
	}
	if subMeterTestTags(labelled)["node_id"] != "node" || subMeterTestTags(plain)["node_id"] != "" {
		t.Error("the parents' tags are mixed up")
	}
	if decorated.SubMeter("bank", Tag("svc", "bank")) != labelled {
		t.Error("the decorated parent does not keep its sub-meter")
	}

	// Nested: a sub-meter keeps its own.
	nested := plain.SubMeter("keeper")
	if nested != plain.SubMeter("keeper") || nested == root.SubMeter("keeper") {
		t.Error("nested sub-meters are not kept per parent")
	}

	other, _ := subMeterTestRoot(t)
	if other.SubMeter("bank", Tag("svc", "bank")) == plain {
		t.Error("two providers share a sub-meter")
	}

	var absent *meter
	if absent.SubMeter("bank") != NewNilMeter() {
		t.Error("the sub-meter of a nil meter is not the nil meter")
	}
}

func TestSubMeterUnderConcurrentCalls(t *testing.T) {
	root, exporter := subMeterTestRoot(t)
	scopes := []constantName{"auth", "bank", "exchange", "oracle"}

	var group sync.WaitGroup
	got := make([][]Meter, 16)
	for worker := range got {
		group.Add(1)
		go func() {
			defer group.Done()
			for round := range 300 {
				scope := scopes[(worker+round)%len(scopes)]
				scoped := root.SubMeter(scope, Tag("svc", string(scope)))
				if round < len(scopes) {
					got[worker] = append(got[worker], scoped)
				}
				// A varying tag on the same scopes, at the same time.
				_ = root.SubMeter(scope, Tag("market_id", string(rune('a'+round%5))))
				_, stop := scoped.FuncTimingCtx(context.Background(), "Call")
				stop()
			}
		}()
	}
	group.Wait()

	if kept := len(*root.subMeters.Load()); kept != len(scopes) {
		t.Fatalf("%d sub-meters kept, expected %d", kept, len(scopes))
	}
	for _, scope := range scopes {
		kept := (*root.subMeters.Load())[string(scope)].meter
		if tags := subMeterTestTags(kept); tags["svc"] != string(scope) && tags["market_id"] == "" {
			t.Errorf("the kept %s sub-meter has neither tag set: %v", scope, tags)
		}
	}
	if spans := len(exporter.GetSpans()); spans != 16*300 {
		t.Errorf("%d spans, expected %d", spans, 16*300)
	}
}

// BenchmarkSubMeterKept measures what a keeper pays that asks for its
// sub-meter on every timed call.
func BenchmarkSubMeterKept(b *testing.B) {
	root, _ := subMeterTestRoot(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = root.SubMeter("bank", Tag("svc", "bank"))
	}
}

// What a kept sub-meter reports must be what a sub-meter built for every
// call reports: the same spans and the same metric points.
func TestKeptSubMeterReportsWhatAFreshOneDoes(t *testing.T) {
	type subMeterSource func(parent *meter, scope constantName, tags ...TagAttribute) Meter
	sources := map[string]subMeterSource{
		"kept": func(parent *meter, scope constantName, tags ...TagAttribute) Meter {
			return parent.SubMeter(scope, tags...)
		},
		"fresh": func(parent *meter, scope constantName, tags ...TagAttribute) Meter {
			return parent.newSubMeter(string(scope), tags)
		},
	}

	spans, points := map[string]string{}, map[string]string{}
	for name, source := range sources {
		setup := deferredTestProviders(t, deferredTestConfig(0))
		root := setup.meter.(*meter)
		failure := errors.New("insufficient funds")
		for round := range 40 {
			bank := source(root, "bank", Tag("svc", "bank"))
			ctx, stopSend := bank.FuncTimingCtx(context.Background(), "SendCoins", TraceTag("round", int64(round)))
			_, stopBalance := bank.FuncTimingCtx(ctx, "GetBalance")
			stopBalance()
			if round%10 == 0 {
				stopSend(&failure)
			} else {
				stopSend()
			}
			_ = bank.Count("transfers", 1, Tag("denom", "inj"))
			_ = bank.Histogram("amount", int64(round))
			_ = bank.Gauge("height", int64(round))
			bank.Func("Touched")

			market := []string{"0xaaa", "0xbbb"}[round%2]
			executor := source(source(root, "exchange", Tag("svc", "exchange")).(*meter), "SpotMarketOrderExecutor", Tag("market_id", market))
			_, stopMatch := executor.FuncTimingCtx(context.Background(), "Match")
			stopMatch()
		}
		setup.flush(t)
		spans[name] = strings.Join(setup.describeSpans(), "\n")
		points[name] = strings.Join(setup.describeMetrics(t), "\n")
	}

	if spans["kept"] == "" || points["kept"] == "" {
		t.Fatal("nothing was reported")
	}
	if spans["kept"] != spans["fresh"] {
		t.Errorf("spans differ.\nkept:\n%s\nfresh:\n%s", spans["kept"], spans["fresh"])
	}
	if points["kept"] != points["fresh"] {
		t.Errorf("metric points differ.\nkept:\n%s\nfresh:\n%s", points["kept"], points["fresh"])
	}
}

func BenchmarkSubMeterKeptPrebuiltTags(b *testing.B) {
	root, _ := subMeterTestRoot(b)
	tags := []TagAttribute{Tag("svc", "bank")}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = root.SubMeter("bank", tags...)
	}
}

// A kept sub-meter carries the tags its parent had when it was created. The
// parent's tags must therefore be fixed: a caller that changes the slice it
// gave to NewMeter changes neither the root meter nor sub-meters created
// later.
func TestRootMeterTagsDoNotFollowTheCallersSlice(t *testing.T) {
	root, _ := subMeterTestRoot(t)
	tags := []TagAttribute{Tag("env", "testnet")}
	scoped, err := root.metrics.NewMeter("node", tags...)
	if err != nil {
		t.Fatal(err)
	}
	early := scoped.SubMeter("bank")
	tags[0] = Tag("env", "changed")
	late := scoped.SubMeter("auth")

	for name, instrument := range map[string]Meter{"the root meter": scoped, "a sub-meter from before": early, "a sub-meter from after": late} {
		if got := subMeterTestTags(instrument)["env"]; got != "testnet" {
			t.Errorf("%s reports env=%q after the caller changed its slice", name, got)
		}
	}
}
