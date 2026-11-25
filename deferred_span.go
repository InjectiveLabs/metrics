package metrics

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// Span states. A span is open until End; busy while a rarely used method
// (SetAttributes, AddEvent, ...) changes it.
const (
	spanOpen uint32 = iota
	spanBusy
	spanEnded
)

// kind bits, fixed when the span is created.
const (
	kindSampled          uint8 = 1 << iota // the trace is recorded: emit an SDK span
	kindRoot                               // no parent span: the span starts a trace
	kindTransactionStage                   // carries the tx_stage attribute
	kindTraceState                         // the parent has a W3C trace state
	kindWatchdog                           // a stuck-function watchdog waits on extra.done
)

// result bits, written by whoever ends the span, read by the worker.
const (
	resultFunc  uint8 = 1 << iota // a FuncTiming stop: record the func metrics
	resultOK                      // the function returned without error
	resultError                   // the function returned extra.err
)

// maxDeferredTags is what fits the record's tag count; a call with more tags
// takes the inline path.
const maxDeferredTags = 255

// deferredSpan is the record of one FuncTiming call or one transaction span.
// It is the trace.Span the instrumented code sees, and, with tracing on, the
// context.Context handed back to it, so that one allocation serves as record,
// span and context. It is never reused: a context kept beyond the call stays
// valid.
//
// The struct is kept at 120 bytes; fields a common call does not need live
// in spanExtra.
type deferredSpan struct {
	embedded.Span

	m      *meter
	name   string
	parent context.Context // the caller's context; nil only for records that never become a context
	tags   *TagAttribute   // first of ntags tags of the call
	extra  *spanExtra

	start, end int64 // monotonic nanoseconds since clockBase

	traceID trace.TraceID
	spanID  trace.SpanID

	state      atomic.Uint32
	kind       uint8
	result     uint8
	traceFlags trace.TraceFlags
	ntags      uint8
}

// spanExtra holds what only some spans have.
type spanExtra struct {
	err error // the error the function returned

	name              string // set by SetName
	statusCode        codes.Code
	statusDescription string
	attributes        []attribute.KeyValue
	events            []deferredEvent
	links             []trace.Link

	startTime, endTime time.Time // explicit timestamps, as given
	transaction        []byte    // hashed by the worker for the tx_hash attribute

	done chan struct{} // closed by the stop function, for the watchdog
}

type deferredEvent struct {
	name       string
	time       time.Time
	attributes []attribute.KeyValue
}

var (
	_ trace.Span      = (*deferredSpan)(nil)
	_ context.Context = (*deferredSpan)(nil)
)

func randomSpanID() trace.SpanID {
	var id trace.SpanID
	for {
		binary.NativeEndian.PutUint64(id[:], rand.Uint64())
		if id.IsValid() {
			return id
		}
	}
}

func randomTraceID() trace.TraceID {
	var id trace.TraceID
	for {
		binary.NativeEndian.PutUint64(id[:8], rand.Uint64())
		binary.NativeEndian.PutUint64(id[8:], rand.Uint64())
		if id.IsValid() {
			return id
		}
	}
}

// otelSpanKey is the context key under which OpenTelemetry keeps the current
// span. The key is not exported; it is read once from a lookup. With it a
// deferredSpan answers the span lookup itself, which saves the
// context.WithValue allocation of trace.ContextWithSpan. When the lookup does
// not behave as expected the key stays nil and trace.ContextWithSpan is used.
var otelSpanKey = captureSpanKey()

type spanKeyProbe struct {
	context.Context
	key   any
	calls int
}

func (probe *spanKeyProbe) Value(key any) any {
	probe.key = key
	probe.calls++
	return nil
}

func captureSpanKey() any {
	probe := &spanKeyProbe{Context: context.Background()}
	trace.SpanFromContext(probe)
	if probe.calls != 1 || probe.key == nil {
		return nil
	}
	// The key must be the one trace.ContextWithSpan stores under.
	marker := &deferredSpan{}
	if trace.ContextWithSpan(context.Background(), marker).Value(probe.key) != any(marker) {
		return nil
	}
	return probe.key
}

// asContext returns the context that carries span as the current span and
// otherwise behaves as parent.
func (span *deferredSpan) asContext(parent context.Context) context.Context {
	if otelSpanKey == nil {
		return trace.ContextWithSpan(parent, span)
	}
	return span
}

// context.Context, on top of the caller's context.

func (span *deferredSpan) Deadline() (time.Time, bool) { return span.parent.Deadline() }
func (span *deferredSpan) Done() <-chan struct{}       { return span.parent.Done() }
func (span *deferredSpan) Err() error                  { return span.parent.Err() }

func (span *deferredSpan) Value(key any) any {
	if key == otelSpanKey {
		return span
	}
	return span.parent.Value(key)
}

func (span *deferredSpan) String() string {
	return fmt.Sprintf("%v.WithSpan(%s)", span.parent, span.name)
}

func (span *deferredSpan) setTags(tags []TagAttribute) {
	if len(tags) > 0 {
		span.tags = unsafe.SliceData(tags)
		span.ntags = uint8(len(tags)) //nolint:gosec // callers check maxDeferredTags
	}
}

func (span *deferredSpan) tagSlice() []TagAttribute {
	if span.ntags == 0 {
		return nil
	}
	return unsafe.Slice(span.tags, int(span.ntags))
}

// inherit takes trace identity from the current span of ctx and reports
// whether there is one. Without a parent the span starts a new, sampled
// trace.
func (span *deferredSpan) inherit(ctx context.Context) bool {
	parent := trace.SpanFromContext(ctx)
	if deferredParent, deferred := parent.(*deferredSpan); deferred {
		span.traceID = deferredParent.traceID
		span.traceFlags = deferredParent.traceFlags
		span.kind |= deferredParent.kind & (kindSampled | kindTraceState)
		return true
	}

	parentContext := parent.SpanContext()
	if !parentContext.IsValid() {
		span.kind |= kindRoot | kindSampled
		span.traceFlags = trace.FlagsSampled
		return false
	}
	span.traceID = parentContext.TraceID()
	span.traceFlags = parentContext.TraceFlags()
	if parentContext.IsSampled() {
		span.kind |= kindSampled
	}
	if parentContext.TraceState().Len() > 0 {
		span.kind |= kindTraceState
	}
	return true
}

// lock gives the caller the right to change the span and reports false when
// the span has ended. It spins: holders keep it for a few instructions.
func (span *deferredSpan) lock() bool {
	for {
		if span.state.CompareAndSwap(spanOpen, spanBusy) {
			return true
		}
		if span.state.Load() == spanEnded {
			return false
		}
		runtime.Gosched()
	}
}

func (span *deferredSpan) unlock() { span.state.Store(spanOpen) }

// finish moves the span to its ended state and reports whether the caller
// is the one who ended it. The winner owns the span until it enqueues it.
func (span *deferredSpan) finish() bool {
	for {
		if span.state.CompareAndSwap(spanOpen, spanEnded) {
			return true
		}
		if span.state.Load() == spanEnded {
			return false
		}
		runtime.Gosched()
	}
}

func (span *deferredSpan) extras() *spanExtra {
	if span.extra == nil {
		span.extra = &spanExtra{}
	}
	return span.extra
}

// stop is the StopFn of a FuncTiming call.
func (span *deferredSpan) stop(errors ...*error) {
	end := monotonicNow()

	if span.kind&kindWatchdog != 0 {
		close(span.extra.done)
	}

	var err error
	if len(errors) > 0 {
		err = *errors[0]
	}

	if !span.state.CompareAndSwap(spanOpen, spanEnded) && !span.finish() {
		// Something else ended the span: the watchdog, or the function
		// through FuncError. The span is with the workers already, so the
		// metrics of the call are recorded here.
		span.m.recordFuncMetrics(span.name, span.tagSlice(), err, time.Duration(end-span.start).Milliseconds())
		return
	}

	if span.kind&kindSampled == 0 && span.m.Meter == nil {
		return // neither a span nor a metric to record
	}

	span.end = end
	if err != nil {
		span.extras().err = err
		span.result = resultFunc | resultError
	} else {
		span.result = resultFunc | resultOK
	}
	span.m.metrics.deferred.enqueue(span)
}

// trace.Span.

// End ends the span. Only the first call has an effect.
func (span *deferredSpan) End(options ...trace.SpanEndOption) {
	end := monotonicNow()
	if !span.finish() {
		return
	}
	span.end = end
	if len(options) > 0 {
		config := trace.NewSpanEndConfig(options...)
		if timestamp := config.Timestamp(); !timestamp.IsZero() {
			span.extras().endTime = timestamp
		}
	}
	if span.kind&kindSampled == 0 {
		return // nothing to export, and no func metrics ride on End
	}
	span.m.metrics.deferred.enqueue(span)
}

func (span *deferredSpan) IsRecording() bool {
	return span.kind&kindSampled != 0 && span.state.Load() != spanEnded
}

func (span *deferredSpan) SpanContext() trace.SpanContext {
	config := trace.SpanContextConfig{TraceID: span.traceID, SpanID: span.spanID, TraceFlags: span.traceFlags}
	if span.kind&kindTraceState != 0 {
		config.TraceState = trace.SpanContextFromContext(span.parent).TraceState()
	}
	return trace.NewSpanContext(config)
}

func (span *deferredSpan) TracerProvider() trace.TracerProvider {
	return span.m.metrics.tracerProvider
}

func (span *deferredSpan) SetAttributes(attributes ...attribute.KeyValue) {
	if span.kind&kindSampled == 0 || len(attributes) == 0 || !span.lock() {
		return
	}
	extra := span.extras()
	extra.attributes = append(extra.attributes, attributes...)
	span.unlock()
}

// SetStatus follows the SDK: Ok is final, Error does not replace Ok, Unset
// is ignored, and only Error keeps a description.
func (span *deferredSpan) SetStatus(code codes.Code, description string) {
	if span.kind&kindSampled == 0 || code == codes.Unset || !span.lock() {
		return
	}
	extra := span.extras()
	if extra.statusCode <= code {
		extra.statusCode = code
		extra.statusDescription = ""
		if code == codes.Error {
			extra.statusDescription = description
		}
	}
	span.unlock()
}

func (span *deferredSpan) SetName(name string) {
	if span.kind&kindSampled == 0 || !span.lock() {
		return
	}
	span.extras().name = name
	span.unlock()
}

func (span *deferredSpan) AddLink(link trace.Link) {
	if span.kind&kindSampled == 0 || !link.SpanContext.IsValid() || !span.lock() {
		return
	}
	extra := span.extras()
	extra.links = append(extra.links, link)
	span.unlock()
}

func (span *deferredSpan) AddEvent(name string, options ...trace.EventOption) {
	if span.kind&kindSampled == 0 {
		return
	}
	config := trace.NewEventConfig(options...)
	span.addEvent(deferredEvent{name: name, time: config.Timestamp(), attributes: config.Attributes()})
}

// RecordError adds the exception event the SDK would add. The stack, when
// asked for, is the caller's.
func (span *deferredSpan) RecordError(err error, options ...trace.EventOption) {
	if span.kind&kindSampled == 0 || err == nil {
		return
	}
	options = append(options, trace.WithAttributes(
		semconv.ExceptionType(errorTypeName(err)),
		semconv.ExceptionMessage(err.Error()),
	))
	config := trace.NewEventConfig(options...)
	attributes := config.Attributes()
	if config.StackTrace() {
		stack := make([]byte, 2048)
		stack = stack[:runtime.Stack(stack, false)]
		attributes = append(attributes, semconv.ExceptionStacktrace(string(stack)))
	}
	span.addEvent(deferredEvent{name: semconv.ExceptionEventName, time: config.Timestamp(), attributes: attributes})
}

func (span *deferredSpan) addEvent(event deferredEvent) {
	if !span.lock() {
		return
	}
	extra := span.extras()
	extra.events = append(extra.events, event)
	span.unlock()
}

func errorTypeName(err error) string {
	errorType := reflect.TypeOf(err)
	if errorType.PkgPath() == "" && errorType.Name() == "" {
		return errorType.String() // likely a builtin type
	}
	return errorType.PkgPath() + "." + errorType.Name()
}
