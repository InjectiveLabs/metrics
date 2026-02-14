package metrics

func CustomReport(reportFn func(s Statter, tagSpec []string), tags ...Tags) {
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
