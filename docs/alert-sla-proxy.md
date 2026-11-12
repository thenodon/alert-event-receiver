# Alert SLA Proxy — Phase 2

`alert-sla-proxy` is an independent, stateless observation service. It validates
Alertmanager notifications, selects label-based SLA policies and evaluates their
calendar eligibility at each alert's `startsAt`. It **does not persist alerts,
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
| `evaluation_horizon_days` | `370` | Local calendar dates searched, including today; 1 through 3660 |
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

## Calendar semantics

Each alert is classified at its original `startsAt` instant, interpreted in the
selected policy's time zone. Request latency, the current clock and endsAt do not
change whether that occurrence began inside SLA. For example, an alert that began
at 16:55 in an 08:00–17:00 window remains inside SLA when it arrives at 17:05.
`evaluated_at` is processing metadata only.

Calendar evaluations are reused only for the same selected policy pair and exact
startsAt instant within one request. UTC normalization makes equivalent offsets
share a cache entry without losing nanoseconds. No decisions are cached across
requests. Responses still identify distinct fingerprint/startsAt occurrences;
repeated notifications for the same occurrence and policy share an alert_count.
Resolved alerts use their original startsAt too; the classification does not
decide future resolution delivery, which must follow per-sink continuity.
Enrichment remains a no-op and does not affect policy evaluation.

Holiday exclusions cover the entire **local date being tested**, beginning with
the occurrence's startsAt date, including overnight carry. For a Monday
22:00–Tuesday 06:00 window, a Tuesday holiday stops eligibility
at Tuesday midnight. A Monday holiday does not exclude Tuesday's carry unless
Tuesday itself is excluded. Explicit dates take precedence over country checks.
Omitting holidays means only the weekly schedule applies.

The evaluator maps actual instants to local wall time using IANA offset intervals.
Spring-forward wall times that do not exist have no eligible instants. If a window
partly crosses the gap, its real portion still applies. During autumn rollback,
both occurrences of a matching wall time are eligible. The next instant can be
the second occurrence of a window that already ended in the first occurrence.
Non-hour offsets and transition sizes are supported.

Search examines startsAt's local date and the next horizon-1 dates, independently
of 23/24/25-hour day length. With horizon=1, only the rest of that date is searched.
When startsAt is inside SLA, next_eligible_at equals starts_at. Otherwise it is
the earliest eligible instant at or after startsAt, in UTC. It can precede
processing time for a delayed webhook. The response field was renamed from
next_evaluation_at to next_eligible_at; the old JSON field is no longer emitted.
Exhaustion returns `no_eligibility_within_horizon` with no next time; this does
not establish permanent
ineligibility. A next time is informational: **nothing wakes up or forwards an
alert at that time**. Finding a later eligible instant does not reclassify an
outside-SLA occurrence as inside SLA. Deferring its notification until a window
opens would be a separate delivery rule requiring approval before later phases.

policy_version hashes the policy pair, configuration, search horizon, provider
version and evaluator rule version. It does not identify an alert occurrence or
serialize the deployed IANA database. Rebuild/retest after time-zone data updates;
configuration changes require restarting the service.

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

For example, submit a selected alert:

```bash
curl -i http://localhost:9012/webhook \
  -H 'Content-Type: application/json' \
  -d '{"version":"4","receiver":"sla-proxy","status":"firing","alerts":[{"status":"firing","fingerprint":"demo-001","startsAt":"2026-11-11T08:00:00.123456789Z","labels":{"alertname":"Demo","sla_id":"business-hours-se"}}]}'
```

An example response for this startsAt, assuming processing at 09:00 UTC on the
following day, is:

```json
{
  "mode": "observe_only",
  "persisted": false,
  "forwarded": false,
  "received": 1,
  "selected": 1,
  "skipped": 0,
  "evaluated": 1,
  "eligible": 1,
  "ineligible": 0,
  "evaluation_errors": 0,
  "evaluated_at": "2026-11-12T09:00:00Z",
  "policy_observations": [{
    "sla_label": "sla_id",
    "sla_value": "business-hours-se",
    "fingerprint": "demo-001",
    "starts_at": "2026-11-11T08:00:00.123456789Z",
    "alert_count": 1,
    "eligible": true,
    "reason": "eligible",
    "next_eligible_at": "2026-11-11T08:00:00.123456789Z",
    "policy_version": "<SHA-256 of configuration and rules>"
  }]
}
```

HTTP 200 acknowledges **observation only**. Alertmanager considers that request
successfully handled. There is no durable acceptance, forwarding guarantee or
replay after restart. Invalid batches return 400 without observing any alerts;
oversized bodies return 413, wrong webhook methods 405, and requests arriving
while stopping 503. Transport failures may leave the client uncertain whether
observation counters incremented; counters are not an exactly-once audit log.

Runtime evaluation errors return HTTP 500 and the same observation envelope with
evaluation_errors and a bounded error reason on each failed policy observation.
Other selected policies are still evaluated. An errored policy's eligible=false
is **not an authoritative ineligible decision**: check error first; it has no
policy_version or next_eligible_at. No success decision counter increments for
that failed evaluation. Retrying can repeat observation counters; there is no
rollback, deduplication or durable acceptance. Configuration/coverage errors fail
startup instead of turning into runtime ineligibility.

selected = evaluated + evaluation_errors; evaluated = eligible + ineligible.
policy_observations contains one entry per distinct selected policy and occurrence
(fingerprint plus exact startsAt), in first-seen order, with alert_count and
canonical UTC starts_at. Different fingerprints remain separate even if the
calendar result was reused. Missing/unknown selections do not enter evaluation.
Successful reasons are eligible, outside_window, holiday, additional_date or
no_eligibility_within_horizon. Errors are explicitly separate from these results.

## Metrics and logs

| Metric | Labels | Meaning |
|---|---|---|
| `sla_proxy_notifications_received_total` | `status` = 2xx/4xx/5xx | Requests to the webhook handler by response class |
| `sla_proxy_webhook_duration_seconds` | `status` | Webhook handler latency |
| `sla_proxy_alerts_received_total` | `status` = firing/resolved | Alerts in fully valid batches, including selected/skipped alerts and repeats |
| `sla_proxy_alerts_selected_total` | `sla_label`, `sla_value`, `status` | Observation selections by configured policy pair |
| `sla_proxy_alerts_skipped_total` | `reason` = missing_sla_label/unknown_sla | Selection skips |
| `sla_proxy_policy_decisions_total` | `sla_label`, `sla_value`, `decision`, `reason` | Successful per-alert startsAt classifications; decision = eligible/ineligible |
| `sla_proxy_policy_errors_total` | `reason` | Failed evaluator calls; one call per unique policy pair plus exact startsAt in each batch |
| `sla_proxy_holiday_evaluations_total` | `country`, `result` | Country calendar checks, including future search dates; result = holiday/not_holiday/error |

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
