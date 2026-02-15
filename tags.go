package metrics

import "go.opentelemetry.io/otel/attribute"

type TagAttr = attribute.KeyValue

func Tag[V string | int64 | bool](k string, v V) TagAttr {
	switch anyV := any(v).(type) {
	case string:
		return attribute.String(k, anyV)
	case int64:
		return attribute.Int64(k, anyV)
	case bool:
		return attribute.Bool(k, anyV)
	}
	return TagAttr{}
}

func (m *Meter) getMergedTags(tags ...TagAttr) []TagAttr {
	if len(tags) == 0 {
		return m.baseTags
	}
	mergedTags := make([]TagAttr, 0, len(m.baseTags)+len(tags))
	mergedTags = append(append(mergedTags, m.baseTags...), tags...)

	return mergedTags
}
