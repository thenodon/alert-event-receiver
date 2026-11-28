# Securing inbound HTTP endpoints

[Documentation index](README.md) · [Shared environment example](examples/http-security.env)

Both `alert-event-receiver` and `alert-sla-proxy` support the **same
environment-variable-only inbound security settings**: optional
HTTPS and independent HTTP Basic Auth for webhooks and metrics.
Defaults are **plain HTTP with no authentication**.

The existing listener serves HTTP **or** HTTPS on its configured
address/port. There is no second listener or automatic HTTP-to-HTTPS
redirect. The SLA proxy has **no inbound security YAML settings**;
outbound sink authentication and TLS are configured separately.

**Do not send Basic Auth credentials over cleartext HTTP on an
untrusted network.** Use native HTTPS or a trusted TLS-terminating
proxy such as Envoy with an isolated backend network. Basic Auth
provides neither rate limiting nor account management.

## Choose a deployment pattern

| Pattern | Inbound TLS variables | Basic Auth | Considerations |
|---|---|---|---|
| Local testing on isolated loopback | Unset | Optional, usually unset | Default HTTP; not suitable for untrusted networks |
| Native HTTPS | Certificate and key configured | Optional, recommended where applicable | Clients must trust the certificate and verify hostname |
| HTTPS at a trusted reverse proxy | Usually unset on application | At proxy and/or application | Secure client-to-proxy TLS, isolate proxy-to-application network and handle Authorization forwarding deliberately |

All routes, including health probes, use the listener's chosen protocol.
TLS and authentication are loaded **once at startup**; restart to
apply credential, reference or certificate changes. No hot reload
is provided.

## Quick setup

### Plain HTTP without authentication

In a fresh shell without inherited security variables:

```bash
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-observe.yaml
```

From another terminal:

```bash
curl --fail http://127.0.0.1:9012/healthz
```

### HTTPS with separate webhook and metrics credentials

Provision real certificate/key files through your PKI and use a
secret manager or a hidden prompt for passwords. The paths below
are placeholders:

```bash
export HTTP_WEBHOOK_BASIC_AUTH_USERNAME=alertmanager
export HTTP_WEBHOOK_BASIC_AUTH_PASSWORD_ENV=ALERTMANAGER_WEBHOOK_PASSWORD
read -rsp 'Webhook password: ' ALERTMANAGER_WEBHOOK_PASSWORD; echo
export ALERTMANAGER_WEBHOOK_PASSWORD

export HTTP_METRICS_BASIC_AUTH_USERNAME=prometheus
export HTTP_METRICS_BASIC_AUTH_PASSWORD_ENV=PROMETHEUS_SCRAPE_PASSWORD
read -rsp 'Metrics password: ' PROMETHEUS_SCRAPE_PASSWORD; echo
export PROMETHEUS_SCRAPE_PASSWORD

export HTTP_TLS_CERT_FILE=/etc/alert-http/server.crt
export HTTP_TLS_KEY_FILE=/etc/alert-http/server.key

/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-observe.yaml
```

The receiver uses the same six variables. Start
`/tmp/alert_event_receiver` only after configuring its Valkey/OTLP
dependencies. Its default port is `9011`; the SLA proxy defaults
to `9012`. Certificate SANs must cover the hostnames/IPs used by
Alertmanager, Prometheus and diagnostic clients.

In a second terminal, substitute your real hostname, port and CA:

```bash
# Health/readiness bypass Basic Auth, but require TLS trust.
curl --fail --cacert /etc/alert-http/ca.crt \
  https://alerts.example.net:9012/healthz
curl --fail --cacert /etc/alert-http/ca.crt \
  https://alerts.example.net:9012/readyz

# Without metrics credentials, expect 401 and the metrics challenge.
curl -i --cacert /etc/alert-http/ca.crt \
  https://alerts.example.net:9012/metrics

# curl prompts for the password; it is not placed on the command line.
curl --fail --cacert /etc/alert-http/ca.crt -u prometheus \
  https://alerts.example.net:9012/metrics

# Generate a test webhook, then authenticate to the webhook endpoint.
python3 docs/examples/make-test-webhooks.py /tmp/http-security-webhook
curl --fail --cacert /etc/alert-http/ca.crt -u alertmanager \
  -H 'Content-Type: application/json' \
  --data-binary @/tmp/http-security-webhook-firing.json \
  https://alerts.example.net:9012/webhook
```

For the receiver, change the port to `9011`.
For a custom SLA proxy webhook path, change the URL. Without Basic
Auth, omit `-u`; without native TLS, use `http://` and omit `--cacert`.

## Environment variable reference

All six settings are optional strings, empty by default, and read
at startup by **both** executables:

| Variable | Purpose |
|---|---|
| `HTTP_WEBHOOK_BASIC_AUTH_USERNAME` | Webhook username; nonblank, no colon or control characters |
| `HTTP_WEBHOOK_BASIC_AUTH_PASSWORD_ENV` | **Name** of an environment variable containing the webhook password |
| `HTTP_METRICS_BASIC_AUTH_USERNAME` | Independent `/metrics` username, with the same validation |
| `HTTP_METRICS_BASIC_AUTH_PASSWORD_ENV` | **Name** of the environment variable containing the metrics password |
| `HTTP_TLS_CERT_FILE` | Readable PEM certificate chain path |
| `HTTP_TLS_KEY_FILE` | Readable matching PEM private key path |

Each Basic Auth username must be configured with its corresponding
password **reference**, and each referenced variable must exist and
contain a nonempty password without control characters. Password
reference names must match `[a-zA-Z_][a-zA-Z0-9_]*`.

For example, `HTTP_WEBHOOK_BASIC_AUTH_PASSWORD_ENV` should contain
`ALERTMANAGER_WEBHOOK_PASSWORD`, **not** the password itself and not
`${ALERTMANAGER_WEBHOOK_PASSWORD}`. The service then reads the
password from that named environment variable.

Neither executable loads `.env` files automatically. The
[shared Bash example](examples/http-security.env) may be sourced
explicitly or adapted to systemd/container secret injection.

### TLS selection and validation

| Certificate setting | Key setting | Outcome |
|---|---|---|
| Unset | Unset | HTTP |
| Empty | Empty | HTTP |
| Unset | Empty (or vice versa) | HTTP |
| Valid certificate path | Matching readable key path | HTTPS |
| Set | Unset or empty | Startup error |
| Unset or empty | Set | Startup error |
| Invalid certificate/key | Configured | Startup error |

Explicit empty strings count as unset. An entirely empty Basic Auth
pair disables that endpoint's authentication. Partial pairs,
whitespace-only usernames/references, missing/empty password
variables, unsafe credentials, unreadable/malformed PEM files and
mismatched certificate/key pairs fail startup **before listening**.
Error messages do not expose resolved passwords or private-key
material.

The implementation verifies authentication per request using
fixed-size hashes and constant-time comparisons. Resolved
credentials are retained as digests; TLS key material remains in
process memory. Secrets still exist in the inherited process
environment. Restrict process/diagnostic access and key-file
permissions; never log environment dumps or Authorization headers.

Changing inbound credentials or TLS does **not** change SLA policy
hashes, epochs, frozen bindings or delivery identities. Do not
increment `policy_epoch` solely for inbound security rotation.

## Which endpoints are protected?

| Endpoint | Authentication if configured | Otherwise |
|---|---|---|
| Receiver `POST /webhook` | Webhook credentials | Existing webhook handler |
| SLA proxy configured `webhook_path` | Webhook credentials | Existing handler and method checks |
| Either service `GET /metrics` | Metrics credentials | Existing metrics handler |
| SLA proxy occurrence and delivery-work inspection routes in durable modes | Inspection credentials | Unauthenticated inspection |
| Either service `GET /healthz` | **No Basic Auth** | Liveness probe |
| Either service `GET /readyz` | **No Basic Auth** | Service readiness probe |

Restrict unauthenticated probe access at the network layer where appropriate.

Missing, malformed or incorrect credentials produce
`401 Unauthorized` with a `WWW-Authenticate` challenge for the configured
realm (`webhook`, `metrics`, or `inspection`). The protected handler is not invoked:
an unauthorized webhook does not change alert state, emit events
or increment application webhook-processing counters. A 401 is
an **ingress authentication failure**, not an SLA rejection or
delivery receipt. Webhook credentials do not authorize metrics
unless you deliberately configure identical credentials.

Native TLS uses Go's standard library, minimum TLS 1.2 and secure
defaults. Plain HTTP requests cannot reach application handlers
on an HTTPS listener. Clients must trust the certificate and
validate the hostname; private-CA or self-signed certificates
work if explicitly trusted. There is **no mTLS or insecure
verification mode**. Graceful shutdown and existing server
timeouts are unchanged.

## Configure Alertmanager and Prometheus

The following examples assume the SLA proxy at
`alerts.example.net:9012`, native HTTPS and independent credentials.
Paths in the first snippet refer to files on the **Alertmanager host**,
not the receiving service host:

```yaml
receivers:
  - name: sla-proxy
    webhook_configs:
      - url: https://alerts.example.net:9012/webhook
        send_resolved: true
        http_config:
          basic_auth:
            username: alertmanager
            password_file: /etc/alertmanager/secrets/webhook-password
          tls_config:
            ca_file: /etc/alertmanager/pki/ca.crt
```

Use `:9011/webhook` for the receiver. The password file must
contain the same password that the receiver process resolves
from its referenced environment variable.

Prometheus uses the **metrics** credentials:

```yaml
scrape_configs:
  - job_name: alert-sla-proxy
    scheme: https
    static_configs:
      - targets: ['alerts.example.net:9012']
    basic_auth:
      username: prometheus
      password_file: /etc/prometheus/secrets/metrics-password
    tls_config:
      ca_file: /etc/prometheus/pki/ca.crt
```

Validate these snippets in your complete Alertmanager/Prometheus
configuration. Prefer CA trust to disabling verification.

With a TLS-terminating reverse proxy, clients connect to the
proxy's hostname/certificate. Leave native TLS variables unset
if the isolated backend uses HTTP. Configure Authorization-header
forwarding deliberately and prevent direct access to the backend.

## Troubleshooting

| Symptom | Check |
|---|---|
| Startup security error | Both settings in each enabled pair, referenced secret variables in the **service** environment, readable/matching PEM files |
| `401 Unauthorized` | Correct endpoint credential pair, client Authorization header, restart after credential rotation |
| TLS trust or hostname error | Correct client CA and certificate SAN hostname; do not disable verification |
| Probes fail after enabling HTTPS | Update probes to HTTPS and trusted CA; Basic Auth is still bypassed |
| `/readyz` fails but TLS works | Check Valkey connectivity on the receiver. On the SLA proxy, check state store, policy and calendar readiness |
| Webhook accepted but no downstream delivery | Receiver OTLP or SLA outbound sink operations; inbound security does not configure either |

Shell exports are **not** automatically inherited by systemd or
Docker. For downstream investigations see
[event receiver operations](event-receiver/operations.md) or
[SLA proxy operations](sla-proxy/operations.md).
