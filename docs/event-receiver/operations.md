# Receiver operations and troubleshooting

[Getting started](getting-started.md) · [Concepts](concepts.md) · [Configuration](configuration.md) · [Metrics and event reference](reference.md)

The event receiver maintains lifecycle state in Redis/Valkey and exports transition
records through the OpenTelemetry Logs SDK. **Webhook success, state persistence,
SDK submission, OTLP export and backend visibility are different outcomes.**
This guide helps identify which stage failed and what can be recovered.

## Find the problem

| Question / symptom | First checks |
|---|---|
| Is the process healthy? | `/healthz` checks HTTP liveness. Use `/readyz` to check Valkey connectivity |
| Was a webhook accepted? | Check HTTP response and request/outcome counters. A `webhook received` log precedes processing completion |
| Was an event delivered? | Compare generated, SDK-submitted and exported counters, then query the backend by fingerprint or `event.id` |
| HTTP 400 | Fix malformed JSON, fingerprint or lifecycle timestamps. The entire batch is rejected before state writes |
| HTTP 401 | Check webhook Basic Auth credentials before investigating lifecycle processing |
| HTTP 500 | Required state/processing operation failed. Other alerts in the batch may already have committed |
| No event for a repeated alert | Expected: same occurrence/status refreshes state but emits no new transition |
| Orphan or stale resolution | Compare fingerprint and exact `startsAt` with the latest working state; see [Valkey inspection](redis-debugging.md) |
| Valkey outage | Required operations fail with HTTP 500; receiver startup fails if the initial ping fails |
| OTLP outage | SDK retries and buffers in memory only; final failures or queue overflow may lose records |
| Restart | Valkey working state remains; in-memory batches do not. Stored state or idempotency keys may suppress later emission |

Use `/readyz` for traffic routing and `/healthz` for liveness. If readiness
returns `503`, check Valkey connectivity and credentials. Monitor OTLP export
through exporter errors and SDK queue diagnostics.

## Trace a webhook through processing and delivery

### 1. Webhook validation and lifecycle state

Alertmanager webhook delivery is asynchronous and may be retried. The receiver
validates the **entire batch before changing state**:

- Malformed JSON or invalid alerts return **HTTP 400**; no alerts in that batch
  enter lifecycle processing. An empty batch is accepted.
- Required Valkey read/write failures cause **HTTP 500**. After an operational
  error, the receiver still attempts the remaining valid alerts.
- **HTTP 500 does not roll back earlier writes.** A retried batch can contain
  transitions that have already been processed.

Valkey stores lifecycle state and emission-idempotency keys, **not a delivery
queue**. An idempotency command failure is "best effort": it is logged and counted,
and emission continues after successful lifecycle state processing. Existing
idempotency keys suppress emission only; they do not change the detected
processing outcome.

For exact lifecycle transition rules, see [concepts](concepts.md#identity-and-transitions).

### 2. Event generation, SDK submission and export

| Stage | Evidence | Important limitation |
|---|---|---|
| Generated | `alertreceiver_events_generated_total` | Created after required state operation, before emission deduplication; an existing key can suppress submission |
| SDK submission | `alertreceiver_events_emitted_total`; `EmissionAccepted` | `Logger.Emit` returned without a synchronous error. No per-call queue-retention receipt exists |
| OTLP exporter success | `alertreceiver_otlp_export_requests_total{result="success"}` | An `Export` call returned nil; exporter-managed retries are included, not counted as separate calls |
| Backend visibility | Query the log backend for the event | The receiver does not verify final durable persistence, including when exporting to an intermediate Collector |

Repeated or stale notifications generate no new lifecycle event. SDK queue
overflow, shutdown or a no-op provider can discard records without a synchronous
`Logger.Emit` error. An OTLP exporter error can represent a partial result;
export metrics are **not exact event-loss counters**.

Events have a stable `event.id`: the lowercase SHA-256 digest of the JSON-encoded
string array `[fingerprint, startsAtUTC, transition]`, where `startsAtUTC` uses
RFC3339Nano. The ID preserves nanosecond identity, is stable across timezone
representations and repeated notifications, and excludes `endsAt`. Event record
timestamps use the original lifecycle time.

Relevant processing failure logs include `event.id`, `alert_fingerprint` and
`alert_transition`. Aggregate export failures intentionally do not identify
individual events. Normal per-alert outcomes are exposed through counters,
not success logs.

## Monitoring examples

The following PromQL expressions help locate failures. They describe rates
or processing outcomes, **not guaranteed backend persistence**.

```promql
# Webhook processing failures and invalid requests
sum(rate(alertreceiver_webhook_requests_total{status="5xx"}[5m]))
sum(rate(alertreceiver_webhook_requests_total{status="4xx"}[5m]))

# Valkey operation errors, including idempotency errors once
sum by (operation) (rate(alertreceiver_redis_errors_total[5m]))
sum(rate(alertreceiver_redis_idemp_setnx_total{result="error"}[5m]))

# Export calls returning errors (not an event-loss rate)
sum by (protocol) (rate(alertreceiver_otlp_export_requests_total{result="error"}[5m]))

# Generated transitions and SDK submissions
sum by (transition) (rate(alertreceiver_events_generated_total[5m]))
sum by (transition) (rate(alertreceiver_events_emitted_total[5m]))

# Repeat/stale notifications
sum by (outcome) (rate(alertreceiver_processing_outcomes_total{outcome=~"repeated_.*|stale_.*"}[5m]))
```

Interpret the counters by stage. A difference between generated and submitted
events can reflect idempotency suppression. A difference between submission and
export metrics is **not** an exact loss count: batching, partial failures,
concurrent scrapes and restarts affect comparisons. See the full
[metrics reference](reference.md#prometheus-metrics).

## OTLP export failures and shutdown

The event receiver uses the OpenTelemetry Logs SDK to batch and export lifecycle events. Events are buffered **in memory only**; there is no persistent queue or replay mechanism.

### Export failures and buffering

When the OTLP destination is unavailable or returns a retryable error, the exporter retries within its configured timeout.

Events can be permanently lost when:

- The in-memory queue becomes full.
- An export fails after its retries or timeout are exhausted.
- The receiver crashes or is forcibly terminated.
- The receiver shuts down before all pending events are exported.

The receiver does not automatically regenerate lost events. Redis/Valkey may already contain the lifecycle state and idempotency key, preventing subsequent notifications from producing the same event again.

For batching, queue capacity, retry and timeout settings, see [Configuration](configuration.md#sdk-batching-and-attribute-limits).

### Monitoring and diagnostics

Export failures and SDK warnings are written to structured application logs.

The following Prometheus metrics are useful:

| Metric | Purpose |
|---|---|
| `alertreceiver_otlp_export_requests_total` | Successful and failed OTLP export calls |
| `alertreceiver_otel_sdk_errors_total` | Errors reported through the SDK error handler |
| `alertreceiver_events_emitted_total` | Events submitted to the SDK, not necessarily exported |

**These metrics do not provide an exact count of lost events.**

In particular, SDK queue overflow produces a warning in the application logs but does not necessarily increment an error counter. A crash may also occur before the warning is written.

For additional monitoring queries, see [Monitoring examples](#monitoring-examples).

### Shutdown behavior

During a normal shutdown, the receiver:

1. Allows up to 10 seconds for active HTTP requests to complete.
2. Closes the Redis/Valkey connection.
3. Allows up to 5 seconds for the OpenTelemetry SDK to flush pending events and shut down.

Shutdown and export errors are written to structured application logs.

A successful SDK flush does not guarantee that events have been durably stored in the final log backend.

### Diagnostic log security

Application and SDK diagnostics use structured logging with best-effort redaction of known credentials and sensitive URL components.

Redaction is not guaranteed to remove arbitrary sensitive information. Restrict access to application logs and avoid including secrets in diagnostic messages.

## Recovery and reliability limits

Lifecycle state and idempotency keys are written **before SDK submission**.
Crashes in that window, SDK queue drops, failed exports and forced termination
can lose events. A crash may leave no diagnostic. Normal shutdown only
attempts to flush the in-memory queue within its deadline.

Required state errors do not undo writes that Valkey may already have accepted.
Concurrent lifecycle decisions remain non-atomic; tombstone expiry limits
occurrence history. The receiver has **no transactional outbox, persistent
queue, durable replay, custom retries or exactly-once/end-to-end persistence
guarantee**.

There is no supported manual replay or durable outbox repair procedure.
Do **not** delete Valkey state or idempotency keys as a delivery-replay
workaround: doing so can create duplicate or misclassified transitions.
See [Valkey debugging](redis-debugging.md) for inspection and idempotency
troubleshooting.

## Change configuration safely

Restart after changing environment variables. OTLP configuration changes do
not require deleting Valkey keys. TTL changes affect **future expirations
only**. Moving to another Redis/Valkey server or database starts a separate
state domain unless the data is preserved.

Protect labels, annotations, OTLP headers and Valkey credentials. For exact
key formats, TTL investigation and step-by-step diagnostics, use
[Redis/Valkey debugging](redis-debugging.md).

## Secure webhook and metrics access

Use the [shared inbound security guide](../http-security.md) for native HTTPS,
independent Basic Auth, trusted-proxy deployments, CA trust and smoke tests.
Defaults remain HTTP without authentication. Security configuration errors
fail before listening.

Unauthorized requests return HTTP 401 before webhook processing; check client
credentials before investigating lifecycle or export failures. Health probes
bypass Basic Auth but must use HTTPS when TLS is configured. Restrict probe
and backend access through network controls. Restart after password or
certificate rotation.

Inbound security is independent of outbound connections and does not require
deleting Valkey state or changing SLA policy epochs.
