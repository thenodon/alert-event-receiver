# Event receiver configuration

[Getting started](getting-started.md) · [Concepts](concepts.md) · [Operations](operations.md) · [Reference](reference.md)

The event receiver reads configuration from **process environment variables at
startup**. It has no YAML loader or reload endpoint. An `.env` file is a
convenience for your shell or service manager; the executable does **not**
read it automatically. See the minimal
[event-receiver.env](../examples/event-receiver.env) example.

Restart the receiver to apply configuration changes.

## Configuration overview

The settings fall into four groups:

| Group | What it controls | Typical use |
|---|---|---|
| [Listener and Valkey](#listener-and-valkey) | HTTP listener, working state and retention | Required deployment settings |
| [Inbound HTTPS and authentication](#inbound-https-and-endpoint-authentication) | Webhook and metrics access | Secure network exposure |
| [OTLP destination and identity](#otlp-destination-and-identity) | Export protocol, endpoint, credentials and resource identity | Connect to a Collector or log backend |
| [SDK batching and attribute limits](#sdk-batching-and-attribute-limits) | In-memory queue, export batching and record limits | Advanced performance and reliability tuning |

**Common** settings are those most deployments need to review. **Advanced**
settings should generally remain at their defaults until there is an
operational reason to change them.

The receiver's Redis/Valkey configuration and best-effort OTLP export are
independent of the SLA proxy's durable delivery configuration. Do not assume
that a similarly named setting has the same behavior in both services.

## Listener and Valkey

| Variable | Type | Default | Use / constraints | Level |
|---|---|---|---|---|
| `ADDRESS` | string | `:9011` | TCP listen address; set a loopback/private address or another port | Common |
| `REDIS_ADDR` | string | `localhost:6379` | Redis/Valkey address reachable from this process | Common |
| `REDIS_PASSWORD` | string | empty | Password for the configured server; supply through a secret mechanism | Common when authenticated |
| `REDIS_DB` | integer | `0` | Database number; invalid integer text fails startup; server/client rejects unsupported database values | Advanced |
| `CLOSED_STATE_TTL` | Go duration | `24h` | Closed occurrence tombstone retention time. This is the time for a closed/resolved alert before its deleted; increase for delayed notifications | Advanced |
| `IDEMPOTENCY_TTL` | Go duration | `168h` | Avoid emission deduplication to the OTLP destination; increase for late duplicate notifications | Advanced |

### Retention and parsing behavior

Empty values use defaults. **Malformed duration strings silently use defaults.**
Parsed durations are not checked for positivity by the application; configure
positive values.

- `CLOSED_STATE_TTL` applies to closed occurrence tombstones. Open firing state
  has **no TTL**. Controlled by valkey key `alertstate:{fingerprint}`
- `IDEMPOTENCY_TTL` controls how long emission-deduplication keys are retained. This is **not** a replay or delivery 
  guarantee, since the idempotency key is created before OTEL SDK submission. Controlled by valkey key `alertidemp:{fingerprint}:{startsAt}:{transition}` 
  where `transition` is `firing` or `resolved`
  It only suppresses repeated transition emissions to the OTLP destination.
- Changing either TTL affects **newly applied expirations**, not existing keys.

### Fixed HTTP limits

These HTTP server values are fixed rather than environment-configurable:

| Behavior | Limit |
|---|---|
| Read timeout | 10 seconds |
| Write timeout | 15 seconds |
| Idle timeout | 60 seconds |
| HTTP shutdown drain | 10 seconds |
| OpenTelemetry shutdown | 5 seconds |

The receiver has **no configurable request-body size limit**. Apply suitable
network controls or reverse-proxy limits when exposing the webhook.

## Inbound HTTPS and endpoint authentication

The receiver supports optional HTTPS and separate HTTP Basic Authentication
for the webhook and metrics endpoints. All settings below are strings and
default to empty. They are environment variables, **not YAML settings**.

| Variable | Purpose |
|---|---|
| `HTTP_WEBHOOK_BASIC_AUTH_USERNAME` | Webhook username |
| `HTTP_WEBHOOK_BASIC_AUTH_PASSWORD_ENV` | Name of the environment variable holding its password |
| `HTTP_METRICS_BASIC_AUTH_USERNAME` | Independent metrics username |
| `HTTP_METRICS_BASIC_AUTH_PASSWORD_ENV` | Name of the environment variable holding its password |
| `HTTP_TLS_CERT_FILE` | PEM server certificate chain path |
| `HTTP_TLS_KEY_FILE` | Matching PEM private-key path |

An entirely empty pair disables that feature. Partial or invalid pairs,
and missing referenced passwords, **fail startup**.

HTTPS uses the existing listener and port. `/healthz` bypasses Basic
Authentication but still uses HTTPS when TLS is enabled. The event receiver
has **no `/readyz` endpoint**.

Restart after changing credentials or certificates. Basic Authentication
over cleartext HTTP is unsafe on untrusted networks; use HTTPS or an
isolated backend behind a trusted TLS proxy.

See the [shared HTTP security reference](../http-security.md) for
configuration examples, CA trust, Alertmanager and Prometheus clients,
and rotation procedures. Inbound security is independent of outbound
OTLP and Valkey connections.

## OTLP destination and identity

### Select the protocol and resource identity

| Variable | Type | Default | Use / constraints | Level |
|---|---|---|---|---|
| `OTEL_EXPORTER_OTLP_PROTOCOL` | enum | `grpc` | `grpc`, `http/protobuf`, or alias `http`; other values fail initialization | Common |
| `OTEL_SERVICE_NAME` | string | `alert-event-receiver` | Emitted resource service name | Common |
| `OTEL_RESOURCE_ATTRIBUTES` | comma-separated attributes | empty | Additional resource metadata, e.g. `cluster=dev,tenant=platform` | Common |

Set service attributes consistently: conflicting resource attributes can
change the identity used for log grouping and queries.

### Configure the OTLP log exporter

The installed OTLP log exporters read the variables below. Each general
`OTEL_EXPORTER_OTLP_<SUFFIX>` variable has a log-specific
`OTEL_EXPORTER_OTLP_LOGS_<SUFFIX>` counterpart. Log-specific settings take
precedence.

**Important exception:** select the exporter protocol with
`OTEL_EXPORTER_OTLP_PROTOCOL` above.
`OTEL_EXPORTER_OTLP_LOGS_PROTOCOL` does **not** select this application's
exporter.

| Suffix | Type | Default | Purpose / allowed values | Level |
|---|---|---|---|---|
| `ENDPOINT` | URL | gRPC: `https://localhost:4317`; HTTP base: `https://localhost:4318` | Destination; explicit `http://` selects plaintext. HTTP general endpoint appends `/v1/logs`; logs-specific endpoint is the exact logs URL (default `https://localhost:4318/v1/logs`) | Common |
| `HEADERS` | `key=value` list | none | Authentication/tenant/backend headers; log-specific nonempty value replaces general headers | Common when required |
| `TIMEOUT` | integer milliseconds | `10000` | Use a positive timeout; HTTP applies to each request, gRPC to its export/retry operation; malformed integer text uses exporter defaults. Nonpositive values omit the exporter timeout; the batch context still bounds export | Advanced |
| `COMPRESSION` | enum | none | `gzip`; unset/unsupported values fall back to no compression | Advanced |
| `INSECURE` | boolean | `false` | Plaintext transport when endpoint scheme does not already determine it; **not** a skip-certificate-validation switch | Advanced |
| `CERTIFICATE` | file path | system roots | PEM trust certificate(s) for the server | Advanced |
| `CLIENT_CERTIFICATE` | file path | none | PEM client certificate for mutual TLS; supply with client key | Advanced |
| `CLIENT_KEY` | file path | none | PEM private key for mutual TLS; protect filesystem permissions | Advanced |

The application parses headers itself: comma-separated segments without `=`
continue the previous value. This supports header values such as
`VL-Stream-Fields=a,b,c`. A first segment without `key=value` fails
initialization.

Do not put credentials in shell history or shared diagnostic output.

### Example: VictoriaLogs over OTLP/HTTP

For a local VictoriaLogs HTTP destination:

```bash
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:9428/insert/opentelemetry

# Optional backend stream fields:
export OTEL_EXPORTER_OTLP_HEADERS='VL-Stream-Fields=service.name,am.external_url,alert.alertname,alert.fingerprint,alert.status,alert.transition'
```
By default, VictoriaLogs uses all OpenTelemetry resource attributes as stream fields. Explicitly selecting stream fields 
avoids unnecessary stream cardinality while preserving the remaining attributes as searchable log fields.

Avoid using high-cardinality fields such as alert.fingerprint and event.id as stream fields.

Use the endpoint and headers supported by your deployed backend. An OpenTelemetry Collector is optional. 
When using a Collector, configure the VictoriaLogs stream fields in its exporter instead.

**Exporter success is not proof of final backend persistence.** In
particular, a successful export to an intermediate Collector only
establishes acceptance by that exporter destination. See
[delivery semantics](concepts.md#what-delivery-means).

## SDK batching and attribute limits

These are **advanced** settings read by the installed Go Logs SDK
(`v1.47.0`); exporters are `v0.23.0`. All time settings in the table are
milliseconds.

| Variable | Type | Default | Change when / constraints |
|---|---|---|---|
| `OTEL_BLRP_MAX_QUEUE_SIZE` | positive integer | `2048` records | Adjust bounded in-memory buffering; overflow displaces oldest queued records |
| `OTEL_BLRP_SCHEDULE_DELAY` | positive integer ms | `1000` | Tune export batching latency |
| `OTEL_BLRP_MAX_EXPORT_BATCH_SIZE` | positive integer | `512` | Tune records per export; SDK caps batch size to queue size |
| `OTEL_BLRP_EXPORT_TIMEOUT` | positive integer ms | `30000` | Bound each batch export including exporter retries |
| `OTEL_LOGRECORD_ATTRIBUTE_COUNT_LIMIT` | integer | `128` | Limit attributes retained on each log; zero records no attributes; negative means unlimited; invalid integer text uses default |
| `OTEL_LOGRECORD_ATTRIBUTE_VALUE_LENGTH_LIMIT` | integer | unlimited | Limit string attribute values; zero allows empty strings only; negative means unlimited; invalid integer text uses default |

SDK environment parsing falls back to defaults for invalid batching values.

### Reliability implications

The queue is **in memory**, not durable. There is no persistent delivery
queue, delivery guarantee or configurable replay mechanism. Increasing queue
size or timeouts cannot recover a batch after its final export failure.

The exporter retry policy uses a 60-second elapsed budget, 5-second initial
interval and 30-second maximum interval; it is also bounded by the batch
deadline. These retry values are Go API defaults, **not** additional
environment settings exposed by this application.

For queue-overflow examples, failure behavior and shutdown, see
[SDK diagnostics and shutdown](operations.md#otlp-export-failures-and-shutdown).
