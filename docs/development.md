# Development and documentation maintenance

[Documentation index](README.md) · [Specifications and implementation history](specifications/README.md)

This guide covers source builds, tests, documentation checks and repository
maintenance. Commands run from the **repository root** unless stated otherwise.
Specifications preserve design decisions, historical implementation details and
benchmarks; they are not deployment prerequisites.

## Build the executables

Use **Go 1.26.11 or newer**, matching the version pinned in CI and images.

| Executable | Source |
|---|---|
| `alert_event_receiver` | `cmd/server` |
| `alert-sla-proxy` | `cmd/alert-sla-proxy` |
| `alertload` | `cmd/alertload` |
| `httpsink-mock` | `cmd/httpsink-mock` |

```bash
go build -o /tmp/alert_event_receiver ./cmd/server
go build -o /tmp/alert-sla-proxy ./cmd/alert-sla-proxy
go build -o /tmp/alertload ./cmd/alertload
go build -o /tmp/httpsink-mock ./cmd/httpsink-mock
```

`-version` prints the injected version, or `unknown` if no version was
injected. The two services have separate `main.version` variables.
For example:

```bash
go build -ldflags '-X main.version=documentation-test' \
  -o /tmp/alert-sla-proxy ./cmd/alert-sla-proxy
/tmp/alert-sla-proxy -version
```

### Packaging limitations

The current Dockerfile and release artifact build **only the event
receiver**. Do not assume a published SLA proxy image or release
artifact exists. The Dockerfile's `EXPOSE 8080` does not change the
receiver's default listener `:9011`; map the actual listener.

The release workflow uses a linker target different from `main.version`.
Verify the version reported by a release binary rather than assuming
it was injected. These are packaging limitations; documentation changes
do not alter application source or release behavior.

## Run tests and static checks

```bash
go test ./...
go test -race ./internal/slaproxy/...
go vet ./...
govulncheck ./...
```

`govulncheck` is installed in CI but may need to be installed locally.
For broader race detection, run `go test -race ./...`.

### Real-Valkey SLA integration tests

These tests require `valkey-server` on `PATH`, or
`SLA_PROXY_VALKEY_SERVER` pointing to its executable:

```bash
go test -tags=integration ./internal/slaproxy/...
go test -race -tags=integration ./internal/slaproxy/...
```

The second command adds optional concurrency validation. Test fixtures
launch their own loopback Valkey servers with temporary AOF directories
(`appendfsync always`) and deny scripting. They do **not** use an
existing user database. A missing Valkey executable causes test
failure rather than a skip. Restricted sandboxes may need permission
to bind local listeners and launch child processes.

Receiver telemetry integration tests use local HTTP/gRPC fixtures,
not a real log backend. Backend query visibility requires separate
manual validation; see [alertload verification](alertload.md#optional-victorialogs-visibility-check).

## Validate documentation

Run after changing Markdown, examples or documentation helpers:

```bash
python3 docs/validate.py
python3 docs/validate.py --examples
git diff --check
```

| Check | What it verifies |
|---|---|
| `docs/validate.py` | Local Markdown links and heading anchors; fenced JSON and shell syntax; syntax of documentation Python helpers |
| `docs/validate.py --examples` | Above checks plus temporary Go overlay tests against every SLA YAML example and both alertload scenario files |
| `git diff --check` | Whitespace errors in the working-tree diff |

The basic validator does **not** fetch external links or execute shell
examples. The examples check invokes the real SLA configuration
loader and calendar validator, plus the alertload profile loader.
It writes temporary Go test sources **outside** the checkout and
does not edit application or test source. It initializes no sinks
or Valkey state and requires no credentials.

### Documentation conventions

- Use the [event receiver configuration reference](event-receiver/configuration.md)
  and [SLA proxy configuration reference](sla-proxy/configuration.md) as the
  canonical explanations of options.
- Keep minimal examples small and put complete annotations in the
  [full SLA example](examples/alert-sla-proxy.yaml).
- When moving files or renaming headings, update incoming links and
  run the anchor validator.
- Update focused example tests and validation when examples move.
- Preserve historical specifications as records of their stage:
  update notices or links when current behavior changes, rather
  than silently rewriting earlier design decisions.

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/server` | Event receiver executable |
| `cmd/alert-sla-proxy` | Independent SLA proxy executable |
| `cmd/alertload` | Webhook load generator |
| `cmd/httpsink-mock` | Local HTTP destination that prints full webhook payloads |
| `internal/models` | Shared Alertmanager wire types and lifecycle event model |
| `internal/processor`, `internal/state`, `internal/webhook` | Receiver lifecycle, working state and webhook handler |
| `internal/telemetry` | Receiver OTLP, SDK diagnostics and metrics |
| `internal/slaproxy` | Calendar, durable state, dispatcher and SLA HTTP service |
| `internal/alertload` | Deterministic generator, bounded scheduler and verification |
| `grafana` | Dashboard artifacts |
| `docs/specifications`, `docs/benchmarks` | Designs and measurements |

The SLA proxy reuses wire types and general libraries, **not** the
receiver's processor, state store or OTLP telemetry as its delivery
engine. The applications have separate operational semantics.

## Offline holiday dataset development

For deployment and supported calendar formats, start with
[holiday datasets](sla-proxy/holiday-datasets.md). This section
documents the Go parser dependency, its restrictions and the
optional offline dataset tooling.

### Go ICS parser and accepted input

The service pins `github.com/arran4/golang-ical v0.3.8`
(Apache-2.0; [license](https://github.com/arran4/golang-ical/blob/v0.3.8/LICENSE),
[release history](https://github.com/arran4/golang-ical/releases)).
The parser has tagged 2026 maintenance releases and parser/fuzz tests,
and adds no production dependencies beyond the Go standard library.
It is pre-1.0 and seeks a co-maintainer. Keep versions pinned and
review upgrades; maintenance history does not guarantee freedom
from vulnerabilities. An implementation-time `govulncheck ./...`
reported none; rerun it for upgrades.

Only **reader-based parsing** is used. The application does not invoke
URL loading, recurrence, attachment fetching, serialization or
parser recovery/skip options. Its guards bound bytes, unfolded lines,
component envelopes, event counts and expansion. The envelope guard
rejects truncated calendars even though the upstream calendar-level
EOF behavior does not require a closing marker.

The library handles RFC content lines, folding and property syntax.
The application implements restricted all-day property validation
and civil-day expansion. See the
[accepted ICS profile](sla-proxy/holiday-datasets.md#manifest-and-accepted-ics-profile).

### Dataset identity and policy changes

The old country adapter, `HolidayProvider` interface and `rickar/cal`
dependency have been removed. Public holidays use **ICS datasets
only**. Compiled policies hold dataset snapshots reused by canonical
path. Semantic hashes include sorted dates, coverage and a rule
version. Policy hashes use normalized configuration and dataset
semantics; an unrelated policy does not change version when another
file changes.

The durable store schema and action/occurrence/sink identities remain
unchanged. Dataset changes use existing epoch fences and policy holds,
not a separate snapshot store or automatic migration.

### Test and generate datasets offline

`tools/holidays/requirements.txt` pins the optional offline exporter
environment. After dependency changes, verify `holidays-ics --help`.

```bash
/tmp/sla-holiday-tools/bin/python -B -m unittest discover -s tools/holidays -v
go test -tags=integration ./internal/slaproxy -run '^TestDataset' -count=1
```

The [generator guide](../tools/holidays/README.md) documents
provenance and normalized explicit-date fixtures. Python is not a
runtime requirement of the Go service or ordinary Go tests.

For examples, `docs/validate.py --examples` maps the example-only
`/etc/alert-sla-proxy/example-holidays/` prefix to checked-in
fixtures within a temporary Go overlay test. It does **not** modify
production configuration.
