package metrics

import (
	"context"
	"reflect"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestTraceAndMetricTagFiltering(t *testing.T) {
	t.Parallel()
	shared := Tag("service", "test")
	traceOnly := TraceTag("tx_hash", "ABC")
	metricOnly := MetricTag("metric_only", "yes")
	for _, test := range []struct {
		name       string
		tags       []TagAttribute
		wantMetric []TagAttribute
		wantTrace  []TagAttribute
	}{
		{name: "trace only", tags: []TagAttribute{traceOnly}, wantTrace: []TagAttribute{traceOnly}},
		{name: "metric only", tags: []TagAttribute{metricOnly}, wantMetric: []TagAttribute{metricOnly}},
		{name: "trace first", tags: []TagAttribute{traceOnly, shared}, wantMetric: []TagAttribute{shared}, wantTrace: []TagAttribute{traceOnly, shared}},
		{name: "metric first", tags: []TagAttribute{metricOnly, shared}, wantMetric: []TagAttribute{metricOnly, shared}, wantTrace: []TagAttribute{shared}},
		{name: "mixed", tags: []TagAttribute{traceOnly, metricOnly, shared}, wantMetric: []TagAttribute{metricOnly, shared}, wantTrace: []TagAttribute{traceOnly, shared}},
		{name: "shared first", tags: []TagAttribute{shared, traceOnly, metricOnly}, wantMetric: []TagAttribute{shared, metricOnly}, wantTrace: []TagAttribute{shared, traceOnly}},
	} {
		t.Run(test.name, func(t *testing.T) {
			meter := &meter{}
			gotMetric := meter.getMergedMetricTags(test.tags...)
			gotTrace := meter.getMergedTraceTags(test.tags...)
			if !reflect.DeepEqual(toAttrs(gotMetric), toAttrs(test.wantMetric)) {
				t.Fatalf("metric tags = %v, want %v", gotMetric, test.wantMetric)
			}
			if !reflect.DeepEqual(toAttrs(gotTrace), toAttrs(test.wantTrace)) {
				t.Fatalf("trace tags = %v, want %v", gotTrace, test.wantTrace)
			}
		})
	}
}

func TestWithTraceTagsPreservesScopeAndOriginalMeter(t *testing.T) {
	t.Parallel()
	instrument, exporter, reader := transactionTestMeter(t, true)
	original := instrument.(*meter)
	// Spare capacity catches accidental append mutations of the original slice.
	original.baseTags = make([]TagAttribute, 1, 8)
	original.baseTags[0] = Tag("service", "existing")
	decorated := WithTraceTags(instrument, TraceTag("node_id", "sentry"), TraceTag("chain_id", "chain-test"))
	if decorated == instrument {
		t.Fatal("decorator returned the original meter")
	}
	if len(original.baseTags) != 1 || original.baseTags[:cap(original.baseTags)][1] != nil {
		t.Fatal("decorator mutated the original tags or backing array")
	}
	if decorated.(*meter).Meter != original.Meter || decorated.(*meter).name != original.name {
		t.Fatal("decorator replaced the existing metric meter or scope")
	}
	for _, observed := range []Meter{instrument, decorated} {
		if err := observed.Count("scope-preservation", 1); err != nil {
			t.Fatal(err)
		}
	}
	_, originalSpan := StartTransactionSpan(context.Background(), instrument, []byte("transaction"), "original")
	originalSpan.End()
	_, decoratedSpan := StartTransactionSpan(context.Background(), decorated, []byte("transaction"), "decorated")
	decoratedSpan.End()
	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if _, present := transactionTestAttributes(spans[0])["node_id"]; present {
		t.Fatal("original trace unexpectedly acquired decorated identity")
	}
	if attributes := transactionTestAttributes(spans[1]); attributes["node_id"].AsString() != "sentry" || attributes["chain_id"].AsString() != "chain-test" {
		t.Fatalf("decorated trace lost identity: %v", attributes)
	}

	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.ScopeMetrics) != 1 || data.ScopeMetrics[0].Scope.Name != original.name {
		t.Fatalf("metric scope changed: %+v", data.ScopeMetrics)
	}
	metric := data.ScopeMetrics[0].Metrics[0].Data.(metricdata.Sum[int64])
	if len(metric.DataPoints) != 1 || metric.DataPoints[0].Value != 2 {
		t.Fatalf("decorator split existing metric series: %+v", metric.DataPoints)
	}
	for _, key := range []string{"node_id", "chain_id"} {
		if _, present := metric.DataPoints[0].Attributes.Value(attribute.Key(key)); present {
			t.Fatalf("trace-only identity %s became a metric dimension", key)
		}
	}
}

func TestWithTraceTagsLeavesNilAndUnsupportedMetersUnchanged(t *testing.T) {
	t.Parallel()
	type customMeter struct{ Meter }
	custom := &customMeter{}
	for _, instrument := range []Meter{nil, NewNilMeter(), custom} {
		if got := WithTraceTags(instrument, TraceTag("node_id", "sentry")); got != instrument {
			t.Fatal("decorator replaced a nil or unsupported meter")
		}
	}
}
