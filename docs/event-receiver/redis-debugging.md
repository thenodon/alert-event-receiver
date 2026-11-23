# Event receiver Redis/Valkey debugging

[Documentation index](../README.md) · [Receiver concepts](concepts.md) · [Receiver operations](operations.md) · [Receiver configuration](configuration.md)

The `alert-event-receiver` uses Redis/Valkey for **working lifecycle state and
emission idempotency**, not historical event storage or a durable OTLP queue.
Historical lifecycle records are exported to a log backend. This guide explains
the stored keys, read-only inspection, and troubleshooting.

**Do not delete keys to retry OTLP delivery.** Deletion can cause duplicate
or misclassified transitions, and cannot recover an event lost after a state
write. 

## Key families and occurrence identity

| Key | Redis type | Lifetime | Purpose |
|---|---|---|---|
| `alertstate:{fingerprint}` | Hash | No TTL while firing; `CLOSED_STATE_TTL` when closed | Latest occurrence and its labels |
| `alertidemp:{fingerprint}:{startsAt}:{transition}` | String (`1`) | `IDEMPOTENCY_TTL` | Suppress duplicate lifecycle event emission |

The receiver uses the **fingerprint supplied by Alertmanager**; it does not
recalculate it from labels. Occurrence identity is `(fingerprint, startsAt)`,
where `startsAt` is stored and encoded in idempotency keys as UTC RFC3339Nano.
The transition is `firing` or `resolved`; `endsAt` is **not** part of the
idempotency key.

For example:

```text
alertstate:4a4f0d2b7c9e1a23
alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:firing
alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:resolved
```

See [lifecycle transition rules](concepts.md#identity-and-transitions) for
ordering, stale notifications, supersession and orphan resolutions.

## State hash fields

| Field | Meaning |
|---|---|
| `status` | `firing` or `closed` |
| `starts_at` | Start of the current occurrence, RFC3339Nano |
| `ends_at` | Resolution timestamp when closed; zero timestamp when firing |
| `first_firing_at` | First firing timestamp recorded for this occurrence |
| `last_seen_at` | Last observed firing notification timestamp; not refreshed by a repeated resolution |
| `alertname` | Name copied from alert labels |
| `label.*` | Alert labels, e.g. `label.severity`, `label.service`, `label.instance` |

Illustrative firing hash:

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

When a newer occurrence replaces an older one, the entire hash is replaced:
old labels and closure timestamps do not carry over. A closed hash contains
the corresponding `ends_at` and receives a TTL. A resolved notification
without a matching open occurrence also writes a closed tombstone.

## TTLs and retention

| Setting | Default | Applies to |
|---|---|---|
| `CLOSED_STATE_TTL` | `24h` | Closed `alertstate:*` hashes |
| `IDEMPOTENCY_TTL` | `168h` (7 days) | `alertidemp:*` keys |

Open firing state is persistent (`TTL = -1`). Closed tombstones preserve
limited ordering history: they prevent a late repeated resolution from
immediately looking like an orphan. Idempotency keys protect against
repeated **event emission** for longer.

A repeated resolution **does not refresh** its tombstone TTL. After the
tombstone expires, the receiver can classify a delayed resolution as an
orphan. A late firing notification can also be treated as new if all
relevant state has disappeared.

Redis `TTL` results:

| Result | Meaning |
|---|---|
| `-1` | Key exists without expiration, typically an open firing hash |
| `-2` | Key does not exist |
| Positive number | Remaining lifetime in seconds |

Increasing TTLs consumes more Redis memory; decreasing them weakens
protection against delayed retries. Changes affect newly applied
expirations, not existing keys.

`alertreceiver_state_entries` is a write-path gauge, **not a live key
census**. It can diverge after restarts, expiration or multiple writers.

## Connect and inspect state

Use the correct Redis/Valkey host and database:

```bash
redis-cli -h localhost -p 6379
redis-cli -h localhost -p 6379 -n 2
```

For authenticated deployments, use an approved secret mechanism rather
than passing the password directly on the command line:

```bash
export REDISCLI_AUTH="$REDIS_PASSWORD"
redis-cli -h localhost -p 6379
```

Use `SCAN` rather than `KEYS` on a busy production server:

```bash
redis-cli --scan --pattern 'alertstate:*'
redis-cli --scan --pattern 'alertidemp:*'
```

Inspect a specific fingerprint:

```bash
FP=4a4f0d2b7c9e1a23
redis-cli HGETALL "alertstate:$FP"
redis-cli HMGET "alertstate:$FP" status starts_at ends_at first_firing_at last_seen_at alertname
redis-cli TTL "alertstate:$FP"
redis-cli --scan --pattern "alertidemp:$FP:*"
```

Inspect a transition key:

```bash
KEY='alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:resolved'
redis-cli GET "$KEY"
redis-cli TTL "$KEY"
```

Read label fields without parsing alternating `HGETALL` output using `grep`:

```bash
redis-cli --raw HSCAN "alertstate:$FP" 0 MATCH 'label.*' COUNT 100
redis-cli HGET "alertstate:$FP" label.service
```

`HSCAN` can require multiple cursor iterations for large hashes.

## Example: firing to resolved

Suppose Alertmanager sends a firing notification:

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
alertstate:4a4f0d2b7c9e1a23                              status=firing, no TTL
alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:firing    value=1, expiring
```

Repeating that firing occurrence updates `last_seen_at`, but does not
generate another lifecycle transition.

Now send a resolution with **the same fingerprint and `startsAt`**:

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
alertstate:4a4f0d2b7c9e1a23                               status=closed, ends_at=2026-04-15T09:41:01Z, TTL applied
alertidemp:4a4f0d2b7c9e1a23:2026-04-15T09:22:58Z:resolved  value=1, expiring
```

The resolved event duration is 18 minutes and 3 seconds (1083 seconds).
Repeating the resolution leaves the tombstone unchanged and emits nothing.

## Troubleshooting

### Alert is firing, but no lifecycle event is visible

1. Check the webhook HTTP response and application logs. HTTP success
   does **not** guarantee backend persistence.
2. Inspect `alertstate:{fingerprint}`. A matching `firing` occurrence
   may be a repeat and correctly emit no event.
3. Inspect the corresponding `alertidemp:*:firing` key. An existing key
   can suppress emission even if the event never reached the backend.
4. Check `alertreceiver_emit_errors_total`,
   `alertreceiver_redis_errors_total`, exporter metrics and downstream
   Collector/backend logs.
5. Remember the state-before-emission failure window.

See [processing and delivery](operations.md#trace-a-webhook-through-processing-and-delivery)
for interpreting the separate delivery stages.

### Resolved alert appears as an orphan

Possible causes: original firing was never seen, Redis state was cleared or
evicted, the tombstone expired, a newer occurrence replaced it, or
notifications arrived out of order. Compare the stored `starts_at` and
`status` with the webhook fingerprint and `startsAt`.

### Notification is ignored as stale

An incoming `startsAt` older than the stored occurrence is stale. A firing
notification for the same already-closed occurrence is also stale. Inspect
the incoming payload and hash before assuming a lost update.

### Duplicates appear after a delay

Check `CLOSED_STATE_TTL` and `IDEMPOTENCY_TTL`. Once keys expire, Redis
cannot reconstruct old occurrence history. Increasing retention improves
late-retry protection at a memory cost.

### Redis errors cause webhook retries

Required state read/write failures cause HTTP 500 after other valid alerts
are attempted; earlier writes are not rolled back. Check connectivity,
credentials, database and operation logs.

Idempotency `SET NX` failure is **best effort**: it is logged and counted,
but emission proceeds after successful state processing, potentially
allowing duplicate emission.

### State-entry metrics disagree with Redis

`alertreceiver_state_entries` changes on write operations and does not
scan Redis. Use direct inspection and `SCAN` when actual keys matter.

## Implementation and reliability notes

- `GetState` validates stored timestamps and occurrence state; corrupt
  data causes a processing error rather than a guessed decision.
- `SetFiring` and `SetClosed` replace the full hash; closure also sets
  the TTL in a Redis transaction pipeline.
- `UpdateLastSeen` changes only `last_seen_at`.
- `SET NX` creates idempotency keys **before** emission.
- Hash replacement is atomic **within the transaction**, but the
  `GetState → compare → Set* → idempotency → emit` sequence is **not atomic**.
- Concurrent notifications for the same fingerprint can race.
- A returned Redis error does not prove the server did not execute the operation.
- OTLP emitter acceptance does not establish delivery to VictoriaLogs
  or another backend.

A future durable outbox is a design possibility, not an implemented
recovery mechanism. Currently, Redis holds working state, **not** a
reliable event queue or complete historical record.

### Useful metrics

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

See the [metric reference](reference.md#prometheus-metrics) for definitions.

### Delivery failures and idempotency metrics

Valkey state and idempotency operations are unchanged in v0.2.0. A
generated transition, existing idempotency key, or HTTP 200 does not
prove backend persistence. The entire webhook batch is validated
before state access: invalid alerts return HTTP 400; required state
failures return HTTP 500.

For idempotency errors, inspect either
`alertreceiver_redis_errors_total{operation="setnx_idemp"}` or
`alertreceiver_redis_idemp_setnx_total{result="error"}`. Do **not**
sum them as independent failures. The former processor-side
`operation="idempotency"` increment was removed to prevent double
counting.

`event.id` is present in emitted events and relevant processing
failure logs, but aggregate exporter failures do not identify
individual events. `alertreceiver_otlp_export_requests_total{result="error"}`
counts failed exporter calls, not confirmed permanent loss of every
record in a batch. See [delivery metrics and limitations](operations.md#trace-a-webhook-through-processing-and-delivery).
