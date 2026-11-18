# Documentation

Choose an application, then start with its getting-started guide. Commands assume
the repository root unless a guide says otherwise.

| Task | Event receiver | SLA proxy |
|---|---|---|
| Install and verify locally | [Getting started](event-receiver/getting-started.md) | [Getting started and mode selection](sla-proxy/getting-started.md) |
| Configure | [Environment variables and OTLP](event-receiver/configuration.md) | [YAML settings and examples](sla-proxy/configuration.md) |
| Understand normal behavior | [Occurrences and lifecycle events](event-receiver/concepts.md) | [SLA eligibility and delivery](sla-proxy/concepts.md) |
| Diagnose problems | [Operations](event-receiver/operations.md) | [Operations and configuration changes](sla-proxy/operations.md) |
| Look up contracts | [Webhook, OTLP fields and metrics](event-receiver/reference.md) | [Webhook, payloads, metrics and logs](sla-proxy/reference.md) |

## Holiday data

[Offline ICS datasets](sla-proxy/holiday-datasets.md) let operators add countries
and subdivisions through local files and YAML, without runtime Python or network.

## Shared tools and advanced operations

- [Inbound HTTP security](http-security.md): shared HTTPS and endpoint Basic Auth for both applications.

- [Configuration examples](examples/README.md): minimal scenarios and the complete SLA configuration.
- [Redis/Valkey debugging](redis-debugging.md): key inspection, retention, immutable registries and corruption recovery.
- [alertload](alertload.md): deterministic load generation, mixed scenarios and optional verification.
- [Development](development.md): builds, tests, integration prerequisites and documentation validation.
- [Specifications](specifications/README.md): historical designs, decisions, implementation details and benchmark reports.

The event receiver's Valkey working state does not provide durable OTLP delivery.
The SLA proxy's durable state coordinates independent destinations and retries;
neither application promises exactly-once external delivery.
