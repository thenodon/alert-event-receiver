# Redis/Valkey storage and debugging

This document covers two independent services sharing a Redis/Valkey-compatible
backend. Sections 1–8 describe the **working state** of `alert-event-receiver`;
its historical lifecycle events are exported as OTLP logs. The
[Alert SLA Proxy section](#alert-sla-proxy-keys-and-debugging) describes its durable
occurrences, delivery receipts, configuration registries and recovery.

**Configuration changes normally require no key deletion.** Use the
[configuration-change table](#configuration-changes-and-key-deletion) before
changing the SLA proxy's epoch, destination registry or namespace. The two
services have different key families and reliability contracts.

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

## Alert SLA Proxy keys and debugging

These keys belong to `alert-sla-proxy`, independently of the receiver's
`alertstate:*` and `alertidemp:*`. They exist only in durable modes. Examples use
Valkey database 0 and `durable.valkey.namespace: sla`; substitute your configured
host, port, database and namespace. `redis-cli` supports the same commands as
`valkey-cli` here. Use deployment-approved authentication/TLS settings.

The prefix `sla:{sla}:` consists of the configured namespace `sla`, the literal
hash tag `{sla}`, and the key suffix. The hash tag places transaction keys in the
same Valkey Cluster slot. It does not imply that the current single-primary
client supports Cluster discovery, MOVED handling or Sentinel failover.

### Three control keys created at startup

A successful first durable startup creates these shared control records, even
before an alert is accepted. Normal restarts retain them.

| Key | Type | Meaning | Routine deletion? |
|---|---|---|---|
| `sla:{sla}:index-schema` | String containing JSON | Active index protocol, readiness and schema generation | No |
| `sla:{sla}:policies` | String containing JSON | Active configuration epoch and catalog digest | No |
| `sla:{sla}:destinations` | String containing JSON | Registered logical destination IDs and fingerprints | No |

Example `index-schema`:

```json
{"version":1,"state":"ready","generation":"cfb3ca66f88ca34b434010b5afab0985"}
```

- `version`: internal index-schema protocol version, not the binary release,
  occurrence format version or policy epoch.
- `state`: `ready` permits processing. `maintenance` fences operations during
  coordinated repairs; detected corruption can set `quarantined`.
- `generation`: opaque identity for this schema generation. Running processes
  retain the exact expected fence. A changed version, state or generation stops
  incompatible operations; they do not adopt a repaired generation automatically.
  A generation is not regenerated on each normal restart.

Example `policies`:

```json
{"epoch":1,"digest":"32d45fda0f67d40ce0c88c5d62239619caaabff648f708701b088c6d324f167b"}
```

- `epoch`: the positive operator-controlled `durable.policy_epoch`. Increase it
  to explicitly activate supported configuration changes. A lower epoch is
  rejected; the same epoch with a different digest is rejected.
- `digest`: deterministic fingerprint of ordered SLA namespaces, their policy
  versions, sink catalog, delivery enablement and store protocol. Individual
  policy versions also include policy rules, evaluation horizon and calendar
  provider/rule versions. This is not a hash of every YAML option.
- A newly activated epoch/catalog fences older instances through readiness and
  transaction checks. Coordinate the rollout across all instances using this
  namespace. An epoch increment does not migrate records or repair missing keys.

Example `destinations`:

```json
{"audit":"c19963290b35a5810a7780ba0ed2747e4621bb37d741f11a50eab3845c5d40e5","servicenow":"dbfc29de017e47355579268168208dda871869ad9963e12515c18641948821d3"}
```

Each property is a stable sink ID, not a sink type. Its SHA-256 fingerprint covers
sink configuration: type, HTTP URL/query, HTTP timeout, credential references/modes
and idempotency-header setting. Literal and resolved credential **values** are
excluded; raw URLs, usernames and passwords are not stored in this registry.
Switching authentication references/modes still changes configuration identity.

The registry is append-only: existing IDs must retain their fingerprints.
Changing/removing a registered destination fails startup even with a higher
epoch and even if its work appears complete. This prevents queued notifications
from being redirected or discarded. There is no retirement/migration/redrive API.

### Occurrences, indexes and leases

All keys below use the same `sla:{sla}:` prefix:

| Suffix | Type | Contents and purpose |
|---|---|---|
| `occ:<occurrence_id>` | String, JSON | Authoritative occurrence: original alert/group data, lifecycle, policy bindings, eligibility, intents, retries, claims and receipts |
| `claim:<occurrence_id>` | Expiring string | SLA scheduler ownership token; matching fence/deadline are in the occurrence |
| `due` | Sorted set | Occurrence IDs awaiting scheduler work or claim recovery |
| `retention` | Sorted set | Occurrence IDs due for payload compaction/tombstone deletion |
| `unresolved` | Set | Known-firing occurrence IDs |
| `pending` | Set | Firing occurrence IDs with a binding pending SLA eligibility |
| `held` | Set | Occurrence IDs with a lifecycle/policy binding hold |
| `intent-index` | Set | Occurrence IDs with ready, uncanceled intents; not a receipt or due-only queue |
| `delivery-claim:<occurrence_id>:<binding_id>` | Expiring string | One binding's delivery ownership token |
| `delivery-due` | Sorted set | Binding work IDs due for delivery/retry or expired-claim recovery |
| `delivery-ready` | Set | Queued binding work, including retry-delayed work without an active claim |
| `delivery-claimed` | Set | Persisted binding claims, including expired claims awaiting recovery |
| `delivery-held` | Set | Bindings with permanent delivery failures or relevant policy/destination holds |
| `delivery-pending:<sink_id>` | Set | Queued binding work for one sink, including retry delay |
| `delivery-inflight:<sink_id>` | Set | Persisted delivery claims for one sink |
| `delivery-failed:<sink_id>` | Set | Held delivery bindings for one sink |

An occurrence ID hashes the original fingerprint and canonical UTC RFC3339Nano
startsAt. Binding IDs include occurrence, SLA label/value and sink ID. Each
firing/resolved intent ID includes occurrence, binding and action. A delivery
index member is `<occurrence_id>:<binding_id>`; lifecycle indexes use occurrence
IDs alone. These identities preserve nanoseconds, independent of arrival time.

Sorted-set scores are Unix epoch milliseconds, rounded up only for fractional
milliseconds so work never runs before its exact deadline. Authoritative JSON
keeps precise timestamps. A delivery claim's score can represent lease expiry,
not a first SLA opening. Index names do not by themselves prove a sink call ran.

The occurrence freezes the full sink set on first durable acceptance, including
initially pending SLA occurrences. Each binding independently holds firing and
resolved receipts. Look for `bindings.<binding_id>.intents.firing.receipt` or
`resolved.receipt` in JSON: `eligibility: eligible`, `pending_delivery` or a claim
alone does not mean successful delivery. A resolved receipt requires that same
sink's firing receipt. HTTP 2xx is API acceptance, not confirmed backend processing.

Occurrence/control keys normally have `TTL = -1`. Occurrence retention is handled
by the maintenance worker and `retention` index, not a native row TTL. Unresolved
occurrences have no automatic expiration. Default resolved payload retention is
30 days and tombstone retention is 90 days after first recorded resolution;
stored deadlines are `compact_at`/`delete_at`. Outstanding/held delivery pins
required state beyond these deadlines. Tombstone expiry limits old replay protection.
Claim tokens have real TTLs; expiry permits another worker to recover work using
fencing. Do not delete a token manually to force another delivery attempt.

Empty sets/sorted sets may not exist at all. They are created when needed and
Valkey removes them after their last member is removed. Their absence after a
clean startup is normal; missing control records in an initialized namespace is
not. Secondary indexes are repairable from occurrence documents; source records
and delivery receipts cannot be reconstructed from indexes.

### Read-only inspection commands

Inspect control records and expected types:

```bash
valkey-cli -n 0 GET 'sla:{sla}:index-schema'
valkey-cli -n 0 GET 'sla:{sla}:policies'
valkey-cli -n 0 GET 'sla:{sla}:destinations'
valkey-cli -n 0 TYPE 'sla:{sla}:delivery-due'
valkey-cli -n 0 --scan --pattern 'sla:{sla}:occ:*'
```

Inspect bounded samples and queue counts:

```bash
valkey-cli -n 0 SCARD 'sla:{sla}:unresolved'
valkey-cli -n 0 SCARD 'sla:{sla}:pending'
valkey-cli -n 0 SCARD 'sla:{sla}:held'
valkey-cli -n 0 ZRANGE 'sla:{sla}:due' 0 9 WITHSCORES
valkey-cli -n 0 ZRANGE 'sla:{sla}:delivery-due' 0 9 WITHSCORES
valkey-cli -n 0 SSCAN 'sla:{sla}:delivery-pending:servicenow' 0 COUNT 20
valkey-cli -n 0 SCARD 'sla:{sla}:delivery-inflight:servicenow'
valkey-cli -n 0 SCARD 'sla:{sla}:delivery-failed:servicenow'
```

SCAN/SSCAN counts are hints: follow returned cursors to finish a scan. ZRANGE
above shows the earliest scores, which may still be in the future. Compare them
with Valkey `TIME`, not a differently configured local clock.

Use IDs from structured transition/delivery logs or discovered keys:

```bash
OCC_ID='replace-with-occurrence-id'
BINDING_ID='replace-with-binding-id'
valkey-cli -n 0 --raw GET "sla:{sla}:occ:$OCC_ID"
valkey-cli -n 0 PTTL "sla:{sla}:claim:$OCC_ID"
valkey-cli -n 0 PTTL "sla:{sla}:delivery-claim:$OCC_ID:$BINDING_ID"
valkey-cli -n 0 ZSCORE 'sla:{sla}:delivery-due' "$OCC_ID:$BINDING_ID"
```

Occurrence JSON contains original labels, annotations and notification metadata;
treat output as sensitive. Do not paste full payloads into public diagnostics.
The registry fingerprints do not contain those payloads or credential values.

### Configuration changes and key deletion

**No routine configuration change requires deleting individual keys.** Restart
with supported configuration changes, use an explicit higher epoch where needed,
and retain authoritative occurrences, receipts and registries together.

| Change | Increase `policy_epoch`? | Delete keys? / required action |
|---|---|---|
| Restart with identical configuration | No | No; recover existing work |
| SLA schedule, timezone, holidays, policy addition/removal, namespace selection order or evaluation horizon | Yes | No; affected pinned policy bindings are held, not migrated; new occurrences use active policy selection |
| Add a new uniquely named sink while retaining existing sinks unchanged | Yes | No; only new occurrences gain that binding, including when older occurrences are still pending SLA |
| Enable/disable `delivery.enabled` | Yes | No; disabling pauses dispatch and preserves prepared/authorized work |
| Change an existing sink's type, URL/query, HTTP timeout, auth reference/mode or idempotency setting | An increment is insufficient | No deletion workaround; startup rejects the changed fingerprint. Restore the registered configuration. A new sink ID may be added, but existing work is not transferred to it |
| Remove/rename a registered sink | An increment is insufficient | Keep its registered configuration; removal/rename is rejected. Retirement/migration is not implemented |
| Rotate literal username/password values or values supplied by the same `${NAME}` references | No | No; restart to load the new values. Permanent failures are not automatically redriven |
| Change shared dispatcher/scheduler workers, batch size, polling, backoff or lease budgets | No | No; restart with valid timeout/lease relationships. Existing stored claims/retry deadlines remain authoritative |
| Change listen address, webhook path, body limit or shutdown timeout | No | No; restart with validated configuration |
| Change completed/tombstone retention settings | No | No; existing stored resolution deadlines are not rewritten; new resolution acceptance uses the new durations |
| Change Valkey connection/authentication while reaching the same database/namespace | No | No; retain access to the same state and restart |
| Use another Valkey database, namespace or empty server | Not a migration mechanism | No automatic transfer. It is a separate state domain; pending work and replay protection do not follow it |
| Missing control registry or unsupported development record format | An increment cannot repair it | For disposable development data use a new unused namespace. Otherwise restore consistent state from backups; no automatic schema migration |
| Corrupt index types/quarantined schema | No epoch-only fix | Coordinated fence-first repair; preserve authoritative records/receipts/registries. Do not clear the schema key to bypass quarantine |

The policy catalog digest deliberately excludes runtime tuning options, but HTTP
**per-sink** timeout is part of destination identity. The shared dispatcher
`delivery_timeout` is runtime tuning, subject to startup lease validation. Calendar
provider/rule upgrades can change policy versions and therefore require a higher
epoch and a reviewed rollout even if YAML schedules are unchanged.

A higher epoch activates a compatible new catalog; it does not bypass immutable
destinations, migrate frozen bindings, remove policy holds, erase receipts or fix
missing/corrupt state. Restoring the exact pinned policy under a higher epoch can
release its hold. Changing to a different policy version cannot silently reinterpret
historical eligibility. Merely reordering sink entries does not change their
ID-to-fingerprint catalog; SLA definition order does affect first-match selection.

For the separate **event receiver**, changing OTLP exporter settings does not
require deleting `alertstate:*` or `alertidemp:*`. Deleting them is not a delivery
retry mechanism and can cause duplicate or misclassified lifecycle events.
Changing `CLOSED_STATE_TTL` or `IDEMPOTENCY_TTL` affects newly applied expirations;
it does not retroactively rewrite existing TTLs. Changing that service's Valkey
database/server similarly starts a separate state domain unless data is preserved.

### Incomplete registry, corruption and development reset

For `incomplete configuration registry`, check presence separately:

```bash
valkey-cli -n 0 EXISTS 'sla:{sla}:policies'
valkey-cli -n 0 EXISTS 'sla:{sla}:destinations'
```

Both should exist after successful startup. If one exists without the other,
older development data, manual deletion or data loss are possible causes. Do not
create a replacement destination map from current YAML: outstanding intents may
belong to different historical destinations. Do not delete only `policies`,
`destinations` or `index-schema` to make startup pass. Restore registries and
occurrences from a consistent backup when work must be preserved.

For **disposable development state**, stop all instances using the old namespace,
select a new unused namespace and restart:

```yaml
durable:
  valkey:
    namespace: sla-dev-clean-1
```

This starts empty; it preserves the old keys, but does not carry over pending
notifications, tombstones, authorization evidence or receipts. Replaying old
webhooks in the new namespace can generate new deliveries. Do not run both old
and new namespaces against the same production notification stream as an
accidental reset strategy.

If intentionally deleting abandoned development data later, delete only the
explicitly chosen old SLA namespace after stopping its users and deciding its
work is disposable. This is a whole-state reset, not a configuration update.
Never use `FLUSHDB`/`FLUSHALL` on a shared backend or remove the receiver's
`alertstate:*`/`alertidemp:*` as part of SLA proxy cleanup. This guide performs no
key deletion and supplies no automatic reset script.

For wrong-type index corruption, ordinary MULTI/EXEC has no runtime-error rollback:
a failed index command can coexist with a successful source write. The application
quarantines the namespace and does not acknowledge successful processing. Stop
instances, preserve/back up data, install a fresh maintenance fence generation
before repair, restore declared index types, install the ready fence with the new
generation, then restart. Follow the complete
[fence-first repair procedure](specifications/sla-proxy-hardening.md#transactions-and-the-schema-ownership-contract).
Missing memberships/scores are reconciled without rewriting unchanged occurrences
or renewing active leases. Missing/incorrect/orphan discovery entries are repaired
eventually; do not delete source records to clear queue counts.

See the [SLA proxy service guide](alert-sla-proxy.md#multi-sink-delivery)
for delivery ordering, retries, at-least-once crash windows and per-sink metrics.
