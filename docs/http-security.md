# Secure inbound HTTP endpoints

Both applications use the same environment-only security implementation. Defaults
remain HTTP with no authentication. The existing address and port serve either
HTTP or HTTPS, with no second port or HTTP-to-HTTPS redirect. The SLA proxy has
**no inbound security YAML fields**; outbound sink authentication/TLS is separate.

**Basic Auth over cleartext HTTP exposes credentials on untrusted networks.**
Basic Authentication works over both HTTP and HTTPS; plain HTTP does not protect
credentials from interception. Use native HTTPS or a trusted TLS-terminating
reverse proxy such as Envoy with an isolated
backend network. Native TLS is optional so trusted proxy deployments can keep
HTTP on the backend. Basic Auth provides no rate limiting or account management.

## Shared configuration

These are the same six settings in both executables. All are optional strings,
empty by default, read once at startup:

| Environment variable | Purpose and validation |
|---|---|
| `HTTP_WEBHOOK_BASIC_AUTH_USERNAME` | Webhook username; configure together with its password reference. Nonblank, no colon or control characters |
| `HTTP_WEBHOOK_BASIC_AUTH_PASSWORD_ENV` | Bare environment variable **name**, e.g. `ALERTMANAGER_WEBHOOK_PASSWORD`; that variable must exist and contain a nonempty password without control characters |
| `HTTP_METRICS_BASIC_AUTH_USERNAME` | Independent `/metrics` username; same validation as webhook username |
| `HTTP_METRICS_BASIC_AUTH_PASSWORD_ENV` | Bare environment variable name for the metrics password; same validation as webhook reference |
| `HTTP_TLS_CERT_FILE` | Readable PEM server certificate chain; configure together with key path |
| `HTTP_TLS_KEY_FILE` | Readable matching PEM private key; protect file permissions and directory access |

Password reference names must match `[a-zA-Z_][a-zA-Z0-9_]*`. Do not put a password
or `${NAME}` interpolation in a `PASSWORD_ENV` setting. Neither application loads
an `.env` file itself. The [shared Bash example](examples/http-security.env) may be
sourced explicitly or adapted to a service manager/container secret environment.

Explicitly empty values are treated as unset. TLS selection follows this table:

| `HTTP_TLS_CERT_FILE` | `HTTP_TLS_KEY_FILE` | Result |
|---|---|---|
| Unset | Unset | HTTP |
| Empty | Empty | HTTP |
| Valid certificate path | Valid matching key path | HTTPS |
| Set | Unset or empty | Startup error |
| Unset or empty | Set | Startup error |
| Invalid certificate/key | Configured | Startup error |

Mixed unset/empty TLS values also select HTTP. Both variables must contain valid
paths to a readable, matching certificate/key pair to enable HTTPS.

An entirely empty pair disables that feature. A partial pair, whitespace-only
username/reference, missing/empty referenced password, unsafe credential, unreadable
or malformed PEM, or mismatched certificate/key fails startup before listening.
Errors do not include resolved passwords or private-key material. Authentication
is verified per request with fixed-size hashes and constant-time comparisons.

Certificates and Basic Auth credentials are loaded at startup. The service retains
a startup snapshot. Restart after changing passwords,
references or certificate/key contents; there is no hot reload. These settings
have no effect on SLA policy hashes, epochs, frozen bindings or delivery identities.
Do not bump a policy epoch solely for inbound security rotation.

Secrets remain in the inherited process environment; resolved credentials are
retained as digests and TLS key material remains in process memory. Limit service
and diagnostic access, keep key files readable only by the service/trusted
administrators, and avoid logging environment dumps or Authorization headers.

## Endpoint behavior

| Route | Authentication when configured | When not configured |
|---|---|---|
| Receiver `POST /webhook` | Webhook credentials | Existing webhook handler |
| SLA proxy configured `webhook_path` | Webhook credentials | Existing webhook handler, including its method check |
| Either service `GET /metrics` | Metrics credentials | Existing metrics handler |
| Either service `GET /healthz` | Always bypasses Basic Auth | Liveness probe |
| SLA proxy `GET /readyz` | Always bypasses Basic Auth | Existing readiness checks |

All routes use the listener's configured protocol, including probes. Restrict
probe access at the network layer if necessary. Unknown routes and existing
method routing are unchanged; the receiver does not gain a `/readyz` endpoint.
Missing, malformed or wrong credentials return `401 Unauthorized` and
`WWW-Authenticate: Basic realm="webhook"` (or `"metrics"`). The protected handler
is not called: unauthorized webhooks do not process alerts, change state, emit
events or increment application webhook-processing counters. A 401 is an ingress
failure, not an SLA rejection or delivery receipt. Webhook credentials do not
authorize metrics unless operators deliberately configure the same pair for both.

TLS uses Go's standard library with minimum TLS 1.2 and secure defaults. Plain
HTTP requests to an HTTPS listener cannot reach application handlers. Clients
must trust the server certificate and validate its hostname; self-signed/private
CA certificates work when explicitly trusted. No mTLS or insecure verification
mode is provided. Graceful shutdown and existing server timeouts are unchanged.

## Start with HTTP or HTTPS

For a local HTTP/no-auth deployment, leave all six variables unset. For example,
in a fresh shell with no security environment inherited:

```bash
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-observe.yaml
curl --fail http://127.0.0.1:9012/healthz
```

For HTTPS with independent credentials, use real files provisioned by your PKI
and a secret manager or a hidden prompt; example paths are placeholders:

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

Use the same environment with `/tmp/alert_event_receiver` after configuring its
Valkey/OTLP dependencies. Its default port is 9011; the proxy's is 9012. Certificate
SANs must cover the hostname/IP used by Alertmanager, Prometheus and curl.

From a second terminal, adapting host, port and CA file (curl prompts for the
password with `-u username`, avoiding a real password in the command line):

```bash
# Expected 200 without authentication; the TLS CA still needs to be trusted.
curl --cacert /etc/alert-http/ca.crt https://alerts.example.net:9012/healthz
curl --cacert /etc/alert-http/ca.crt https://alerts.example.net:9012/readyz
# Expected 401 and WWW-Authenticate: Basic realm="metrics".
curl -i --cacert /etc/alert-http/ca.crt https://alerts.example.net:9012/metrics
# Expected 200 after entering the independent metrics password.
curl --fail --cacert /etc/alert-http/ca.crt -u prometheus https://alerts.example.net:9012/metrics
# Generate a fresh payload and authenticate to the webhook (default path shown).
python3 docs/examples/make-test-webhooks.py /tmp/http-security-webhook
curl --fail --cacert /etc/alert-http/ca.crt -u alertmanager \
  -H 'Content-Type: application/json' --data-binary @/tmp/http-security-webhook-firing.json \
  https://alerts.example.net:9012/webhook
```

For the receiver, change the port to 9011 and omit the `/readyz` check. For a
custom proxy webhook path, change the URL accordingly. Without auth the same
requests use no `-u`; without TLS use `http://` and omit `--cacert`.

## Alertmanager and Prometheus

Configure Alertmanager to authenticate to the webhook and trust your private CA.
Paths below are on the **Alertmanager host**, not the receiving application host:

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

Use port 9011 for the event receiver, and its same fixed `/webhook` path. The
password file must contain the password referenced by the receiving process.
Prometheus uses the independent metrics credentials:

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

Validate these snippets in your complete Alertmanager/Prometheus configuration.
Use CA trust rather than disabling verification. With a TLS-terminating reverse
proxy, client URLs and certificates refer to that proxy; leave the native TLS pair
unset if the isolated backend uses HTTP. Configure proxy header forwarding for
Basic Auth deliberately and protect access to the backend listener.

## Troubleshoot startup and access

- Security startup error: check that each configured pair is complete, referenced
  secrets exist in the service/container environment, and PEM files match and are
  readable. Shell exports are not automatically inherited by systemd or Docker.
- 401: check the endpoint's credential pair, client Authorization configuration,
  and whether the service restarted after rotation. No state was processed.
- TLS trust/hostname failure: install the correct CA on the client and use the
  certificate's SAN hostname; do not disable verification.
- Health/readiness fails only after enabling TLS: update probes to use HTTPS and
  suitable CA trust. Basic Auth remains bypassed; application readiness can still
  fail for Valkey, policy or calendar reasons.
- Webhook succeeds but delivery fails: investigate OTLP or outbound sinks in the
  application operations guide. Inbound HTTPS/authentication does not secure or
  configure those separate connections.
