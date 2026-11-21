# Receiver operations and troubleshooting

[Getting started](getting-started.md) · [Concepts](concepts.md) · [Configuration](configuration.md) · [Metrics and event reference](reference.md)

The event receiver maintains lifecycle state in Redis/Valkey and exports transition
records through the OpenTelemetry Logs SDK. **Webhook success, state persistence,
SDK submission, OTLP export and backend visibility are different outcomes.**
This guide helps identify which stage failed and what can be recovered.

## Find the problem

| Question / symptom | First checks |
|---|---|
| Is the process healthy? | `/healthz` proves HTTP liveness only. Inspect Valkey operation errors and OTLP exporter errors separately |
| Was a webhook accepted? | Check HTTP response and request/outcome counters. A `webhook received` log precedes processing completion |
| Was an event delivered? | Compare generated, SDK-submitted and exported counters, then query the backend by fingerprint or `event.id` |
| HTTP 400 | Fix malformed JSON, fingerprint or lifecycle timestamps. The entire batch is rejected before state writes |
| HTTP 401 | Check webhook Basic Auth credentials before investigating lifecycle processing |
| HTTP 500 | Required state/processing operation failed. Other alerts in the batch may already have committed |
| No event for a repeated alert | Expected: same occurrence/status refreshes state but emits no new transition |
| Orphan or stale resolution | Compare fingerprint and exact `startsAt` with the latest working state; see [Valkey inspection](../redis-debugging.md) |
| Valkey outage | Required operations fail with HTTP 500; receiver startup fails if the initial ping fails |
| OTLP outage | SDK retries and buffers in memory only; final failures or queue overflow may lose records |
| Restart | Valkey working state remains; in-memory batches do not. Stored state or idempotency keys may suppress later emission |

The receiver has **no readiness endpoint**. A successful `/healthz` request does
not establish that Valkey or the OTLP destination is currently reachable.

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
queue**. An idempotency command failure is best effort: it is logged and counted,
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
[metrics reference](reference.md#metrics).

## SDK diagnostics, batching and shutdown

### Structured diagnostics and redaction

Application and SDK failures use structured logs with component, operation and
best-effort redaction of textual error context. String values, error messages
and resolved `slog` groups are checked for configured authentication-header
values, Redis passwords, and credentials/query strings in HTTP URLs.

Opaque `slog.KindAny` values—including maps, slices, structs, custom Stringers
and JSON marshalers—are omitted rather than serialized; their contents are
not recursively sanitized. `LogValuer` values are resolved and their resulting
kind is handled by the same rules. Full alert payloads and annotations are
not deliberately logged.

**Redaction is not comprehensive sanitization of arbitrary SDK diagnostics.**
Unknown secrets, attribute/group names and sensitive text that does not match
the known patterns may remain visible. Avoid secrets in diagnostic text or
backend error messages and restrict access to application logs.

### SDK error reporting and queue overflow

The SDK global error handler observes diagnostics independently of exporter
metrics. SDK logging and error-handler configuration are process-global and
installed once at startup.

Queue-full warnings are routed to structured logs. This implementation has
**no stable per-event rejection callback or queue-drop Prometheus counter**;
experimental SDK self-metrics and undocumented internals are not used.
Queue-overflow warnings are emitted when the SDK next exports or flushes, so
a crash can prevent even that warning.

A queue drop is not counted in `alertreceiver_otlp_export_requests_total`
unless an `Export` call actually occurs. Nor is it counted in
`alertreceiver_otel_sdk_errors_total` unless the SDK invokes its error handler.
In the installed SDK, queue overflow produces a logging warning, **not** an
error-handler callback. A later export is counted independently of the drop.
Neither counter is a queue-drop or permanent-loss counter.

For queue, batching, timeout, retry and TLS settings, see
[configuration](configuration.md#sdk-batching-and-attribute-limits). The
application preserves SDK-managed batching and retry behavior and adds no
separate retry queue. Export duration includes exporter-managed retries.

### Failure scenarios and capacity

**Exporter outage example.** Assume the HTTP OTLP destination responds with
retryable HTTP 503 errors and no special server-requested delay. A single event
normally enters an export batch at the next scheduled export, roughly within
one second when idle. The exporter retries, but the default batch deadline
ends the export call after roughly **30 seconds**. The SDK then discards that
failed batch rather than requeueing it. In this example, the event may be held
for roughly **31 seconds from submission**, not indefinitely until the
destination recovers. A non-retryable error may discard it much sooner.

**Queue-capacity example.** If an export is blocked and new lifecycle events
arrive at **100 records/second**, an initially empty **2,048-record** pending
queue fills in approximately:

```text
2048 records / 100 records per second = 20.48 seconds
```

Further arrivals displace the oldest queued records, even while the in-flight
export is retrying. This estimate concerns the pending queue, **excluding**
the batch already being exported. Actual behavior depends on arrival rate
and export completion times. Only generated lifecycle events enter the queue,
not every repeated Alertmanager notification.

There is no fixed retention duration for queued events. A larger queue can
delay overflow; longer timeouts can allow longer export attempts. Neither
makes buffering durable or restores a batch after final export failure.
A restart loses in-memory records, while retained Valkey state or idempotency
keys may suppress emission of the lost events on later notifications.

### Shutdown and startup failures

Shutdown follows this sequence:

1. Allow up to **10 seconds** for HTTP requests to drain.
2. Close Valkey.
3. Allow up to **5 seconds** for `LoggerProvider.Shutdown` to flush pending
   batches and release the exporter.

Shutdown uses a fresh, non-cancelled context. Startup failure paths also clean
up initialized resources. Unexpected HTTP listener failures, including an
occupied port, are logged and return a nonzero process exit code after cleanup;
they are not treated as graceful shutdown.

Timeout, flush and exporter shutdown errors produce structured logs. No
shutdown-specific metrics are added, and final process counters may not be
scraped before exit. A successful flush still provides **no query-level
persistence verification**.

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
See [Valkey debugging](../redis-debugging.md) for inspection and idempotency
troubleshooting.

## Change configuration safely

Restart after changing environment variables. OTLP configuration changes do
not require deleting Valkey keys. TTL changes affect **future expirations
only**. Moving to another Redis/Valkey server or database starts a separate
state domain unless the data is preserved.

Protect labels, annotations, OTLP headers and Valkey credentials. For exact
key formats, TTL investigation and step-by-step diagnostics, use
[Redis/Valkey debugging](../redis-debugging.md).

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
