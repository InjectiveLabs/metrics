//go:build !go1.25

package flightrecorder

import (
	"context"
	"fmt"
	"os"
	rtrace "runtime/trace"
	"time"

	"golang.org/x/exp/trace"
)

type traceRecorderLegacy struct {
	*trace.FlightRecorder

	snapshotThreshold time.Duration
}

// NewTraceRecorder creates new trace flight recorder that will continuously record latest execution trace in a circullar buffer
// and snapshot it to file only if region takes more than snapshotThreshold
func NewTraceRecorder(period, snapshotThreshold time.Duration, bufferSizeBytes int) (TraceFlightRecorder, error) {
	tr := &traceRecorderLegacy{
		FlightRecorder:    trace.NewFlightRecorder(),
		snapshotThreshold: snapshotThreshold,
	}
	tr.SetPeriod(period)
	tr.SetSize(bufferSizeBytes)

	return tr, nil
}

// Stop re-implements TraceFlightRecorder, since legacy FlightRecorder returns error
func (tr *traceRecorderLegacy) Stop() {
	_ = tr.Stop()
}

// StartRegion starts measuring execution time of a region and if it passes the snapshotThreshold
// then it flushes recorder trace buffer to file
func (tr traceRecorderLegacy) StartRegion(tagName, tagValue string) (stopRegion func() error) {
	start := time.Now()
	_, task := rtrace.NewTask(context.Background(), fmt.Sprintf("%s=%s", tagName, tagValue))
	return func() error {
		task.End()
		if time.Since(start) > tr.snapshotThreshold { // snapshot trace
			fileName := fmt.Sprintf("trace-%s-%s-%d.out", tagName, tagValue, start.Unix())
			fmt.Printf("::: writing Trace Recorder snapshot to file %s :::\n", fileName)
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
