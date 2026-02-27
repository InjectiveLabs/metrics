package metrics

import (
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const (
	forBoth byte = iota
	forMetric
	forTrace
)

type TagAttribute interface {
	Type() byte
}

type TagAttr attribute.KeyValue

func (TagAttr) Type() byte { return forBoth }

type TagAttrTrace attribute.KeyValue

func (TagAttrTrace) Type() byte { return forTrace }

type TagAttrMetric attribute.KeyValue

func (TagAttrMetric) Type() byte { return forMetric }

const ServiceNameKey = string(semconv.ServiceNameKey)

// Tag adds a new attribute both to metrics data and trace span.
// WARNING: don't use it for high-cardinality tags, use TraceTag() instead.
func Tag[V string | int64 | bool](k string, v V) TagAttr {
	switch anyV := any(v).(type) {
	case string:
		return TagAttr(attribute.String(k, anyV))
	case int64:
		return TagAttr(attribute.Int64(k, anyV))
	case bool:
		return TagAttr(attribute.Bool(k, anyV))
	}
	return TagAttr{}
}

// TraceTag only adds a new attribute to a trace span, avoiding metrics data.
// Use it for high-cardinality tags, like "tx_hash" or "block_height", cause
// it's okey to have high-cardinality traces, but catastrophic for metrics.
func TraceTag[V string | int64 | bool](k string, v V) TagAttrTrace {
	return TagAttrTrace(Tag(k, v))
}

// MetricTag only adds a new attribute to a metrics data, avoiding tracing span
func MetricTag[V string | int64 | bool](k string, v V) TagAttrMetric {
	return TagAttrMetric(Tag(k, v))
}

// getMergedTags merges any type of tags
func (m *meter) getMergedTags(tags ...TagAttribute) []TagAttribute {
	if m == nil {
		return nil
	}

	if len(tags) == 0 {
		return m.baseTags
	}
	mergedTags := make([]TagAttribute, 0, len(m.baseTags)+len(tags))
	mergedTags = append(append(mergedTags, m.baseTags...), tags...)

	return mergedTags
}

// getMergedTraceTags merges m.baseTags with tags but omits MetricTags
func (m *meter) getMergedTraceTags(tags ...TagAttribute) []TagAttribute {
	mergedTags := m.getMergedTags(tags...)

	var filteredTags []TagAttribute

	for i, tag := range mergedTags {
		switch tag.Type() {
		case forMetric: // we need to filter this tag
			if len(filteredTags) == 0 { // if it's a first tag to omit, initialize filtered slice
				filteredTags = append([]TagAttribute{}, mergedTags[:i]...)
			}
		default: // include this tag, but only if we need to filter anything
			if len(filteredTags) > 0 {
				filteredTags = append(filteredTags, tag)
			}
		}
	}

	if len(filteredTags) > 0 { // if we filtered anything
		return filteredTags
	}

	return mergedTags // otherwise return as-is
}

// getMergedMetricTags merges m.baseTags with tags but omits TraceTags
func (m *meter) getMergedMetricTags(tags ...TagAttribute) []TagAttribute {
	mergedTags := m.getMergedTags(tags...)

	var filteredTags []TagAttribute

	for i, tag := range mergedTags {
		switch tag.Type() {
		case forTrace: // we need to filter this tag
			if len(filteredTags) == 0 { // if it's a first tag to omit, initialize filtered slice
				filteredTags = append([]TagAttribute{}, mergedTags[:i]...)
			}
		default: // include this tag, but only if we need to filter anything
			if len(filteredTags) > 0 {
				filteredTags = append(filteredTags, tag)
			}
		}
	}

	if len(filteredTags) > 0 { // if we filtered anything
		return filteredTags
	}

	return mergedTags // otherwise return as-is
}

func toAttrs(tags []TagAttribute) []attribute.KeyValue {
	attr := make([]attribute.KeyValue, 0, len(tags))

	for _, t := range tags {
		switch tt := t.(type) {
		case TagAttr:
			attr = append(attr, toAttr(tt))
		case TagAttrTrace:
			attr = append(attr, toAttr(tt))
		case TagAttrMetric:
			attr = append(attr, toAttr(tt))
		}
	}

	return attr
}

func toAttr[T TagAttr | TagAttrTrace | TagAttrMetric](tag T) attribute.KeyValue {
	return attribute.KeyValue(tag)
}
