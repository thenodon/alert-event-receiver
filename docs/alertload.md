# Load testing with `alertload` (Phases 1 and 2)

`alertload` is an independent, standard-library HTTP client for generating
Alertmanager-compatible webhook traffic. It does not import receiver processor
or state packages, connect directly to Valkey, or change receiver behavior.
**Requests sent to a receiver do change that receiver's state and may produce
real downstream events. Use a dedicated test receiver and Valkey instance/DB.**
Run IDs isolate alert identities; they do not isolate aggregate Prometheus
metrics or prevent writes to production state.

```bash
go build -o /tmp/alertload ./cmd/alertload
/tmp/alertload \
  -url http://127.0.0.1:9011/webhook \
  -scenario lifecycle \
  -rate 100 -duration 10s -batch-size 10 -workers 20 \
  -resolve-after 1h \
  -seed 42 \
  -base-time "$(date -u -d '65 minutes ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -run-id bench-20261109-a \
  -output /tmp/alertload-report.json
```

`-rate` always means **HTTP requests/second**, not alerts/second. The example
nominally offers 100 requests/second × 10 alerts/request = 1,000 notifications/
second. A lifecycle run plans 1,000 unique firing batches and up to 1,000 matching
resolved batches. Each nonempty phase has its own ten-second scheduling window,
followed by a bounded drain: total time is roughly twenty seconds plus drains,
not one ten-second window shared between both phases.

## Scenarios, ordering and synthetic timestamps

| Scenario | Completion-barrier phases |
|---|---|
| `firing` | Unique firing occurrences |
| `lifecycle` | Firing → matching resolved |
| `duplicates` | Firing → repeated firing → matching resolved → repeated resolved |
| `orphans` | Resolved occurrences with no preceding firing |
| `stale` | Newer firing → older firing → older resolved → matching newer resolved |
| `reopen` | Firing → resolved → newer firing with same fingerprint → matching newer resolved |
| `supersede` | Firing → newer firing with same fingerprint → older resolved → matching newer resolved |
| `mixed` | Deterministically interleaved weighted cohorts of any of the seven profiles |

A phase completes before the next begins, including its HTTP requests and bounded
drain. Only original firing batches with a completed HTTP 2xx result are eligible
for resolution; only successful resolution batches are eligible for repeated
resolution. Repeated-firing failures do not remove eligibility already established
by the original firing. A failed or timed-out firing is **unknown** if it may
have mutated state; it is never automatically retried or assumed safe to resolve.
HTTP 4xx is reported separately as rejection. Entire batches share a consistent
alert status and top-level status.

`-resolve-after` is synthetic `endsAt - startsAt`, **not a sleep** between firing
and resolution. Occurrence timestamps start at `-base-time` and advance by one
nanosecond per alert index. Fingerprints are deterministic hashes of run ID,
seed, and occurrence index. Repeated transitions reuse the same labels,
fingerprint and startsAt. The seed namespaces identities; it does not introduce
random request timing. Mixed-cohort assignment is shuffled with the seed, and
scenario files can choose a different fixed synthetic duration for each profile.

The default base time is fixed at `2026-01-01T00:00:00Z`, independent of the wall
clock. The same seed, base time, run ID and batch size generate identical planned
payloads. Actual timing, queue saturation and failures can change which requests
are sent. Reuse those flags for reproducibility **only against fresh test state**;
reusing an identity against existing state can cause repeats/stale notifications.
By default, each CLI invocation generates a fresh cryptographically random run ID.
Supplied IDs must be 1–64 ASCII letters/digits/`._-`, starting with a letter or digit.

For duration boundary tests, run isolated lifecycles with `-resolve-after 0s`,
`299s`, `300s`, `301s`, `3600s`, `86400s`, `604800s`, or `604801s`. These test zero,
finite histogram boundaries, and the greater-than-seven-day `+Inf` bucket without
waiting those durations. Use a base time appropriate for the destination's log
retention window: synthetic timestamps can fall in the past or future.

## Flags and safety

| Flag | Default | Meaning |
|---|---|---|
| `-url` | `http://127.0.0.1:9011/webhook` | Direct webhook destination |
| `-rate` | `10` | Target requests/second per phase |
| `-duration` | `1m` | Scheduling window per nonempty phase, excluding drain |
| `-batch-size` | `1` | Alerts per request |
| `-workers` | `10` | Fixed maximum simultaneous requests |
| `-timeout` | `10s` | Timeout per HTTP request, including response reading |
| `-max-pending` | `1000` | Maximum queued request descriptors |
| `-scenario` | `lifecycle` | Scenario from the table above |
| `-resolve-after` | `10m` | Synthetic resolution duration; zero is valid |
| `-seed` | `1` | Deterministic identity seed |
| `-base-time` | `2026-01-01T00:00:00Z` | RFC3339Nano synthetic timestamp base |
| `-run-id` | generated | Safe namespace for fingerprints and alert labels |
| `-output` | empty | Optional JSON report file, schema version 1 |
| `-max-response-bytes` | `4096` | Response prefix read limit (one extra byte may be read); bodies are discarded, never retained in reports |
| `-drain-timeout` | `10s` | Grace per phase before canceling active and abandoning queued requests |
| `-allow-remote` | `false` | Required for a non-loopback destination; prints a prominent warning |
| `-max-failure-rate` | `0` | Allowed failed/sent request fraction, from 0 to 1; equality is allowed |
| `-fail-on-drop` | `true` | Treat queue overflow or missed scheduling deadlines as run failure |

Remote destinations require explicit opt-in; loopback IP literals and `localhost`
are allowed by default. Redirects are never followed. URL credentials, query
strings and fragments are rejected. Reports include only the endpoint's scheme
and host, excluding its path. Response bodies and raw transport error text are
not reported, avoiding accidental disclosure of tokens echoed by a server.
Custom authentication headers and proxy configuration are not implemented; use a dedicated directly reachable test endpoint.

## Scheduling, bounds and report interpretation

The scheduler uses absolute monotonic deadlines and never waits for a free queue
slot. When saturated it increments `not_scheduled_queue_full`; requests that
cannot be offered before the phase deadline increment `not_scheduled_deadline`.
Workers construct and marshal payloads only after dequeueing a small request
descriptor. The tool retains no unbounded payload pool or latency sample list.

The plan is limited to **1,000,000 requests per phase** (`ceil(rate × duration)`).
Phase 1 uses two compact eligibility bitsets (about 250 KB at the limit).
Phase 2 additionally uses a one-byte-per-cohort assignment and at most 28
success bitsets, about 4.5 MB of planning state at the limit. Latency/size recorders use fixed logarithmic
histograms. Rate is limited to 1,000,000 requests/second; scheduling windows,
request timeouts and drain graces are at most 24 hours; batch size is 1–10,000,
workers 1–1,024, workers × batch size at most 1,000,000, pending descriptors at
most 1,000,000, and response prefixes at most 1 MiB. These are resource limits,
not throughput guarantees. Payload memory still grows with workers × batch size.

At each phase's end the producer stops, drains for the grace period, then cancels
requests and abandons unsent descriptors. A drain timeout fails the run and stops
further phases. SIGINT/SIGTERM stops scheduling and later phases, allows the same
bounded drain, marks the report interrupted, and returns exit code 130. Normal
success returns 0, workload/output failure returns 1, and invalid configuration
returns 2. The tool adds no retries.

The terminal summary and optional JSON report include:

- Target, offered, scheduled, sent, completed and successful request counts/rates,
  alert counts/rates, and generator drops/abandonment. Offered rates use measured
  scheduling-window time; achieved sent/completed rates use phase/total wall time
  including drain. Later phases have fewer eligible requests after earlier
  failures, so their actual offered rate can be below the nominal target.
- HTTP classes, timeouts, network errors, cancellations, unknown/rejected alerts,
  and peak in-flight concurrency, separately for each scenario phase.
- HTTP latency p50/p90/p95/p99/max, queue wait, construction time, scheduling lag,
  and payload-size distributions. Quantiles are bounded-histogram estimates
  (about 4.43% bin width), not exact raw-sample percentiles. All reported time
  distributions are in seconds; payload sizes are bytes.
- Planned occurrences, submitted initial occurrences, HTTP-accepted initial
  occurrences, and eligible/accepted expected outcome counts. `alerts_sent`
  counts alerts in attempted HTTP calls, even when transport failure prevents
  delivery. `completed_requests` counts received HTTP responses; reading a
  response can still fail, in which case it is not considered successful.
- Matching closure candidates and their conditional expected cumulative duration
  histogram, with duplicate and orphan candidates listed separately. Repeated
  resolutions and orphans never add expected duration observations.

Expected outcomes are **conditional on isolated clean state and successful
receiver state operations**, not verified lifecycle decisions. Even HTTP 2xx
cannot prove OTLP export or backend persistence. A non-2xx or timeout can follow
state mutation; such uncertain deliveries are not repaired or replayed. Rerunning
the generator against existing state can change receiver outcomes.

For receiver-side comparison, use a dedicated receiver/Valkey DB or baseline/end
counter snapshots without other traffic. Run-specific alert labels do **not**
become labels on the receiver's aggregate Prometheus metrics. For example:

```promql
sum(increase(alertreceiver_webhook_requests_total[10m]))
sum by (outcome) (increase(alertreceiver_processing_outcomes_total[10m]))
sum(increase(alertreceiver_alert_duration_seconds_count[10m]))
sum by (le) (increase(alertreceiver_alert_duration_seconds_bucket[10m]))
```

Account for scrape timing, resets and extrapolation when comparing counters.
By default, the tool makes no verification requests; the optional Phase 2 checks
below require explicit flags. Real Alertmanager API traffic and real-time
lifecycle sleeps remain out of scope.


## Phase 2: weighted profiles and scenario files

```bash
go run ./cmd/alertload \
  -scenario mixed \
  -mix 'firing=5,lifecycle=50,duplicates=20,orphans=5,stale=5,reopen=10,supersede=5' \
  -rate 50 -duration 2s -batch-size 10 -workers 10 \
  -resolve-after 5m \
  -base-time "$(date -u -d '30 minutes ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -output /tmp/alertload-mixed-report.json
```

Weights apply to **initial fingerprint/batch cohorts**, not percentages of HTTP
requests, alert status, or valid closures. The root cohort count remains
`ceil(rate × duration)`. Counts are apportioned by largest remainder with stable
scenario-name tie-breaking, then assigned to batch indices by a seeded shuffle.
Small runs can allocate zero cohorts to low-weight profiles. The default mix is
`firing=1,lifecycle=1,duplicates=1,orphans=1`.

Profiles with more transitions produce more traffic: a firing cohort produces
one request, a lifecycle cohort two, and a duplicates/stale/reopen/supersede cohort
four. Reports include weights, exact planned cohort allocations, and actual
per-round/profile traffic rather than claiming that request proportions equal
input weights. At most one step per cohort is sent in each of four rounds, using
the existing total request rate for that round, not a separate rate per profile.
Homogeneous request batches can be interleaved with other statuses in that round.
A round completes and drains before any dependent step is scheduled. Sparse later
rounds retain their scheduling window; completely empty rounds are skipped.

The new profiles deliberately exercise these expected outcomes:

- `stale`: opened, stale_firing, stale_resolved, closed. Its initial firing belongs
  to generation 1; generation 0 notifications arrive later and must be ignored.
- `reopen`: opened, closed, reopened, closed. Both successful matching closures
  contribute expected duration observations.
- `supersede`: opened, superseded_firing, stale_resolved, closed. No resolution is
  synthesized for the superseded generation, and its late resolve is stale.

Generation 1 retains the same fingerprint/labels and has startsAt equal to
generation 0 startsAt plus its synthetic resolution duration plus one second.
Only startsAt/endsAt change between generations. Every dependent Phase 2 step is
conservatively gated on **all preceding steps in that profile returning 2xx**.
This includes repeated/stale steps: a failure reduces later coverage instead of
making an authoritative assumption about the receiver. Existing standalone Phase
1 profiles keep their original gating behavior. No failed request is retried.

Use `-scenario-file` to load weighted profiles from
[JSON](examples/alertload-mixed.json) or [CSV](examples/alertload-mixed.csv):

```bash
go run ./cmd/alertload \
  -scenario-file docs/examples/alertload-mixed.json \
  -rate 50 -duration 2s -batch-size 10 \
  -base-time "$(date -u -d '3 hours ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -output /tmp/alertload-file-report.json
```

A scenario file implies `-scenario mixed` unless an explicit scenario was supplied;
other explicit scenarios are rejected. Files and an explicit `-mix` cannot be
combined. JSON accepts a single object with a `profiles` array; each profile has
`scenario`, positive integer `weight`, and optional duration-string `resolve_after`.
CSV requires the exact header `scenario,weight,resolve_after`; leave the duration
cell empty to inherit `-resolve-after`. There must be one to seven unique supported
profiles, weights are 1..1,000,000, unknown JSON fields are rejected, and files are
limited to 1 MiB. Input is validated before network traffic. Equivalent CSV/JSON
profiles produce the same plan independently of input row order.

Per-profile durations are reflected in the report's expected count, sum, and
cumulative histogram buckets, including zero and greater-than-seven-day values.
The existing `occurrences_planned` field counts initial fingerprint cohorts;
`occurrence_generations_planned` counts distinct fingerprint/startsAt generations
that appear in the full plan (including deliberately stale generations).
`workloads`, per-phase `profiles`, expected outcomes and verification results are
additive JSON schema version 1 fields.

**Timestamp warning:** a reopen/supersede profile may place its final resolved
record at `base-time + 2 × resolve-after + 1s`, plus the tiny per-alert nanosecond
offset. Stale profiles also use this newer generation. Choose a base time that
keeps all desired records within the backend's retention/query range and outside
the future. No conversion to wall-clock event time is performed.

## Optional receiver metric comparison

```bash
go run ./cmd/alertload \
  -scenario lifecycle -rate 10 -duration 2s -resolve-after 5m \
  -base-time "$(date -u -d '15 minutes ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -metrics-url http://127.0.0.1:9011/metrics \
  -comparison-isolated -settle-time 2s -check-timeout 5s \
  -output /tmp/alertload-verified.json
```

`-metrics-url` is a direct receiver Prometheus **text exposition** endpoint, not
a Prometheus query API. `-comparison-isolated` is mandatory: metrics have no run-ID
labels, so the operator must provide a dedicated receiver with no concurrent
traffic and fresh identities/state. This flag acknowledges isolation; the tool
cannot enforce it. Baseline failure stops the run before sending load.

The tool scrapes once before traffic and once after traffic/drain and the optional
settle delay. It computes counter deltas and checks HTTP 2xx/4xx/5xx counts,
lifecycle outcome counts, generated events, SDK submissions, and matching duration
count/sum/buckets against conditional expectations. Bucket label numbers such as
`300` and `300.0` are normalized. Valkey, synchronous emission, and OTLP export
error counters are also reported as measurements; they are not equated with
permanent loss or backend delivery and are not automatically required to be zero.

Comparisons report `matched`, `mismatch`, `inconclusive`, or `error`. Failed,
dropped, abandoned or interrupted traffic makes correctness comparison
inconclusive, even if the configured HTTP failure/drop policy tolerates it.
Counter decreases, disappearing nonzero series, or changed exposed process-start
time also invalidate comparison. Resets that are hidden by a missing process-start
metric and later counter growth cannot be reliably detected. Unexpected concurrent
traffic may appear as a mismatch. A matching comparison is evidence for observed
counter deltas, not proof of OTLP export or persistence.

## Optional VictoriaLogs visibility check

```bash
go run ./cmd/alertload \
  -scenario lifecycle -rate 10 -duration 2s -resolve-after 5m \
  -base-time "$(date -u -d '15 minutes ago' +%Y-%m-%dT%H:%M:%SZ)" \
  -victorialogs-url http://127.0.0.1:9428/select/logsql/query \
  -settle-time 3s -check-timeout 5s \
  -output /tmp/alertload-victorialogs.json
```

The tool makes one read-only POST query using the exact run label and event kind:

```logsql
event.kind:="alert_transition" alert.label.load_run:="<run-id>"
| stats by (alert.transition) count() total
```

The JSON-lines response is compared against expected firing and resolved record
counts (opened/reopened/superseded firing events, and closed/orphan resolutions).
No time filter is imposed: the fresh run ID scopes the query across retained
synthetic event times. This is an **aggregate query-time visibility check**, not
per-event ID validation, a durability test, or an exactly-once guarantee. Reused
run IDs, expired/future timestamps, exporter failure, ingestion delay or a wrong
tenant can produce a mismatch. The check uses the default tenant; custom auth or
tenant headers are not supported. Ambiguous workload failures skip this query
and report inconclusive rather than asserting an exact expected backend count.
See the [VictoriaLogs query API](https://docs.victoriametrics.com/victorialogs/querying/).

Both checks are opt-in and can be combined. All verification destinations obey
`-allow-remote`, reject URL credentials/query strings/fragments, and refuse
redirects just like the load endpoint. Server response/error text is not embedded
in reports. Responses are limited to 4 MiB, metric lines to 64 KiB and selected
series to 4,096. Only known receiver metrics and bounded label values are retained.

`-settle-time` defaults to 1s and permits 0..1m; it is applied once before final
verification. `-check-timeout` defaults to 5s and permits up to 1m per HTTP check.
There is no polling, automatic retry, replay, or guaranteed wait for eventual
export. A mismatch/error/inconclusive requested check makes the command exit
nonzero. Workload wall time and rate denominators exclude verification;
`verification_seconds` reports the additional elapsed verification time.
