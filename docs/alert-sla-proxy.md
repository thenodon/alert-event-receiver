# Alert SLA Proxy — Phase 2

`alert-sla-proxy` is an independent, stateless observation service. It validates
Alertmanager notifications, selects label-based SLA policies and evaluates their
calendar eligibility plus alert active-interval overlap. It **does not persist alerts,
connect to Valkey, schedule work or forward notifications**. Keep operational
receivers active through test/fan-out routing. Calendar results are observations,
not enforced downstream delivery decisions.

The existing event receiver continues to process lifecycle transitions and emit
OTLP logs independently. SLA selections do not generate lifecycle OTLP events.

## Build and run

With Go 1.26.11 or newer:

```bash
go build -o /tmp/alert-sla-proxy ./cmd/alert-sla-proxy
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy.yaml
```

The `-config` flag defaults to `alert-sla-proxy.yaml` in the working directory.
`-version` prints the build version (`unknown` unless injected with
`-ldflags "-X main.version=..."`). Configuration is loaded once at startup;
receiver environment variables have no effect on this service.

| YAML setting | Default | Meaning |
|---|---|---|
| `listen_address` | `:9012` | TCP host:port; port 0 permits an ephemeral listener |
| `webhook_path` | `/webhook` | Exact absolute path, distinct from built-in endpoints |
| `max_body_bytes` | `1048576` | Request body limit, 1 byte through 64 MiB |
| `shutdown_timeout` | `10s` | HTTP drain deadline, greater than zero through 1 minute |
| `evaluation_horizon_days` | `370` | Local calendar dates searched from startsAt; 1 through 3660 |
| `sla_definitions` | required | Ordered, independent label/policy namespaces |

The checked-in [configuration example](examples/alert-sla-proxy.yaml) contains
two independent entries that deliberately reuse a policy name. The conventional
label `sla_id` is explicit in the example; each definition requires `sla_label`.

Unknown YAML fields, duplicate mapping keys, extra documents, duplicate/empty
selection labels, empty SLA names and empty policy maps fail startup. Label names
use ASCII `[a-zA-Z_][a-zA-Z0-9_]*`. Each policy requires an explicit IANA time zone
(including `UTC`; process-dependent `Local` is rejected) and a nonempty schedule
map. IANA data is embedded for minimal deployment images.

Weekday keys are lowercase full English names. Windows use exact
`HH:MM-HH:MM` syntax; `24:00` is allowed only as an end. Equal start/end is
invalid; use `00:00-24:00` for a full day. Starts are inclusive and ends exclusive.
Overnight windows continue into the following weekday. Overlaps are rejected
within and across weekdays, including Sunday/Monday and Saturday/Sunday boundaries;
adjacent windows are allowed. An empty weekday list represents no configured
windows, though an overnight window from the previous weekday may still apply.

An optional holidays block requires country or explicit dates; subdivision
requires country. Country syntax is two uppercase ASCII letters. Explicit dates
must be valid, exact `YYYY-MM-DD` values. Coverage is checked before listening:

| Country | Subdivision | Coverage |
|---|---|---|
| `SE` | omitted | Swedish public holidays |
| `DE` | omitted | Holidays common nationally in Germany |
| `DE` | `BY` | Bavaria's statewide public holidays |

Other combinations fail startup. The offline adapter uses `rickar/cal/v2`
v2.1.32; no remote calendar service is contacted. Only public holidays are used.
Swedish Christmas Eve, New Year's Eve and Midsummer's Eve are not included as
public holidays; add explicit dates for company closures. Bavarian municipality-
specific holidays, including Augsburg's Peace Festival and local Assumption Day
observance, also need explicit dates. A country-less list of explicit dates is
supported. Provider upgrades require reviewed dependency updates and regression
tests; expanding coverage requires explicit adapter support.

## Calendar versus lifecycle eligibility

The calendar engine answers whether a supplied instant is inside SLA and returns
its first eligible instant at or after that instant. The webhook evaluates this
calendar at startsAt, but **does not treat that boolean as final alert eligibility**.
An alert is eligible when its active interval intersects an SLA window.

For resolved alerts, the known interval is `[startsAt, endsAt)`. If the first
eligible calendar instant is strictly before endsAt, overlap is confirmed.
If it equals or follows endsAt, no overlap occurred. Equality is excluded: an
alert ending exactly at 08:00 never overlaps a window opening at 08:00.
A zero-duration interval is empty, even if startsAt is inside SLA.
Webhook arrival time does not change these historical conclusions.

For firing alerts, the end is unknown. A firing notification establishes activity
at startsAt; it does not verify continuous activity through reception. A firing
endsAt may be an expiry estimate and is not treated as a resolution. evaluated_at
is the processing clock and observation cutoff, never a replacement start/end.
It distinguishes a future calendar opportunity from one whose time has passed;
it does not prove the alert was still active when that opportunity arrived.

| eligibility_status | Required evidence / meaning |
|---|---|
| eligible | Resolved: first eligible instant is strictly before endsAt. Firing: startsAt itself is inside SLA and is not after the observation cutoff. This confirms historical overlap, not current activity or delivery. |
| ineligible | Resolved only: the complete active interval is proven to have no overlap, or is empty. Never assigned merely because a firing alert started outside SLA. |
| pending | Firing: startsAt is not after the cutoff, the calendar found a future eligible instant, and that instant is strictly after the cutoff. This is a conditional opportunity, not a durable pending record. |
| unknown | Firing: the later eligible instant has arrived but activity then is unverified; or startsAt is after the cutoff; or the calendar search cannot establish overlap/no overlap over the required range. |
| error | Calendar evaluation failed. No authoritative calendar or lifecycle conclusion is returned. |

For an 08:00–17:00 calendar:

| Notification evidence | Processing time | Lifecycle observation |
|---|---|---|
| Resolved 07:30–07:45 | 08:05 or later | ineligible |
| Resolved 07:30–08:00 | any arrival time | ineligible; endsAt is exclusive |
| Resolved 07:30–08:30 | any arrival time | eligible from 08:00 |
| Firing since 07:30 | 07:35 | pending; first calendar opportunity 08:00 |
| Firing since 07:30 | 08:05 | unknown; activity at 08:00 needs verification |
| Firing since 16:55 | 17:10 | eligible based on activity at startsAt |
| Resolved 16:55–17:30 | later | eligible; future delivery continuity applies outside SLA too |
| Resolved 17:05–18:00 | later | ineligible |

### Calendar search and precision

Calendar evaluations are reused only for the same selected policy pair and exact
startsAt instant within one request. Equivalent time-zone offsets share a cache
entry without losing nanoseconds. No decisions are cached across requests.
Lifecycle observations remain separate by policy, fingerprint, startsAt, alert
status and (for resolved alerts) endsAt. Repeats of exactly that evidence share
alert_count. Firing/resolved inputs or different resolution times must not merge.
Enrichment remains a no-op.

Holiday exclusions cover the entire local date being tested, including overnight
carry. For Monday 22:00–Tuesday 06:00, a Tuesday holiday excludes Tuesday's carry;
a Monday holiday alone does not exclude Tuesday. Explicit dates take precedence
over country checks. With no holidays block, only the weekly schedule applies.

IANA offset intervals preserve the existing DST behavior: nonexistent spring
wall times have no eligible instants; matching autumn wall times are eligible
on both occurrences. Partial gaps, non-hour offsets and skipped dates remain
supported. Window starts are inclusive and ends exclusive.

Search examines startsAt's local date and the next horizon-1 local dates,
independently of day length. next_eligible_at is the **first eligible calendar
instant at or after startsAt**, in canonical UTC with nanosecond precision.
It equals startsAt when the start is inside SLA and may precede evaluated_at
for delayed notifications. It is still reported when it falls at/after a resolved
alert's end; it must not be interpreted as overlap with that resolved interval.

On exhaustion, calendar_reason is no_eligibility_within_horizon and no next
instant is returned. calendar_search_until, when present, is exclusive: no
eligible instant was found between startsAt and that boundary. A resolved
interval ending at/before this boundary is fully covered and can be ineligible;
if it extends beyond the boundary (or coverage is unavailable), status is unknown.
A firing alert with no known future instant is unknown. None of these bounded
search results claims permanent calendar ineligibility.

A future Phase 3 scheduler may use next_eligible_at as a scheduling target. It
must verify non-resolution and sufficient freshness before sending firing.
Phase 2 creates no wakeup, durable pending state, activity verification or delivery.

policy_version hashes policy configuration, horizon, provider and calendar rule
versions; the calendar algorithm and its versioning are unchanged by the lifecycle
helper. It is not an occurrence ID or a serialization of deployed IANA data.
Rebuild/retest after time-zone data updates; config changes require restart.

## HTTP contract

| Method | Path | Meaning |
|---|---|---|
| POST | configured webhook path | Validate and observe a notification batch |
| GET | `/metrics` | Independent Prometheus registry |
| GET | `/healthz` | HTTP liveness |
| GET | `/readyz` | Observation capability; HTTP 503 while stopping |

Webhook input uses the shared Alertmanager v4 wire model. Strict JSON decoding
rejects unknown fields, duplicate keys, null payloads, malformed timestamps and
trailing documents. Require `version: "4"`, a nonempty receiver, group status
`firing` or `resolved`, a non-null alerts array and nonnegative truncatedAlerts.
Each alert requires firing/resolved status, a nonempty fingerprint and nonzero
startsAt. Resolved alerts also require nonzero endsAt at or after startsAt.
An explicit empty alerts array is accepted. Group metadata, original labels,
annotations and exact alert timestamps are preserved; fingerprints are never
recomputed. The entire batch is validated before incrementing alert counters.

Selection is per alert:

1. Find the first configured entry whose label has a nonempty value.
2. Look up the value only in that entry's SLA map.
3. Count a known pair as selected. Skip unknown values as `unknown_sla`, without
   falling back to later entries.
4. If no label matches, skip as `missing_sla_label`.

Skips do not reject an otherwise valid batch. Repeated, old and orphan
notifications are observed independently; there is no occurrence state machine.
Selected means a configured policy was found, not that its SLA window is open.

For example, submit an alert resolved after the SLA window opened:

```bash
curl -i http://localhost:9012/webhook \
  -H 'Content-Type: application/json' \
  -d '{"version":"4","receiver":"sla-proxy","status":"resolved","alerts":[{"status":"resolved","fingerprint":"demo-001","startsAt":"2026-11-11T06:30:00Z","endsAt":"2026-11-11T07:30:00Z","labels":{"alertname":"Demo","sla_id":"business-hours-se"}}]}'
```

Example schema version 2 response, processed the following day:

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

The per-observation eligible boolean has been removed. eligible_at_start answers
only the calendar question; eligibility_status and eligibility_reason answer the
lifecycle question. Resolved ends_at and alert_status make the evidence explicit.
Calendar reasons retain eligible, outside_window, holiday, additional_date and
no_eligibility_within_horizon. Lifecycle reasons are bounded:
start_in_sla, active_interval_overlaps_sla, empty_active_interval,
resolved_before_first_eligible_instant, resolved_interval_searched,
awaiting_first_eligible_instant, activity_at_eligibility_unverified,
start_after_observation_cutoff, search_horizon_exhausted, calendar_evaluation_error.

HTTP 200 acknowledges successful **observation only**, including pending and
unknown statuses. There is no persistence, forwarding guarantee or replay.
Invalid batches return 400 without observing alerts; oversized bodies return
413, wrong webhook methods 405, and requests arriving while stopping 503.
Calendar failures return HTTP 500 with eligibility_status=error and a bounded
error code; eligible_at_start, calendar_reason, policy_version, next_eligible_at
and calendar_search_until are absent for those failed evaluations. Other policies
are still observed. Errors are not counted as authoritative ineligibility.

Schema version 2 intentionally changes the response contract: top-level eligible
and ineligible are now lifecycle counts, with pending and unknown added. Together
these successful counts equal evaluated; selected = evaluated + evaluation_errors.
Per-observation counts, including error entries, sum to selected. INFO logs retain
an observation object exactly matching this response. Consumers of the former
calendar-only boolean/counts must update to schema 2 and eligible_at_start.
Repeated notifications and retried batches can increase counters again; these
process-local metrics and logs are not an exactly-once historical audit store.

## Metrics and logs

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
| `sla_proxy_holiday_evaluations_total` | `country`, `result` | Country calendar checks, including future search dates; result = holiday/not_holiday/error |

The ambiguous sla_proxy_policy_decisions_total metric is removed; update queries
to the calendar or lifecycle metric above. Existing policy error, holiday, request,
selection and skip metrics retain their meanings. Calendar success metrics count
notifications (including cache reuse); error metrics count actual evaluator calls.

Policy-pair labels are bounded by loaded configuration; unknown values never
create metric series. No fingerprints, alert names, site IDs or alert label sets
are metric labels. The registry includes Go/process collectors, without receiver
metrics or speculative delivery counters. All counters reset on restart.

Holiday-check counters measure date checks, not alerts or decisions; explicit
dates and omitted country calendars do not invoke the country provider. Error
reason labels are bounded: calendar_error, unknown_policy,
invalid_evaluation_time, cancelled and evaluation_error. There is no sink label
or destination-specific policy configuration yet. The evaluator accepts a policy
reference; future routing must select it per destination.

JSON logs go to stdout. Startup warns that calendar evaluation is observational
and alerts are not persisted/forwarded. Rejections, policy evaluation failures
and shutdown/listener errors have component
and operation fields. Each fully validated webhook produces one INFO
`webhook observation` log with `http_status` and an `observation` object matching
the JSON response, including counts and occurrence/policy classifications.
This also applies to HTTP 500 evaluation results; the separate ERROR logs remain.
Rejected malformed requests retain their warning logs without an observation.
The INFO record describes processing, not successful HTTP delivery, persistence
or forwarding. It includes fingerprints and selected policy identifiers, but
does not include full webhook payloads, original label sets or annotations.

## Shutdown and routing

SIGINT/SIGTERM stops readiness, closes the listener and drains active HTTP
requests for at most shutdown_timeout. Expiry force-closes connections and exits
nonzero. Listener failures, including occupied ports, also exit nonzero.
HTTP header/read/write/idle timeouts are 5/10/15/60 seconds.

Adapt the design's [fan-out route example](sla-proxy-design.md#3-alertmanager-routing-and-input-contract)
to your existing routing tree. Set `send_resolved: true` for each webhook and
keep `continue: true` on the selected SLA child route followed by an unconditional
operational/log receiver route. A parent's receiver is a fallback, not automatic
fan-out. Verify the final configuration with `amtool check-config` and a real
Alertmanager integration before deployment; httptest tests exercise the proxy,
not Alertmanager routing itself.

See the [decision register](sla-proxy-decisions.md) for architecture reuse and
approval gates. Durable scheduling/state, sink delivery and real enrichment
remain outside this implementation.
