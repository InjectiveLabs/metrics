package metrics

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Deferred telemetry.
//
// With the pipeline on, FuncTiming and the transaction span helpers do only
// what has to happen on the calling goroutine: read the clock, pick a span
// ID, allocate one record and put it on a queue. Worker goroutines turn the
// records into OpenTelemetry spans and metric updates, with the same
// identifiers, timestamps, attributes and instruments the calling goroutine
// would have produced. Block execution is single-threaded, so this moves the
// work to cores it cannot use.
//
// A record that finds the queue full is dropped and counted, see
// TelemetryQueueStats. Nothing ever blocks the caller.

const (
	defaultTelemetryQueueSize = 1 << 18
	minTelemetryQueueSize     = 1 << 6
	maxTelemetryWorkers       = 64

	workerMinIdle = 500 * time.Microsecond
	workerMaxIdle = 8 * time.Millisecond

	clockCheckInterval = time.Second
	// The wall clock and the monotonic clock advance together except when
	// the wall clock is stepped. Two readings of the pair differ by the time
	// between the two system calls, so only a larger change is a step.
	clockStepThreshold = int64(100 * time.Microsecond)
)

// Records carry monotonic nanoseconds since clockBase: one clock read where
// time.Now takes two. Workers convert to wall time with clockOffset, which
// follows steps of the wall clock.
var (
	clockBase     = time.Now()
	clockBaseWall = clockBase.UnixNano()
	clockOffset   atomic.Int64
)

func monotonicNow() int64 { return int64(time.Since(clockBase)) }

func wallTime(monotonic int64) time.Time {
	return time.Unix(0, clockBaseWall+monotonic+clockOffset.Load())
}

// refreshClockOffset re-reads both clocks and moves clockOffset when the wall
// clock was stepped since the last look.
func refreshClockOffset() {
	now := time.Now()
	offset := now.UnixNano() - clockBaseWall - int64(now.Sub(clockBase))
	delta := offset - clockOffset.Load()
	if delta > clockStepThreshold || delta < -clockStepThreshold {
		clockOffset.Store(offset)
	}
}

type cacheLinePad [64]byte

type spanQueueCell struct {
	sequence atomic.Uint64
	span     *deferredSpan
}

// spanQueue is a bounded lock-free queue for many producers and many
// consumers (D. Vyukov's array queue). A push or a pop is two atomic loads,
// one compare-and-swap and one atomic store.
type spanQueue struct {
	cells []spanQueueCell
	mask  uint64

	_    cacheLinePad
	tail atomic.Uint64 // next position to push; also the count of pushes
	_    cacheLinePad
	head atomic.Uint64 // next position to pop
	_    cacheLinePad
}

func newSpanQueue(size int) *spanQueue {
	capacity := minTelemetryQueueSize
	for capacity < size {
		capacity <<= 1
	}
	queue := &spanQueue{cells: make([]spanQueueCell, capacity), mask: uint64(capacity - 1)} //nolint:gosec // positive
	for index := range queue.cells {
		queue.cells[index].sequence.Store(uint64(index)) //nolint:gosec // positive
	}
	return queue
}

// push returns false when the queue is full.
func (queue *spanQueue) push(span *deferredSpan) bool {
	position := queue.tail.Load()
	for {
		cell := &queue.cells[position&queue.mask]
		difference := int64(cell.sequence.Load() - position) //nolint:gosec // wraps on purpose
		switch {
		case difference == 0:
			if queue.tail.CompareAndSwap(position, position+1) {
				cell.span = span
				cell.sequence.Store(position + 1)
				return true
			}
			position = queue.tail.Load()
		case difference < 0:
			return false
		default:
			position = queue.tail.Load()
		}
	}
}

// pop returns nil when the queue is empty.
func (queue *spanQueue) pop() *deferredSpan {
	position := queue.head.Load()
	for {
		cell := &queue.cells[position&queue.mask]
		difference := int64(cell.sequence.Load() - (position + 1)) //nolint:gosec // wraps on purpose
		switch {
		case difference == 0:
			if queue.head.CompareAndSwap(position, position+1) {
				span := cell.span
				cell.span = nil
				cell.sequence.Store(position + queue.mask + 1)
				return span
			}
			position = queue.head.Load()
		case difference < 0:
			return nil
		default:
			position = queue.head.Load()
		}
	}
}

// TelemetryQueueStats counts the records that left the calling goroutines
// for the workers. Enqueued minus Processed is what waits in the queue.
// Dropped records found the queue full: their spans and their func.called,
// func.timing and func.error updates are lost. All zero when the pipeline is
// off.
type TelemetryQueueStats struct {
	Enqueued  uint64 // records accepted by the queue
	Processed uint64 // records a worker finished, failed ones included
	Dropped   uint64 // records refused because the queue was full
	Failed    uint64 // records whose processing panicked
}

// Metric names the stats are exported under when metrics are enabled.
const (
	TelemetryRecordsEnqueuedMetric  = "telemetry.records.enqueued"
	TelemetryRecordsProcessedMetric = "telemetry.records.processed"
	TelemetryRecordsDroppedMetric   = "telemetry.records.dropped"
	TelemetryRecordsFailedMetric    = "telemetry.records.failed"
)

type deferredPipeline struct {
	queue *spanQueue

	// deferRoots is false when the sampler may reject a new trace. Spans
	// without a parent then start on the calling goroutine, where the
	// sampler sees them; their descendants follow the parent's decision
	// and stay deferred.
	deferRoots bool

	closed  atomic.Bool
	stop    chan struct{}
	workers sync.WaitGroup

	_         cacheLinePad
	processed atomic.Uint64
	failed    atomic.Uint64
	_         cacheLinePad
	dropped   atomic.Uint64
}

func (cfg *Config) telemetryWorkers() int {
	workers := cfg.TelemetryWorkers
	if workers <= 0 {
		// A quarter of the cores, at least two so that one slow record
		// does not hold the queue.
		workers = min(max(runtime.GOMAXPROCS(0)/4, 2), 8)
	}
	return min(workers, maxTelemetryWorkers)
}

// newDeferredPipeline returns nil when the configuration asks for telemetry
// to be built on the calling goroutine.
func newDeferredPipeline(cfg *Config) *deferredPipeline {
	if cfg.TelemetryQueueSize < 0 {
		return nil
	}
	size := cfg.TelemetryQueueSize
	if size == 0 {
		size = defaultTelemetryQueueSize
	}
	pipeline := &deferredPipeline{
		queue:      newSpanQueue(size),
		deferRoots: rootSpansAlwaysSampled(),
		stop:       make(chan struct{}),
	}
	for range cfg.telemetryWorkers() {
		pipeline.workers.Add(1)
		go pipeline.work()
	}
	return pipeline
}

// rootSpansAlwaysSampled reports whether the SDK's sampler, which the
// library leaves to the environment, accepts every new trace.
func rootSpansAlwaysSampled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER"))) {
	case "", "always_on", "parentbased_always_on":
		return true
	default:
		return false
	}
}

func (pipeline *deferredPipeline) enqueue(span *deferredSpan) {
	if pipeline.closed.Load() {
		return
	}
	if !pipeline.queue.push(span) {
		pipeline.dropped.Add(1)
	}
}

func (pipeline *deferredPipeline) stats() TelemetryQueueStats {
	if pipeline == nil {
		return TelemetryQueueStats{}
	}
	return TelemetryQueueStats{
		Enqueued:  pipeline.queue.tail.Load(),
		Processed: pipeline.processed.Load(),
		Dropped:   pipeline.dropped.Load(),
		Failed:    pipeline.failed.Load(),
	}
}

// TelemetryQueueStats returns the deferred telemetry counters; zero when the
// pipeline is off.
func (ms *Metrics) TelemetryQueueStats() TelemetryQueueStats {
	if ms == nil {
		return TelemetryQueueStats{}
	}
	return ms.deferred.stats()
}

// work drains the queue and sleeps when it is empty. The producers never
// wake a worker: a wake-up is a system call on the calling goroutine's
// thread, and it would put the worker on the caller's processor.
func (pipeline *deferredPipeline) work() {
	defer pipeline.workers.Done()

	idle := workerMinIdle
	timer := time.NewTimer(idle)
	defer timer.Stop()
	clockChecked := time.Now()

	for {
		if pipeline.drain() > 0 {
			idle = workerMinIdle
		} else if idle < workerMaxIdle {
			idle *= 2
		}
		if time.Since(clockChecked) >= clockCheckInterval {
			refreshClockOffset()
			clockChecked = time.Now()
		}

		timer.Reset(idle)
		select {
		case <-pipeline.stop:
			return
		case <-timer.C:
		}
	}
}

// drain processes records until the queue is empty and returns how many.
func (pipeline *deferredPipeline) drain() int {
	count := 0
	for span := pipeline.queue.pop(); span != nil; span = pipeline.queue.pop() {
		pipeline.process(span)
		count++
	}
	return count
}

// flush returns once every record enqueued before the call has been
// processed, by the workers or by the caller, or when ctx ends.
func (pipeline *deferredPipeline) flush(ctx context.Context) error {
	target := pipeline.queue.tail.Load()
	for {
		pipeline.drain()
		if pipeline.processed.Load() >= target {
			return nil
		}
		// A worker holds the remaining records, or a producer has claimed
		// a position and not filled it yet.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Microsecond):
		}
	}
}

// close stops the workers and processes what is left in the queue. Records
// that arrive afterwards are discarded.
func (pipeline *deferredPipeline) close() {
	if pipeline.closed.Swap(true) {
		return
	}
	close(pipeline.stop)
	pipeline.workers.Wait()
	pipeline.drain()
}

// process builds the span and the metric updates of one record. A panic,
// for instance from the Error method of a recorded error, is counted and
// goes no further: telemetry must not take the process down.
func (pipeline *deferredPipeline) process(span *deferredSpan) {
	defer func() {
		if recovered := recover(); recovered != nil {
			pipeline.failed.Add(1)
		}
		pipeline.processed.Add(1)
	}()

	scoped := span.m
	tags := span.tagSlice()
	funcTimed := span.result&resultFunc != 0

	var err error
	if span.result&resultError != 0 {
		err = span.extra.err
	}

	// Metrics first: they do not need the text of the error.
	if funcTimed {
		scoped.recordFuncMetrics(span.name, tags, err, time.Duration(span.end-span.start).Milliseconds())
	}
	if span.kind&kindSampled != 0 && scoped.tracer != nil {
		span.emit(tags, err)
	}
}

// presetIDsKey carries a record to presetIDGenerator through the context
// given to the SDK's tracer.
type presetIDsKey struct{}

// emit starts and ends the SDK span of a record. The SDK applies its own
// limits, resource and processors, so the exported span is what the calling
// goroutine would have produced.
func (span *deferredSpan) emit(tags []TagAttribute, err error) {
	scoped := span.m
	extra := span.extra

	valueCtx := span.parent
	if valueCtx == nil {
		valueCtx = context.Background()
	}
	start, end := wallTime(span.start), wallTime(span.end)
	name := span.name
	if extra != nil {
		if extra.transaction != nil {
			hash := sha256.Sum256(extra.transaction)
			if !transactionSampled(hash[:], scoped.transactionSampleLimit()) {
				return // an event of a transaction outside the sample
			}
			valueCtx = withTransactionHashSum(valueCtx, &hash)
		}
		if !extra.startTime.IsZero() {
			start = extra.startTime
		}
		if !extra.endTime.IsZero() {
			end = extra.endTime
		}
	}

	attributes := transactionSpanAttributes(valueCtx, scoped, tags)
	if span.kind&kindTransactionStage != 0 {
		attributes = append(attributes, attribute.String("tx_stage", name))
	}

	// Only span identity goes to the SDK: no deadline, no values, and no
	// reference to a parent span object.
	startCtx := context.Background()
	if span.kind&kindRoot == 0 {
		startCtx = trace.ContextWithSpanContext(startCtx, trace.SpanContextFromContext(span.parent))
	}
	startCtx = context.WithValue(startCtx, presetIDsKey{}, span)

	options := []trace.SpanStartOption{trace.WithAttributes(attributes...), trace.WithTimestamp(start)}
	if extra != nil && len(extra.links) > 0 {
		options = append(options, trace.WithLinks(extra.links...))
	}
	_, sdkSpan := scoped.tracer.Start(startCtx, name, options...)

	if extra != nil {
		if extra.name != "" {
			sdkSpan.SetName(extra.name)
		}
		if len(extra.attributes) > 0 {
			sdkSpan.SetAttributes(extra.attributes...)
		}
		for index := range extra.events {
			event := &extra.events[index]
			sdkSpan.AddEvent(event.name, trace.WithTimestamp(event.time), trace.WithAttributes(event.attributes...))
		}
		if extra.statusCode != codes.Unset {
			sdkSpan.SetStatus(extra.statusCode, extra.statusDescription)
		}
	}
	switch {
	case err != nil:
		sdkSpan.SetStatus(codes.Error, err.Error())
	case span.result&resultOK != 0:
		sdkSpan.SetStatus(codes.Ok, "")
	}
	sdkSpan.End(trace.WithTimestamp(end))
}

// presetIDGenerator gives the SDK the identifiers a record was assigned on
// the calling goroutine, and random ones to every other span.
type presetIDGenerator struct{}

func (presetIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if span, preset := ctx.Value(presetIDsKey{}).(*deferredSpan); preset {
		return span.traceID, span.spanID
	}
	return randomTraceID(), randomSpanID()
}

func (presetIDGenerator) NewSpanID(ctx context.Context, traceID trace.TraceID) trace.SpanID {
	if span, preset := ctx.Value(presetIDsKey{}).(*deferredSpan); preset && span.traceID == traceID {
		return span.spanID
	}
	return randomSpanID()
}

// funcMetricOptions is the attribute set of a function's func.called,
// func.timing and func.error updates when the call passed no tags.
type funcMetricOptions struct {
	add    []metric.AddOption
	record []metric.RecordOption
}

func (m *meter) funcOptions(fn string) *funcMetricOptions {
	if cached, found := m.funcOpts.Load(fn); found {
		return cached.(*funcMetricOptions)
	}
	set := attribute.NewSet(toAttrs(m.getMergedMetricTags(Tag("func_name", fn)))...)
	option := metric.WithAttributeSet(set)
	cached, _ := m.funcOpts.LoadOrStore(fn, &funcMetricOptions{
		add:    []metric.AddOption{option},
		record: []metric.RecordOption{option},
	})
	return cached.(*funcMetricOptions)
}

// recordFuncMetrics does what Func, Error and the func.timing histogram do
// for one finished call, in one pass.
func (m *meter) recordFuncMetrics(fn string, tags []TagAttribute, err error, milliseconds int64) {
	if m.Meter == nil {
		return
	}
	if len(tags) > 0 {
		m.Func(fn, tags...)
		if err != nil {
			m.Error(fn, err, tags...)
		}
		m.Histogram("func.timing", milliseconds, append([]TagAttribute{Tag("func_name", fn)}, tags...)...) //nolint:errcheck
		return
	}

	options := m.funcOptions(fn)
	background := context.Background()
	if called, instrumentErr := m.getCounter("func.called"); instrumentErr == nil {
		called.Add(background, 1, options.add...)
	}
	if err != nil {
		if failed, instrumentErr := m.getCounter("func.error"); instrumentErr == nil {
			failed.Add(background, 1, options.add...)
		}
	}
	if timing, instrumentErr := m.getHistogram("func.timing"); instrumentErr == nil {
		timing.Record(background, milliseconds, options.record...)
	}
}

// registerTelemetryQueueMetrics exports the queue counters through the
// metrics provider, read on every collection.
func (ms *Metrics) registerTelemetryQueueMetrics() error {
	scope := ms.meterProvider.Meter("metrics")
	names := []struct{ name, description string }{
		{TelemetryRecordsEnqueuedMetric, "telemetry records accepted by the queue"},
		{TelemetryRecordsProcessedMetric, "telemetry records a worker finished"},
		{TelemetryRecordsDroppedMetric, "telemetry records refused because the queue was full"},
		{TelemetryRecordsFailedMetric, "telemetry records whose processing panicked"},
	}
	counters := make([]metric.Int64ObservableCounter, len(names))
	observables := make([]metric.Observable, len(names))
	for index, entry := range names {
		counter, err := scope.Int64ObservableCounter(entry.name, metric.WithDescription(entry.description))
		if err != nil {
			return fmt.Errorf("%s: %w", entry.name, err)
		}
		counters[index], observables[index] = counter, counter
	}
	_, err := scope.RegisterCallback(func(_ context.Context, observer metric.Observer) error {
		stats := ms.deferred.stats()
		observer.ObserveInt64(counters[0], int64(stats.Enqueued))  //nolint:gosec // counters fit
		observer.ObserveInt64(counters[1], int64(stats.Processed)) //nolint:gosec // counters fit
		observer.ObserveInt64(counters[2], int64(stats.Dropped))   //nolint:gosec // counters fit
		observer.ObserveInt64(counters[3], int64(stats.Failed))    //nolint:gosec // counters fit
		return nil
	}, observables...)
	if err != nil {
		return fmt.Errorf("telemetry queue metrics callback: %w", err)
	}
	return nil
}
