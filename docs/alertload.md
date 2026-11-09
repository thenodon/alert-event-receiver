# Load testing with `alertload` (Phase 1)

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
  -seed 42 -base-time 2026-11-09T10:00:00Z \
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
random timing or a random duration distribution in Phase 1.

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
| `-scenario` | `lifecycle` | Phase 1 scenario from the table above |
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
Custom authentication headers and proxy configuration are not implemented in
Phase 1; use a dedicated directly reachable test endpoint.

## Scheduling, bounds and report interpretation

The scheduler uses absolute monotonic deadlines and never waits for a free queue
slot. When saturated it increments `not_scheduled_queue_full`; requests that
cannot be offered before the phase deadline increment `not_scheduled_deadline`.
Workers construct and marshal payloads only after dequeueing a small request
descriptor. The tool retains no unbounded payload pool or latency sample list.

The plan is limited to **1,000,000 requests per phase** (`ceil(rate × duration)`).
Two compact eligibility bitsets retain successful batch indices, bounded to about
250 KB in total at the limit. Latency/size recorders use fixed logarithmic
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
The tool does not scrape Prometheus automatically. Weighted `mixed` workloads,
stale/reopen/supersede profiles, scenario files, real Alertmanager API traffic,
real-time lifecycle sleeps and backend persistence checks are later phases.
