# Event receiver configuration

[Getting started](getting-started.md) · [Operations](operations.md) · [Reference](reference.md)

Configuration is loaded from the process environment at startup. The receiver
has no YAML loader or reload endpoint. An `.env` file is only a convenience for
your shell or service manager; the executable does not read it automatically.
See [event-receiver.env](../examples/event-receiver.env) for a minimal example.

**Common** settings choose the listener, Valkey and OTLP destination. **Advanced**
settings tune retention, export, security and resource use. Restart to apply changes.

## Listener and Valkey

| Variable | Type | Default | Use / constraints | Level |
|---|---|---|---|---|
| `ADDRESS` | string | `:9011` | TCP listen address; set a loopback/private address or another port | Common |
| `REDIS_ADDR` | string | `localhost:6379` | Redis/Valkey address reachable from this process | Common |
| `REDIS_PASSWORD` | string | empty | Password for the configured server; supply through a secret mechanism | Common when authenticated |
| `REDIS_DB` | integer | `0` | Database number; invalid integer text fails startup; server/client rejects unsupported database values | Advanced |
| `CLOSED_STATE_TTL` | Go duration | `24h` | Closed occurrence tombstone retention; increase for delayed notifications | Advanced |
| `IDEMPOTENCY_TTL` | Go duration | `168h` | Emission deduplication key retention; increase for late duplicate notifications | Advanced |

Empty values use defaults. **Malformed duration strings silently use defaults.**
Parsed durations are not checked for positivity by the application; use positive
values. Changing TTL settings affects newly applied expirations, not existing
keys. Open firing state has no TTL. The receiver does not expose Valkey username,
TLS, Sentinel or Cluster settings; do not assume SLA proxy settings apply here.

The HTTP server has fixed read/write/idle timeouts of 10/15/60 seconds. HTTP drain
is bounded to 10 seconds and OTel shutdown to 5 seconds. There is no configurable
inbound authentication, TLS or request-body size limit; deploy behind appropriate
network controls/reverse proxy limits.

## OTLP destination and identity

| Variable | Type | Default | Use / constraints | Level |
|---|---|---|---|---|
| `OTEL_EXPORTER_OTLP_PROTOCOL` | enum | `grpc` | `grpc`, `http/protobuf`, or alias `http`; other values fail initialization | Common |
| `OTEL_SERVICE_NAME` | string | `alert-event-receiver` | Emitted resource service name | Common |
| `OTEL_RESOURCE_ATTRIBUTES` | comma-separated attributes | empty | Additional resource metadata, e.g. `cluster=dev,tenant=platform` | Common |

The installed OTLP log exporters read the settings below. Every general
`OTEL_EXPORTER_OTLP_<SUFFIX>` has a log-specific
`OTEL_EXPORTER_OTLP_LOGS_<SUFFIX>` counterpart. Log-specific settings take
precedence. Select protocol with the general `PROTOCOL` variable above;
`OTEL_EXPORTER_OTLP_LOGS_PROTOCOL` does not select this application's exporter.

| Suffix | Type | Default | Purpose / allowed values | Level |
|---|---|---|---|---|
| `ENDPOINT` | URL | gRPC: `https://localhost:4317`; HTTP base: `https://localhost:4318` | Destination; explicit `http://` selects plaintext. HTTP general endpoint appends `/v1/logs`; logs-specific endpoint is the exact logs URL (default `https://localhost:4318/v1/logs`) | Common |
| `HEADERS` | `key=value` list | none | Authentication/tenant/backend headers; log-specific nonempty value replaces general headers | Common when required |
| `TIMEOUT` | integer milliseconds | `10000` | Use a positive timeout; HTTP applies to each request, gRPC to its export/retry operation; malformed integer text uses exporter defaults. Nonpositive values omit the exporter timeout; the batch context still bounds export | Advanced |
| `COMPRESSION` | enum | none | `gzip`; unset/unsupported values fall back to no compression | Advanced |
| `INSECURE` | boolean | `false` | Plaintext transport when endpoint scheme does not already determine it; not a skip-certificate-validation switch | Advanced |
| `CERTIFICATE` | file path | system roots | PEM trust certificate(s) for the server | Advanced |
| `CLIENT_CERTIFICATE` | file path | none | PEM client certificate for mutual TLS; supply with client key | Advanced |
| `CLIENT_KEY` | file path | none | PEM private key for mutual TLS; protect filesystem permissions | Advanced |

The application parses headers itself: comma segments without `=` continue the
previous value, supporting values such as `VL-Stream-Fields=a,b,c`. A first segment
without `key=value` fails initialization. Do not include secrets in shell history
or shared diagnostic output. Resource attributes can affect resource identity;
set service attributes consistently rather than defining conflicting values.

For a local VictoriaLogs HTTP destination:

```bash
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:9428/insert/opentelemetry
# Optional backend stream fields:
export OTEL_EXPORTER_OTLP_HEADERS='VL-Stream-Fields=service.name,am.external_url,alert.alertname,alert.fingerprint,alert.status,alert.transition'
```

Use the endpoint and headers supported by your deployed backend. An OTel
Collector is optional. A successful export to a Collector does not confirm final
backend persistence.

## SDK batching and attribute limits

These advanced settings are read by the installed Go Logs SDK (`v1.47.0`);
exporters are `v0.23.0`. All time settings in this table are milliseconds.

| Variable | Type | Default | Change when / constraints |
|---|---|---|---|
| `OTEL_BLRP_MAX_QUEUE_SIZE` | positive integer | `2048` records | Adjust bounded in-memory buffering; overflow displaces oldest queued records |
| `OTEL_BLRP_SCHEDULE_DELAY` | positive integer ms | `1000` | Tune export batching latency |
| `OTEL_BLRP_MAX_EXPORT_BATCH_SIZE` | positive integer | `512` | Tune records per export; SDK caps batch size to queue size |
| `OTEL_BLRP_EXPORT_TIMEOUT` | positive integer ms | `30000` | Bound each batch export including exporter retries |
| `OTEL_LOGRECORD_ATTRIBUTE_COUNT_LIMIT` | integer | `128` | Limit attributes retained on each log; zero records no attributes; negative means unlimited; invalid integer text uses default |
| `OTEL_LOGRECORD_ATTRIBUTE_VALUE_LENGTH_LIMIT` | integer | unlimited | Limit string attribute values; zero allows empty strings only; negative means unlimited; invalid integer text uses default |

SDK environment parsing falls back to defaults for invalid batching values.
There is no durable queue, delivery guarantee or configurable replay mechanism.
Increasing queue size or timeouts cannot recover a finally failed export batch.
The exporter retry policy uses a 60s elapsed budget, 5s initial interval and 30s
maximum interval; it is also bounded by the batch deadline. These retry-policy
values are Go API defaults, not additional environment settings exposed here.
See [failure examples and shutdown](operations.md#sdk-diagnostics-batching-and-shutdown).
