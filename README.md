# alert-event-receiver

An Alertmanager webhook receiver that converts alert lifecycle transitions into structured OpenTelemetry log events. It uses Redis/Valkey to track the latest alert occurrence and suppress duplicate lifecycle event emissions.

## Why?

Alertmanager is designed to repeat notifications until an alert is resolved or otherwise handled. This is useful for urgent incidents, but not every alert requires immediate human action. **alert-event-receiver** preserves a searchable history of firing and resolved transitions without requiring every alert to become a ticket or a page.

Events can be sent through an OpenTelemetry Collector or directly to an OTLP-compatible backend, such as VictoriaLogs.

> **Delivery limitation:** This initial implementation uses best-effort OTLP event delivery. State is written before emission; a crash or export failure can permanently lose a lifecycle event. Concurrent processing of the same fingerprint can also race. A durable outbox is planned; do not treat this release as a guaranteed-delivery audit log.

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

Requirements: Go 1.23+, Redis or Valkey, and an OTLP-compatible log receiver.

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

Idempotency checks suppress **event emission**, not lifecycle processing. Required Redis/Valkey read/write failures return processing errors rather than authoritative state decisions. If any alert in a webhook batch fails processing, the handler returns HTTP 500 after attempting the remaining alerts; Alertmanager may retry the entire batch. Malformed JSON returns HTTP 400.

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
| `alertreceiver_events_emitted_total` | Events accepted by the OTLP emitter, by transition |
| `alertreceiver_emit_errors_total` | Emitter failures |
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

## Development and releases

```bash
go test ./...
go test -race ./...
go vet ./...
go build -o alert_event_receiver ./cmd/server
./alert_event_receiver -version
```

The repository uses Go 1.23. GitHub Releases provide versioned builds; the `-version` flag prints the version injected at build time (or `unknown` when not set).

## Current limitations and roadmap

This version intentionally does not implement a durable outbox, guaranteed OTLP delivery, atomic read/compare/write processing across concurrent requests, per-destination incident delivery, SLA/calendar policies, or NetBox enrichment. Only the latest occurrence is kept in Redis/Valkey; after tombstone expiry or state loss, older occurrence ordering cannot be reconstructed from Redis.

The next planned step is a **Valkey-backed durable outbox**, coordinating lifecycle state transitions with queued events and adding retry/recovery behavior. Longer-term plans include enrichment, policy evaluation, and independent incident/notification sinks.

## Repository layout

```text
cmd/server/          Application entry point
internal/config/     Environment configuration
internal/models/     Alertmanager and lifecycle event models
internal/processor/  Occurrence-aware lifecycle processing
internal/state/      Redis/Valkey store
internal/telemetry/  OTLP export, Prometheus metrics, JSON logging
internal/webhook/    Alertmanager webhook HTTP handler
docs/redis-debugging.md  Redis/Valkey operational reference
```
