# Receiver lifecycle concepts

[Getting started](getting-started.md) · [Configuration](configuration.md) · [Operations](operations.md) · [Reference](reference.md)

The event receiver answers one question: **which firing and resolved transitions did Alertmanager report?** It maintains alert lifecycle state and emits OpenTelemetry (OTLP) log events. It does **not** evaluate SLA policies or deliver tickets and notifications.

```text
Alertmanager webhook → lifecycle state → event → asynchronous OTLP export
                            ↕
                      Redis / Valkey
```

Redis/Valkey maintains **working state**. An OTLP destination receives lifecycle events for historical querying. These responsibilities are separate: storing a lifecycle transition does not guarantee its log event reaches the backend.

## Identity and transitions

### Alert and occurrence identity

The receiver uses the original **Alertmanager fingerprint** as the alert identity. A distinct **occurrence** is identified by the fingerprint together with its `startsAt` instant, compared at nanosecond precision regardless of timezone offset.

`endsAt` indicates when an occurrence resolved; it is **not** part of occurrence identity. Valid firing and resolved notifications require a nonempty fingerprint and nonzero `startsAt`. Resolved notifications also require nonzero `endsAt >= startsAt`.

### Lifecycle transition rules

The receiver compares each incoming alert with the latest stored occurrence for its fingerprint:

| Stored occurrence | Incoming alert | Result | Lifecycle event |
|---|---|---|---|
| Missing | Firing | Opened | Firing |
| Missing | Resolved | Orphan resolution; closed tombstone | Resolved |
| Firing | Same occurrence, firing | Refresh `last_seen_at` | None |
| Firing | Same occurrence, resolved | Closed | Resolved |
| Firing | Older occurrence | Stale; state unchanged | None |
| Firing | Newer firing | Supersede old open occurrence | Firing |
| Firing | Newer resolved | Orphan resolution; replace occurrence | Resolved |
| Closed | Same occurrence, resolved | Repeated resolution | None |
| Closed | Same or older firing | Stale; state unchanged | None |
| Closed | Older resolved | Stale; state unchanged | None |
| Closed | Newer firing | Reopened | Firing |
| Closed | Newer resolved | Orphan resolution; replace occurrence | Resolved |

**New occurrences while an older occurrence is still firing**

If a firing notification arrives with the same fingerprint but a newer `startsAt`, the receiver treats it as a new occurrence of the alert.

The new occurrence replaces the older one in Redis/Valkey, and a new firing lifecycle event is generated. The receiver does not generate a resolved event for the older occurrence because Alertmanager has not reported its resolution.

The processing result in an internal state called `SupersededFiring`, indicating that an older firing occurrence was replaced. The generated firing event has `state_write_result=opened`, indicating that the new occurrence was opened. It can also be observed in metrics as `alertreceiver_processing_outcomes_total{outcome="superseded_firing"}`.

The generated OpenTelemetry lifecycle event contains alert.transition="firing" and alert.state_write_result="opened". The superseded processing outcome is not included in the event.

**Repeated resolutions and Redis/Valkey expiration**

When an occurrence resolves, the receiver stores its state as `closed` and applies the Redis/Valkey key expiration configured by `CLOSED_STATE_TTL`.

If the same resolution notification arrives again, it is recognized as a repeat. The stored state is not changed, no additional resolved lifecycle event is generated, and the key's expiration is not extended.

For example, with `CLOSED_STATE_TTL=24h`, a resolution first processed at 12:00 expires at approximately 12:00 the following day, even if the same resolution is received again later.

### Batch validation and processing errors

The receiver validates the **entire webhook batch before accessing state**. Invalid alerts, malformed JSON, null payloads or trailing JSON return HTTP 400 without processing any alerts. An empty alerts batch is accepted.

After validation, alerts are processed individually. Required Redis/Valkey read/write failures are processing errors, not authoritative lifecycle decisions. If one occurs, the handler attempts the remaining alerts and returns HTTP 500. Previously completed writes are **not rolled back**. Alertmanager may retry the whole batch, including alerts already processed.

## Working state versus history

Redis/Valkey holds the **latest occurrence per fingerprint**, not a complete event history. A newer firing replaces the previous working occurrence; it does not create an implicit resolution for the older one. Closed tombstones retain limited ordering history. The log backend is where operators query historical lifecycle events.

### Idempotency suppresses emission, not processing

Deduplication keys identify a transition using the fingerprint, canonical UTC nanosecond `startsAt`, and transition (`firing` or `resolved`). `endsAt` does not contribute to this identity.

An existing key suppresses **event emission**, not lifecycle processing. State changes and idempotency keys precede SDK submission. Consequently, a crash or export failure can leave state and deduplication keys in place even though the event never reached the log backend. A subsequent Alertmanager retry may be treated as a repeat and cannot be relied upon to restore that event.

## What delivery means

Lifecycle processing and log delivery have distinct stages:

| Stage | What it establishes | What it does **not** establish |
|---|---|---|
| Lifecycle state updated | The required Redis/Valkey state operation completed | OTLP submission or delivery |
| Event generated | A transition event was constructed | That it passed idempotency suppression |
| SDK submission | The event was handed to the OpenTelemetry Logs SDK | That it survived buffering or export |
| Exporter success | The OTLP exporter returned success | Durable storage or queryability in the final backend |
| Backend visibility | The event can be found in the queried backend | An end-to-end exactly-once delivery guarantee |

The receiver provides **best-effort OTLP delivery**. SDK batching and retries use in-memory buffering; there is **no persistent delivery queue or replay mechanism**. State changes can therefore outlive log events lost during queue overflow, failed export, shutdown or a crash.

Lifecycle decisions also use a read/decide/write sequence that is **not atomic across concurrent requests**. Use a single receiver when the documented ordering model matters; running multiple instances does not introduce coordination or fencing.

For failure windows, monitoring and recovery limitations, see [operations](operations.md). For event attributes, HTTP behavior and metrics, see [reference](reference.md). For storage inspection, see [Redis/Valkey debugging](redis-debugging.md).
