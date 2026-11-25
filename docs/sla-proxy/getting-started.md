# Run the SLA proxy locally

Start in **observe-only** mode to check labels and calendars without changing
state or sending notifications. Use **durable-state-only** to inspect scheduling
and restart behavior with Valkey. Choose **durable delivery** when ready to send
to configured destinations. Keep existing operational Alertmanager routes until
you have verified the new destinations.

| Mode (response value) | `durable.enabled` | `delivery.enabled` | Dependencies |
|---|---|---|---|
| Observe-only (`observe_only`) | false | false | None |
| Durable-state-only (`durable_state_only`) | true | false | Valkey |
| Durable delivery (`delivery_enabled`) | true | true | Valkey and configured destinations |

The three modes let you validate the SLA Proxy incrementally. Observe-only verifies
policy selection and calendar evaluation without persistence. Durable-state-only
adds lifecycle tracking and scheduling but does not contact destinations. Durable
delivery enables the asynchronous dispatcher, which sends authorized notifications
and records per-destination delivery receipts.

## Prerequisites and build

Install Go 1.26.11+, Bash and curl. Python 3 generates test webhook files; the
HTTP mock uses Go. Durable examples also use Docker.

Run all commands from the repository root. Ports 9012, 6380 and 8080 must be free.

```bash
go build -o /tmp/alert-sla-proxy ./cmd/alert-sla-proxy
```

## Observe without dependencies

The [minimal observe-only example](../examples/alert-sla-proxy-observe.yaml)
contains a label-selected, always-open UTC policy:

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

Start the service in one terminal:

```bash
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-observe.yaml
```

In another terminal, check readiness, generate a fresh test occurrence and send
its firing and resolved notifications:

```bash
curl -fsS http://127.0.0.1:9012/readyz
python3 docs/examples/make-test-webhooks.py /tmp/sla-demo
curl -i http://127.0.0.1:9012/webhook \
  -H 'Content-Type: application/json' \
  --data-binary @/tmp/sla-demo-firing.json
curl -i http://127.0.0.1:9012/webhook \
  -H 'Content-Type: application/json' \
  --data-binary @/tmp/sla-demo-resolved.json
```
The proxy should return HTTP 200 for both test webhooks. The response should indicate:

- `mode: observe_only` — the proxy is running in observation mode.
- `selected: 1` — the test alert matched the configured `demo` SLA policy.
- `persisted: false` — no alert state was stored in Valkey.
- `forwarded: false` — no notification was sent to an external destination.

Because the example SLA schedule is open 24 hours a day, the selected alert should be evaluated as eligible.

**Observe-only mode evaluates SLA policies and schedules without storing alert state or delivering notifications.** It is useful for verifying that alerts match the intended SLA policies before enabling durable processing and delivery.

An HTTP 200 response alone does not mean that an alert matched an SLA policy. For example, an alert with a missing or unknown `sla_id` label may be accepted but skipped. Check the `selected` field to verify that the expected alert was selected.

## Persist state without delivery

Stop the observe-only process with Ctrl-C. Start a dedicated disposable Valkey:

```bash
docker run --rm -d --name sla-demo-valkey \
  -p 127.0.0.1:6380:6379 \
  valkey/valkey:7.2 valkey-server --appendonly yes --appendfsync always
docker exec sla-demo-valkey valkey-cli PING
```

The [state-only example](../examples/alert-sla-proxy-durable.yaml) adds the Valkey
address, a dedicated namespace and `durable.enabled: true`. Runtime scheduler
and retention settings use defaults; no downstream credentials are needed.

```bash
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-durable.yaml
```

Generate a **fresh** pair of webhooks with the Python command above, then send
both using the same `curl` commands. Fresh occurrences are useful in durable
mode because previously accepted occurrences retain their identity and state;
replaying an old pair is not a new lifecycle test.

After sending the firing and resolved test webhooks, both requests should return HTTP 200.

The responses should contain:

- `mode: durable_state_only` — the proxy is storing alert lifecycle information without delivering notifications.
- `persisted: true` — all selected alerts in the request were successfully written to Redis/Valkey.
- `occurrences` — summaries of the alert occurrences stored or updated.

Unlike observe-only mode, **durable-state-only mode stores alert information in Redis/Valkey**. However, it does not send notifications to any external destination.

You can restart the SLA proxy without losing its stored state, provided Redis/Valkey retains the data, and you restart with the same configuration.

**Note:** The Valkey container used in this example has no persistent storage volume. Removing the container deletes its stored data. For production, configure Valkey persistence and appropriate storage.

For details about the HTTP response fields, see [HTTP response reference](reference.md#durable-responses).

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
`MOCK_USERNAME` and `MOCK_PASSWORD` are set; setting only one fails startup.

Set matching credentials in the proxy's terminal. The two variable names below
are referenced by the proxy's YAML; the mock reads the separate `MOCK_*` variables:

```bash
export MOCK_USERNAME=documentation-user
export MOCK_PASSWORD=documentation-password
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-delivery-http.yaml
```

The [minimal HTTP delivery example](../examples/alert-sla-proxy-delivery-http.yaml)
uses Valkey on port 6380, a **different namespace** from the state-only example,
the local HTTP endpoint, and an independent `audit` log sink. Both enable flags
are true.

Generate a fresh webhook pair and send **only the firing notification** first,
using the Python and firing `curl` commands from the observe-only example.
Wait until the HTTP mock reports the firing request **and** the SLA Proxy has
recorded a successful firing delivery receipt for the HTTP destination. Then
send the resolved notification with the resolved `curl` command.

The mock's HTTP 200 confirms sink acceptance, but the Valkey receipt is committed
separately. Check the proxy's receipt-commit logs and the delivery receipt metric;
do not rely solely on the mock output. For this fresh demonstration occurrence,
a successful firing receipt should be visible for the HTTP sink before you send
its resolution. See [investigate an occurrence](operations.md#investigate-an-occurrence)
for checking an individual occurrence and its binding receipts.

Verify the HTTP mock output, `SLA alert delivered` audit logs and metrics:

```bash
curl -fsS http://127.0.0.1:9012/metrics
```

Look for `sla_proxy_delivery_receipts_total` for each sink and action. The metric
is a counter aggregated by sink and action, not an occurrence-specific receipt;
use the receipt-commit logs or Valkey inspection to correlate a particular test.
`SLA HTTP request completed` with success reports sink acceptance, while receipt
commit is separate. The webhook handler returns `forwarded: false` because
delivery is asynchronous. HTTP 200 from the webhook does not prove delivery.
Sending resolution before first firing authorization can correctly cancel
both notifications; resolution after authorization can race an in-progress
firing delivery. Waiting for the committed firing receipt avoids that race in
this walkthrough.

For a log-only trial, use
[alert-sla-proxy-delivery-log.yaml](../examples/alert-sla-proxy-delivery-log.yaml)
with the same Valkey server. It uses its own namespace and needs no HTTP mock.
Wait for the log sink's committed firing receipt before sending the resolution,
just as in the HTTP trial; an emitted audit record alone is not the receipt.

## Add public holidays without country-specific code

Use [the ICS example](../examples/alert-sla-proxy-ics.yaml) after installing a
matching dataset and manifest pair:

```bash
install -d /etc/alert-sla-proxy/example-holidays
install -m 0644 internal/slaproxy/testdata/holidays/holidays_se.ics \
  /etc/alert-sla-proxy/example-holidays/holidays_se.ics
install -m 0644 internal/slaproxy/testdata/holidays/holidays_se.ics.json \
  /etc/alert-sla-proxy/example-holidays/holidays_se.ics.json
/tmp/alert-sla-proxy -config docs/examples/alert-sla-proxy-ics.yaml
```

Stop the previously running SLA proxy before starting this example. The installation commands may require elevated privileges to write to `/etc/alert-sla-proxy`.

### Test the holiday-aware SLA policy

The ICS example uses an SLA policy named `business-hours-se`, which applies Swedish public holidays and business-hour restrictions.

The test webhook generator used earlier creates alerts with `sla_id: demo`. To test the new policy:

1. Generate a fresh pair of firing and resolved webhooks using the earlier Python command.
2. In both generated JSON files, change `sla_id` from `demo` to `business-hours-se`.
3. Send the webhooks to the proxy using the same `curl` commands as before.

Because this policy includes business hours and public holidays, the results depend on the alert timestamps. An observation may be `eligible`, `pending`, `ineligible` or `unknown`. These results do not necessarily indicate an error.

The included Swedish holiday dataset covers 2020–2040. For other countries or regions, or to generate updated holiday data, see [Holiday datasets](holiday-datasets.md).

## Common startup and verification problems

| Symptom | Action |
|---|---|
| Config file not found | Pass `-config` explicitly; the default is `alert-sla-proxy.yaml` in the current directory |
| Strict YAML error | Use current keys; unknown/duplicate fields and extra YAML documents are rejected |
| ICS read/SHA/coverage failure | Install a valid complete pair with sufficient years; see [dataset diagnostics](holiday-datasets.md#troubleshoot) |
| Missing HTTP credentials | Set referenced variables when delivery is enabled; disabled delivery does not resolve them |
| Valkey unavailable | Check port 6380 and `PING`; durable startup requires connectivity |
| Epoch/catalog or destination conflict | Use the matching original configuration; follow [safe configuration changes](operations.md#change-policies-and-destinations) |
| HTTP 200 but no delivery | Check mode, selection, SLA eligibility, holds, authorization and receipts; see [occurrence investigation](operations.md#investigate-an-occurrence) |
| `unknown_sla` | Demo webhooks use `sla_id: demo`; match the selected example's policy name |

The examples that use Redis/Valkey are configured with separate namespaces to keep their stored state independent.

When testing different configurations, keep these namespaces separate. Do not reuse an existing production namespace for the examples.

For production systems, changes to SLA policies or delivery configuration may require updating the policy epoch. See [Changing policies and destinations](operations.md#change-policies-and-destinations).

Stop the proxy and HTTP mock with Ctrl-C. After finishing all durable demos:

```bash
docker stop sla-demo-valkey
```

Continue with [concepts](concepts.md), [configuration](configuration.md),
[reference](reference.md) and [Valkey debugging](redis-debugging.md).
