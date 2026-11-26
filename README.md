# Metrics v2

TLDR: `defer k.Meter(ctx).FuncTiming(&ctx, "createTokenPair")()`.

Metrics v2 are purely OpenTelemetry based and consist of two components: metrics and traces. They can be enabled / disabled separately via CLI flags:
--metrics-enable-metrics true
--metrics-enable-tracing true

Metrics v2 do not rely on package-level global state (e.g. no more `metrics.ReportFunc...`), but instead all data is submitted through Meter() interface instances.

I've added one global instance of Meter into `sdk.Context`, it can be retrieved via `ctx.Meter()`, but also added sub-meters into
each keepers. SubMeter() is used to narrow the scope by appending additional root tags to every metrics submitted through it.
That's why inside modules we must use specialized `keeper.Meter(ctx)`, instead of global `ctx.Meter()`.

Each method has last catch-all argument to provide tags to mark the metric, these tags will be appended to SubMeter and Meter root tags:
```defer ctx.Meter().FuncTiming(&ctx, "runTx", metrics.Tag("mode", int64(mode)), metrics.TraceTag("tx_hash", txHash))(&err)```

To add tags as attributes, the rule of thumb is:
1) use `metrics.Tag()` to add the tag to both metric and trace
2) for high-cardinality tags (like *"tx_hash"* or *"block_height"*), only use `metrics.TraceTag()`, since traces are meant to have any combination of attributes
3) never use high-cardinality tags for metrics, since every combination of attribute set provides a new data series for a single metric. Metrics attribute value set must not be unbounded.
4) if you want to only add tag into metric but not into trace span, use `metric.MetricTag()`

## Examples:

1. When we have `ctx sdk.Context`
```
// no error handling
defer k.Meter(ctx).FuncTiming(&ctx, "createTokenPair")()

// with auto error handling
func (k Keeper) createTokenPair(ctx sdk.Context, ...) (err error) {
	defer k.Meter(ctx).FuncTiming(&ctx, "createTokenPair")(&err)   
}
```

2. When we have `c context.Context`
```
// via unwrap
ctx := sdk.UnwrapSDKContext(c)
defer k.Meter(ctx).FuncTiming(&ctx, "createTokenPair")()

// directly on context.Context using FuncTimingCtx
c, stop := k.Meter(c).FuncTimingCtx(c, "createTokenPair")()
defer stop()
```

3. Manually error-out the function call
```
k.Meter(ctx).FuncError(ctx, "createTokenPair", err)
```

4. Just increment some counter
```
k.Meter(ctx).Count("gas_used", tx.GasUsed(), metrics.TraceTag("tx_hash", tx.Hash())))
```

## How to view metrics

You need our fork of SigNoz and Docker:
```
git pull github.com:InjectiveLabs/signoz.git
git checkout main-inj
./start.sh
```

Navigate to http://localhost:8080/ to check out metrics and traces from your local node.

## How to use the package

1. Initialize metrics instance (once)
```go
appMetrics, err := metrics.NewMetrics(metrics.Config{
	Endpoint:         metricsEndpoint,
	InsecureEndpoint: metricsInsecure,
	MetricsEnabled:   metricsEnabled,
	TracingEnabled:   tracingEnabled,
})
```

2. Create Meter (or multiple) from Metrics
```go
appMeter, err := appMetrics.NewMeter("injectived", metrics.Tag("env", "mainnet"))
```

3. Use Meter to send metrics & traces
```go
// increment any counter
appMeter.Count("blacklisted", 1, metrics.TraceTag("address", user.Address))

// record func call
appMeter.Func("GetParams")

// record func call and execution time
defer appMeter.FuncTiming(&sdkCtx, "EndBlocker", metrics.TraceTag("block_height", block.height))()
```
4. Optionally, you can split metrics by application modules by using SubMeters derived from root Meter:
```go
exchangeMeter := appMeter.SubMeter("exchange")
```

## Deferred telemetry

`FuncTiming`, `FuncTimingCtx`, `StartTransactionSpan` and `RecordTransactionEvent`
do only what must happen on the calling goroutine: read the clock, pick the
span ID and put one record on a queue. Worker goroutines build the
OpenTelemetry spans and the `func.called`, `func.timing` and `func.error`
updates from the records. The exported spans and metrics are the ones the
calling goroutine would have produced: same identifiers, parents, timestamps,
attributes and instruments.

| Config key | Meaning |
|---|---|
| `telemetry-queue-size` | records held for the workers, rounded up to a power of two. `0` is the default, 262144. A negative value turns the queue off: spans and metrics are built on the calling goroutine. |
| `telemetry-workers` | worker goroutines. `0` is a quarter of the cores, between 2 and 8. |

What to know:

- A record that finds the queue full is dropped and counted; the caller never
  waits. A dropped record loses its span and its func metrics.
  `Metrics.TelemetryQueueStats()` returns the counters, and with metrics
  enabled they are exported as `telemetry.records.enqueued`, `.processed`,
  `.dropped` and `.failed`.
- `func.called` is counted when the function returns, not when it starts.
- The tags of a call and the bytes given to `RecordTransactionEvent` are read
  by a worker after the call returned, so they must not be changed afterwards.
  The text of a returned error is also read by the worker.
- `FlushTraces` and `Shutdown` process the queue before they export.
  `FlushTraces` waits for the records queued before the call only, and not
  beyond its context: records that arrive during the flush are left to the
  workers.
- The first span of a trace is deferred only while the SDK samples every
  trace (`OTEL_TRACES_SAMPLER` unset, `always_on` or `parentbased_always_on`).
  With another sampler it starts in the SDK, on the calling goroutine, and the
  spans below it follow its decision.

## Transaction trace sampling

`trace-sample-rate = N` traces one transaction in N and leaves no spans for
the others. `0` and `1` trace every transaction.

- The choice comes from the transaction's SHA256 hash: a transaction is
  traced when the first eight bytes of the hash, read as a big-endian number,
  are at most 2^64/N. Nodes with the same N therefore trace the same
  transactions, and a traced transaction is traced at every stage.
- `StartTransactionSpan`, `RecordTransactionEvent` and
  `TransactionTraceContext` apply it when they are given the transaction
  bytes. For a transaction outside the sample they return a context that is
  marked, and no span started with that context, by these functions or by
  `FuncTiming`, is recorded.
- Code that times a transaction with `FuncTiming` only calls
  `SampleTransactionContext` with the hash first:
  ```go
  txCtx = metrics.SampleTransactionContext(txCtx, meter, txHash)
  ```
  The context must be the transaction's own, or the mark reaches the
  transactions after it.
- Spans that are not below a transaction, those of a block for instance, are
  always recorded.
- Metrics are not sampled. `func.called`, `func.timing` and `func.error`
  stay exact: a call inside an untraced transaction still hands its record to
  the deferred queue, without a span to build.

## Skipping timed functions

`func-timing-skip` lists functions that `FuncTiming` and `FuncTimingCtx`
ignore: no call counter, no span, no timing histogram and no error report from
the stop function. It is meant for functions that run for a few microseconds
many times per transaction, where the instrumentation costs more than the
function. The call sites keep their `FuncTiming` line; the list alone decides.

```go
metrics.Config{
	FuncTimingSkip: []string{"bank.GetBalance", "GetParams"},
}
```

- `"scope.Function"` applies to the meter with that scope name only: the name
  given to `NewMeter` or `SubMeter`.
- A bare function name applies to every scope.
- The time of a skipped function stays visible in the spans of its callers.
- An empty list times everything.

## Trace Flight Recorder package

It is implemented via new FlightRecorder in go (code). Can be enabled via `--trace-recorder-threshold N` flag, where N is a number of seconds after which block execution time is considered slow and trace will be saved to disk. It works by continuously recording the trace for the last N seconds, and after we detect slow block, we flush the whole trace to disk (to cwd folder). Each block execution time is also marked by the Task with block height in it’s name, so you can focus on specific blocks. To view the trace, find the interesting trace-XXXXXX.out file in root folder ("trace-%s-%s-%d.out", tagName, tagValue, start.Unix()) and from you local machine run:
```
go tool trace trace-64240020.out
```

## Transaction tracing

Transaction stage helpers use the existing meter's tracer and OTLP exporter.
They are enabled by the existing `TracingEnabled` configuration. Span volume
is limited with `trace-sample-rate`, see "Transaction trace sampling" above;
without it every transaction is traced, so measure span volume before enabling
instrumentation across a fleet. The shared OpenTelemetry provider controls
recording and export. Disabled or unsupported meters do not hash transaction
bytes.

Export is best effort through the existing bounded batch queue. A full queue
drops spans instead of blocking transaction processing, and exporter failures
are reported to the OpenTelemetry error handler. The current instance-local
provider does not expose a queue-drop counter by default. A missing span can
reflect export loss or incomplete instrumentation; it cannot prove that a
transaction never reached a node. Verify collector health and observation
coverage before interpreting gaps.

```go
if metrics.TransactionTracingEnabled(meter) {
	ctx, span := metrics.StartTransactionSpan(ctx, meter, signedTransactionBytes,
		"tx.mempool.check", metrics.TraceTag("source", "rpc"))
	defer span.End()
	// Existing FuncTimingCtx / FuncTiming calls using ctx inherit tx_hash in
	// their trace spans, without adding it to metric attributes.
	_ = ctx
}
```

`StartTransactionSpan` creates a span only: it does not emit function counters or
histograms, create a watchdog goroutine, or retain signed transaction bytes. It
sets `tx_hash` to uppercase SHA256 of the exact bytes. A nil transaction argument
reuses the hash in the incoming context; non-nil bytes establish a new hash.
`tx_stage` is always the stage's span name, independent of caller-provided tags.
Other tags belong to the current stage and are not inherited by later stages.
Stage names and outcome labels should be fixed values. Do not attach signed
bytes, authentication data, raw application errors or other unbounded payloads.

Use `DetachTransactionContext(ctx)` before handing work to a callback or storing
an admitted entry's context. It preserves only the OpenTelemetry span identity
and the fixed transaction hash, dropping request cancellation, deadlines and
arbitrary request values. Request cancellation must not erase a later inclusion
observation. A disabled helper returns the original context and an inert span;
mutate the returned span rather than a recording parent obtained from the
incoming context. Preserving the context lets other enabled components continue
the same local trace.

For a point observation, capture the boundary time before a batch loop and use:

```go
metrics.RecordTransactionEvent(ctx, meter, signedTransactionBytes,
	"tx.block.committed", observedAt, metrics.TraceTag("height", height))
```

The event is exported as a span with identical start and end timestamps. Its
timestamp means the specific local boundary being instrumented, not the block
header time or another node's observation.

The hash is a trace attribute and a join key, never an OpenTelemetry trace ID or
a metric dimension. Record chain ID, node ID, process/session identity and build
revision as deployment resource attributes. Independently received transactions
on different nodes create independent traces that analysis can correlate by
chain ID and hash. This does not invent a distributed parent/child relationship
where the wire protocol carries no trace context. Comparing timestamps between
nodes requires synchronized clocks and known observation coverage.
