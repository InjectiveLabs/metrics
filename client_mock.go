package metrics

import (
	"time"

	log "github.com/InjectiveLabs/suplog"
)

func newMockStatter(cfg *StatterConfig) Statter {
	return &mockStatter{
		l: log.WithFields(log.Fields{
			"module": "mock_statter",
		}),
		threshold: cfg.MockingThreshold,
	}
}

type mockStatter struct {
	l         log.Logger
	threshold time.Duration
}

func (s *mockStatter) Count(name string, value int64, tags []string, rate float64) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.WithFields(s.withTagFields(tags)).Debugf("Count %s: %v", name, value)
	return nil
}

func (s *mockStatter) Incr(name string, tags []string, rate float64) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.WithFields(s.withTagFields(tags)).Debugf("Incr %s", name)
	return nil
}

func (s *mockStatter) Decr(name string, tags []string, rate float64) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.WithFields(s.withTagFields(tags)).Debugf("Decr %s", name)
	return nil
}

func (s *mockStatter) Gauge(name string, value float64, tags []string, rate float64) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.WithFields(s.withTagFields(tags)).Debugf("Gauge %s: %v", name, value)
	return nil
}

func (s *mockStatter) Timing(name string, value time.Duration, tags []string, rate float64) error {
	if value > s.threshold {
		s.l.WithFields(s.withTagFields(tags)).Debugf("Timing %s: %v", name, value)
	}
	return nil
}

func (s *mockStatter) Histogram(name string, value float64, tags []string, rate float64) error {
	if value > float64(s.threshold.Milliseconds()) {
		s.l.WithFields(s.withTagFields(tags)).Debugf("Histogram %s: %v", name, value)
	}
	return nil
}

func (s *mockStatter) Unique(bucket string, value string) error {
	if s.threshold > 0 {
		return nil
	}
	s.l.Debugf("Unique %s: %v", bucket, value)
	return nil
}

func (s *mockStatter) Close() error {
	s.l.Debugf("closed at %s", time.Now())
	return nil
}

func (s *mockStatter) withTagFields(tags []string) log.Fields {
	fields := make(log.Fields)
	for i := 0; i < len(tags); i += 2 {
		if i+1 >= len(tags) { // protect against odd number of tags
			break
		}
		fields[tags[i]] = tags[i+1]
	}
	return fields
}
