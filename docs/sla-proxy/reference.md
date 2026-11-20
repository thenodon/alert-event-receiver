# SLA proxy HTTP, delivery and metrics reference

[Getting started](getting-started.md) · [Concepts](concepts.md) · [Configuration](configuration.md) · [Operations](operations.md)

This is the technical reference for the SLA proxy's HTTP API, outbound
notification format, metrics and structured logs. For the operational meaning
of eligibility, authorization and receipts, see [concepts](concepts.md).
For diagnosis and recovery procedures, see [operations](operations.md).

## HTTP contract

| Method | Path | Meaning |
|---|---|---|
| POST | configured webhook path | Validate a notification batch; observe or persist according to mode |
| GET | `/metrics` | Independent Prometheus registry |
| GET | `/healthz` | HTTP liveness |
| GET | `/readyz` | Observation capability and rolling dataset coverage, or additionally active policy/store readiness in durable modes; HTTP 503 while stopping/unavailable |

### Input validation

Webhook input uses the shared Alertmanager v4 wire model. Strict JSON decoding
rejects unknown fields, duplicate keys, null payloads, malformed timestamps and
trailing documents. Require `version: "4"`, a nonempty receiver, group status
`firing` or `resolved`, a non-null alerts array and nonnegative `truncatedAlerts`.
Each alert requires firing/resolved status, a nonempty fingerprint and nonzero
`startsAt`. Resolved alerts also require nonzero `endsAt` at or after `startsAt`.
An explicit empty alerts array is accepted.

Group metadata, original labels, annotations and exact alert timestamps are
preserved; fingerprints are never recomputed. The entire batch is validated
before incrementing alert counters.

Selection follows the [policy selection rules](concepts.md#selecting-a-policy).
Durable modes also accept known resolutions after policy removal.

### Response semantics by mode

| Mode | Successful webhook response means | Does not establish |
|---|---|---|
| `observe_only` | Notification evidence was evaluated; selected alerts may be eligible, ineligible, pending or unknown | Durable state or sink delivery |
| `durable_state_only` | All selected notifications were committed to Valkey | Sink delivery |
| `delivery_enabled` | All selected notifications were committed to Valkey; dispatch is asynchronous | Sink acceptance or durable action receipt |

A successful response can contain selection skips. `forwarded: false` describes
the **synchronous webhook handler**; it does not mean that an enabled asynchronous
dispatcher will never deliver. Sink acceptance and the subsequent durable receipt
commit are distinct events. See [delivery ordering](operations.md#delivery-ordering-and-retries).

## Observe-only responses

Use the fresh [test webhooks](getting-started.md#observe-without-dependencies)
for live tests. The following response illustrates a resolved 07:30–08:30 local
interval overlapping a Swedish business-hours window; it is not a current-time
verification fixture.

Illustrative observe-only response (schema version 2):

```json
{
  "schema_version": 2,
  "mode": "observe_only",
  "persisted": false,
  "forwarded": false,
  "received": 1,
  "selected": 1,
  "skipped": 0,
  "evaluated": 1,
  "eligible": 1,
  "ineligible": 0,
  "pending": 0,
  "unknown": 0,
  "evaluation_errors": 0,
  "evaluated_at": "2026-11-12T09:00:00Z",
  "policy_observations": [{
    "sla_label": "sla_id",
    "sla_value": "business-hours-se",
    "fingerprint": "demo-001",
    "starts_at": "2026-11-11T06:30:00Z",
    "alert_status": "resolved",
    "ends_at": "2026-11-11T07:30:00Z",
    "alert_count": 1,
    "eligible_at_start": false,
    "calendar_reason": "outside_window",
    "eligibility_status": "eligible",
    "eligibility_reason": "active_interval_overlaps_sla",
    "next_eligible_at": "2026-11-11T07:00:00Z",
    "policy_version": "<SHA-256 of calendar configuration and rules>"
  }]
}
```

`eligible_at_start` answers only the calendar question; `eligibility_status` and
`eligibility_reason` answer the lifecycle question. `ends_at` and `alert_status`
make the resolution evidence explicit. Calendar reasons retain `eligible`,
`outside_window`, `holiday`, `additional_date` and
`no_eligibility_within_horizon`.

Lifecycle reasons are bounded: `start_in_sla`,
`active_interval_overlaps_sla`, `empty_active_interval`,
`resolved_before_first_eligible_instant`, `resolved_interval_searched`,
`awaiting_first_eligible_instant`, `activity_at_eligibility_unverified`,
`start_after_observation_cutoff`, `search_horizon_exhausted` and
`calendar_evaluation_error`.

In observe-only mode, HTTP 200 acknowledges successful **observation only**,
including pending and unknown statuses. There is no persistence, forwarding
guarantee or replay. Invalid batches return 400 without observing alerts;
oversized bodies return 413, wrong webhook methods 405, and requests arriving
while stopping 503. Calendar failures return HTTP 500 with
`eligibility_status=error` and a bounded error code; `eligible_at_start`,
`calendar_reason`, `policy_version`, `next_eligible_at` and
`calendar_search_until` are absent for those failed evaluations. Other policies
are still observed. Errors are not counted as authoritative ineligibility.

In observe-only responses, top-level `eligible` and `ineligible` are lifecycle
counts, alongside `pending` and `unknown`. Together these successful counts
equal `evaluated`; `selected = evaluated + evaluation_errors`. Per-observation
counts, including error entries, sum to `selected`. INFO logs contain an
observation object matching the response. Repeated notifications and retried
batches can increase counters again; process-local metrics and logs are not an
exactly-once audit store.

## Durable responses

Both durable modes return schema version 3 with `mode: durable_state_only` or
`mode: delivery_enabled`. Responses include per-occurrence state summaries and
acceptance counts. `forwarded: false` means the handler does not synchronously
forward alerts; enabled delivery runs asynchronously after acceptance.

HTTP 200 and `persisted: true` require every selected notification to commit to
Valkey. A failed input returns HTTP 503 with `persisted: false`,
`persisted_count` and `failed_count`; other inputs are still attempted.
Alertmanager can retry the whole batch, including already committed occurrences,
without duplicating logical intents. Valkey acceptance is subject to its
persistence and failover settings; a lost transaction response does not prove
rollback. Validation failures return 400 before state access, and oversized
requests return 413.

The `occurrences` array contains occurrence ID, fingerprint, `starts_at`,
optional `ends_at`, lifecycle and committed version. Each binding summary contains
`sla_label`, `sla_value`, `sink_id`, `policy_version`, eligibility, delivery,
`hold_reason` when present, and `next_evaluation_at` when scheduled. These are
**acceptance-time snapshots**, not later asynchronous delivery results.
`delivery_enabled: true` is present in delivery mode; the false value is omitted
in state-only responses. For lifecycle rules see
[concepts](concepts.md#occurrences-and-known-lifecycle).

## HTTP destination contract

### Outbound payload

Every HTTP request is a POST with `Content-Type: application/json` and **exactly
one alert**, using the original Alertmanager notification metadata and alert
data:

```json
{
  "receiver": "sla-proxy",
  "status": "firing",
  "alerts": [{
    "status": "firing",
    "labels": {"alertname": "HighCPU", "sla_id": "production"},
    "annotations": {"summary": "CPU utilization is high"},
    "startsAt": "2026-11-15T07:00:00.123456789Z",
    "endsAt": "0001-01-01T00:00:00Z",
    "generatorURL": "https://prometheus.example/graph",
    "fingerprint": "original-fingerprint"
  }],
  "groupLabels": {}, "commonLabels": {}, "commonAnnotations": {},
  "externalURL": "", "version": "4", "groupKey": "", "truncatedAlerts": 0
}
```

`receiver`, `groupKey`, group/common labels and annotations, `externalURL`,
`version` and `truncatedAlerts` are preserved where supplied. Missing original
strings stay empty; empty map containers serialize as `{}`. No receiver, group
key, labels or annotations are synthesized from unrelated records.
Fingerprint, `generatorURL`, labels, annotations and nanosecond timestamps are
preserved. Top-level and alert statuses match the intent action. Resolved
delivery uses the stored resolution notification. Firing `endsAt` is forwarded
as originally received; it is **not** used as authoritative resolution state.
No SLA-specific labels are injected.

The full per-alert notification and original group metadata are already
retained in the occurrence. Authorization freezes that notification onto the
intent before I/O, so later webhook updates and process restarts do not change
its logical retry payload. Different alerts in an incoming batch never share
reconstructed payloads.

### External destination expectations

This is a generic Alertmanager webhook, suitable for the
[ServiceNow Prometheus connector](https://www.servicenow.com/docs/r/it-operations-management/event-management/prometheus-events-integration.html).
Configure its Event Management connector/plugin, integration user permissions
and field/CI handling in ServiceNow. The proxy does not implement a ServiceNow
API, severity mapping or guarantee connector processing after acceptance.
Validation uses local HTTP fixtures; a real ServiceNow tenant must be tested
before production.

Use HTTPS for production Basic Authentication. TLS verifies certificates through
normal system roots; there is no insecure-skip option. Plain HTTP is allowed for
local/private deployment testing and exposes credentials/payloads on the wire.
Redirects are never followed. One shared HTTP transport reuses connections;
HTTP/1 with a non-rewindable POST body prevents transparent transport replays,
including when `Idempotency-Key` is present. All retries belong to the durable
dispatcher.

Responses have a 64 KiB header limit; at most 64 KiB + 1 response-body bytes
are read/discarded before closing. Oversized bodies are not retained or logged
and may prevent connection reuse. A 2xx remains API acceptance even if its
discarded body is truncated; body contents do not define application-level
success here.

URLs, authentication values/headers, response bodies and alert payloads are not
included in HTTP delivery logs or returned transport errors. Labels and
annotations are deliberately sent to the configured destination and remain
sensitive stored payload data. Protect YAML, credential environment, Valkey,
logs and network access. There is no arbitrary per-alert URL routing. Endpoints
and any environment proxy settings are trusted operator configuration.

## Metrics and logs

### Webhook, selection and calendar metrics

| Metric | Labels | Meaning |
|---|---|---|
| `sla_proxy_notifications_received_total` | `status` = 2xx/4xx/5xx | Requests to the webhook handler by response class |
| `sla_proxy_webhook_duration_seconds` | `status` | Webhook handler latency |
| `sla_proxy_alerts_received_total` | `status` = firing/resolved | Alerts in fully valid batches, including selected/skipped alerts and repeats |
| `sla_proxy_alerts_selected_total` | `sla_label`, `sla_value`, `status` | Observation selections by configured policy pair |
| `sla_proxy_alerts_skipped_total` | `reason` = missing_sla_label/unknown_sla | Selection skips |
| `sla_proxy_calendar_classifications_total` | `sla_label`, `sla_value`, `classification`, `reason` | Successful per-notification calendar classification at startsAt; inside_sla/outside_sla |
| `sla_proxy_alert_eligibility_observations_total` | `sla_label`, `sla_value`, `alert_status`, `eligibility_status`, `reason` | Per-notification lifecycle status, including pending, unknown and error |
| `sla_proxy_policy_errors_total` | `reason` | Failed evaluator calls; one call per unique policy pair plus exact startsAt in each batch |
| `sla_proxy_holiday_evaluations_total` | `result` | ICS dataset checks, including future search dates; result = holiday/not_holiday/error |

Calendar success metrics count notifications (including within-request cache
reuse); error metrics count actual evaluator calls. In durable modes,
observations are available for accepted records, not failed state reads.
Notification evidence metrics remain separate from durable known lifecycle and
delivery state.

Policy-pair labels are bounded by loaded configuration; unknown values never
create metric series. No fingerprints, alert names, site IDs or alert label sets
are metric labels. The registry includes Go/process collectors, without receiver
metrics. Durable and delivery metrics are described separately below. All
counters reset on restart.

Holiday-check counters measure date checks, not alerts or decisions; explicit
dates within valid coverage and omitted calendars do not invoke the holiday
provider. Error reason labels are bounded: `calendar_error`, `unknown_policy`,
`invalid_evaluation_time`, `cancelled` and `evaluation_error`. Observation
metrics have no sink label. Every configured destination uses the occurrence's
selected policy; there is no destination-specific SLA selection.

### Durable state and scheduler metrics

Durable modes also expose:

| Metric | Meaning |
|---|---|
| `sla_proxy_unresolved_occurrences` | Known-firing occurrences in the namespace |
| `sla_proxy_pending_occurrences` | Occurrences pending SLA, including policy-held pending bindings |
| `sla_proxy_held_occurrences` | Occurrences with policy, horizon or resolution-conflict holds |
| `sla_proxy_lifecycle_transitions_total{from,to,reason}` | Durable lifecycle transitions |
| `sla_proxy_scheduler_claims_total{result}` | Scheduler claim outcomes |
| `sla_proxy_scheduler_evaluations_total{result}` | Scheduler evaluation outcomes |
| `sla_proxy_scheduler_errors_total{operation}` | Scheduler operational errors |
| `sla_proxy_scheduler_lag_seconds` | Scheduler lag |
| `sla_proxy_state_store_operations_total{operation,result}` | State-store operation outcomes |
| `sla_proxy_state_store_errors_total{operation}` | State-store errors |

Durable gauges are namespace-wide reconciliation snapshots: use **max**, not
sum, across replicas. Counters are process-local and reset on restart.

### Structured webhook logs

JSON logs go to stdout. Startup reports the active mode and whether delivery is
enabled. Rejections, policy evaluation failures and shutdown/listener errors
have component and operation fields. Each fully validated webhook produces one
INFO `webhook observation` log with `http_status` and an `observation` object
matching the JSON response, including counts and occurrence/policy
classifications. This includes observe-only HTTP 500 evaluation results and
durable acceptance summaries; separate failure logs provide operational
context. Rejected malformed requests retain their warning logs without an
observation.

The INFO record describes processing, not proof of successful HTTP delivery,
log-output persistence or a sink receipt. It includes fingerprints and selected
policy identifiers, but does not include full webhook payloads, original label
sets or annotations.

## Delivery logs and metrics

### Delivery logs and transport failures

The log destination emits one `SLA alert delivered` structured record per
successful sink call. Record time and `delivery_at` are actual sink processing
time. `starts_at`, optional `ends_at` and `eligible_at` remain separate
historical attributes. Intent/occurrence/binding IDs, sink, action, policy and
attempt are included; full labels/annotations/payloads are omitted. The
handler's reported error is honored and disabled INFO output is not successful
delivery. `slog` does not guarantee durable log storage; a handler that hides
write errors cannot prove persistence. Duplicate log records with the same
intent ID are possible.

HTTP attempts log `SLA HTTP request completed` with sink/type,
intent/occurrence/binding, action, attempt, status, duration and outcome. A
receipt-commit log is separate from sink success. Failure logs omit raw sink
errors.

Transport failures include a bounded `failure_class`:

| Class | Meaning |
|---|---|
| `canceled` | Context cancellation |
| `timeout` | Deadline expiry or typed network timeout |
| `tls_error` | Typed certificate verification or TLS record error |
| `connection_error` | Other typed network operation/DNS error |
| `transport_error` | Other transport failure |
| `none` | Successful call or HTTP status response |

Classification checks cancellation before timeout, then TLS before generic
connection errors; it uses standard error types without hostname inspection
or message matching.

All five transport failure classes remain retryable, including TLS errors and
cancellation. The HTTP metric uses `status_class="network_error"` for these
failures and `result="temporary_failure"`; the finer diagnostic class appears
in structured logs. Raw transport errors are discarded, including from the
returned error chain. HTTP status classification and `Retry-After` follow the
[retry rules](operations.md#delivery-ordering-and-retries). These diagnostics
do not remove ambiguous external outcomes or provide exactly-once delivery.

### Delivery metrics

| Metric | Meaning |
|---|---|
| `sla_proxy_delivery_attempts_total{sink_id,action,result}` | Calls by success/temporary_failure/permanent_failure |
| `sla_proxy_delivery_receipts_total{sink_id,action}` | Observed successful durable receipt commits |
| `sla_proxy_delivery_receipt_commits_total{sink_id,result}` | Committed, error or fenced outcome persistence |
| `sla_proxy_delivery_retries_total{sink_id,action}` | Calls after first authorized attempt |
| `sla_proxy_delivery_claims_total{sink_id,result}` | Claimed, busy, inactive, reclaimed, fenced or error |
| `sla_proxy_delivery_errors_total{operation}` | Poll/dispatch operational errors |
| `sla_proxy_delivery_duration_seconds{sink_id,action}` | Individual sink call latency |
| `sla_proxy_delivery_eligibility_delay_seconds{sink_id}` | First SLA opportunity to firing success, after receipt commit |
| `sla_proxy_delivery_pending_bindings{sink_id}` | Queued binding work, including retry-delayed work |
| `sla_proxy_delivery_inflight_bindings{sink_id}` | Persisted claims, including expired claims awaiting recovery |
| `sla_proxy_delivery_failed_bindings{sink_id}` | Held/permanently failed delivery bindings |
| `sla_proxy_delivery_ready_bindings` | Due work, including expired claims |
| `sla_proxy_delivery_claimed_bindings` | All persisted binding claims |
| `sla_proxy_delivery_held_bindings` | All held delivery bindings |
| `sla_proxy_http_requests_total{sink_id,status_class,result}` | HTTP API call outcomes with bounded status classes |
| `sla_proxy_http_request_duration_seconds{sink_id}` | HTTP request plus bounded response-drain time |

Gauges are namespace-wide snapshots sampled during polling: use **max**, not
sum, across replicas. Pending delivery gauges count prepared binding work, not
alerts still waiting for SLA with no ready intent. Counters/histograms are
process-local; sum replica rates as appropriate. Labels contain configured sink
IDs and bounded outcomes, never fingerprints, URLs, usernames or alert labels.
A sink success with an unconfirmed receipt can increment attempts without a
corresponding receipt counter. A lost EXEC reply can also undercount actual
persisted receipts.

Enrichment, destination routing rules, sink editing/redrive APIs and downstream
processing guarantees are not implemented. See [Redis/Valkey
debugging](../redis-debugging.md#alert-sla-proxy-keys-and-debugging) for
operational inspection, configuration changes and recovery.

## ICS holiday datasets

The [dataset contract](holiday-datasets.md#manifest-and-accepted-ics-profile)
defines accepted ICS properties, resource limits, manifest fields and coverage.
Invalid files fail startup. Missing date coverage propagates as
`calendar_error`, never `not_holiday`. Observe-only evaluation failures return
500; failed new durable acceptance returns 503. Existing pinned lifecycle
facts are retained.

The holiday metric uses only `{result}` with three bounded values; the former
`country` label has been removed. No manifest country, paths, hashes or policy
IDs are labels. Explicit additional dates within coverage keep their precedence
and bypass lookup counters. Coverage errors produce the bounded `error` result.
