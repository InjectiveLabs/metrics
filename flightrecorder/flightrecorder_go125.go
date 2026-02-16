//go:build go1.25

package flightrecorder

import (
	"context"
	"fmt"
	"os"
	"runtime/trace"
	"time"
)

type traceRecorder struct {
	*trace.FlightRecorder

	snapshotThreshold time.Duration
}

// NewTraceRecorder creates new trace flight recorder that will continuously record latest execution trace in a circullar buffer
// and snapshot it to file only if region takes more than snapshotThreshold
func NewTraceRecorder(period, snapshotThreshold time.Duration, bufferSizeBytes uint64) TraceFlightRecorder {
	tr := &traceRecorder{
		FlightRecorder: trace.NewFlightRecorder(trace.FlightRecorderConfig{
			MinAge:   period * 2,
			MaxBytes: bufferSizeBytes,
		}),
		snapshotThreshold: snapshotThreshold,
	}
	return tr
}

// StartRegion starts measuring execution time of a region and if it passes the snapshotThreshold
// then it flushes recorder trace buffer to file
func (tr traceRecorder) StartRegion(tagName, tagValue string) (stopRegion func() error) {
	start := time.Now()
	_, task := trace.NewTask(context.Background(), fmt.Sprintf("%s=%s", tagName, tagValue))
	return func() error {
		task.End()
		if tr.Enabled() && time.Since(start) > tr.snapshotThreshold { // snapshot trace
			go tr.snapshotToFile(tagName, tagValue)
		}
		return nil
	}
}

func (tr traceRecorder) snapshotToFile(tagName, tagValue string) {
	fileName := fmt.Sprintf("trace-%s-%s-%d.out", tagName, tagValue, time.Now().Unix())
	fmt.Printf("::: writing Flight Recorder snapshot to file %s :::\n", fileName)
	f, err := os.Create(fileName)
	if err != nil {
		fmt.Printf("error creating file: %s", err)
		return
	}
	_, err = tr.WriteTo(f)
	if err != nil {
		fmt.Printf("error writing to file file: %s", err)
		return
	}
}
