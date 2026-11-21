# SLA proxy Redis/Valkey debugging and recovery

[Documentation index](../README.md) · [SLA proxy concepts](concepts.md) · [Operations](operations.md) · [Configuration](configuration.md)

This guide covers **durable modes** of `alert-sla-proxy`: occurrence documents,
configuration registries, delivery indexes, claims, receipts, configuration
changes and recovery. These keys are independent of the event receiver's
`alertstate:*` and `alertidemp:*` keys; see
[event receiver Redis/Valkey debugging](../event-receiver/redis-debugging.md).

**No routine configuration change requires deleting keys.** Preserve
authoritative occurrences, receipts and registries together. Start with
[configuration changes](#configuration-changes-and-key-deletion) before
changing epochs, destinations or namespaces.

## Namespace and key layout

Examples use Valkey database `0` and
`durable.valkey.namespace: sla`. Substitute your configured host, database
and namespace. `redis-cli` and `valkey-cli` support the same commands shown
here. Use deployment-approved authentication and TLS settings.

The prefix `sla:{sla}:` contains the configured namespace `sla`, the
literal Redis hash tag `{sla}`, and a key suffix. The hash tag places
transaction keys in the same Valkey Cluster slot; it does **not** imply
that the current single-primary client supports Cluster discovery, MOVED
handling or Sentinel failover.

## Control registries created at startup

A successful first durable startup creates three shared control keys,
even before an alert is accepted. Normal restarts retain them.

| Key | Type | Meaning | Routine deletion? |
|---|---|---|---|
| `sla:{sla}:index-schema` | JSON string | Active index protocol, readiness and schema generation | No |
| `sla:{sla}:policies` | JSON string | Active configuration epoch and catalog digest | No |
| `sla:{sla}:destinations` | JSON string | Registered logical destination IDs and fingerprints | No |

### Schema fence: `index-schema`

Example:

```json
{"version":1,"state":"ready","generation":"cfb3ca66f88ca34b434010b5afab0985"}
```

- `version` is the internal index-schema protocol version, not a binary
  release, occurrence format or policy epoch.
- `state: ready` permits processing. `maintenance` fences operations
  during coordinated repair; detected corruption may set `quarantined`.
- `generation` identifies the schema generation. Running processes
  retain the expected fence; a changed version, state or generation
  stops incompatible operations. Processes do not adopt a repaired
  generation automatically. Normal restarts do not regenerate it.

### Policy registry: `policies`

Example:

```json
{"epoch":1,"digest":"32d45fda0f67d40ce0c88c5d62239619caaabff648f708701b088c6d324f167b"}
```

- `epoch` is the positive operator-controlled `durable.policy_epoch`.
  Lower epochs and identical epochs with different digests are rejected.
- `digest` is a deterministic fingerprint of ordered SLA namespaces,
  policy versions, sink catalog, delivery enablement and store protocol.
  Individual policy versions include rules, evaluation horizon and
  calendar provider/rule versions. The digest is not a hash of every
  YAML option.
- A newly activated epoch/catalog fences older instances through
  readiness and transaction checks. Coordinate rollouts across every
  instance using the namespace. Increasing the epoch does not migrate
  records or repair missing keys.

### Destination registry: `destinations`

Example:

```json
{"audit":"c19963290b35a5810a7780ba0ed2747e4621bb37d741f11a50eab3845c5d40e5","servicenow":"dbfc29de017e47355579268168208dda871869ad9963e12515c18641948821d3"}
```

Each property is a stable **sink ID**, not a sink type. Its SHA-256
fingerprint covers sink type, HTTP URL/query, HTTP timeout, credential
references/modes and idempotency-header setting. Literal/resolved
credential **values** are excluded; raw URLs, usernames and passwords
are not stored in the registry. Changing authentication references or
modes still changes configuration identity.

The registry is **append-only**. Existing sink IDs must retain their
fingerprints. Changing or removing a registered destination fails
startup even with a higher epoch and even when its work appears
complete. This prevents queued notifications from being redirected
or discarded. No retirement, migration or redrive API is implemented.

## Occurrences, indexes and leases

All suffixes below use `sla:{sla}:` as their prefix.

| Suffix | Type | Contents and purpose |
|---|---|---|
| `occ:<occurrence_id>` | JSON string | Authoritative occurrence: original alert/group data, lifecycle, policy bindings, eligibility, intents, retries, claims and receipts |
| `claim:<occurrence_id>` | Expiring string | Scheduler ownership token; matching fence/deadline in occurrence |
| `due` | Sorted set | Occurrence IDs awaiting scheduler work or claim recovery |
| `retention` | Sorted set | Occurrence IDs due for payload compaction/tombstone deletion |
| `unresolved` | Set | Known-firing occurrence IDs |
| `pending` | Set | Firing occurrences with a binding pending SLA eligibility |
| `held` | Set | Occurrences with lifecycle/policy binding holds |
| `intent-index` | Set | Occurrences with ready, uncanceled intents; not a receipt or due-only queue |
| `delivery-claim:<occurrence_id>:<binding_id>` | Expiring string | Binding delivery ownership token |
| `delivery-due` | Sorted set | Bindings due for delivery/retry or expired-claim recovery |
| `delivery-ready` | Set | Queued binding work, including retry-delayed work without active claim |
| `delivery-claimed` | Set | Persisted binding claims, including expired claims awaiting recovery |
| `delivery-held` | Set | Bindings with permanent delivery failures or policy/destination holds |
| `delivery-pending:<sink_id>` | Set | Queued binding work for one sink, including retry delay |
| `delivery-inflight:<sink_id>` | Set | Persisted delivery claims for one sink |
| `delivery-failed:<sink_id>` | Set | Held delivery bindings for one sink |

An occurrence ID hashes the original fingerprint and canonical UTC
RFC3339Nano `startsAt`. Binding IDs include occurrence, SLA label/value
and sink ID. Firing/resolved intent IDs include occurrence, binding and
action. Delivery-index members are
`<occurrence_id>:<binding_id>`; lifecycle indexes use occurrence IDs.
Identity preserves nanoseconds, independently of arrival time.

Sorted-set scores are Unix epoch milliseconds, rounded **up** only for
fractional milliseconds so work never executes before its deadline.
Authoritative JSON retains exact timestamps. A delivery claim score
may represent lease expiry, not first SLA opening. Index membership
does not prove a sink call occurred.

### Frozen bindings and receipts

On first durable acceptance, an occurrence freezes the full sink set,
including for initially pending SLA occurrences. Each binding holds
independent firing and resolved receipts.

Inspect `bindings.<binding_id>.intents.firing.receipt` and
`bindings.<binding_id>.intents.resolved.receipt` in the occurrence JSON.
`eligibility: eligible`, `pending_delivery`, or an active claim is **not**
evidence of successful delivery. A resolved receipt requires the same
sink's firing receipt. HTTP 2xx proves API acceptance, not completed
downstream processing.

### Retention and index behavior

Occurrence/control keys normally have `TTL = -1`. The maintenance
worker uses the `retention` index for compaction and tombstone deletion,
rather than a native row TTL. Unresolved occurrences do not expire
automatically.

Default resolved payload retention is **30 days**, and tombstone
retention is **90 days after first recorded resolution**. Stored
deadlines are `compact_at` and `delete_at`. Outstanding/held delivery
pins required state beyond those deadlines. Tombstone expiration
limits protection against old replays.

Claim tokens have actual TTLs; expiry allows another worker to
recover work using fencing. **Do not delete claim tokens manually**
to force delivery.

Empty sets/sorted sets may be absent: they are created as needed and
Valkey removes them when their final member disappears. Missing
control registries in an initialized namespace are **not** normal.
Secondary indexes can be repaired from occurrence documents;
authoritative occurrences and receipts cannot be reconstructed from
indexes.

## Read-only inspection

### Check registries and key types

```bash
valkey-cli -n 0 GET 'sla:{sla}:index-schema'
valkey-cli -n 0 GET 'sla:{sla}:policies'
valkey-cli -n 0 GET 'sla:{sla}:destinations'
valkey-cli -n 0 TYPE 'sla:{sla}:delivery-due'
valkey-cli -n 0 --scan --pattern 'sla:{sla}:occ:*'
```

### Inspect bounded samples and queue counts

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

`SCAN`/`SSCAN` counts are hints: follow returned cursors to finish a
scan. `ZRANGE` displays the earliest scores, which may be in the
future. Compare with Valkey `TIME`, not a differently configured
local clock.

### Inspect an occurrence and its claims

Obtain IDs from structured logs or discovered keys:

```bash
OCC_ID='replace-with-occurrence-id'
BINDING_ID='replace-with-binding-id'
valkey-cli -n 0 --raw GET "sla:{sla}:occ:$OCC_ID"
valkey-cli -n 0 PTTL "sla:{sla}:claim:$OCC_ID"
valkey-cli -n 0 PTTL "sla:{sla}:delivery-claim:$OCC_ID:$BINDING_ID"
valkey-cli -n 0 ZSCORE 'sla:{sla}:delivery-due' "$OCC_ID:$BINDING_ID"
```

Occurrence JSON contains original labels, annotations and notification
metadata; treat it as sensitive. Do not paste full payloads into public
diagnostics. Registry fingerprints do not contain those payloads or
credential values.

## Configuration changes and key deletion

**No routine configuration change requires deleting individual keys.**
Restart with supported changes, use a higher epoch when required, and
preserve occurrences, receipts and registries together.

| Change | Increase `policy_epoch`? | Delete keys? / required action |
|---|---|---|
| Restart with identical configuration | No | No; recover existing work |
| SLA schedule, timezone, holidays, policy addition/removal, namespace selection order or evaluation horizon | Yes | No; affected pinned bindings are held, not migrated; new occurrences use active selection |
| Add a new uniquely named sink while retaining existing sinks unchanged | Yes | No; only new occurrences gain that binding, even when older occurrences remain pending |
| Enable/disable `delivery.enabled` | Yes | No; disabling pauses dispatch and preserves prepared/authorized work |
| Change existing sink type, URL/query, HTTP timeout, auth reference/mode or idempotency setting | Epoch increment insufficient | No deletion workaround; restore registered configuration. A new sink ID may be added, but existing work is not transferred |
| Remove/rename a registered sink | Epoch increment insufficient | Keep registered configuration; removal/rename rejected. Retirement/migration not implemented |
| Rotate literal credentials or values under the same `${NAME}` references | No | No; restart. Permanent failures are not automatically redriven |
| Change shared dispatcher/scheduler workers, batch size, polling, backoff or lease budgets | No | No; restart with valid timeout/lease relationships; existing claims/retry deadlines remain authoritative |
| Change listen address, webhook path, body limit or shutdown timeout | No | No; restart with validated configuration |
| Change completed/tombstone retention | No | No; existing stored resolution deadlines unchanged; new resolution acceptance uses new durations |
| Change Valkey connection/authentication to the same database/namespace | No | No; retain same state and restart |
| Use another Valkey database, namespace or empty server | Not a migration | No automatic transfer; separate state domain without prior work/replay protection |
| Missing registry or unsupported development record format | Epoch cannot repair | Use a new namespace for disposable development data; otherwise restore consistent backups |
| Wrong index types/quarantined schema | No epoch-only fix | Coordinated fence-first repair; preserve authoritative records/receipts/registries |

The policy catalog digest excludes runtime tuning, but **per-sink HTTP
timeout** is part of destination identity. Shared dispatcher
`delivery_timeout` is runtime tuning subject to startup lease validation.
Calendar provider/rule upgrades can change policy versions and require
a higher epoch and reviewed rollout even if YAML is unchanged.

A higher epoch activates a compatible catalog; it does **not** bypass
immutable destinations, migrate frozen bindings, erase receipts,
remove holds or repair missing/corrupt state. Restoring the exact
pinned policy under a higher epoch may release its hold; a different
policy version cannot silently reinterpret historical eligibility.
Reordering sink entries does not change the ID-to-fingerprint catalog;
SLA definition order affects first-match selection.

## Incomplete registry, corruption and development reset

### Incomplete configuration registry

Check the registry keys separately:

```bash
valkey-cli -n 0 EXISTS 'sla:{sla}:policies'
valkey-cli -n 0 EXISTS 'sla:{sla}:destinations'
```

Both should exist after successful startup. One missing registry can
indicate old development data, manual deletion or data loss. **Do not**
reconstruct a destination map from current YAML: outstanding intents
may belong to older destinations. Do not delete `policies`,
`destinations` or `index-schema` to bypass startup checks. Restore
registries and occurrences from a consistent backup when work matters.

### Disposable development reset

Stop all instances using the old namespace, choose a new unused
namespace, and restart:

```yaml
durable:
  valkey:
    namespace: sla-dev-clean-1
```

The new namespace starts empty while old keys remain. Pending work,
tombstones, authorization evidence and receipts do **not** transfer.
Replaying old webhooks can create new deliveries. Never run old and
new namespaces against the same production stream as an accidental
reset strategy.

If intentionally removing abandoned development data, first stop
all users and decide that its work is disposable. Delete only the
explicitly selected **old SLA namespace**, not unrelated data.
Never use `FLUSHDB`/`FLUSHALL` on a shared backend, and never remove
the receiver's `alertstate:*`/`alertidemp:*` as SLA cleanup.
This guide performs no deletion and provides no automatic reset script.

### Wrong-type indexes and quarantine

Ordinary Redis `MULTI/EXEC` has no runtime-error rollback: an index
command can fail while a sibling source write succeeds. The application
quarantines the namespace and does not acknowledge successful processing.

Stop instances, back up data, install a fresh maintenance fence
generation **before** changing key types, restore declared index
types, install the ready fence with the new generation, then restart.
See the [fence-first repair procedure](#fence-first-quarantine-repair).
Missing, incorrect and orphan discovery entries are reconciled
eventually without rewriting unchanged occurrences or renewing active
leases. Do not delete source records merely to reduce queue counts.

See [SLA proxy operations](operations.md) for delivery ordering,
retries, at-least-once crash windows and per-sink metrics.

## Fence-first quarantine repair

This advanced recovery procedure applies to the current owned-schema
protocol. It is **not** a way to redirect work, erase receipts or
migrate unsupported development schemas.

The `<namespace>:{sla}:index-schema` JSON string has `version`,
`state` and `generation`. Current version is `1`; normal operation
uses `ready`. Repair requires a **new** generation.

1. **Stop all instances** using the namespace and pause input.
   Preserve/back up occurrence JSON, receipts and both configuration
   registries. Investigate key, command, persistence, resource and
   ACL errors. A failed `EXEC` may have committed sibling writes;
   do not overwrite possibly committed receipts with older state.
2. **Install a maintenance fence first.** Before altering key types,
   set version `1`, `state: maintenance`, with a fresh unique
   generation. Never reuse an old ready fence value. Use the same
   protocol for planned type-changing maintenance.
3. **Restore declared types.** Shared sets are `unresolved`, `pending`,
   `held`, `intent-index`, `delivery-ready`, `delivery-claimed`,
   `delivery-held`; per-sink `delivery-pending`, `delivery-inflight`
   and `delivery-failed` keys are sets. Shared sorted sets are
   `due`, `retention`, `delivery-due`. Occurrences, registries,
   schema fence and claim tokens are strings. Missing discovery
   indexes can be rebuilt; do not delete authoritative records,
   tokens or registries as a shortcut. Restore corrupt authoritative
   data from consistent backups where necessary.
4. **Verify and reopen.** Verify the schema, then set version `1`,
   `state: ready`, keeping the **new** generation. Restart processes.
   Existing running processes cannot clear fatal state or adopt a
   different generation in place.
5. **Allow reconciliation.** Bounded reconciliation restores
   memberships/scores and removes orphan discovery entries. It
   preserves unchanged source versions, timestamps and active lease
   expirations. Recovery is eventual: inspect queues, holds, readiness
   and logs before resuming traffic. Readiness does not promise
   a complete scan.

A new repair generation alone does not require a policy epoch change
when catalog, mode and protocol are unchanged. A store-protocol
upgrade may require a higher epoch; consult its implementation/release
notes and do not mix incompatible clients. There is no repair/redrive
API or automatic deletion of corrupt source records.

Quarantine marking is best effort when Valkey is unavailable or
cannot persist the marker. The detecting process still fails closed,
but peers may not see an unconfirmed marker; stop **all** instances
operationally. An authorized external call may finish while the store
fails, so receipt ambiguity can cause a same-ID retry after recovery.

See [implementation notes](../specifications/sla-proxy-implementation.md)
and [historical hardening evidence](../specifications/sla-proxy-hardening.md)
for transaction and error-detection limitations.
