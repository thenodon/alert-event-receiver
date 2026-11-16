# Alert SLA Proxy

The SLA proxy selects label-based calendars and optionally persists lifecycle and
delivery work in Valkey. It runs independently of the event receiver.

| Task | Guide |
|---|---|
| Choose a mode and run a local test | [Getting started](sla-proxy/getting-started.md) |
| Configure policies, Valkey and destinations | [Configuration reference](sla-proxy/configuration.md) |
| Understand eligibility and ordering | [Concepts](sla-proxy/concepts.md) |
| Investigate pending/retried/held work | [Operations](sla-proxy/operations.md) |
| Look up responses, HTTP payloads and metrics | [Reference](sla-proxy/reference.md) |
| Inspect stored state or recover quarantine | [Redis/Valkey debugging](redis-debugging.md) |

## Multi-sink delivery

Start with [HTTP and log delivery](sla-proxy/getting-started.md#deliver-to-a-local-http-destination-and-audit-log).
Minimal examples are indexed in [examples](examples/README.md). The
[complete annotated example](examples/alert-sla-proxy.yaml) remains available for
advanced configuration. Delivery requires both durable and delivery enable flags.

Each destination's firing/resolution ordering, retries and receipts are independent.
Delivery is at least once; external acceptance and receipt persistence are separate.
See [delivery operations](sla-proxy/operations.md#delivery-ordering-and-retries).

[All documentation](README.md) · [Implementation history](specifications/README.md)
