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
```defer ctx.Meter().FuncTiming(&ctx, "runTx", metrics.Tag("mode", int64(mode)), metrics.Tag("tx_hash", txHash))(&err)```


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
k.Meter(ctx).Count("gas_used", tx.GasUsed(), metrics.Tag("tx_hash", tx.Hash())))
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
appMeter.Count("blacklisted", 1, metrics.Tag("address", user.Address))

// record func call
appMeter.Func("GetParams")

// record func call and execution time
defer appMeter.FuncTiming(&sdkCtx, "EndBlocker", metrics.Tag("block_height", block.height))()
```
4. Optionally, you can split metrics by application modules by using SubMeters derived from root Meter:
```go
exchangeMeter := appMeter.SubMeter("exchange")
```

## Trace Flight Recorder package

It is implemented via new FlightRecorder in go (code). Can be enabled via `--trace-recorder-threshold N` flag, where N is a number of seconds after which block execution time is considered slow and trace will be saved to disk. It works by continuously recording the trace for the last N seconds, and after we detect slow block, we flush the whole trace to disk (to cwd folder). Each block execution time is also marked by the Task with block height in it’s name, so you can focus on specific blocks. To view the trace, find the interesting trace-XXXXXX.out file in root folder ("trace-%s-%s-%d.out", tagName, tagValue, start.Unix()) and from you local machine run:
```
go tool trace trace-64240020.out
```