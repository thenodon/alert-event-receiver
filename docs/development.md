# Development and documentation maintenance

User documentation starts at the [documentation index](README.md).
[Specifications](specifications/README.md) preserve design decisions, implementation
history and benchmark reports; they are not deployment prerequisites.

## Build

Run from the repository root with Go 1.26.11+ (the version pinned in CI and images):

```bash
go build -o /tmp/alert_event_receiver ./cmd/server
go build -o /tmp/alert-sla-proxy ./cmd/alert-sla-proxy
go build -o /tmp/alertload ./cmd/alertload
```

`-version` prints the injected version, or `unknown` when not set. The two services
use independent `main.version` variables. A source build can inject a version:

```bash
go build -ldflags '-X main.version=documentation-test'   -o /tmp/alert-sla-proxy ./cmd/alert-sla-proxy
/tmp/alert-sla-proxy -version
```

The current Dockerfile and release artifact build only the receiver. Do not assume
an SLA proxy image or release artifact exists. The Dockerfile's `EXPOSE 8080` does
not change the receiver default listener `:9011`; map the actual listener. The
release workflow's linker target differs from `main.version`; verify release
version output rather than assuming it was injected. These packaging issues are
outside the documentation restructure and application source is unchanged.

## Tests and checks

```bash
go test ./...
go test -race ./internal/slaproxy/...
go vet ./...
govulncheck ./...
```

`govulncheck` is an external tool installed by CI; it may not be installed locally.
For broader race coverage use `go test -race ./...`.

Real-Valkey SLA integration tests require native `valkey-server` on PATH, or
`SLA_PROXY_VALKEY_SERVER` set to an executable path:

```bash
go test -tags=integration ./internal/slaproxy/...
# Optional additional concurrency validation:
go test -race -tags=integration ./internal/slaproxy/...
```

Fixtures launch their own loopback servers and temporary AOF directories with
appendfsync always; scripting is denied. They never use an existing user database.
A missing executable fails tests rather than skipping them. Network/filesystem
sandboxes may require permission for local listeners and child processes.
Receiver telemetry integration tests use local HTTP/gRPC fixtures rather than a
real log backend. Backend query visibility is a separate manual validation.

## Documentation validation

```bash
python3 docs/validate.py
python3 docs/validate.py --examples
git diff --check
```

The first checks local Markdown links/anchors, fenced JSON and shell syntax, and
syntax of documentation Python helpers. It does not fetch external links or run
shell examples. `--examples` additionally runs a temporary Go test through an
overlay to call the actual SLA configuration loader and calendar validator for
**every YAML example**, plus the alertload profile loader for both scenario files.
The temporary test source lives outside the checkout; no application/test source
is edited. This check initializes no sinks or Valkey state and needs no credentials.

Use the configuration references as the canonical source of option explanations.
Keep minimal examples small, keep complete annotations in the full example, and
update focused example tests/validation when file locations change. Historical
specifications describe their own stage; update notices and links, not their
recorded decisions, when current behavior moves on.

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/server` | Event receiver executable |
| `cmd/alert-sla-proxy` | Independent SLA executable |
| `cmd/alertload` | Webhook load generator |
| `internal/models` | Shared Alertmanager wire types and lifecycle event model |
| `internal/processor`, `internal/state`, `internal/webhook` | Receiver lifecycle, working state and handler |
| `internal/telemetry` | Receiver OTLP, SDK diagnostics and metrics |
| `internal/slaproxy` | Independent calendar, durable state, dispatcher and HTTP service |
| `internal/alertload` | Deterministic generator, bounded scheduler and verification |
| `grafana` | Existing dashboard artifacts |
| `docs/specifications`, `docs/benchmarks` | Designs and supporting measurements |

The SLA proxy reuses wire types and general libraries, not the receiver processor,
state store or OTLP telemetry as its delivery engine. Preserve separate semantics.
