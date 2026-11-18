# Configuration and test examples

Run commands from the repository root. All destinations here are local test
fixtures or clearly marked placeholders. Configure persistence, authentication,
network access and HTTPS for production; demo namespaces are not migrations.

| File | Purpose | Prerequisites |
|---|---|---|
| [http-security.env](http-security.env) | Optional shared inbound HTTPS and Basic Auth for either application | Certificate/key pair and separately supplied secret environment |
| [event-receiver.env](event-receiver.env) | Minimal Bash environment for the receiver | Local Valkey and VictoriaLogs |
| [alert-sla-proxy-ics.yaml](alert-sla-proxy-ics.yaml) | File-backed holidays, independent of compiled countries | Install the checked-in ICS/manifest pair or generate another dataset offline |
| [alert-sla-proxy-observe.yaml](alert-sla-proxy-observe.yaml) | Minimal stateless policy observation | None |
| [alert-sla-proxy-durable.yaml](alert-sla-proxy-durable.yaml) | Minimal durable state without delivery | Valkey at 127.0.0.1:6380 |
| [alert-sla-proxy-delivery-log.yaml](alert-sla-proxy-delivery-log.yaml) | Minimal log delivery | Same Valkey, independent namespace |
| [alert-sla-proxy-delivery-http.yaml](alert-sla-proxy-delivery-http.yaml) | HTTP delivery and audit log | Same Valkey, Go HTTP mock, demo credential variables |
| [alert-sla-proxy.yaml](alert-sla-proxy.yaml) | Complete annotated reference; defaults to observe-only | Configure Valkey and placeholder HTTP endpoint before enabling |
| [alert-sla-proxy-vgr.yaml](alert-sla-proxy-vgr.yaml) | Environment-specific bronze/silver/gold policies | Review labels, endpoint, credentials and Valkey before use |
| [alertload-mixed.json](alertload-mixed.json), [alertload-mixed.csv](alertload-mixed.csv) | Equivalent weighted load profiles | [alertload guide](../alertload.md) |
| [make-test-webhooks.py](make-test-webhooks.py) | Fresh current-time firing/resolved JSON pair, `sla_id: demo` | Python 3 |
| [Go HTTP mock](../../cmd/httpsink-mock/main.go) | Local HTTP 200 destination; prints full payload and idempotency key | Go; use `-listen 127.0.0.1:8080` and matching `MOCK_*` credentials |

The executable does not load `.env` files automatically. With Bash you can load
the trusted receiver example explicitly:

```bash
source docs/examples/event-receiver.env
/tmp/alert_event_receiver
```

SLA examples each specify a complete standalone configuration, not mergeable
fragments. Their separate demo namespaces prevent catalog conflicts between
walkthroughs. Do not copy them over an existing production namespace without
following [safe configuration changes](../sla-proxy/operations.md#change-policies-and-destinations).

Minimal policies are always-open UTC so examples work on any weekday. The complete
example demonstrates business hours, optional holidays and independent label
namespaces. Its policy names differ from the minimal `demo` policy: adjust test
labels accordingly, rather than treating a successful skip response as delivery.

[Offline ICS workflow](../sla-proxy/holiday-datasets.md) ·
[Real ICS/manifest fixtures](../../internal/slaproxy/testdata/holidays/README.md)
