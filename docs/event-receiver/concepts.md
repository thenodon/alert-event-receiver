# Receiver lifecycle concepts

The event receiver answers: **what firing/resolved transitions did Alertmanager
report?** It is independent of SLA policy and ticket/notification delivery.

```text
Alertmanager webhook → lifecycle state → event → asynchronous OTLP export
                            ↕
                      Redis / Valkey
```

## Identity and transitions

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

Idempotency checks suppress **event emission**, not lifecycle processing. Required Redis/Valkey read/write failures return processing errors rather than authoritative state decisions. The whole batch is validated before any state access; invalid alerts, malformed JSON, null payloads, or trailing JSON receive HTTP 400. An empty alerts batch remains accepted. If an operational error occurs during processing, the handler returns HTTP 500 after attempting the remaining alerts; Alertmanager may retry the entire batch.

## Working state versus history

Valkey stores the latest occurrence per fingerprint, not the full event history.
A newer firing replaces the previous working occurrence; it does not synthesize
a resolution for that older occurrence. Closed tombstones retain limited ordering
history. The log backend is where operators query historical lifecycle events.

Deduplication keys include fingerprint, canonical UTC nanosecond `startsAt` and
transition. `endsAt` is timing, not occurrence identity. A key suppresses emission,
not lifecycle processing. State and keys precede SDK submission; a crash can
therefore permanently lose an event even when a retry becomes a repeat.

## What delivery means

A generated transition, successful SDK submission, successful exporter call and
queryable backend record are distinct observations. The receiver offers best-effort
OTLP delivery, with no persistent queue or replay. Its lifecycle read/decide/write
sequence is not atomic across concurrent requests. Use a single receiver when you
need the documented ordering model; multiple instances do not add fencing.

See [operations](operations.md) for failure windows and
[reference](reference.md) for event fields and metric meanings. Detailed storage
inspection is in [Redis/Valkey debugging](../redis-debugging.md).
