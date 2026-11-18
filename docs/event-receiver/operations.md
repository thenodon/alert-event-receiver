# Receiver operations and troubleshooting

[Getting started](getting-started.md) · [Configuration](configuration.md) · [Metrics and event reference](reference.md)

## Find the problem

| Question / symptom | First checks |
|---|---|
| Is it healthy? | `/healthz` proves liveness only. Check Valkey operation errors and exporter errors separately |
| Was a webhook accepted? | HTTP 200 and request/outcome counters; `webhook received` logs precede processing completion |
| Was an event delivered? | Compare generated/submitted/export counters, then query the backend by fingerprint or event.id |
| HTTP 400 | Fix invalid JSON, fingerprint or lifecycle timestamps; the whole batch is rejected before state writes |
| HTTP 500 | Required state/processing error; other alerts may already have committed. Inspect errors before assuming retry restores events |
| No event for a repeated alert | Expected: same occurrence/status refreshes state but emits no new transition |
| Orphan or stale resolution | Compare fingerprint and exact startsAt with latest working state; see [inspection](../redis-debugging.md) |
| Valkey outage | Required operations fail with 500; receiver startup fails if initial ping fails |
| OTLP outage | SDK retries/buffers in memory only; final failures or queue overflow may lose records |
| Restart | Working state remains in Valkey, in-memory batches do not. Already stored state can suppress later emission |

## Delivery Semantics and Failure Handling

Alertmanager webhook delivery is asynchronous and may be retried. The receiver
validates each entire batch before changing state. Malformed JSON and invalid
alerts receive HTTP 400; required Valkey read/write failures receive HTTP 500.
Other valid alerts are still attempted after an operational error. A 500 response
does **not** roll back previously written state, and a retried batch may already
contain transitions that have been processed. An empty batch is accepted.

Valkey maintains lifecycle state and emission idempotency keys, not a delivery
queue. An idempotency command failure is best effort: it is logged and counted,
and emission continues after successful lifecycle state processing. Existing
keys suppress emission only, without changing the detected processing outcome.

The delivery stages have different meanings:

1. **Generated:** a lifecycle event is created after its required state operation
   succeeds, before emission deduplication. Repeated/stale notifications generate
   no event. Existing idempotency keys can suppress a generated event's emission.
2. **SDK submission:** the emitter calls Logger.Emit and returns nil. The
   `events_emitted_total` metric and `EmissionAccepted` result mean only this.
   The current Logs API offers no per-call queue-retention receipt: overflow,
   shutdown, or a no-op provider may still discard records without an emit error.
3. **Exporter success:** the configured OTLP exporter's Export call returned nil.
   Its own network retries, if any, are included in that call and its duration.
   This is not a count of network attempts.
4. **Backend persistence:** the receiver does not query VictoriaLogs to verify
   stored events. Exporter success, including success from an intermediate
   Collector, is not proof that every event is durably stored or queryable.

Events carry `event.id`: a lowercase SHA-256 digest of the JSON-encoded string
array `[fingerprint, startsAtUTC, transition]`, with startsAt in RFC3339Nano.
It is stable across repeat notifications and timezone representations, preserves
nanosecond identity, and excludes endsAt. Event timestamps use original lifecycle time. Relevant processing
failure logs contain the same ID, alert_fingerprint, and alert_transition; aggregate
export failures intentionally do not identify a particular event. Normal per-alert
outcomes are counters rather than success logs.

## Monitoring examples

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

## SDK diagnostics, batching and shutdown

Application and SDK failures use structured logs with component, operation and
best-effort redaction of textual error context. String values, error messages,
and resolved slog groups are checked for configured authentication-header values,
Redis passwords, and credentials/query strings in HTTP URLs. Opaque
`slog.KindAny` values (such as maps, slices, structs, custom Stringers and JSON
marshalers) are omitted rather than serialized; their contents are not recursively
sanitized. `LogValuer` values are resolved and their resulting kind is handled by
the same rules. Full alert payloads and annotations are not deliberately logged.

This is **not comprehensive sanitization of arbitrary SDK diagnostic data**.
Unknown secrets, attribute/group names, and sensitive text not matching the known
redaction patterns can remain visible. Operators should avoid putting secrets in
diagnostic text or backend error messages and restrict access to application logs.

The SDK global error handler observes diagnostics independently of exporter
metrics. Both SDK logging and error-handler configuration are process-global and
installed once at startup. Queue-full warnings are routed to structured logs;
there is no stable per-event rejection callback or queue-drop Prometheus counter
in this implementation. Experimental SDK self-metrics and undocumented internals
are not used. Queue overflow warnings are emitted when the SDK next exports or
flushes, so a crash can prevent even that warning. Queue drops are observable
through these structured SDK diagnostics but are **not counted** in
`alertreceiver_otlp_export_requests_total` unless an Export call actually occurs,
nor in `alertreceiver_otel_sdk_errors_total` unless the SDK invokes its error
handler. In the installed SDK, queue overflow produces a logging warning, not an
error-handler callback. Any later export is counted independently of the queue
drop; neither counter is a queue-drop or permanent-loss counter.

For SDK queue, batching, timeout, retry and TLS settings see the
[configuration reference](configuration.md#sdk-batching-and-attribute-limits).
The application preserves SDK-managed batching/retry behavior and adds no retry
queue. Export duration includes the exporter's own retries.

**Outage example:** assume the HTTP receiver responds with retryable HTTP 503
errors, with no special server-requested delay. A single event normally enters
an export batch at the next scheduled export, roughly within one second when
idle. The exporter retries, but the default batch deadline ends that export
call after roughly 30 seconds. The SDK then discards that failed batch rather
than putting it back into the queue. In this example the event may be held for
roughly 31 seconds from submission, not indefinitely until the receiver returns.
A non-retryable error can cause it to be discarded much sooner.

**Queue capacity example:** if an export is blocked and new lifecycle events
arrive at 100 records/second, an initially empty 2,048-record pending queue fills
in about `2048 / 100 = 20.5 seconds`. Further arrivals displace the oldest queued
records, even while the in-flight export is still retrying. This estimate concerns
the pending queue and excludes the batch already being exported; actual behavior
depends on arrival rate and export completion times. Only generated lifecycle
events enter this queue, not every repeated Alertmanager notification.

There is no fixed retention duration for queued events. Increasing queue capacity
can postpone overflow, and increasing timeouts can permit longer export attempts,
but neither makes buffering durable or requeues a batch after final export
failure. A restart loses in-memory records, and Valkey state/idempotency may
suppress later notifications for events that were never delivered.

Shutdown first allows up to 10 seconds for HTTP requests to drain, then closes
Valkey and allows up to 5 seconds for LoggerProvider.Shutdown to flush pending
batches and release the exporter. Shutdown uses a fresh, non-cancelled context;
startup failure paths also clean up initialized resources. Unexpected HTTP listener
failures, including an occupied port, are logged and return a nonzero process exit
code after cleanup; they are not treated as graceful shutdown. Timeout/flush/exporter
shutdown errors produce structured logs. No shutdown-specific metrics are added.
Final process counters may not be scraped before exit. A successful flush still
provides no query-level persistence verification.

### Remaining reliability limits

Lifecycle state and idempotency keys are written **before** SDK submission.
Crashes in that window, SDK queue drops, failures after submission, and forced
termination can lose events. A crash may leave no diagnostic; normal shutdown
only attempts to flush the in-memory queue within its deadline. Required state
errors do not undo writes Valkey may already have accepted. Concurrent lifecycle
decisions remain non-atomic; tombstone expiry limits occurrence history.

The receiver is deliberately best effort: it has no transactional outbox,
persistent queue, durable replay, custom retries, or exactly-once/end-to-end
persistence guarantee. See [Valkey debugging](../redis-debugging.md) for state
inspection and idempotency troubleshooting.

## Change configuration safely

Restart after changing environment variables. OTLP changes do not require deleting
Valkey keys. Deletion is not a delivery replay mechanism and can create duplicate
or misclassified transitions. TTL changes affect future expirations only. Moving
to another server/database starts a separate state domain unless data is preserved.
Protect labels, annotations, OTLP headers and Valkey credentials; HTTP ingress
supports native HTTPS/Basic Auth or a trusted TLS proxy; see the shared guide.

For exact key formats, TTL investigation and step-by-step diagnostics, use
[Redis/Valkey debugging](../redis-debugging.md). The receiver has no supported
manual replay or durable outbox repair procedure.

## Secure webhook and metrics access

Use the [shared inbound security guide](../http-security.md) for native HTTPS,
independent Basic Auth, trusted-proxy deployments, CA trust and smoke tests.
Defaults remain HTTP/no auth. Security errors fail before listening. Unauthorized
requests return 401 before webhook processing; check client credentials rather
than investigating lifecycle or sink retries. Health probes bypass Basic Auth
but must use HTTPS when TLS is configured. Restrict probe/backend access through
network controls. Restart after password/certificate rotation. Inbound security
is independent of outbound connections and does not require deleting Valkey state
or changing SLA policy epochs.
