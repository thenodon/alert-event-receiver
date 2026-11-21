# Run the event receiver locally

[Concepts](concepts.md) · [Configuration](configuration.md) · [Operations](operations.md) · [Reference](reference.md)

This walkthrough runs the event receiver with **Valkey** for working lifecycle
state and **VictoriaLogs** as a local OTLP/HTTP destination. You will send one
firing notification and its matching resolution, then verify the resulting log
events.

The example is intended for local testing, not production deployment.

## Prerequisites

Install Go 1.26.11+, Docker, Bash, curl and Python 3. Run all commands from the
**repository root**. The example uses these local ports:

| Port | Service | Purpose |
|---|---|---|
| `6379` | Valkey | Working lifecycle state and idempotency |
| `9428` | VictoriaLogs | OTLP/HTTP ingestion and LogsQL queries |
| `9011` | Event receiver | Webhook, metrics and health endpoints |

The dependencies bind to loopback. Choose different ports if any are already
in use.

## 1. Build and start dependencies

Build the receiver:

```bash
go build -o /tmp/alert_event_receiver ./cmd/server
```

Start disposable Valkey and VictoriaLogs containers:

```bash
docker run --rm -d --name aer-valkey \
  -p 127.0.0.1:6379:6379 \
  valkey/valkey:7.2 \
  valkey-server --appendonly yes --appendfsync always

docker run --rm -d --name aer-victorialogs \
  -p 127.0.0.1:9428:9428 \
  victoriametrics/victoria-logs:v1.54.0
```

The VictoriaLogs image version follows its
[official quick start](https://docs.victoriametrics.com/victorialogs/quickstart/);
it is not a production version recommendation.

Check that both dependencies are responding:

```bash
docker exec aer-valkey valkey-cli PING
curl -fsS http://127.0.0.1:9428/health
```

Expect `PONG` from Valkey and a successful HTTP health request from VictoriaLogs.
These containers use disposable storage. For production, configure persistent
volumes, suitable retention and access controls.

## 2. Configure and start the receiver

The receiver reads **environment variables at startup**, not YAML. In a
**separate terminal**, run:

```bash
export REDIS_ADDR=127.0.0.1:6379
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:9428/insert/opentelemetry

/tmp/alert_event_receiver
```

Keep this terminal open while testing. These settings also appear in
[event-receiver.env](../examples/event-receiver.env). For gRPC, TLS or
other backends, see [configuration](configuration.md).

From another terminal, check the receiver's health endpoint:

```bash
curl -fsS http://127.0.0.1:9011/healthz
```

A successful response confirms that the HTTP listener is alive. **It does
not verify ongoing Valkey or OTLP backend readiness.** The event receiver
has no separate readiness endpoint.

## 3. Send firing and resolved notifications

Generate test webhooks with a fresh fingerprint and current timestamps:

```bash
python3 docs/examples/make-test-webhooks.py /tmp/aer-demo
```

The generator creates a matching pair of files. Use **the same pair** for
firing and resolution so that the fingerprint and `startsAt` identify the
same occurrence.

Send the firing notification:

```bash
curl -i http://127.0.0.1:9011/webhook \
  -H 'Content-Type: application/json' \
  --data-binary @/tmp/aer-demo-firing.json
```

Then send the resolution:

```bash
curl -i http://127.0.0.1:9011/webhook \
  -H 'Content-Type: application/json' \
  --data-binary @/tmp/aer-demo-resolved.json
```

Both requests should return **HTTP 200**. Sending the same files again
should not generate new lifecycle transitions while the relevant state is
retained. To test a new occurrence, generate a fresh pair of files.

## 4. Verify lifecycle processing and exported logs

Inspect the receiver metrics:

```bash
curl -fsS http://127.0.0.1:9011/metrics
```

Look for these metric families:

| Metric | What to check |
|---|---|
| `alertreceiver_processing_outcomes_total` | Lifecycle processing results |
| `alertreceiver_events_generated_total` | Firing and resolved transitions created |
| `alertreceiver_otlp_export_requests_total` | OTLP exporter calls and their results |

Query VictoriaLogs:

```bash
curl -fsS http://127.0.0.1:9428/select/logsql/query \
  --data-urlencode 'query=_time:15m alert.label.service:="documentation-demo"'
```

Allow a few seconds for asynchronous export. Repeat the read-only query if
needed. You should find **firing and resolved** lifecycle records with the
same fingerprint. The test generator prints that fingerprint.

**What verification proves:** HTTP 200 means webhook processing succeeded,
not that the event is persisted in VictoriaLogs. The receiver can generate
an event and submit it to the OpenTelemetry SDK without the exporter
succeeding. Exporter success, in turn, does not prove final backend
persistence. Query results establish current visibility only. See
[delivery semantics](operations.md#trace-a-webhook-through-processing-and-delivery).

## Common problems

| Symptom | Check |
|---|---|
| Receiver exits after a Valkey ping failure | Confirm the container is running and `REDIS_ADDR`, password and database are correct |
| Listener address already in use | Set `ADDRESS=127.0.0.1:9013` and use port `9013` in subsequent requests |
| OTLP initialization fails | Check protocol, endpoint, headers and certificate settings |
| Export errors after startup | Check OTLP backend availability and that HTTP protocol uses the HTTP endpoint above |
| Webhook returns HTTP 400 | Check JSON, nonempty fingerprint and `startsAt`; resolved alerts require `endsAt >= startsAt` |
| No new events when repeating the demo | Expected for repeats; generate a fresh occurrence |

For more detailed failure diagnosis, see [operations](operations.md).

## Stop and clean up

Stop the receiver with **Ctrl-C** in its terminal. This allows bounded
HTTP draining and SDK flushing.

Then stop only the two containers created in this walkthrough:

```bash
docker stop aer-valkey aer-victorialogs
```

Continue with [concepts](concepts.md) for lifecycle behavior,
[configuration](configuration.md) for deployment settings,
[reference](reference.md) for event fields and metrics, or
[Valkey inspection](../redis-debugging.md) for stored state.
