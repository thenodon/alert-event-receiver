# Load testing the event receiver with `alertload`

[Documentation index](README.md) · [Receiver operations](event-receiver/operations.md) · [Development](development.md)

`alertload` sends Alertmanager-compatible webhooks directly to a receiver to
exercise lifecycle handling, throughput and failure behavior. It supports
single scenarios, weighted mixed workloads, JSON/CSV scenario definitions,
and optional checks against receiver metrics and VictoriaLogs.

**Use a dedicated test receiver and Valkey database.** Webhooks mutate receiver
state and can generate real downstream events. A unique run ID isolates alert
identities, **not** the receiver's aggregate Prometheus metrics or production state.
`alertload` does not connect directly to Valkey.

## Quick start: run a lifecycle test

From the repository root, build the generator:

```bash
go build -o /tmp/alertload ./cmd/alertload
```

With a test receiver listening on `127.0.0.1:9011`, run:

```bash
/tmp/alertload \
  -url http://127.0.0.1:9011/webhook \
  -scenario lifecycle \
  -rate 10 -duration 10s -batch-size 1 -workers 10 \
  -resolve-after 5m \
  -base-time "$(date -u -d '15 minutes ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -output /tmp/alertload-report.json
```

This schedules **10 HTTP requests/second**, each containing one alert, for
a ten-second firing phase, then up to ten seconds of matching resolutions.
Each phase has its own scheduling window and bounded drain. The duration
between firing and resolution is synthetic: `-resolve-after 5m` changes
`endsAt - startsAt`; it does **not** sleep five minutes.

For a larger example, use `-rate 100 -duration 10s -batch-size 10
-workers 20 -resolve-after 1h`, with a base time at least 65 minutes
in the past. That nominally offers **1,000 alert notifications/second**
(100 requests × 10 alerts), with 1,000 initial firing batches and up
to 1,000 matching resolution batches. Its two nonempty phases take
roughly 20 seconds **plus** drains.

The generator prints a terminal summary. `-output` writes a JSON
report (schema version 1). See [interpreting results](#interpreting-results).

## Choose a scenario

| Scenario | Ordered phases |
|---|---|
| `firing` | Unique firing occurrences |
| `lifecycle` | Firing → matching resolved |
| `duplicates` | Firing → repeated firing → matching resolved → repeated resolved |
| `orphans` | Resolved occurrences with no preceding firing |
| `stale` | Newer firing → older firing → older resolved → matching newer resolved |
| `reopen` | Firing → resolved → newer firing with same fingerprint → matching newer resolved |
| `supersede` | Firing → newer firing with same fingerprint → older resolved → matching newer resolved |
| `mixed` | Seeded, interleaved cohorts of the seven profiles |

A **phase** is an execution step such as sending firing batches or their
matching resolutions. The generator completes and drains each phase before
the next. All alerts in one request share the same status, including the
top-level status.

For `lifecycle` and `duplicates`, only an original firing batch with a
completed HTTP 2xx result is eligible for resolution. Only successful
resolution batches are eligible for repeated resolution. Failure of a
repeated firing does not revoke eligibility established by the original
firing. A timed-out or failed firing is **unknown** because it may already
have mutated receiver state; the generator does not retry it or assume
it is safe to resolve. HTTP 4xx is separately classified as rejection.

For the `stale`, `reopen`, `supersede` and mixed workloads, see
[dependent phases and generations](#dependent-phases-and-generations).

## Synthetic time and reproducibility

`-base-time` sets the first occurrence timestamp; each alert index adds
one nanosecond. Fingerprints are deterministic hashes of the run ID,
seed and occurrence index. Repeated transitions retain labels,
fingerprint and `startsAt`. The seed namespaces identities but does
not randomize request timing; it also controls mixed-cohort shuffling.

The default base time is fixed at `2026-01-01T00:00:00Z`, **not** the
current time. By default, each invocation generates a cryptographically
random run ID. An explicit `-run-id` must contain 1–64 ASCII letters,
digits or `._-`, beginning with a letter or digit.

The same run ID, seed, base time and batch size reproduce the planned
payloads, although saturation and failures can alter actual sends.
**Reproduce a run only against fresh test state.** Reusing alert identities
against existing state may produce duplicate or stale outcomes.

To test duration histogram boundaries without waiting, run isolated
lifecycles with `-resolve-after` set to `0s`, `299s`, `300s`, `301s`,
`3600s`, `86400s`, `604800s` or `604801s`. These cover zero, finite
boundaries and the greater-than-seven-day `+Inf` bucket.

Choose a base time that places all synthetic event timestamps within
the backend's retention/query window and not in the future. See the
additional warning for [multi-generation workloads](#dependent-phases-and-generations).

## CLI flags

| Flag | Default | Meaning |
|---|---|---|
| `-url` | `http://127.0.0.1:9011/webhook` | Direct webhook destination |
| `-rate` | `10` | Target HTTP requests/second **per phase** |
| `-duration` | `1m` | Scheduling window per nonempty phase, excluding drain |
| `-batch-size` | `1` | Alerts per HTTP request |
| `-workers` | `10` | Maximum concurrent requests |
| `-timeout` | `10s` | Per-request timeout including response reading |
| `-max-pending` | `1000` | Queued request descriptor limit |
| `-scenario` | `lifecycle` | Scenario from the table above |
| `-resolve-after` | `10m` | Synthetic `endsAt - startsAt`; zero allowed |
| `-seed` | `1` | Deterministic identity seed |
| `-base-time` | `2026-01-01T00:00:00Z` | RFC3339Nano synthetic base time |
| `-run-id` | generated | Fingerprint and alert-label namespace |
| `-output` | empty | Optional JSON report (schema version 1) |
| `-max-response-bytes` | `4096` | Response prefix read limit; bodies discarded, not saved in reports |
| `-drain-timeout` | `10s` | Per-phase grace before cancellation/abandonment |
| `-allow-remote` | `false` | Required for non-loopback destinations |
| `-max-failure-rate` | `0` | Allowed failed/sent fraction in `[0,1]` |
| `-fail-on-drop` | `true` | Fail on generator queue overflow or missed deadlines |

The response reader may consume one byte beyond `-max-response-bytes` to
detect truncation. The tool does not retain response bodies in reports.

### Network and data safety

Remote destinations require `-allow-remote` and trigger a warning;
`localhost` and loopback IP literals are allowed by default. Redirects
are never followed. URLs containing credentials, query strings or
fragments are rejected. Reports include only endpoint scheme and host,
not path, response bodies or raw transport error text, to reduce
accidental secret exposure. Custom authentication headers and proxy
configuration are not implemented; use a dedicated, directly reachable
test endpoint.

## Scheduling and resource limits

The scheduler uses absolute monotonic deadlines; it does not block
waiting for queue capacity. Saturation increments
`not_scheduled_queue_full`; requests not offered before a phase deadline
increment `not_scheduled_deadline`. Workers construct and marshal
payloads only after dequeuing small descriptors. No unbounded payload
pool or raw latency sample list is retained.

The maximum plan is **1,000,000 requests per phase**
(`ceil(rate × duration)`). At that limit, basic scenario planning uses
about 250 KB; mixed/stale/reopen/supersede planning remains below about
4.5 MB. Fixed logarithmic histograms hold latency and payload-size
measurements.

Other limits:

| Resource | Limit |
|---|---|
| Rate | Up to 1,000,000 HTTP requests/second |
| Scheduling window, request timeout, drain grace | At most 24 hours each |
| Batch size | 1–10,000 alerts |
| Workers | 1–1,024 |
| Workers × batch size | At most 1,000,000 |
| Pending descriptors | At most 1,000,000 |
| Response prefix | At most 1 MiB |

These are bounds, **not** throughput guarantees. Payload memory can
still grow with workers × batch size.

At phase end, the producer stops, drains for the grace period, then
cancels active requests and abandons unsent descriptors. Drain timeout
fails the run and stops subsequent phases. SIGINT/SIGTERM stops future
scheduling, permits the same bounded drain and marks the report
interrupted. No retries are performed.

| Exit code | Meaning |
|---|---|
| `0` | Success |
| `1` | Workload, output or requested verification failure |
| `2` | Invalid configuration |
| `130` | Interrupted |

## Interpreting results

The terminal summary and optional JSON report distinguish:

- **Planned/offered/scheduled/sent/completed/successful:** request and
  alert counts and rates, generator drops and abandonment. Offered
  rates use scheduling-window time; achieved rates use phase/total
  wall time including drains. Later phases can have fewer eligible
  requests after earlier failures.
- **HTTP and transport:** response classes, timeouts, network errors,
  cancellations, unknown/rejected alerts and peak in-flight requests,
  separated by phase.
- **Timing and sizes:** HTTP latency p50/p90/p95/p99/max, queue wait,
  payload construction, scheduling lag and payload size. Quantiles
  are logarithmic-histogram estimates (approximately 4.43% bin
  width), not exact sample percentiles. Times are seconds; sizes
  are bytes.
- **Lifecycle expectations:** planned occurrences, submitted and
  HTTP-accepted initial occurrences, eligible/accepted expected
  outcomes and matching closure candidates. `alerts_sent` counts
  alerts in attempted HTTP calls even when transport delivery fails;
  `completed_requests` counts received HTTP responses, including
  responses whose body reading subsequently fails.
- **Duration histograms:** conditional expected cumulative counts,
  with duplicate and orphan candidates separate. Repeated
  resolutions and orphans do not add duration observations.

Expected outcomes assume **clean isolated state and successful receiver
state operations**. They are not verified lifecycle decisions.
Even HTTP 2xx does not prove OTLP export or log-backend persistence.
A non-2xx or timeout may occur after a state mutation; such ambiguous
deliveries are not repaired or replayed.

For manual receiver-side comparisons, isolate the receiver/Valkey DB
or take counter snapshots without concurrent traffic. Run-specific
alert labels do **not** become labels on aggregate receiver metrics:

```promql
sum(increase(alertreceiver_webhook_requests_total[10m]))
sum by (outcome) (increase(alertreceiver_processing_outcomes_total[10m]))
sum(increase(alertreceiver_alert_duration_seconds_count[10m]))
sum by (le) (increase(alertreceiver_alert_duration_seconds_bucket[10m]))
```

Allow for scrape timing, resets and counter extrapolation. No
verification requests occur by default. The tool does not exercise
Alertmanager's API or real-time lifecycle sleeps.

## Weighted mixed workloads

Example with seven weighted profiles:

```bash
go run ./cmd/alertload \
  -scenario mixed \
  -mix 'firing=5,lifecycle=50,duplicates=20,orphans=5,stale=5,reopen=10,supersede=5' \
  -rate 50 -duration 2s -batch-size 10 -workers 10 \
  -resolve-after 5m \
  -base-time "$(date -u -d '30 minutes ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -output /tmp/alertload-mixed-report.json
```

Weights describe **initial fingerprint/batch cohorts**, not
percentages of HTTP requests, alert statuses or valid closures.
The root cohort count is `ceil(rate × duration)`. Largest-remainder
allocation with stable scenario-name tie-breaking determines cohort
counts; a seeded shuffle assigns them to batch indices. Small runs
may allocate zero cohorts to low-weight profiles. The default mix
is `firing=1,lifecycle=1,duplicates=1,orphans=1`.

A firing cohort sends one request, lifecycle two, and
duplicates/stale/reopen/supersede four. Thus request proportions
differ from input weights. Reports contain exact allocations and
per-round/profile traffic.

Up to four rounds send at most one step per cohort, at the shared
request rate for that round. Homogeneous batches can interleave
statuses. Each round drains before dependent work starts. Sparse
nonempty rounds keep their scheduling windows; empty rounds are
skipped.

### Dependent phases and generations

The expected transition outcomes are:

| Profile | Expected outcomes |
|---|---|
| `stale` | `opened`, `stale_firing`, `stale_resolved`, `closed` |
| `reopen` | `opened`, `closed`, `reopened`, `closed` |
| `supersede` | `opened`, `superseded_firing`, `stale_resolved`, `closed` |

In `stale`, generation 1 fires first; late generation 0 notifications
are ignored. In `reopen`, both matching closures contribute duration
observations. In `supersede`, no matching resolution is synthesized
for the superseded generation; its late resolution is stale.

Generation 1 retains fingerprint/labels and uses
`startsAt = generation 0 startsAt + resolve-after + 1s`.
Only `startsAt`/`endsAt` differ. For mixed and standalone
stale/reopen/supersede scenarios, **every preceding cohort step must
return 2xx** before a dependent step is sent, including repeated
and stale steps. Failures reduce later coverage. Standalone lifecycle
and duplicates use the simpler eligibility rules above.

**Timestamp warning:** the final resolved record in reopen or
supersede may be at `base-time + 2 × resolve-after + 1s`, plus
per-alert nanosecond offsets. Stale also uses the newer generation.
Keep the entire synthetic time range inside retention/query bounds
and out of the future. No wall-clock conversion is performed.

### JSON and CSV scenario files

The repository includes [JSON](examples/alertload-mixed.json) and
[CSV](examples/alertload-mixed.csv) examples:

```bash
go run ./cmd/alertload \
  -scenario-file docs/examples/alertload-mixed.json \
  -rate 50 -duration 2s -batch-size 10 \
  -base-time "$(date -u -d '3 hours ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -output /tmp/alertload-file-report.json
```

A scenario file implies `mixed` unless an explicit scenario is given;
an explicit non-mixed scenario is rejected. Do not combine
`-scenario-file` and explicit `-mix`.

JSON requires an object containing `profiles`; each profile has
`scenario`, positive integer `weight` and optional duration-string
`resolve_after`. CSV requires exactly
`scenario,weight,resolve_after`; an empty duration inherits
`-resolve-after`.

Files are limited to 1 MiB, may contain 1–7 unique supported profiles
and weights from 1 to 1,000,000. Unknown JSON fields are rejected.
Validation happens **before** traffic. Equivalent CSV/JSON profiles
produce the same plan regardless of input row order.

Per-profile durations affect expected counts, sums and cumulative
histogram buckets, including zero and values above seven days.
`occurrences_planned` counts initial cohorts;
`occurrence_generations_planned` counts distinct
fingerprint/`startsAt` generations, including deliberately stale ones.
JSON schema version 1 includes `workloads`, per-phase `profiles`,
expected outcomes and requested verification results.

## Optional receiver metric comparison

```bash
go run ./cmd/alertload \
  -scenario lifecycle -rate 10 -duration 2s -resolve-after 5m \
  -base-time "$(date -u -d '15 minutes ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -metrics-url http://127.0.0.1:9011/metrics \
  -comparison-isolated -settle-time 2s -check-timeout 5s \
  -output /tmp/alertload-verified.json
```

`-metrics-url` points to the receiver's **Prometheus text exposition**
endpoint, **not** the Prometheus query API. `-comparison-isolated` is
mandatory because aggregate metrics have no run-ID label. You must
provide an otherwise idle receiver with fresh identities/state;
the flag acknowledges isolation but cannot enforce it. Baseline
scrape failure stops the run before load.

The tool scrapes before traffic and after traffic/drain/settle delay,
then compares counter deltas against conditional expectations for
HTTP response classes, lifecycle outcomes, generated events, SDK
submissions and duration count/sum/buckets. Bucket labels such as
`300` and `300.0` are normalized. Valkey, synchronous emission and
OTLP export errors are measured but not automatically required to
be zero or treated as confirmed permanent loss.

| Result | Interpretation |
|---|---|
| `matched` | Observed counter deltas match conditional expectations |
| `mismatch` | Observed and expected values differ |
| `inconclusive` | Workload failures/drops/abandonment/interruption or invalid comparison prevent a reliable conclusion |
| `error` | Verification itself failed |

Counter decreases, disappearing nonzero series or changed exposed
process-start time invalidate comparison. Resets hidden by a missing
process-start metric and later growth cannot be reliably detected.
Unexpected concurrent traffic may cause mismatches. A match proves
neither OTLP export nor backend persistence.

## Optional VictoriaLogs visibility check

```bash
go run ./cmd/alertload \
  -scenario lifecycle -rate 10 -duration 2s -resolve-after 5m \
  -base-time "$(date -u -d '15 minutes ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -victorialogs-url http://127.0.0.1:9428/select/logsql/query \
  -settle-time 3s -check-timeout 5s \
  -output /tmp/alertload-victorialogs.json
```

The tool sends one read-only POST query, scoped by the exact run label
and event kind:

```logsql
event.kind:="alert_transition" alert.label.load_run:="<run-id>"
| stats by (alert.transition) count() total
```

The JSON-lines response is compared with expected firing and resolved
record counts (opened/reopened/superseded firing, and closed/orphan
resolutions). There is no time filter: the fresh run ID scopes retained
synthetic timestamps.

This is an **aggregate query-time visibility check**, not per-event
ID validation, a durability test or an exactly-once guarantee.
Reused IDs, expired/future timestamps, exporter failures, ingestion
delay or wrong tenancy may cause mismatches. The check uses the
default tenant; custom auth/tenant headers are unsupported. Ambiguous
workload failures make this check inconclusive rather than asserting
an exact count. See the
[VictoriaLogs query API](https://docs.victoriametrics.com/victorialogs/querying/).

### Shared verification behavior

Both checks are opt-in and can be combined. Their destinations
follow the same `-allow-remote`, URL and redirect rules as the
load endpoint. Server response/error text is excluded from reports.
Responses are limited to 4 MiB, metric lines to 64 KiB and selected
series to 4,096; only known receiver metrics and bounded label
values are retained.

`-settle-time` defaults to `1s` (allowed `0..1m`), applied once
before final verification. `-check-timeout` defaults to `5s`
(up to `1m` per HTTP check). There is no polling, retry, replay
or guaranteed wait for eventual export.

A requested check reporting mismatch, error or inconclusive causes
a nonzero exit. Workload wall time/rate denominators exclude
verification; `verification_seconds` records that additional time.
