# alert-event-receiver

An Alertmanager webhook receiver that converts alert lifecycle transitions into structured OpenTelemetry log events. It uses Redis/Valkey to track the latest alert occurrence and suppress duplicate lifecycle event emissions.

## Why?

Alertmanager is designed to repeat notifications until an alert is resolved or otherwise handled. This is useful for urgent incidents, but not every alert requires immediate human action. **alert-event-receiver** preserves a searchable history of firing and resolved transitions without requiring every alert to become a ticket or a page.

Events can be sent through an OpenTelemetry Collector or directly to an OTLP-compatible backend, such as VictoriaLogs.

> **Delivery limitation:** This initial implementation uses best-effort OTLP event delivery. State is written before emission; a crash or export failure can permanently lose a lifecycle event. Concurrent processing of the same fingerprint can also race. There is no durable replay; do not treat this release as a guaranteed-delivery audit log.

## Features

- Accepts Alertmanager webhook batches and processes each alert individually.
- Distinguishes alert occurrences using the Alertmanager fingerprint and `startsAt` timestamp.
- Handles opening, repeated firing, resolution, reopening, stale notifications, superseded firing, and orphan resolutions.
- Tracks the latest occurrence in Redis/Valkey and uses expiring idempotency keys for emission deduplication.
- Exports structured `firing` and `resolved` events via OTLP/gRPC or OTLP/HTTP.
- Exposes Prometheus metrics, JSON application logs, and a health endpoint.
- Supports versioned builds (`-version`).

## Architecture

```text
Prometheus / vmalert
        |
        v
    Alertmanager (send_resolved: true)
        |
        | POST /webhook
        v
 alert-event-receiver ---- Redis / Valkey
        |                   latest occurrence state
        |                   idempotency keys
        |
        | OTLP logs (gRPC or HTTP)
        v
 OTel Collector (optional) ----> VictoriaLogs / another OTLP backend
```

Redis/Valkey stores **working state**, not complete historical events. Lifecycle history is intended to live in the log backend.

## Quick start

Requirements: Go 1.26.11+, Redis or Valkey, and an OTLP-compatible log receiver.

```bash
# Build the application
go build -o alert_event_receiver ./cmd/server

# Start a local Redis instance (example)
docker run --rm -d --name alert-event-redis -p 6379:6379 redis:7

# Point OTLP at a running Collector (or compatible receiver)
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
export OTEL_EXPORTER_OTLP_PROTOCOL=grpc

./alert_event_receiver
```

The default HTTP listener is `:9011`. Verify startup:

```bash
curl -fsS http://localhost:9011/healthz
curl -fsS http://localhost:9011/metrics | head
./alert_event_receiver -version
```

The health endpoint returns HTTP 200; it is not a comprehensive Redis or OTLP readiness check.

### Configure Alertmanager

Add a webhook receiver and route alerts to it. This is an **example**: adapt the route to your existing Alertmanager configuration instead of replacing your routing tree.

```yaml
route:
  receiver: alert-event-receiver
  group_by: [alertname]
  group_wait: 30s
  group_interval: 5m
  repeat_interval: 4h

receivers:
  - name: alert-event-receiver
    webhook_configs:
      - url: http://alert-event-receiver.example:9011/webhook
        send_resolved: true
```

The URL must be reachable from Alertmanager. `send_resolved: true` is essential to receive resolution transitions. Keep your existing routing and receivers as appropriate.

### Send a test webhook

With the receiver running, POST an example firing notification:

```bash
curl -i -X POST http://localhost:9011/webhook \
  -H 'Content-Type: application/json' \
  -d '{
    "receiver":"alert-event-receiver",
    "status":"firing",
    "groupKey":"test-group",
    "externalURL":"http://alertmanager.example",
    "alerts":[{
      "status":"firing",
      "fingerprint":"test-fingerprint-001",
      "startsAt":"2026-11-06T10:00:00Z",
      "labels":{"alertname":"TestAlert","severity":"warning","service":"demo"},
      "annotations":{"summary":"Test firing event"}
    }]
  }'
```

A successful HTTP 200 means processing completed without a reported error; it does **not** confirm durable delivery to the log backend. Check application logs, the Prometheus metrics, and the configured OTLP destination. For a resolution test, send another notification with the **same fingerprint and `startsAt`**, `status: "resolved"`, and an `endsAt` timestamp at or after `startsAt`.

## Configuration

All application settings are supplied through environment variables.

### HTTP and Redis/Valkey

| Variable | Default | Description |
|---|---|---|
| `ADDRESS` | `:9011` | HTTP listen address |
| `REDIS_ADDR` | `localhost:6379` | Redis/Valkey endpoint |
| `REDIS_PASSWORD` | empty | Redis/Valkey password |
| `REDIS_DB` | `0` | Database number |
| `CLOSED_STATE_TTL` | `24h` | Retention of closed occurrence tombstones |
| `IDEMPOTENCY_TTL` | `168h` (7 days) | Retention of emission idempotency keys |

Duration settings use Go duration syntax, e.g. `30m`, `24h`, `168h`. **Currently, invalid duration strings silently fall back to the defaults**; verify configuration carefully.

### OpenTelemetry

The SDK reads standard OTLP exporter environment variables.

| Variable | Default / example | Description |
|---|---|---|
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | `grpc` or `http/protobuf` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4317` | OTLP destination for the chosen protocol |
| `OTEL_EXPORTER_OTLP_HEADERS` | empty | Optional authentication or backend headers |
| `OTEL_EXPORTER_OTLP_LOGS_HEADERS` | empty | Optional log-specific exporter headers |
| `OTEL_SERVICE_NAME` | `alert-event-receiver` | Service name on emitted records |
| `OTEL_RESOURCE_ATTRIBUTES` | e.g. `cluster=dev,tenant=platform` | Additional resource attributes |

For an OTLP/HTTP endpoint, configure `http/protobuf` and use the URL required by your backend. For example, the existing VictoriaLogs integration uses:

```bash
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:9428/insert/opentelemetry
export OTEL_EXPORTER_OTLP_HEADERS='VL-Stream-Fields=service.name,am.external_url,alert.alertname,alert.fingerprint,alert.status,alert.transition'
```

Backend-specific headers and URL paths should be checked against your deployed backend version. An OpenTelemetry Collector is an alternative when you want centralized routing or buffering.

## Lifecycle processing

The receiver uses the **Alertmanager fingerprint** as the alert identity. Each distinct occurrence is identified by that fingerprint plus its `startsAt` instant (compared with nanosecond precision, independent of timezone offset). `endsAt` is the resolution time, **not** part of occurrence identity.

Valid firing and resolved notifications require a nonempty fingerprint and nonzero `startsAt`. Resolved notifications additionally require nonzero `endsAt >= startsAt`.

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

A newer firing can supersede an older open occurrence **without synthesizing a resolution** for the old occurrence. The emitted firing event retains `state_write_result=opened`; the processing result distinguishes `SupersededFiring`. A repeated resolution does not refresh the tombstone TTL; the first accepted closure wins.

Idempotency checks suppress **event emission**, not lifecycle processing. Required Redis/Valkey read/write failures return processing errors rather than authoritative state decisions. The whole batch is validated before any state access; invalid alerts, malformed JSON, null payloads, or trailing JSON receive HTTP 400. An empty alerts batch remains accepted. If an operational error occurs during processing, the handler returns HTTP 500 after attempting the remaining alerts; Alertmanager may retry the entire batch.

### Redis/Valkey debugging

The receiver uses:

- `alertstate:{fingerprint}` — hash holding the latest firing or closed occurrence.
- `alertidemp:{fingerprint}:{startsAt}:{transition}` — expiring emission idempotency key; `startsAt` is canonical UTC RFC3339Nano.

Open firing state has no TTL. Closed tombstones default to 24 hours, and idempotency keys default to seven days. These periods trade memory use against protection from delayed duplicate notifications.

**For the full schema, inspection commands, TTL interpretation, failure scenarios, and step-by-step debugging, see [Redis/Valkey debugging](docs/redis-debugging.md).**

## HTTP endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/webhook` | Receive grouped Alertmanager webhook notifications |
| `GET` | `/metrics` | Prometheus operational metrics |
| `GET` | `/healthz` | Basic HTTP health check |

## Observability

The receiver writes structured JSON logs to stdout. Its Prometheus metrics include:

| Metric | Description |
|---|---|
| `alertreceiver_webhook_requests_total` | Webhook requests by HTTP status class |
| `alertreceiver_webhook_duration_seconds` | Webhook request latency |
| `alertreceiver_alert_duration_seconds` | Histogram of durations for successfully closed matching occurrences, independent of OTLP emission |
| `alertreceiver_events_emitted_total` | Emitter calls that returned nil (SDK submission), by transition; not confirmed queue retention or export |
| `alertreceiver_emit_errors_total` | Synchronous emitter errors before successful submission, by transition |
| `alertreceiver_redis_errors_total` | Redis operation errors |
| `alertreceiver_redis_ops_total` | Redis operation outcomes |
| `alertreceiver_redis_op_duration_seconds` | Redis operation latency |
| `alertreceiver_redis_idemp_setnx_total` | Idempotency `SET NX` outcomes |
| `alertreceiver_duplicates_dropped_total` | Event emissions suppressed by idempotency |
| `alertreceiver_state_writes_total` | State write / transition results |
| `alertreceiver_state_entries` | Locally maintained estimate of tracked state entries |
| `alertreceiver_idempotency_keys_created_total` | Created idempotency keys |
| `alertreceiver_resolved_orphans_total` | Resolutions without matching open occurrence |
| `alertreceiver_closed_ttl_seconds` | Applied closed-state TTL observations |

**Metric caveat:** `alertreceiver_state_entries` is updated from this process's write path. It is **not** a live count of all Redis keys; it can diverge after restart, tombstone expiry, or changes made by another instance.

### VictoriaLogs query examples

These examples use VictoriaLogs LogsQL and assume the event attributes are indexed as shown:

```logsql
_time:1h alert.label.service:checkout alert.transition:firing
```

```logsql
_time:7d alert.transition:resolved
| stats by (alert.alertname) avg(alert.duration_seconds) as avg_duration, count() as total
| sort by (avg_duration desc)
```

```logsql
_time:24h event.kind:alert_transition
| stats by (alert.alertname, alert.label.service) count() as transitions
| sort by (transitions desc)
| limit 20
```

Adjust field names and stream settings to match your OTLP ingestion configuration.

## Alert SLA Proxy (Phases 2–3)

The separate `alert-sla-proxy` selects label-based SLA policies and evaluates
weekly IANA schedules, holidays and active-interval overlap. By default it is
stateless `observe_only`. Opt-in Phase 3 persists occurrences in Valkey, schedules
pending alerts, handles out-of-order resolutions and prepares durable intent.
It never forwards notifications or marks them delivered.

```bash
go build -o /tmp/alert-sla-proxy ./cmd/alert-sla-proxy
# Stateless observation:
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy.yaml
# Or durable lifecycle/scheduling, with a configured persistent Valkey:
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-durable.yaml
```

Phase 3 trusts Alertmanager's webhook lifecycle. If an SLA opportunity arrives
without a recorded resolution, the occurrence is considered firing in known
state. There is no API verification, silence/inhibition recheck or invented
freshness timeout. A delayed/lost resolution can therefore produce eligibility
for an alert already resolved underneath; this limitation is accepted and logged.

Durable mode acknowledges HTTP 200 only after all selected occurrences commit;
partial/store failures return 503 and the batch is safely retryable. Valkey
acceptance is not unconditional disk/replica persistence. Lifecycle, SLA eligibility
and delivery status remain independent; policy changes hold pending bindings.
The default listener is `:9012`, with `/webhook`, `/metrics`, `/healthz`, `/readyz`.
Existing receiver lifecycle, Valkey schema and OTLP behavior are unchanged.

See the [service guide](docs/alert-sla-proxy.md),
[Phase 3 implementation and operations](docs/sla-proxy-phase3.md), and
[decision register](docs/sla-proxy-decisions.md).

## Load testing

See the [alertload guide](docs/alertload.md) for lifecycle and weighted mixed load
generation, CSV/JSON profiles, optional receiver/VictoriaLogs checks, safety
precautions, and report interpretation.

## Development and releases

```bash
go test ./...
go test -race ./...
go vet ./...
govulncheck ./...
go build -o alert_event_receiver ./cmd/server
./alert_event_receiver -version
```

The repository uses Go 1.26, with Go 1.26.11 pinned in CI and image builds. GitHub Releases provide versioned builds; the `-version` flag prints the version injected at build time (or `unknown` when not set).


## Delivery Semantics and Failure Handling

Alertmanager webhook delivery is asynchronous and may be retried. The receiver
validates each entire batch before changing state. Malformed JSON and invalid
alerts receive HTTP 400; required Valkey read/write failures receive HTTP 500.
Other valid alerts are still attempted after an operational error. A 500 response
does **not** roll back previously written state, and a retried batch may already
contain transitions that have been processed. The empty-batch behavior is unchanged.

Valkey maintains lifecycle state and emission idempotency keys, not a delivery
queue. An idempotency command failure is best effort: it is logged and counted,
and emission continues after successful lifecycle state processing. Existing
keys suppress emission only, without changing the detected processing outcome.

The delivery stages have different meanings:

1. **Generated:** a lifecycle event is created after its required state operation
   succeeds, before emission deduplication. Repeated/stale notifications generate
   no event. Existing idempotency keys can suppress a generated event's emission.
2. **SDK submission:** the emitter calls Logger.Emit and returns nil. The legacy
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
nanosecond identity, and excludes endsAt. Existing event fields, record timestamps,
severity mapping, version, and Valkey keys remain unchanged. Relevant processing
failure logs contain the same ID, alert_fingerprint, and alert_transition; aggregate
export failures intentionally do not identify a particular event. Normal per-alert
outcomes are counters rather than success logs.

### Delivery metrics

All labels have bounded values; event IDs, alert names, fingerprints, URLs, and
alert labels are never metric labels.

| Metric | Labels | Precise meaning |
|---|---|---|
| `alertreceiver_webhook_requests_total` | `status` = 2xx/4xx/5xx | Requests reaching the webhook handler, classified by HTTP response; 4xx includes invalid requests |
| `alertreceiver_alerts_validated_total` | `transition` = firing/resolved | Valid individual alerts admitted to lifecycle processing, including repeats/stale alerts; rejected HTTP batches do not enter processing |
| `alertreceiver_processing_outcomes_total` | `outcome` | Processor results: opened, reopened, superseded_firing, closed, resolved_without_open_state, repeated_firing, repeated_resolved, stale_firing, stale_resolved, indeterminate, invalid_notification; invalid_notification counts direct processor calls, because invalid HTTP batches are rejected earlier |
| `alertreceiver_events_generated_total` | `transition` | Created transitions after successful state operations, before idempotency suppression |
| `alertreceiver_events_emitted_total` | `transition` | Emitter calls returning nil: SDK submission only |
| `alertreceiver_emit_errors_total` | `transition` | Synchronous emitter errors, not asynchronous exporter failures; the current production emitter cannot receive an export error from Logger.Emit |
| `alertreceiver_otlp_export_requests_total` | `protocol`, `result` | Calls made to the underlying Export method; result is success (nil) or error (non-nil); protocol is grpc or http/protobuf (http alias is normalized) |
| `alertreceiver_otlp_export_records_total` | `protocol`, `result` | Number of records passed in Export calls grouped by the call result, **not** per-record delivery or permanent loss |
| `alertreceiver_otlp_export_duration_seconds` | `protocol` | Export call latency including exporter-managed retries |
| `alertreceiver_otel_sdk_errors_total` | none | Errors reported to the process-global OTel error handler, potentially from any OTel signal; may include the same errors already observed by the exporter decorator |
| `alertreceiver_redis_errors_total` | `operation` | Valkey store-operation errors counted at the store boundary; SET NX uses setnx_idemp (the former duplicate processor-side idempotency increment is removed) |
| `alertreceiver_redis_idemp_setnx_total` | `result` | Idempotency operation outcomes: set/exists/error |

Existing request latency, state-write outcomes, locally estimated state entries,
Redis operation counts/latency, idempotency creation/suppression, orphan, and TTL
metrics remain available as documented above. The exporter decorator is the
**only source** of export metrics. Empty Export calls made during flush/shutdown
also count as calls but contribute zero records. ForceFlush/Shutdown method
failures are logged and do not increment export request or record counters.

A failed export call may have partially succeeded. In particular, an OTLP partial
rejection is an exporter error; all records passed to that call count under error,
although only some may have been rejected. The SDK's rejection error type is
internal: no rejected-record count is inferred by parsing error strings. Do not
interpret error-record counts as a loss counter or sum SDK errors with export
errors. A successful later shutdown/flush cannot retroactively recover a failed
batch. Gaps between generated, submitted, and exported counters do not establish
exact loss: batching, suppression, concurrent scrapes, restarts and partial results
all affect comparisons.

### Monitoring examples

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

### Closed alert duration

`alertreceiver_alert_duration_seconds` is an unlabelled histogram measuring
`event.DurationSeconds` (`endsAt - startsAt`, in seconds) for the `Closed`
lifecycle outcome. An observation is recorded only after the required Valkey
closure write succeeds, before idempotency suppression or OTLP submission.
It therefore includes closures whose emission is suppressed or fails, and does
not establish successful OTLP delivery or backend persistence.

Orphan resolutions, stale notifications, repeated resolutions, firing outcomes,
and failed required state operations contribute no observations. A valid
zero-duration closure contributes one observation of zero seconds. Only observed
successful matching closures are counted; occurrences that close while the
receiver is unavailable may not be represented. Existing concurrent-processing
limitations still apply: this is not an exactly-once global incident count.

Finite bucket boundaries in seconds are:
`300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400, 172800, 259200, 604800`.
Finite boundaries run from five minutes through seven days; the first
`le="300"` bucket includes all durations up to five minutes, including zero and
subsecond closures. Duration is measured from occurrence timestamps, independent
of the Prometheus scrape interval. Count and sum retain exact observed durations;
only quantile resolution becomes coarser below five minutes. The automatic
`+Inf` bucket and count/sum also include durations beyond seven days. There are
no alert-specific metric labels.

The following queries aggregate all scraped receiver instances. Average and
quantiles are in seconds; quantiles are estimates limited by the bucket layout.
The average has no meaningful value when there are no observations in the window.

```promql
# Average duration of matching occurrences closed during the last 24 hours
sum(rate(alertreceiver_alert_duration_seconds_sum[24h]))
/
sum(rate(alertreceiver_alert_duration_seconds_count[24h]))

# Median (P50) duration
histogram_quantile(0.50,
  sum by (le) (rate(alertreceiver_alert_duration_seconds_bucket[24h]))
)

# P95 duration
histogram_quantile(0.95,
  sum by (le) (rate(alertreceiver_alert_duration_seconds_bucket[24h]))
)

# Successfully closed matching occurrences during the last 24 hours
sum(increase(alertreceiver_alert_duration_seconds_count[24h]))
```

The count query excludes orphan resolutions and is a Prometheus extrapolated
estimate, so it may be fractional. These process-local counters reset on restart;
`rate` and `increase` handle observed resets. The histogram is separate from OTLP
export latency (`alertreceiver_otlp_export_duration_seconds`).

### SDK diagnostics, batching and shutdown

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

SDK-managed batching and exporter retry defaults remain unchanged. Supported
batch settings are `OTEL_BLRP_MAX_QUEUE_SIZE`, `OTEL_BLRP_SCHEDULE_DELAY`,
`OTEL_BLRP_EXPORT_TIMEOUT`, and `OTEL_BLRP_MAX_EXPORT_BATCH_SIZE`. Time settings
are SDK environment values in milliseconds. The receiver adds no retry loop.

These are standard OpenTelemetry settings. See the official
[SDK environment variable specification](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/)
and [OTLP exporter configuration](https://opentelemetry.io/docs/specs/otel/protocol/exporter/)
for batching, endpoints, protocols, headers, and exporter retry behavior.

Defaults below apply to the installed Go Logs SDK `v1.47.0` and OTLP exporters
`v0.23.0`; other SDK versions may differ.

| Setting | Default | Meaning |
|---|---|---|
| `OTEL_BLRP_MAX_QUEUE_SIZE` | `2048` records | Maximum pending in-memory queue size; overflow discards the oldest queued records |
| `OTEL_BLRP_SCHEDULE_DELAY` | `1000` ms (1 second) | Interval triggering batch export; reaching the batch size can trigger export sooner |
| `OTEL_BLRP_MAX_EXPORT_BATCH_SIZE` | `512` records | Maximum records passed in one export batch |
| `OTEL_BLRP_EXPORT_TIMEOUT` | `30000` ms (30 seconds) | Deadline for a batch export, including retries within that call |
| `OTEL_EXPORTER_OTLP_LOGS_TIMEOUT` | `10000` ms (10 seconds) | Log exporter timeout; overrides the general `OTEL_EXPORTER_OTLP_TIMEOUT` setting. In this HTTP exporter it limits each HTTP request; the gRPC exporter applies it across its export/retry operation |
| Retry policy maximum elapsed time | 60 seconds | Retry budget for eligible transient failures, subject to earlier export deadlines |
| Initial retry backoff | 5 seconds | Initial base delay, randomized by the exporter |
| Maximum retry backoff | 30 seconds | Maximum base backoff interval; randomized waits or server-requested delays can differ |

The retry policy values are exporter defaults configured through its Go API,
not additional environment variables exposed by this receiver. Only errors the
exporter classifies as retryable are retried; other errors return immediately.
The 60-second retry budget therefore does **not** promise 60 seconds of buffering.

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
persistence guarantee. See [Valkey debugging](docs/redis-debugging.md) for state
inspection and idempotency troubleshooting.

## Current limitations and roadmap

The event receiver intentionally does not implement a durable outbox, guaranteed OTLP delivery, atomic read/compare/write processing across concurrent requests, per-destination incident delivery, SLA/calendar policies, or NetBox enrichment. Only the latest occurrence is kept in Redis/Valkey; after tombstone expiry or state loss, older occurrence ordering cannot be reconstructed from Redis. The separate SLA proxy supports observation and opt-in durable SLA scheduling; downstream delivery remains deferred.

v0.2.0 focuses on best-effort delivery observability. It adds no persistent queue, retries, replay, policies, or sinks.

## Repository layout

```text
cmd/server/          Receiver entry point
cmd/alertload/       Independent webhook lifecycle load generator
cmd/alert-sla-proxy/ Independent SLA proxy (observation or durable state)
internal/slaproxy/   SLA calendars, durable lifecycle/store, scheduler, HTTP and metrics
internal/alertload/  Generator, bounded scheduler, HTTP workers and reports
internal/config/     Environment configuration
internal/models/     Alertmanager and lifecycle event models
internal/processor/  Occurrence-aware lifecycle processing
internal/state/      Redis/Valkey store
internal/telemetry/  OTLP export, Prometheus metrics, JSON logging
internal/webhook/    Alertmanager webhook HTTP handler
docs/redis-debugging.md  Redis/Valkey operational reference
```
