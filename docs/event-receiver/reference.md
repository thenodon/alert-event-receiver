# Receiver HTTP, event and metrics reference

[Getting started](getting-started.md) · [Concepts](concepts.md) · [Configuration](configuration.md) · [Operations](operations.md)

This reference defines the event receiver's HTTP endpoints, OTLP lifecycle
records, Prometheus metrics and query examples. For lifecycle decisions, see
[concepts](concepts.md); for interpreting failures, see
[operations](operations.md).

## HTTP contract

| Method | Path | Meaning |
|---|---|---|
| POST | `/webhook` | Validate the entire alert batch, then process alerts |
| GET | `/metrics` | Prometheus exposition |
| GET | `/healthz` | HTTP liveness only; does not check dependency readiness |

### Validation and responses

| Response | Meaning |
|---|---|
| `200` | The webhook batch was accepted and processed without required state errors; **not** proof of OTLP delivery |
| `400` | Malformed, null or trailing JSON, or semantically invalid alerts; rejected before state processing |
| `500` | Required state or processing failure; remaining alerts are attempted, and prior writes are **not rolled back** |
| `405` | Wrong HTTP method for the route |

Firing and resolved alerts require a nonempty fingerprint and nonzero
`startsAt`. Resolved alerts additionally require nonzero `endsAt >= startsAt`.
An empty alert batch is accepted.

Unlike the SLA proxy's stricter webhook envelope, the event receiver ignores
unknown JSON fields and does not require `version` or `receiver`.

## OTLP lifecycle records

Each emitted record has the body `alert lifecycle event`, event kind
`alert_transition` and event version `1`.

| Transition | Log record timestamp |
|---|---|
| Firing | Alert `startsAt` |
| Resolved | Alert `endsAt` |

### Event attributes

| Attribute | Meaning |
|---|---|
| `event.id` | SHA-256 of JSON tuple `[fingerprint, startsAtUTC, transition]`, with `startsAt` in RFC3339Nano |
| `event.kind`, `event.version` | Event contract identity and version |
| `alert.transition`, `alert.status` | Firing or resolved |
| `alert.fingerprint` | Original Alertmanager fingerprint; never recomputed |
| `alert.fingerprint_source` | Sorted label-name description; **not** a newly calculated fingerprint |
| `alert.alertname` | Original `alertname` label |
| `alert.starts_at`, `alert.ends_at` | UTC string attributes; `ends_at` appears only for resolution |
| `alert.duration_seconds` | Event duration; matching closed occurrences also feed the duration histogram |
| `alert.state_write_result` | `opened`, `reopened`, `closed` or `resolved_without_open_state`; superseding firing retains `opened` |
| `alert.orphan_reason` | Included when applicable to an orphan resolution |
| `alert.generator_url` | Original generator URL |
| `alert.label.*`, `alert.annotation.*` | Flattened original labels and annotations |
| `am.group_key`, `am.receiver`, `am.external_url` | Alertmanager envelope metadata |

**Timestamp precision:** occurrence identity, `event.id` and log-record
timestamps preserve the exact instant. The string attributes
`alert.starts_at` and `alert.ends_at` currently use **whole-second precision**;
do not reconstruct nanosecond occurrence identity from those strings alone.
Large label and annotation sets can also be affected by
[SDK attribute limits](configuration.md#sdk-batching-and-attribute-limits).

### Severity mapping

| Alert severity label | OTLP severity |
|---|---|
| `critical` | Fatal |
| `error` | Error |
| `warning`, `warn` | Warn |
| `info` | Info |
| Other values | Info |

Severity text retains the original label value.

## Prometheus metrics

Metric labels use **bounded outcomes**, not fingerprints, event IDs or arbitrary
alert labels. The registry also exposes Go and process collectors. Counters
reset when the process restarts.

### Webhook and lifecycle processing

| Metric | Labels | Meaning |
|---|---|---|
| `alertreceiver_webhook_requests_total` | `status` = `2xx`/`4xx`/`5xx` | Webhook handler requests by HTTP response class; `4xx` includes invalid requests |
| `alertreceiver_webhook_duration_seconds` | `status` | Webhook request latency |
| `alertreceiver_alerts_validated_total` | `transition` = `firing`/`resolved` | Valid individual alerts admitted to lifecycle processing, including repeats and stale alerts; rejected batches do not enter processing |
| `alertreceiver_processing_outcomes_total` | `outcome` | Processor results: `opened`, `reopened`, `superseded_firing`, `closed`, `resolved_without_open_state`, `repeated_firing`, `repeated_resolved`, `stale_firing`, `stale_resolved`, `indeterminate`, `invalid_notification`. The last counts direct processor calls; invalid HTTP batches are rejected earlier |
| `alertreceiver_events_generated_total` | `transition` | Transitions created after successful state operations, before idempotency suppression |
| `alertreceiver_events_emitted_total` | `transition` | Emitter calls returning nil; **SDK submission only**, not export success |
| `alertreceiver_emit_errors_total` | `transition` | Synchronous emitter errors, not asynchronous export failures; the current production emitter cannot receive an export error from `Logger.Emit` |
| `alertreceiver_duplicates_dropped_total` | none | Event emissions suppressed by idempotency |
| `alertreceiver_resolved_orphans_total` | none | Resolutions without a matching open occurrence |
| `alertreceiver_alert_duration_seconds` | none | Histogram of successfully closed matching occurrence durations, independent of OTLP emission |

### OTLP export and SDK diagnostics

| Metric | Labels | Meaning |
|---|---|---|
| `alertreceiver_otlp_export_requests_total` | `protocol`, `result` | Calls to the underlying `Export` method; result `success` for nil or `error` for non-nil; protocol `grpc` or `http/protobuf` (the `http` alias is normalized) |
| `alertreceiver_otlp_export_records_total` | `protocol`, `result` | Records passed to `Export` calls by call result; **not** per-record delivery or permanent loss |
| `alertreceiver_otlp_export_duration_seconds` | `protocol` | Export call latency, including exporter-managed retries |
| `alertreceiver_otel_sdk_errors_total` | none | Errors sent to the process-global OTel error handler, potentially from any OTel signal; can overlap with exporter decorator errors |

### Valkey state and idempotency

| Metric | Labels | Meaning |
|---|---|---|
| `alertreceiver_redis_errors_total` | `operation` | Valkey store-operation errors counted at the store boundary; SET NX uses `setnx_idemp` |
| `alertreceiver_redis_idemp_setnx_total` | `result` | Idempotency outcomes: `set`, `exists`, `error` |
| `alertreceiver_redis_ops_total` | `operation`, `result` | Redis operation outcomes |
| `alertreceiver_redis_op_duration_seconds` | `operation` | Redis operation latency |
| `alertreceiver_state_writes_total` | `result` | State write and transition results |
| `alertreceiver_state_entries` | `status` | Locally maintained estimate of tracked state entries |
| `alertreceiver_idempotency_keys_created_total` | `transition` | Created idempotency keys |
| `alertreceiver_closed_ttl_seconds` | none | Applied closed-state TTL observations |

`alertreceiver_state_entries` is a **write-path estimate**, not a live Valkey
key census. It can diverge after restart, expiry or writes by another process.

### How to interpret export metrics

The exporter decorator is the **only source** of export metrics. Empty
`Export` calls during flush or shutdown count as requests but contribute
zero records. Failures of `ForceFlush` or `Shutdown` are logged, not counted
as export requests or records.

An export call returning an error may have **partially succeeded**. In
particular, OTLP partial rejection returns an error, so all records passed
to that call count under `result="error"` even if only some were rejected.
The SDK rejection error type is internal; the receiver does not infer a
rejected-record count by parsing error strings.

Therefore:

- **Do not** treat error-labelled exported records as permanently lost records.
- **Do not** sum SDK error counts with exporter errors as if they were distinct failures.
- **Do not** infer exact loss from differences among generated, submitted and exported counters. Batching, suppression, concurrent scrapes, restarts and partial results affect those comparisons.
- A later successful shutdown or flush **cannot retroactively recover** a batch whose export finally failed.

See [delivery failure handling](operations.md#understand-processing-and-delivery)
for operational interpretation.

## Closed alert duration histogram

`alertreceiver_alert_duration_seconds` measures `event.DurationSeconds`
(`endsAt - startsAt`, in seconds) for the **`Closed` lifecycle outcome**.
It has no alert-specific metric labels.

An observation is recorded **after the required Valkey closure write succeeds**
and **before** idempotency suppression or OTLP submission. Consequently, the
histogram includes matching closures even if event emission is suppressed or
fails; it does not establish OTLP delivery or backend persistence.

It excludes orphan resolutions, stale notifications, repeated resolutions,
firing outcomes and failed required state operations. A valid zero-duration
closure contributes an observation of zero. Closures missed while the receiver
is unavailable are not represented, and concurrent-processing limitations
mean this is **not** an exactly-once global incident count.

### Buckets and interpretation

Finite bucket boundaries, in seconds, are:

```text
300, 600, 900, 1800, 3600, 7200, 14400, 21600,
43200, 86400, 172800, 259200, 604800
```

The finite boundaries span **five minutes to seven days**. The first
`le="300"` bucket includes zero and subsecond closures as well as durations
up to five minutes. The automatic `+Inf` bucket, count and sum include
closures beyond seven days.

Duration comes from occurrence timestamps, independently of the Prometheus
scrape interval. Count and sum retain the observed durations; histogram
quantile resolution is coarse below five minutes.

### PromQL examples

These queries aggregate across all scraped receiver instances. Average and
quantiles are in seconds; quantiles are estimates constrained by bucket
boundaries. The average is undefined when the window has no observations.

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

The count query excludes orphan resolutions and uses Prometheus extrapolation,
so the result may be fractional. `rate` and `increase` account for observed
counter resets. This histogram is separate from OTLP export latency,
`alertreceiver_otlp_export_duration_seconds`.

## VictoriaLogs query examples

These examples use **VictoriaLogs LogsQL** and assume event attributes are
indexed under the field names shown.

Find recent firing transitions for a service:

```logsql
_time:1h alert.label.service:checkout alert.transition:firing
```

Calculate average resolution duration by alert name:

```logsql
_time:7d alert.transition:resolved
| stats by (alert.alertname) avg(alert.duration_seconds) as avg_duration, count() as total
| sort by (avg_duration desc)
```

Find the most frequent alert transitions by alert name and service:

```logsql
_time:24h event.kind:alert_transition
| stats by (alert.alertname, alert.label.service) count() as transitions
| sort by (transitions desc)
| limit 20
```

Adjust field names and stream settings to match your OTLP ingestion
configuration.
