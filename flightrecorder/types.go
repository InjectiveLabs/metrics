package flightrecorder

type TraceFlightRecorder interface {
	Start() error
	StartRegion(tagName, tagValue string) (stopRegion func() error)
	Stop()
	Enabled() bool
}
