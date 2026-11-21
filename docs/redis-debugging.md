# Redis/Valkey storage and debugging

[Documentation index](README.md) · [Event receiver operations](event-receiver/operations.md) · [SLA proxy operations](sla-proxy/operations.md)

The two applications use **independent Redis/Valkey key families and
reliability models**. Choose the guide for the application you operate:

| Application | Storage role | Debugging and recovery |
|---|---|---|
| **Event receiver** | Latest lifecycle occurrence and emission-idempotency keys; **not** an OTLP delivery queue | [Event receiver Redis/Valkey debugging](event-receiver/redis-debugging.md) |
| **SLA proxy** | Durable occurrences, indexes, claims, receipts and configuration registries in durable modes | [SLA proxy Redis/Valkey debugging](sla-proxy/redis-debugging.md) |

**Routine configuration changes do not require deleting keys.** Do not
clear state as an attempt to replay failed deliveries. The two services
have different recovery and retention behavior.

This page is retained for compatibility with existing links. New
documentation should link directly to the service-specific guides.

## Event receiver: quick links

- [Key families and identity](event-receiver/redis-debugging.md#key-families-and-occurrence-identity)
- [State hash fields](event-receiver/redis-debugging.md#state-hash-fields)
- [TTLs and retention](event-receiver/redis-debugging.md#ttls-and-retention)
- [Read-only inspection](event-receiver/redis-debugging.md#connect-and-inspect-state)
- [Firing-to-resolved walkthrough](event-receiver/redis-debugging.md#example-firing-to-resolved)
- [Troubleshooting](event-receiver/redis-debugging.md#troubleshooting)
- [Implementation and reliability](event-receiver/redis-debugging.md#implementation-and-reliability-notes)
- [Related metrics](event-receiver/reference.md#prometheus-metrics)
- [Processing and delivery limitations](event-receiver/operations.md#trace-a-webhook-through-processing-and-delivery)

## SLA proxy: quick links

- [Namespace and key layout](sla-proxy/redis-debugging.md#namespace-and-key-layout)
- [Control registries](sla-proxy/redis-debugging.md#control-registries-created-at-startup)
- [Occurrences, indexes and leases](sla-proxy/redis-debugging.md#occurrences-indexes-and-leases)
- [Read-only inspection](sla-proxy/redis-debugging.md#read-only-inspection)
- [Configuration changes and key deletion](sla-proxy/redis-debugging.md#configuration-changes-and-key-deletion)
- [Incomplete registry, corruption and development reset](sla-proxy/redis-debugging.md#incomplete-registry-corruption-and-development-reset)
- [Fence-first quarantine repair](sla-proxy/redis-debugging.md#fence-first-quarantine-repair)

## Legacy section links

The following headings preserve frequently used anchors from the
former combined document. Follow the linked guides for current content.

### 1. Key families

See [receiver key families](event-receiver/redis-debugging.md#key-families-and-occurrence-identity).

### 2. State hash fields

See [receiver state fields](event-receiver/redis-debugging.md#state-hash-fields).

### 3. TTLs and their meaning

See [receiver TTLs](event-receiver/redis-debugging.md#ttls-and-retention).

### 4. Connecting and inspecting keys

See [receiver inspection](event-receiver/redis-debugging.md#connect-and-inspect-state).

### 5. End-to-end lifecycle walkthrough

See [receiver lifecycle example](event-receiver/redis-debugging.md#example-firing-to-resolved).

### 6. Debugging guide

See [receiver troubleshooting](event-receiver/redis-debugging.md#troubleshooting).

### 7. Implementation and reliability notes

See [receiver implementation notes](event-receiver/redis-debugging.md#implementation-and-reliability-notes).

### 8. Related metrics

See [receiver metric reference](event-receiver/reference.md#prometheus-metrics).

## Delivery Semantics and Failure Handling

See [receiver processing and delivery](event-receiver/operations.md#trace-a-webhook-through-processing-and-delivery).

## Alert SLA Proxy keys and debugging

See [SLA proxy storage and debugging](sla-proxy/redis-debugging.md).

### Configuration changes and key deletion

See [SLA proxy configuration changes](sla-proxy/redis-debugging.md#configuration-changes-and-key-deletion).

### Fence-first quarantine repair

See [SLA proxy recovery procedure](sla-proxy/redis-debugging.md#fence-first-quarantine-repair).
