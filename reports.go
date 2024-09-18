package metrics

import (
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

func Counter(metric string, value int64, tags ...Tags) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Count(metric, value, tagSpec, 1)
	}, tags...)
}

func CounterPositive[T constraints.Integer](metric string, value T, tags ...Tags) {
	if value > 0 {
		Counter(metric, int64(value), tags...)
	}
}

func Incr(metric string, tags ...Tags) {
	Counter(metric, 1, tags...)
}

func Timer(metric string, value time.Duration, tags ...Tags) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Timing(metric, value, tagSpec, 1)
	}, tags...)
}

func Gauge(metric string, value float64, tags ...Tags) {
	CustomReport(func(s Statter, tagSpec []string) {
		s.Gauge(metric, value, tagSpec, 1)
	}, tags...)
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
