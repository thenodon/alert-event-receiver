# Run the event receiver locally

The receiver records alert transitions as OTLP log events. This walkthrough uses
Valkey for working state and VictoriaLogs as a local OTLP/HTTP destination.

## Prerequisites

Install Go 1.26.11+, Docker, Bash, curl and Python 3. Run commands from the
repository root. These disposable examples bind dependencies to loopback and
use ports 6379, 9428 and 9011; choose other ports if they are occupied.

## Build and start dependencies

```bash
go build -o /tmp/alert_event_receiver ./cmd/server
docker run --rm -d --name aer-valkey -p 127.0.0.1:6379:6379   valkey/valkey:7.2 valkey-server --appendonly yes --appendfsync always
docker run --rm -d --name aer-victorialogs -p 127.0.0.1:9428:9428   victoriametrics/victoria-logs:v1.54.0
```

The VictoriaLogs command uses the image version in its
[official quick start](https://docs.victoriametrics.com/victorialogs/quickstart/).
It is a local example, not a production version policy.

Wait for the dependencies:

```bash
docker exec aer-valkey valkey-cli PING
curl -fsS http://127.0.0.1:9428/health
```

Expect `PONG` and a successful health request. The container storage is disposable;
use persistent volumes and appropriate backend retention for a real deployment.

## Configure and start

The receiver uses environment variables, not YAML. In a separate terminal:

```bash
export REDIS_ADDR=127.0.0.1:6379
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:9428/insert/opentelemetry
/tmp/alert_event_receiver
```

These values are also in [event-receiver.env](../examples/event-receiver.env).
For gRPC or other backends, see the [configuration reference](configuration.md).

```bash
curl -fsS http://127.0.0.1:9011/healthz
```

This proves HTTP liveness, not ongoing Valkey or backend readiness.

## Send firing and resolved notifications

Generate a fresh fingerprint and current timestamps. Keep the generated files
for both requests so the resolution matches the firing occurrence:

```bash
python3 docs/examples/make-test-webhooks.py /tmp/aer-demo
curl -i http://127.0.0.1:9011/webhook   -H 'Content-Type: application/json' --data-binary @/tmp/aer-demo-firing.json
curl -i http://127.0.0.1:9011/webhook   -H 'Content-Type: application/json' --data-binary @/tmp/aer-demo-resolved.json
```

Expect HTTP 200 for both. Sending the same files again should not create new
transitions while the state is retained. Generate fresh files for a new test.

## Verify the result

```bash
curl -fsS http://127.0.0.1:9011/metrics
curl -fsS http://127.0.0.1:9428/select/logsql/query   --data-urlencode 'query=_time:15m alert.label.service:="documentation-demo"'
```

Allow a few seconds for asynchronous export, then repeat the read-only query if
needed. Look for firing and resolved records with the same fingerprint. The
fresh run's fingerprint is printed by the generator. Check
`alertreceiver_processing_outcomes_total`, `alertreceiver_events_generated_total`
and `alertreceiver_otlp_export_requests_total` in the metrics output.

Webhook 200 confirms successful processing, not log persistence. SDK submission
and exporter success are separate stages; the backend query establishes current
visibility only. See [delivery and monitoring](operations.md#delivery-semantics-and-failure-handling).

## Common startup problems

| Symptom | Check |
|---|---|
| Valkey ping fails; process exits | Dependency is running; `REDIS_ADDR`, password and database are correct |
| Address already in use | Set `ADDRESS=127.0.0.1:9013` and use that port for requests |
| OTLP initialization fails | Protocol, endpoint, headers and certificate settings |
| Export errors after startup | OTLP backend availability; use HTTP protocol with the HTTP endpoint above |
| Webhook returns 400 | Valid JSON, nonempty fingerprint/startsAt, and resolved endsAt at or after startsAt |
| No new events when repeating the demo | Generate a fresh occurrence; repeats deliberately suppress emission |

Stop the receiver with Ctrl-C to allow bounded HTTP draining and SDK flushing.
When finished, stop only the dependencies created here:

```bash
docker stop aer-valkey aer-victorialogs
```

Continue with [concepts](concepts.md), [operations](operations.md),
[reference](reference.md) or [Valkey inspection](../redis-debugging.md).
