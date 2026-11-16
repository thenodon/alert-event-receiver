# Receiver HTTP, event and metrics reference

[Concepts](concepts.md) · [Operations](operations.md) · [Configuration](configuration.md)

## HTTP contract

| Method | Path | Meaning |
|---|---|---|
| POST | `/webhook` | Validate the complete alert batch, then process alerts |
| GET | `/metrics` | Prometheus exposition |
| GET | `/healthz` | HTTP liveness only; no dependency readiness check |

Malformed, null or trailing JSON and semantically invalid alerts return 400 before
state processing. Valid firing/resolved alerts require a nonempty fingerprint and
nonzero `startsAt`; resolution also requires nonzero `endsAt >= startsAt`. Empty
batches are accepted. The receiver does not enforce the SLA proxy's strict envelope
rules: unknown JSON fields are ignored and version/receiver are not required.
Required state failures return 500 after attempting other alerts. HTTP 500 does not
roll back prior writes. Wrong methods are rejected with 405 by the HTTP routing.

## OTLP lifecycle records

Body is `alert lifecycle event`. Record timestamp is `startsAt` for firing and
`endsAt` for resolution. Event kind is `alert_transition`, version 1.

| Attribute | Meaning |
|---|---|
| `event.id` | SHA-256 of JSON tuple `[fingerprint, startsAtUTC, transition]`, startsAt in RFC3339Nano |
| `event.kind`, `event.version` | Contract identity and version |
| `alert.transition`, `alert.status` | Firing or resolved |
| `alert.fingerprint` | Original supplied fingerprint, never recomputed |
| `alert.fingerprint_source` | Sorted label-name description, not a newly calculated fingerprint |
| `alert.alertname` | Original `alertname` label |
| `alert.starts_at`, `alert.ends_at` | UTC string attributes; ends_at only for resolution |
| `alert.duration_seconds` | Event duration; matching closed occurrences also feed the duration histogram |
| `alert.state_write_result` | opened, reopened, closed or resolved_without_open_state; superseding firing retains opened |
| `alert.orphan_reason` | Present when applicable to an orphan resolution |
| `alert.generator_url` | Original generator URL |
| `alert.label.*`, `alert.annotation.*` | Flattened original labels and annotations |
| `am.group_key`, `am.receiver`, `am.external_url` | Alertmanager envelope metadata |

Occurrence identity, event ID and log-record timestamps preserve the exact instant.
The current string attributes `alert.starts_at` and `alert.ends_at` are formatted
at whole-second precision. Do not use these strings alone to reconstruct
nanosecond identity. SDK attribute limits can drop or truncate fields in large
label/annotation sets; see [configuration](configuration.md#sdk-batching-and-attribute-limits).

Severity mapping is critical → Fatal, error → Error, warning/warn → Warn,
info → Info, and other values → Info. Severity text retains the original label.

## Metrics

Labels use bounded outcomes, not fingerprints, event IDs or arbitrary alert labels.
The registry also includes Go/process collectors. Counters reset on restart.

| Metric | Labels | Meaning |
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
| `alertreceiver_redis_errors_total` | `operation` | Valkey store-operation errors counted at the store boundary; SET NX uses setnx_idemp |
| `alertreceiver_redis_idemp_setnx_total` | `result` | Idempotency operation outcomes: set/exists/error |
| `alertreceiver_webhook_duration_seconds` | `status` | Webhook request latency |
| `alertreceiver_alert_duration_seconds` | none | Histogram of durations for successfully closed matching occurrences, independent of OTLP emission |
| `alertreceiver_redis_ops_total` | `operation`, `result` | Redis operation outcomes |
| `alertreceiver_redis_op_duration_seconds` | `operation` | Redis operation latency |
| `alertreceiver_duplicates_dropped_total` | none | Event emissions suppressed by idempotency |
| `alertreceiver_state_writes_total` | `result` | State write / transition results |
| `alertreceiver_state_entries` | `status` | Locally maintained estimate of tracked state entries |
| `alertreceiver_idempotency_keys_created_total` | `transition` | Created idempotency keys |
| `alertreceiver_resolved_orphans_total` | none | Resolutions without matching open occurrence |
| `alertreceiver_closed_ttl_seconds` | none | Applied closed-state TTL observations |

The exporter decorator is the **only source** of export metrics. Empty Export calls made during flush/shutdown
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

`alertreceiver_state_entries` is a write-path estimate, not a live key census;
it can diverge after restart, expiry or another writer.

## Closed alert duration

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

## VictoriaLogs query examples

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
