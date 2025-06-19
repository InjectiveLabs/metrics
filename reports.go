package metrics

import (
	"context"
	"strconv"
	"time"

	"golang.org/x/exp/constraints"
)

func CustomReport(reportFn func(s Statter, tagSpec []string), tags ...Tags) {
	clientMux.RLock()
	defer clientMux.RUnlock()

	if client == nil {
		return
	}

	reportFn(client, JoinTags(tags...))
}

func SlowSubscriberEventsDropped(amount int, tags ...Tags) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Count("pubsub.slow_subscriber.events_dropped", int64(amount), tagSpec, 1)
	})
}

func SpotTradesBatchSubmitted(size int, tags ...Tags) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Count("events.spot_trades_batch.size", int64(size), tagSpec, 1)
	})
}

func DerivativeTradesBatchSubmitted(size int, tags ...Tags) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Count("events.derivative_trades_batch.size", int64(size), tagSpec, 1)
	})
}

func IndexPriceUpdatesBatchSubmitted(size int, tags ...Tags) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Count("events.set_price_batch.size", int64(size), tagSpec, 1)
	})
}

func Counter[T constraints.Integer](metric string, value T, tags ...interface{}) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Count(metric, int64(value), tagSpec, 1)
	}, combineAny(tags...))
}

func CounterPositive[T constraints.Integer](metric string, value T, tags ...interface{}) {
	if value > 0 {
		Counter(metric, value, combineAny(tags...))
	}
}

func Incr(metric string, tags ...interface{}) {
	Counter(metric, 1, combineAny(tags...))
}

func Timer(metric string, value time.Duration, tags ...Tags) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Timing(metric, value, tagSpec, 1)
	}, tags...)
}

// Timing supports both Tags or pairs of key-value arguments.
func Timing(metric string, initialTags ...interface{}) func(deferredTags ...interface{}) {
	start := time.Now()
	it := combineAny(initialTags...)
	return func(deferredTags ...interface{}) {
		dt := combineAny(deferredTags...)
		Timer(metric, time.Since(start), MergeTags(it, dt))
	}
}

// TimingWithErr supports both Tags or pairs of key-value arguments.
func TimingWithErr(metric string, initialTags ...interface{}) func(err *error, deferredTags ...interface{}) {
	stop := Timing(metric, initialTags...)
	return func(err *error, deferredTags ...interface{}) {
		dt := append(deferredTags, "error", BoolTag(err != nil && *err != nil))
		stop(dt...)
	}
}

// TimingCtxWithErr supports both Tags or pairs of key-value arguments.
func TimingCtxWithErr(_ context.Context, metric string, initialTags ...interface{}) func(err *error, deferredTags ...interface{}) {
	return TimingWithErr(metric, initialTags...)
}

func Gauge(metric string, value float64, tags ...interface{}) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Gauge(metric, value, tagSpec, 1)
	}, combineAny(tags...))
}

func BoolTag(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func IntTag[T constraints.Integer](i T) string {
	return strconv.Itoa(int(i))
}
