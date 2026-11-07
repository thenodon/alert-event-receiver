# Redis/Valkey storage and debugging

This document describes the Redis/Valkey **working state** used by `alert-event-receiver`, how to inspect it, and how to diagnose duplicate, stale, or missing lifecycle events. Full historical event records are **not** stored in Redis/Valkey; they are exported as OTLP logs.

For setup and general architecture, see [README](../README.md).

## 1. Key families

| Key | Redis type | Lifetime | Purpose |
|---|---|---|---|
| `alertstate:{fingerprint}` | Hash | No TTL while firing; `CLOSED_STATE_TTL` when closed | Latest occurrence and its labels |
| `alertidemp:{fingerprint}:{startsAt}:{transition}` | String (`1`) | `IDEMPOTENCY_TTL` | Suppress duplicate lifecycle event emission |

The receiver uses the fingerprint supplied by Alertmanager; it does not recalculate it from labels. The occurrence is identified by `(fingerprint, startsAt)`; `startsAt` is stored and encoded in idempotency keys using UTC RFC3339Nano. The transition is `firing` or `resolved`. `endsAt` is **not** part of the idempotency key.

Examples:

```text
alertstate:4a4f0d2b7c9e1a23
alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:firing
alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:resolved
```

## 2. State hash fields

| Field | Meaning |
|---|---|
| `status` | `firing` or `closed` |
| `starts_at` | Start of the current occurrence, RFC3339Nano |
| `ends_at` | Resolution timestamp when closed; zero timestamp when firing |
| `first_firing_at` | First firing timestamp recorded for this occurrence |
| `last_seen_at` | Last observed firing notification timestamp; not refreshed by a repeated resolution |
| `alertname` | Name copied from alert labels |
| `label.*` | Alert labels, e.g. `label.severity`, `label.service`, `label.instance` |

Example firing hash (illustrative):

```text
status=firing
starts_at=2026-04-15T09:22:58Z
ends_at=0001-01-01T00:00:00Z
first_firing_at=2026-04-15T09:22:58Z
last_seen_at=2026-04-15T09:23:14Z
alertname=HighErrorRate
label.alertname=HighErrorRate
label.severity=warning
label.service=checkout
label.instance=checkout-7d8f9
```

When a new occurrence replaces an old one, the hash is fully replaced, preventing old labels or closure times from leaking into the new occurrence. A closed hash contains the corresponding `ends_at` and is assigned a TTL. A resolved notification without a matching open occurrence also writes a closed tombstone.

## 3. TTLs and their meaning

| Setting | Default | Applies to |
|---|---|---|
| `CLOSED_STATE_TTL` | `24h` | Closed `alertstate:*` hashes |
| `IDEMPOTENCY_TTL` | `168h` (7 days) | `alertidemp:*` keys |

Open state is persistent (`TTL = -1`). A closed tombstone prevents a late repeated resolution from looking like an orphan. Idempotency keys provide longer-lived protection against repeated emission. Increasing TTLs increases retention and memory use; reducing them weakens protection against delayed retries.

A repeated resolution for the same occurrence **does not refresh** its tombstone TTL. Once a tombstone expires, Redis no longer knows that occurrence was closed. A delayed resolved notification can then be classified as an orphan. A late firing notification can also be treated as new if all corresponding state has disappeared.

Redis TTL values:

- `-1`: key exists without expiration (typically a firing state hash).
- `-2`: key does not exist.
- Positive value: remaining lifetime in seconds.

**Important:** `alertreceiver_state_entries` is a write-path gauge, not a Redis key census. It can diverge after restart, TTL expiry, or multi-instance writes.

## 4. Connecting and inspecting keys

Connect to the appropriate Redis/Valkey instance and database:

```bash
redis-cli -h localhost -p 6379
redis-cli -h localhost -p 6379 -n 2
```

For authenticated deployments, prefer a secure mechanism such as `REDISCLI_AUTH` rather than putting a password directly in a command-line argument:

```bash
export REDISCLI_AUTH="$REDIS_PASSWORD"
redis-cli -h localhost -p 6379
```

Use `SCAN` for discovery rather than `KEYS` on a busy production instance:

```bash
redis-cli --scan --pattern 'alertstate:*'
redis-cli --scan --pattern 'alertidemp:*'
```

Inspect one alert:

```bash
FP=4a4f0d2b7c9e1a23
redis-cli HGETALL "alertstate:$FP"
redis-cli HMGET "alertstate:$FP" status starts_at ends_at first_firing_at last_seen_at alertname
redis-cli TTL "alertstate:$FP"
redis-cli --scan --pattern "alertidemp:$FP:*"
```

Inspect one transition key:

```bash
KEY='alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:resolved'
redis-cli GET "$KEY"
redis-cli TTL "$KEY"
```

Read label fields using a structured command rather than piping alternating `HGETALL` output into `grep`:

```bash
redis-cli --raw HSCAN "alertstate:$FP" 0 MATCH 'label.*' COUNT 100
```

`HSCAN` may require multiple cursor iterations for large hashes. For a known label:

```bash
redis-cli HGET "alertstate:$FP" label.service
```

## 5. End-to-end lifecycle walkthrough

Suppose Alertmanager sends the following firing alert:

```json
{
  "receiver": "event-webhook",
  "groupKey": "{}:{alertname=\"HighErrorRate\"}",
  "externalURL": "https://alertmanager.example",
  "alerts": [{
    "status": "firing",
    "fingerprint": "4a4f0d2b7c9e1a23",
    "startsAt": "2026-04-15T09:22:58Z",
    "labels": {
      "alertname": "HighErrorRate",
      "severity": "warning",
      "service": "checkout",
      "instance": "checkout-7d8f9"
    },
    "annotations": {"summary": "Checkout error rate is high"}
  }]
}
```

Expected working state:

```text
alertstate:4a4f0d2b7c9e1a23                 status=firing, no TTL
alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:firing   value=1, expiring
```

A repeat of that same firing occurrence updates `last_seen_at` but does not create another lifecycle event.

Later, Alertmanager sends a resolved alert with the **same fingerprint and startsAt**:

```json
{
  "receiver": "event-webhook",
  "groupKey": "{}:{alertname=\"HighErrorRate\"}",
  "externalURL": "https://alertmanager.example",
  "alerts": [{
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
  }]
}
```

Expected state:

```text
alertstate:4a4f0d2b7c9e1a23                 status=closed, ends_at=2026-04-15T09:41:01Z, TTL applied
alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:resolved  value=1, expiring
```

The emitted resolved event includes a duration of 18 minutes and 3 seconds (1083 seconds). Sending the same resolved notification again leaves the tombstone unchanged and emits nothing.

## 6. Debugging guide

### A. Alert is firing, but no lifecycle event is visible

1. Check the webhook response and structured application logs. A successful HTTP response does not guarantee OTLP backend delivery.
2. Inspect `alertstate:{fingerprint}`. If it is already `firing` with the same `starts_at`, a repeat correctly emits no new event.
3. Inspect the matching `alertidemp:*:firing` key. An existing key may suppress emission after a state transition.
4. Check `alertreceiver_emit_errors_total`, `alertreceiver_redis_errors_total`, and exporter/Collector/backend logs.
5. Remember the current **state-before-emission failure window**: state may be recorded even when the lifecycle event was never durably exported.

### B. A resolved alert appears as an orphan

An orphan resolution is expected when there is no matching open occurrence. Possible causes include:

- The receiver never saw the original firing notification.
- Redis state was cleared or evicted.
- The previous closed tombstone expired.
- A newer occurrence replaced the previous one.
- Notifications arrived out of order.

Inspect `starts_at` and `status` in the state hash and compare them with the webhook's fingerprint and `startsAt`.

### C. A notification is ignored as stale

If the incoming `startsAt` is older than the currently stored occurrence's `starts_at`, processing leaves the newer occurrence untouched. A firing notification for an already closed occurrence with the same `startsAt` is also stale. This is deliberate; inspect the webhook payload and state hash before assuming a lost update.

### D. Duplicates appear after a delay

Check both `CLOSED_STATE_TTL` and `IDEMPOTENCY_TTL`. Once the relevant keys expire, Redis cannot reconstruct older occurrence history. Longer TTLs provide more protection against late webhook retries at the cost of additional Redis memory.

### E. Redis errors cause webhook retries

Required state read/write failures return an error; the webhook handler returns HTTP 500 if any alert in the batch fails, after attempting the other alerts. Alertmanager can retry the whole batch. Verify connectivity, credentials, database number, and Redis errors in the receiver logs.

An idempotency `SET NX` failure is currently **best effort**: the receiver logs the error and attempts emission after a successful state update. This can allow duplicate emission.

### F. State-entry metrics do not match Redis

`alertreceiver_state_entries` changes on write operations and does not scan Redis. It can be wrong after process restart, tombstone expiry, or changes made by another instance. Use `SCAN` and direct Redis inspection when you need to debug the actual stored keys.

## 7. Implementation and reliability notes

- `GetState` validates stored timestamps and occurrence state. Corrupt stored data results in a processing error rather than a guessed lifecycle decision.
- `SetFiring` and `SetClosed` replace the full hash; closure also sets the TTL within a Redis transaction pipeline.
- `UpdateLastSeen` updates only the `last_seen_at` field.
- `SET NX` creates idempotency keys before attempting event emission.
- Hash replacement is atomic **within the transaction**, but the overall `GetState → compare → Set* → idempotency → emit` sequence is **not** atomic.
- Concurrent notifications for the same fingerprint can race and overwrite decisions.
- An error returned after a Redis operation does not prove the server did not execute it.
- OTLP emitter acceptance is not proof of delivery to VictoriaLogs or another backend.

A future durable outbox is intended to coordinate lifecycle state changes with pending event records and support retries/recovery. Until then, Redis is operational working state, **not** a reliable event queue or an authoritative historical record.

## 8. Related metrics

Useful metrics during investigation:

```text
alertreceiver_webhook_requests_total
alertreceiver_webhook_duration_seconds
alertreceiver_events_emitted_total
alertreceiver_emit_errors_total
alertreceiver_redis_errors_total
alertreceiver_redis_ops_total
alertreceiver_redis_op_duration_seconds
alertreceiver_redis_idemp_setnx_total
alertreceiver_duplicates_dropped_total
alertreceiver_state_writes_total
alertreceiver_state_entries
alertreceiver_idempotency_keys_created_total
alertreceiver_resolved_orphans_total
alertreceiver_closed_ttl_seconds
```

Consult [README](../README.md#observability) for their descriptions and the application endpoints.


## Delivery Semantics and Failure Handling

Valkey state and idempotency operations are unchanged in v0.2.0. A generated
transition, an existing idempotency key, or HTTP 200 does not establish backend
persistence. The entire HTTP batch is now validated before any state processing;
invalid alerts receive HTTP 400. Operational state failures still produce 500.

Use `alertreceiver_redis_errors_total{operation="setnx_idemp"}` or
`alertreceiver_redis_idemp_setnx_total{result="error"}` for idempotency errors;
the former additional processor-side `operation="idempotency"` increment has
been removed to avoid double counting. Do not sum both metrics for the same error.

`event.id` is available in emitted events and relevant processing failures for
correlation. Batch exporter errors deliberately have no event_id. The new
`alertreceiver_otlp_export_requests_total{result="error"}` reports failed exporter
calls, not rollback of Valkey state or confirmed permanent loss of every record
in a batch. See [delivery metrics and limitations](../README.md#delivery-semantics-and-failure-handling).
