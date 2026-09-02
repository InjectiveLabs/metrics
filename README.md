# Metrics

### Motivation

- We are going to have a lot of microservices requiring metrics -> Unify how + what metrics are sent

- Maintenance in one place, only need update this lib in services implementation


### Supports:

- Telegraf

- DataDog

- OpenTelemetry (OTEL)

### Statsd client refresh

The Telegraf and DataDog statters send over UDP. They resolve their address and dial
once, at construction, and then hold that socket for the life of the process — and
nothing in the write path can tell that the socket has stopped being delivered,
because a connectionless send to a dead peer still succeeds locally. A process that
outlives the agent it was pointed at therefore keeps "reporting" into a hole,
silently, with no errors and a healthy-looking service.

To prevent that, the UDP client is rebuilt every `DefaultRefreshInterval`
(5 minutes). A rebuild is a UDP dial with no handshake, and the client it replaces is
closed, which flushes whatever it had buffered.

- `InitService` starts the refresh automatically. Tune it with
  `ServiceConfig.RefreshInterval`, or set that to a negative value to switch it off.
- Callers that use `Init` directly should call `StartRefresh(ctx, addr, prefix, 0)`
  once afterwards.

OTEL is deliberately left alone: it exports over gRPC/HTTP, which redials on its own.

### Acknowledgement

- Special thanks maintainers of injective-exchange/metrics, this package derives from this PR: https://github.com/InjectiveLabs/injective-exchange/pull/451
