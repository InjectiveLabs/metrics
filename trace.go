package metrics

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/exp/trace"
)

type TraceRecorder struct {
	*trace.FlightRecorder

	snapshotThreshold time.Duration
}

// NewTraceRecorder creates new trace flight recorder that will continuously record latest execution trace in a circullar buffer
// and snapshot it to file only if region takes more than snapshotThreshold
func NewTraceRecorder(period, snapshotThreshold time.Duration, bufferSizeBytes int) *TraceRecorder {
	tr := &TraceRecorder{
		FlightRecorder:    trace.NewFlightRecorder(),
		snapshotThreshold: snapshotThreshold,
	}
	tr.SetPeriod(period)
	tr.SetSize(bufferSizeBytes)

	return tr
}

// StartRegion starts measuring execution time of a region and if it passes the snapshotThreshold
// then it flushes recorder trace buffer to file
func (tr TraceRecorder) StartRegion(tagName, tagValue string) (stopRegion func() error) {
	start := time.Now()
	return func() error {
		if time.Since(start) > tr.snapshotThreshold { // snapshot trace
			fileName := fmt.Sprintf("trace-%s-%s-%d.out", tagName, tagValue, start.Unix())
			fmt.Printf("::: writing Trace Recorder snapshot to file %s:::\n", fileName)
			f, err := os.Create(fileName)
			if err != nil {
				return err
			}
			_, err = tr.WriteTo(f)
			if err != nil {
				return err
			}
		}
		return nil
	}
}
