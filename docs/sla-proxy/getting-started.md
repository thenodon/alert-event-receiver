# Run the SLA proxy locally

Start in **observe-only** mode to check labels and calendars without changing
state or sending notifications. Use **durable-state-only** to inspect scheduling
and restart behavior with Valkey. Choose **durable delivery** when ready to send
to configured destinations. Keep existing operational Alertmanager routes until
you have verified the new destinations.

| Mode | `durable.enabled` | `delivery.enabled` | Dependencies |
|---|---|---|---|
| `observe_only` | false | false | None |
| `durable_state_only` | true | false | Valkey |
| `delivery_enabled` | true | true | Valkey and configured destinations |

## Prerequisites and build

Install Go 1.26.11+, Bash and curl. Python 3 generates test webhook files; the
HTTP mock uses Go. Durable examples also use Docker.
Run commands from the repository root. Ports 9012, 6380 and 8080 must be free.

```bash
go build -o /tmp/alert-sla-proxy ./cmd/alert-sla-proxy
```

## Observe without dependencies

The [minimal observe-only example](../examples/alert-sla-proxy-observe.yaml)
contains just a label-selected, always-open UTC policy:

```yaml
sla_definitions:
  - sla_label: sla_id
    slas:
      demo:
        timezone: UTC
        schedule:
          monday: ["00:00-24:00"]
          tuesday: ["00:00-24:00"]
          wednesday: ["00:00-24:00"]
          thursday: ["00:00-24:00"]
          friday: ["00:00-24:00"]
          saturday: ["00:00-24:00"]
          sunday: ["00:00-24:00"]
```

In another terminal, start the service:

```bash
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-observe.yaml
```

Send a fresh test occurrence:

```bash
curl -fsS http://127.0.0.1:9012/readyz
python3 docs/examples/make-test-webhooks.py /tmp/sla-demo
curl -i http://127.0.0.1:9012/webhook   -H 'Content-Type: application/json' --data-binary @/tmp/sla-demo-firing.json
curl -i http://127.0.0.1:9012/webhook   -H 'Content-Type: application/json' --data-binary @/tmp/sla-demo-resolved.json
```

Expect HTTP 200, `mode: observe_only`, `persisted: false`, `forwarded: false`,
one selected alert and eligible observations. This mode creates no durable
schedule and never delivers to sinks. Missing or unknown SLA labels are counted
skips, so verify `selected`, not only HTTP status.

## Persist state without delivery

Stop the observe-only process with Ctrl-C. Start a dedicated disposable Valkey:

```bash
docker run --rm -d --name sla-demo-valkey -p 127.0.0.1:6380:6379   valkey/valkey:7.2 valkey-server --appendonly yes --appendfsync always
docker exec sla-demo-valkey valkey-cli PING
```

The [state-only example](../examples/alert-sla-proxy-durable.yaml) adds the Valkey
address, a dedicated namespace and `durable.enabled: true`. Runtime scheduler
and retention settings use defaults; no downstream credentials are needed.

```bash
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-durable.yaml
```

Generate a fresh pair of webhooks and send them using the commands above. Expect
schema version 3, `mode: durable_state_only`, `persisted: true` and occurrence
summaries. State survives restarting the process with the same configuration
while Valkey retains the data. Stop/start does not require a higher policy epoch.
Unresolved occurrences have no automatic expiry. This container has no persistent
volume, so stopping/removing it discards this demo's storage.

## Deliver to a local HTTP destination and audit log

Stop the state-only process. In a separate terminal, start the existing
[Go HTTP mock](../../cmd/httpsink-mock/main.go) with local demo credentials:

```bash
export MOCK_USERNAME=documentation-user
export MOCK_PASSWORD=documentation-password
go run ./cmd/httpsink-mock -listen 127.0.0.1:8080
```

The explicit listen address keeps the mock on loopback; its default is `:8080`
(all interfaces). It accepts JSON at `/inbound_event`, returns HTTP 200, and
exposes `/healthz`. Check it from another terminal:

```bash
curl -fsS http://127.0.0.1:8080/healthz
```

The mock prints the **full webhook payload**, including labels and annotations,
plus status, alert count and idempotency key. Use test data only. It is a local
verification tool, not a production destination. `-max-body` sets the request-body
limit (default 1048576 bytes). Basic Authentication is optional when both
`MOCK_USERNAME` and `MOCK_PASSWORD` are unset; setting only one fails startup.

Set matching credentials in the proxy's terminal. The two variable names below
are referenced by the proxy's YAML; the mock reads the separate `MOCK_*` variables:

```bash
export SERVICENOW_USERNAME=documentation-user
export SERVICENOW_PASSWORD=documentation-password
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-delivery-http.yaml
```

The [minimal HTTP delivery example](../examples/alert-sla-proxy-delivery-http.yaml)
uses Valkey on port 6380, a **different namespace** from the state-only example,
the local HTTP endpoint, and an independent `audit` log sink. Both enable flags
are true. Generate a fresh webhook pair. Send firing first and wait until the
mock prints it; then send the resolution file. Sending resolution before the
first firing is authorized can correctly cancel delivery of both notifications.

Verify the HTTP mock output, `SLA alert delivered` audit logs and metrics:

```bash
curl -fsS http://127.0.0.1:9012/metrics
```

Look for `sla_proxy_delivery_receipts_total` for each sink and each action.
`SLA HTTP request completed` with success reports sink acceptance; the subsequent
receipt commit is separate. The handler always returns `forwarded: false` because
delivery is asynchronous. An HTTP 200 webhook response does not prove delivery.

For a log-only trial, use
[alert-sla-proxy-delivery-log.yaml](../examples/alert-sla-proxy-delivery-log.yaml)
with the same Valkey server. It uses its own namespace and needs no HTTP mock.
Wait for the firing audit log before sending the resolution, as in the HTTP trial.

Production HTTP destinations should use HTTPS and credentials from a secret
mechanism. Replace the loopback URL explicitly; the generic Alertmanager payload
requires no ServiceNow-specific translation in this service.

## Add public holidays without country-specific code

Use [the ICS example](../examples/alert-sla-proxy-ics.yaml) after installing a pair:

```bash
install -d /etc/alert-sla-proxy/example-holidays
install -m 0644 internal/slaproxy/testdata/holidays/holidays_se.ics \
  /etc/alert-sla-proxy/example-holidays/holidays_se.ics
install -m 0644 internal/slaproxy/testdata/holidays/holidays_se.ics.json \
  /etc/alert-sla-proxy/example-holidays/holidays_se.ics.json
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-ics.yaml
```

Run installation with suitable privileges and stop the earlier proxy first. This
is a first install into an unused example directory; updates must switch a complete
version directory atomically. The policy name is `business-hours-se`, so change
the demo webhooks' `sla_id` accordingly. Holidays/business hours can make an
otherwise valid input pending or ineligible. The checked-in pair covers 2020–2040;
startup checks today's local date and the search horizon, not just test timestamps.
Generate another country/subdivision using the [offline workflow](holiday-datasets.md).
Python is used only during offline preparation, never by the Go service.

## Common startup and verification problems

| Symptom | Action |
|---|---|
| Config file not found | Pass `-config` explicitly; the default is `alert-sla-proxy.yaml` in the current directory |
| Strict YAML error | Use current keys; unknown/duplicate fields and extra YAML documents are rejected |
| ICS read/SHA/coverage failure | Install a valid complete pair with sufficient years; see [dataset diagnostics](holiday-datasets.md#troubleshoot) |
| Missing HTTP credentials | Set referenced variables when delivery is enabled; disabled delivery does not resolve them |
| Valkey unavailable | Check port 6380 and `PING`; durable startup requires connectivity |
| Epoch/catalog or destination conflict | Use the matching original configuration; follow [safe configuration changes](operations.md#change-policies-and-destinations) |
| HTTP 200 but no delivery | Check mode, selection, SLA eligibility, holds and receipts |
| `unknown_sla` | Demo webhooks use `sla_id: demo`; match the selected example's policy name |

Do not switch these examples over the same production namespace. Namespaces are
independent state domains, not migration tools. Changing an existing durable
catalog or enabling delivery requires a higher epoch and preserves frozen
historical destinations; see [operations](operations.md).

Stop the proxy and HTTP mock with Ctrl-C. After finishing all durable demos:

```bash
docker stop sla-demo-valkey
```

Continue with [concepts](concepts.md), [configuration](configuration.md),
[reference](reference.md) and [Valkey debugging](../redis-debugging.md).
