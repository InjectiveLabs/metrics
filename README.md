# Metrics v2

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