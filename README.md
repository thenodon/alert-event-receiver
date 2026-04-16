alert-event-receiver
--------------------
# Overview
An Alertmanager webhook receiver that turns alert lifecycle transitions into immutable
OpenTelemetry log events, with Redis/Valkey for deduplication and state tracking.

# Why this receiver?

Many teams struggle with alert fatigue:

- alerts are not always treated as urgent incidents
- response times are slow or inconsistent
- people compensate by increasing `repeat_interval`, silencing alerts, or ignoring them

Over time this can reduce trust in alerting and create noisy monitoring setups.

Alertmanager is excellent at reliable notification delivery and is designed around a simple assumption: if nobody acted, keep reminding.
That behavior is exactly right for urgent incidents, but it can be noisy for signals that still matter without requiring immediate action.

This receiver keeps Alertmanager as the notification engine while turning alert lifecycle transitions into immutable OpenTelemetry log events.
Those events can be stored in log backends such as VictoriaLogs, Parseable, or Loki and queried later with rich label-based filters.

That makes it easier to keep truly urgent alerts for human response while still retaining searchable history for lower-priority signals,
review, reporting, and work planning.

---

# How it works

```
Alertmanager (send_resolved: true)
      │
      │  POST /webhook
      ▼
alert-event-receiver
  ├─ reads alert.Fingerprint as fingerprint identity key
  ├─ checks Redis / Valkey for state
  ├─ emits OTLP log record (firing | resolved)
  └─ updates state in Redis / Valkey
      │
      │  OTLP/gRPC or OTLP/HTTP
      ▼
OTel Collector (optional)  →  VictoriaLogs / Loki / Grafana Cloud / …
```

---

# Configuration

All configuration is via environment variables.

## HTTP

| Variable | Default | Description |
|---|---|---|
| `ADDRESS` | `:9011` | HTTP listen address (for example `:9011` or `127.0.0.1:9011`) |

## Redis / Valkey

| Variable | Default | Description |
|---|---|---|
| `REDIS_ADDR` | `localhost:6379` | Redis/Valkey address |
| `REDIS_PASSWORD` | _(empty)_ | Password |
| `REDIS_DB` | `0` | Database number |
| `CLOSED_STATE_TTL` | `24h` | TTL for closed-state tombstones (prevents duplicate resolved events) |
| `IDEMPOTENCY_TTL` | `168h` (7d) | TTL for idempotency keys |

## OpenTelemetry

The OTel SDK reads its standard environment variables automatically.
You only need to set the relevant ones for your deployment:

| Variable | Default | Description |
|---|---|---|
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | `grpc` or `http/protobuf` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://localhost:4317` | Collector or backend endpoint |
| `OTEL_EXPORTER_OTLP_HEADERS` | _(empty)_ | Auth headers (e.g. `Authorization=Bearer ...`) |
| `OTEL_SERVICE_NAME` | `alert-event-receiver` | Service name on emitted records |
| `OTEL_RESOURCE_ATTRIBUTES` | _(empty)_ | Extra resource attributes (e.g. `cluster=prod-eu1,tenant=platform`) |

---

# Redis / Valkey storage model

The receiver stores only **working state** in Redis/Valkey.
It does **not** store the full alert history there.
Full lifecycle history is emitted as OTLP log records.

There are two key families:

## Alert state hash

One hash per alert fingerprint:

```text
alertstate:{fingerprint}
```

Example:

```text
alertstate:4a4f0d2b7c9e1a23
```

Fields currently stored:

- `status` → `firing` or `closed`
- `first_firing_at` → first known firing timestamp for the current lifecycle
- `last_seen_at` → last time a firing notification was seen
- `starts_at` → Alertmanager `startsAt` value
- `alertname` → copied from alert labels
- `label.*` → every alert label is stored with a `label.` prefix

Example hash content:

```text
status=firing
first_firing_at=2026-04-15T09:22:58Z
last_seen_at=2026-04-15T09:23:14Z
starts_at=2026-04-15T09:22:58Z
alertname=HighErrorRate
label.severity=warning
label.service=checkout
label.instance=checkout-7d8f9
```

Behavior:

- When an alert is `firing`, the hash is written/updated and made persistent.
- When an alert is `resolved`, the same hash is marked `status=closed` and a TTL is applied using `CLOSED_STATE_TTL`.
- That short-lived closed tombstone prevents duplicate late `resolved` deliveries from creating extra resolved events.

## Idempotency keys

One short-lived string key per transition delivery:

```text
alertidemp:{fingerprint}:{transition}:{unix_timestamp}
```

Examples:

```text
alertidemp:4a4f0d2b7c9e1a23:firing:1776244978
alertidemp:4a4f0d2b7c9e1a23:resolved:1776246061
```

Behavior:

- The key is written with Redis `SET NX`.
- If it already exists, the receiver treats the delivery as a duplicate and drops it.
- The TTL is controlled by `IDEMPOTENCY_TTL`.
- The stored value is currently just `1`; the key name carries the useful information.

## Inspecting data with `redis-cli`

Connect to Redis/Valkey:

```bash
redis-cli -h localhost -p 6379
```

If you use a password:

```bash
redis-cli -h localhost -p 6379 -a "$REDIS_PASSWORD"
```

If you use a non-default DB:

```bash
redis-cli -h localhost -p 6379 -n 2
```

### List alert state keys

```bash
redis-cli --scan --pattern 'alertstate:*'
```

### Read one alert state hash

```bash
redis-cli HGETALL 'alertstate:4a4f0d2b7c9e1a23'
```

### Read specific fields only

```bash
redis-cli HMGET 'alertstate:4a4f0d2b7c9e1a23' status first_firing_at last_seen_at starts_at alertname
```

### Check remaining TTL on a closed alert tombstone

```bash
redis-cli TTL 'alertstate:4a4f0d2b7c9e1a23'
```

Interpretation:

- `-1` → key exists and has no TTL (typically an open/firing alert)
- `-2` → key does not exist
- positive integer → seconds until the closed tombstone expires

### List idempotency keys

```bash
redis-cli --scan --pattern 'alertidemp:*'
```

### Inspect one idempotency key

```bash
redis-cli GET 'alertidemp:4a4f0d2b7c9e1a23:resolved:1776246061'
redis-cli TTL 'alertidemp:4a4f0d2b7c9e1a23:resolved:1776246061'
```

### Find all keys for one fingerprint

```bash
redis-cli --scan --pattern 'alertstate:4a4f0d2b7c9e1a23'
redis-cli --scan --pattern 'alertidemp:4a4f0d2b7c9e1a23:*'
```

### Show only stored labels for one alert

```bash
redis-cli HGETALL 'alertstate:4a4f0d2b7c9e1a23' | grep '^label\.'
```

## End-to-end example (webhook -> Redis keys)

The example below shows a minimal lifecycle for one alert fingerprint.

### Step 1: Firing webhook alert

```json
{
  "groupKey": "{}:{alertname=\"HighErrorRate\"}",
  "receiver": "event-webhook",
  "externalURL": "https://alertmanager.example",
  "alerts": [
    {
      "status": "firing",
      "fingerprint": "4a4f0d2b7c9e1a23",
      "startsAt": "2026-04-15T09:22:58Z",
      "labels": {
        "alertname": "HighErrorRate",
        "severity": "warning",
        "service": "checkout",
        "instance": "checkout-7d8f9"
      },
      "annotations": {
        "summary": "Checkout error rate is high"
      }
    }
  ]
}
```

Expected Redis writes:

- `alertstate:4a4f0d2b7c9e1a23` hash with `status=firing` and `label.*` fields
- `alertidemp:4a4f0d2b7c9e1a23:firing:1776244978` with value `1` and `IDEMPOTENCY_TTL`

Inspect with `redis-cli`:

```bash
redis-cli HGETALL 'alertstate:4a4f0d2b7c9e1a23'
redis-cli TTL 'alertstate:4a4f0d2b7c9e1a23'
redis-cli GET 'alertidemp:4a4f0d2b7c9e1a23:firing:1776244978'
redis-cli TTL 'alertidemp:4a4f0d2b7c9e1a23:firing:1776244978'
```

At this point, state key TTL should usually be `-1` (open alert, persisted).

### Step 2: Resolved webhook alert

```json
{
  "groupKey": "{}:{alertname=\"HighErrorRate\"}",
  "receiver": "event-webhook",
  "externalURL": "https://alertmanager.example",
  "alerts": [
    {
      "status": "resolved",
      "fingerprint": "4a4f0d2b7c9e1a23",
      "startsAt": "2026-04-15T09:22:58Z",
      "endsAt": "2026-04-15T09:41:01Z",
      "labels": {
        "alertname": "HighErrorRate",
        "severity": "warning",
        "service": "checkout",
        "instance": "checkout-7d8f9"
      }
    }
  ]
}
```

Expected Redis writes/updates:

- `alertstate:4a4f0d2b7c9e1a23` updated to `status=closed`
- `alertstate:*` gets a TTL from `CLOSED_STATE_TTL`
- `alertidemp:4a4f0d2b7c9e1a23:resolved:1776246061` with value `1` and `IDEMPOTENCY_TTL`

Inspect with `redis-cli`:

```bash
redis-cli HMGET 'alertstate:4a4f0d2b7c9e1a23' status first_firing_at last_seen_at starts_at alertname
redis-cli TTL 'alertstate:4a4f0d2b7c9e1a23'
redis-cli GET 'alertidemp:4a4f0d2b7c9e1a23:resolved:1776246061'
redis-cli TTL 'alertidemp:4a4f0d2b7c9e1a23:resolved:1776246061'
```

If you send the exact same transition again, the existing `alertidemp:*` key causes it to be dropped as a duplicate.

## Troubleshooting notes

- If an alert is currently open, expect `status=firing` and `TTL = -1`.
- If an alert was recently resolved, expect `status=closed` and a positive TTL.
- If a duplicate delivery was suppressed, look for a matching `alertidemp:*` key.
- If no `alertstate:*` key exists for a resolved alert, that can still be valid: the receiver emits a resolved orphan event and writes a short-lived closed tombstone.

For the higher-level lifecycle and deduplication rules, see [`docs/architecture.md`](docs/architecture.md).

---

# Running locally

## Prerequisites

- Go 1.23+
- Redis or Valkey
- An OTLP-compatible log backend (or OTel Collector)

```bash
go build -o alert_event_receiver ./cmd/server
```
## Quick start

```bash
# Start Redis
docker run -d -p 6379:6379 redis:7

# Start an OTel Collector or any OTLP-compatible backend, then:
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317
export OTEL_EXPORTER_OTLP_PROTOCOL=grpc
export OTEL_RESOURCE_ATTRIBUTES="cluster=local,tenant=dev"

go run ./cmd/server
```

## Endpoints

| Path | Method | Description |
|---|---|---|
| `/webhook` | POST | Alertmanager webhook receiver |
| `/metrics` | GET | Prometheus metrics (RED pattern) |
| `/healthz` | GET | Health check |

---

# Observability

## Prometheus metrics

| Metric | Labels | Description |
|---|---|---|
| `alertreceiver_webhook_requests_total` | `status` (2xx/4xx/5xx) | Webhook request rate |
| `alertreceiver_webhook_duration_seconds` | `status` | Webhook latency histogram |
| `alertreceiver_events_emitted_total` | `transition` (firing/resolved) | Events emitted via OTLP |
| `alertreceiver_emit_errors_total` | `transition` | OTLP emit failures |
| `alertreceiver_redis_errors_total` | `operation` | Redis operation errors |
| `alertreceiver_redis_ops_total` | `operation`, `result` | Redis operation outcomes |
| `alertreceiver_redis_op_duration_seconds` | `operation` | Redis operation latency histogram |
| `alertreceiver_redis_idemp_setnx_total` | `result` (`set`/`exists`/`error`) | Idempotency `SET NX` outcomes |
| `alertreceiver_duplicates_dropped_total` | — | Events dropped by idempotency check |
| `alertreceiver_state_writes_total` | `result` | Alert transition outcomes such as `opened`, `reopened`, `closed`, `resolved_without_open_state`, `already_firing` |
| `alertreceiver_state_entries` | `status` (`firing`/`closed`) | Current Redis-backed alert state entries maintained from write-path updates |
| `alertreceiver_idempotency_keys_created_total` | `transition` | Idempotency keys created successfully |
| `alertreceiver_resolved_orphans_total` | — | Resolved alerts seen without matching open state |
| `alertreceiver_closed_ttl_seconds` | — | TTL applied to closed alert tombstones |

## Internal logs

Structured JSON written to stdout. All errors against Redis and the OTLP backend
are logged with context (fingerprint, alertname, operation, error).

---

# Running tests

```bash
go test ./...
```

---

# Project structure

```
cmd/server/              main — wires dependencies and starts HTTP server
internal/
  config/                env var config loading
  models/                Alertmanager webhook payload + LifecycleEvent types
  processor/             alert state transition logic
  state/                 Redis/Valkey Store interface + implementation
  telemetry/
    logger.go            JSON slog logger (stdout)
    metrics.go           Prometheus RED metrics
    otel.go              OTel SDK init (log provider)
    emitter.go           OTelEmitter — maps LifecycleEvent → OTel LogRecord
  webhook/               HTTP handler for Alertmanager webhook
docs/
  architecture.md        Full design document
```

# Log storage

## VictoriaMetrics VisctoriaLogs
VictoriaMetrics Logs support direct OTLP ingestion over HTTP.
Configuration example:
```bash
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:9428/insert/opentelemetry
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
# Define the fields tha should be used as stream labels - default will be determine from the log resource attributes
export OTEL_EXPORTER_OTLP_HEADERS="VL-Stream-Fields=service.name,am_external_url"
# Alternatively use the logs-specific var (takes precedence over OTEL_EXPORTER_OTLP_HEADERS):
# export OTEL_EXPORTER_OTLP_LOGS_HEADERS="VL-Stream-Fields=alertname,severity,state_write_result,status,transition"
```
### Query examples in VictoriaLogs
VictoriaLogs queries use LogsQL.

#### All firing events for one service in the last hour
    _time:1h service:checkout transition:firing

#### All resolved events longer than 15 minutes
    transition:resolved duration_seconds:>900
#### Top flapping alerts
Assuming one event per transition:
```
_time:24h event_kind:alert_transition
| stats by (alertname, service) count() as transitions
| sort by (transitions desc)
| limit 20
```

#### Average duration by alertname
```
_time:7d transition:resolved
| stats by (alertname) avg(duration_seconds) as avg_duration, count() as total
| sort by (avg_duration desc)
```

#### Alerts resolved without ticket

If there is a ticket_id label:
    transition:resolved ticket_id:""
