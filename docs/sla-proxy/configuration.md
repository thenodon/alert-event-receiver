# SLA proxy configuration

[Getting started](getting-started.md) · [Concepts](concepts.md) · [Operations](operations.md)

Use [minimal examples](../examples/README.md) for common tasks and
[alert-sla-proxy.yaml](../examples/alert-sla-proxy.yaml) for every configurable
field. YAML is loaded once at startup. Unknown/duplicate keys and extra YAML
documents are rejected. Durations are Go strings such as `5s` or `720h`.
Receiver environment variables do not configure this application.

CLI flags: `-config` (string, default `alert-sla-proxy.yaml` in the current directory),
`-version` (boolean, print version and exit), and `-help`. No hot reload or
configuration-check-only flag is provided. Do not start a production durable
configuration merely to validate it: startup can activate its namespace catalog.

**Required** values define policy/HTTP destinations. **Common** settings select a
mode, address and credentials. Leave **advanced** tuning at its defaults initially.
Changes to an existing durable catalog need the procedure in
[safe configuration changes](operations.md#change-policies-and-destinations).

## Listener and mode

| Key | Type | Default | Purpose / constraints | Level |
|---|---|---|---|---|
| `listen_address` | string | `:9012` | TCP host:port; numeric port 0–65535; 0 selects ephemeral port | Common |
| `webhook_path` | string | `/webhook` | Exact absolute path, distinct from `/metrics`, `/healthz`, `/readyz`; no trailing slash, dot segments or whitespace | Advanced |
| `max_body_bytes` | integer | `1048576` | Webhook request limit, 1–67108864 bytes; raise only for larger expected batches | Advanced |
| `shutdown_timeout` | duration | `10s` | Positive, at most 1m; bounded HTTP drain/runtime shutdown | Advanced |
| `evaluation_horizon_days` | integer | `370` | 1–3660 local dates including startsAt's date; increase for sparse calendars | Advanced |
| `durable.enabled` | boolean | `false` | Enable Valkey state and scheduling | Common |
| `delivery.enabled` | boolean | `false` | Enable asynchronous sink calls; requires durable enabled | Common |

All configured sinks receive structural validation in every mode. Runtime sinks,
Outbound HTTP clients/transports and sink credential resolution are created only when
delivery is enabled. Missing outbound HTTP credential environment variables do not prevent
observe-only or state-only startup. Durable Valkey credentials are needed whenever
durable mode is enabled.

## Inbound HTTPS and endpoint authentication

Both services use the same optional environment variables (strings, empty by
default). These are not YAML settings:

| Variable | Purpose |
|---|---|
| `HTTP_WEBHOOK_BASIC_AUTH_USERNAME` | Webhook username |
| `HTTP_WEBHOOK_BASIC_AUTH_PASSWORD_ENV` | Name of the environment variable holding its password |
| `HTTP_METRICS_BASIC_AUTH_USERNAME` | Independent metrics username |
| `HTTP_METRICS_BASIC_AUTH_PASSWORD_ENV` | Name of the environment variable holding its password |
| `HTTP_TLS_CERT_FILE` | PEM server certificate chain path |
| `HTTP_TLS_KEY_FILE` | Matching PEM private-key path |

An entirely empty pair disables that feature; partial/invalid pairs and missing
referenced passwords fail startup. HTTPS uses the existing listener and port.
Health/readiness routes bypass authentication but still use the listener protocol.
Restart for security changes. Basic Auth over cleartext HTTP is unsafe on untrusted
networks; use HTTPS or an isolated backend behind a trusted TLS proxy. See the
[shared security reference](../http-security.md) for constraints, examples, CA
trust, Alertmanager/Prometheus configuration and rotation. Inbound security is
independent of outbound OTLP, Valkey and HTTP sink security.

## Policies and calendars

| Key | Type | Default | Purpose / constraints | Level |
|---|---|---|---|---|
| `sla_definitions` | ordered list | required for observe-only | At least one definition; durable modes allow an empty list to retain known resolutions after policy removal | Required |
| `sla_definitions[].sla_label` | string | none | Unique selector label matching `[a-zA-Z_][a-zA-Z0-9_]*`; no implicit `sla_id` | Required |
| `sla_definitions[].slas` | map | none | Nonempty map of nonblank policy names, scoped to this definition | Required |
| `slas.<name>.timezone` | string | none | Explicit IANA zone, including `UTC`; `Local` rejected; zone data embedded | Required |
| `slas.<name>.schedule` | weekday → window list | none | Nonempty map; lowercase full English weekdays; windows described below | Required |
| `slas.<name>.holidays` | map | absent | Exclude local dates; omitted means no holiday exclusions | Common |
| `holidays.dataset` | absolute `.ics` path | absent | Local dataset plus adjacent `.ics.json`; see [dataset workflow](holiday-datasets.md) | Common |
| `holidays.additional_dates` | string list | absent | Exact valid `YYYY-MM-DD` local dates; allowed without a dataset | Common |

Policy field paths in the last rows are relative to
`sla_definitions[].slas.<name>`. Definitions are independent namespaces even when
policy names match. The first nonempty matching label selects its definition;
unknown values are skipped without fallback. See [selection](concepts.md#policy-selection).

Weekday keys are lowercase full English names. Windows use exact `HH:MM-HH:MM`
syntax; `24:00` is allowed only as an end. Equal start/end is invalid; use
`00:00-24:00` for a full day. Starts are inclusive and ends exclusive. Overnight
windows continue into the following weekday. Overlaps are rejected within and
across weekdays, including Sunday/Monday and Saturday/Sunday boundaries;
adjacent windows are allowed. An empty weekday list represents no configured
windows, though an overnight window from the previous weekday may still apply.

An optional holidays block requires a dataset, explicit dates, or both.
Datasets support any operator-supplied region without country imports and require
explicit coverage and SHA-bound metadata. Explicit dates must be valid, exact
`YYYY-MM-DD` values. See the [dataset workflow](holiday-datasets.md) for public
holidays, subdivisions, company closures and offline generation.

The former `country` and `subdivision` YAML fields have been removed and are
rejected as unknown fields. Generate/install the appropriate dataset and replace
those fields with `dataset`; do not remove holiday exclusions unintentionally.
Country/subdivision values in manifests and offline generator arguments remain
informational provenance and data selection respectively.

## Durable state and Valkey

| Key under `durable` | Type | Default | Purpose / constraints | Level |
|---|---|---|---|---|
| `policy_epoch` | uint64 | `1` | Positive activation version; increase for catalog/delivery/protocol changes | Common for changes |
| `valkey.address` | string | `localhost:6379` | Single-primary endpoint; numeric port 1–65535 | Common |
| `valkey.database` | integer | `0` | Nonnegative database number | Advanced |
| `valkey.namespace` | string | `sla` | 1–64 bytes; no `{}` or pattern delimiters, colon, backslash or whitespace | Common |
| `valkey.username` | string | empty | Optional server username | Common when authenticated |
| `valkey.password_env` | string | empty | Name of password environment variable; if set, it must resolve nonempty at startup | Common when authenticated |
| `valkey.tls` | boolean | `false` | TLS 1.2+ using system trust roots; no insecure-skip option | Common for remote servers |
| `retention.completed_payload` | duration | `720h` | Positive payload retention after first durable resolution acceptance | Advanced |
| `retention.tombstone` | duration | `2160h` | Longer than payload retention, at most 10 × 365 days | Advanced |

Use a dedicated namespace per Alertmanager lifecycle domain. No Cluster discovery,
Sentinel, custom CA or client-certificate fields are provided. Configure persistence
and noeviction on the server; application acceptance is not a separate disk/replica
receipt. Unresolved records have no automatic expiry. Outstanding/held delivery
pins required state past ordinary retention. Changes to retention do not rewrite
existing stored deadlines. See [operations](operations.md#retention-and-restart).

## Scheduler

All keys below are under `durable.scheduler`. These are advanced settings for
bounded due-work processing; none is needed in a minimal example.

| Key | Type | Default | Change when / constraints |
|---|---|---|---|
| `poll_interval` | duration | `1s` | Due query cadence, 10ms–1m |
| `workers` | integer | `4` | 1–64 concurrent scheduler workers |
| `batch_size` | integer | `100` | 1–1000 records per bounded batch |
| `operation_timeout` | duration | `5s` | Positive, at most 1m; also bounds whole webhook acceptance attempt |
| `lease_duration` | duration | `30s` | At least 2 × operation timeout, at most 10m |
| `retry_initial` | duration | `5s` | Initial transient-error backoff, at least poll interval |
| `retry_max` | duration | `5m` | At least initial backoff, at most 1h |
| `reconcile_interval` | duration | `30s` | Maintenance cadence, at least poll interval and at most 1h |

Durable-specific validation runs when durable mode is enabled. The HTTP write
timeout is max(15s, operation timeout + 1s); header/read/idle timeouts are 5/10/60s.
These fixed HTTP timeouts have no separate YAML settings.

## Destinations and authentication

`delivery.sinks` is a list, defaulting to one `{id: default, type: log}` destination.
Durable modes require 1–32 sinks. Explicit list entries replace the default list.
All sink IDs must be unique. Define stable destinations before accepting alerts:
the full set is frozen at first durable acceptance, even with delivery disabled.

| Key under `delivery.sinks[]` | Type | Default | Purpose / constraints | Level |
|---|---|---|---|---|
| `id` | string | required on explicit entry | `[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}`; stable destination identity | Required |
| `type` | enum | required on explicit entry | `log` or `http`; log must not have an http block | Required |
| `http` | map | absent | Required for HTTP sink | Required for HTTP |
| `http.url` | URL string | none | HTTP/HTTPS host required; valid optional port, no userinfo, fragment, whitespace or opaque URL; query allowed | Required for HTTP |
| `http.timeout` | duration | none | Positive and no greater than dispatcher delivery timeout | Required for HTTP |
| `http.basic_auth.username` | string | absent | Literal or exact `${NAME}`; provide with password | Common when authenticated |
| `http.basic_auth.password` | string | absent | Literal or exact `${NAME}`; provide with username | Common when authenticated |
| `http.idempotency_key` | boolean | `true` | Send stable intent ID in Idempotency-Key; destination deduplication is not assumed | Advanced |

Both auth fields may be absent for an unauthenticated destination. Otherwise each
must resolve nonempty when delivery is enabled. Username cannot contain colon or
CR/LF; password cannot contain CR/LF. Literal and environment values can be mixed.
Only whole-value `${NAME}` interpolation is supported, once, in these two fields;
`prefix${NAME}` and malformed references are rejected. Other YAML fields are not
expanded. Valkey `password_env` instead takes a bare variable name.

Use HTTPS for remote Basic Authentication. TLS verifies system roots; no custom CA,
insecure-skip, client certificate, arbitrary header or per-alert URL setting exists.
Redirects are rejected. HTTP transport can use trusted process proxy environment
settings (`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`) through Go's default transport;
these are not YAML options. Loopback destinations bypass the standard proxy lookup.
Protect configured endpoints, credentials, Valkey payloads and outbound connectivity.
Inbound listener security is configured separately through the shared environment
variables above; it does not change outbound destination identity.

The destination registry includes type, URL/query, timeout, authentication
references/modes and idempotency setting. Changing/removing registered destinations
is rejected even with a higher epoch. Credential **values** may rotate on restart;
changes to auth reference names or modes change identity. See
[safe changes](operations.md#change-policies-and-destinations).

## Dispatcher and retries

All keys below are under `delivery.dispatcher`; validation of these shared tuning
settings applies when delivery is enabled. HTTP sink timeout validation applies
even when delivery is disabled.

| Key | Type | Default | Change when / constraints |
|---|---|---|---|
| `poll_interval` | duration | `1s` | Delivery polling cadence, 10ms–1m |
| `workers` | integer | `4` | 1–64 shared workers; slow destinations occupy workers until deadline |
| `batch_size` | integer | `100` | 1–1000 due bindings per batch |
| `claim_lease` | duration | `30s` | Must cover delivery timeout + 3 × durable scheduler operation timeout; at most 10m |
| `delivery_timeout` | duration | `10s` | Positive, at most 1m; upper bound on individual HTTP timeout |
| `retry_initial` | duration | `5s` | Initial retry backoff, at least poll interval |
| `retry_max` | duration | `5m` | At least initial backoff, at most 1h |

The default lease covers 10s delivery + 3 × 5s state-store budgets. Retry-After may
extend an individual retry deadline up to the separately bounded hint; this does
not change retry_max. No retry-attempt limit, redrive endpoint or sink retirement
setting exists. See [retry behavior](operations.md#delivery-ordering-and-retries).

`durable.sink_id` and `delivery.sink` are obsolete and rejected. Stored occurrence
format is 2; no legacy schema migration is provided. See
[registry recovery](../redis-debugging.md#incomplete-registry-corruption-and-development-reset).
