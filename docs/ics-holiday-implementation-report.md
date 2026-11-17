# ICS holiday dataset implementation report

Implemented on `feature/ics-holiday-datasets`, based on the maintainer's
`fix/holidays` integration branch at `905b8e5`.

## Changes

- `go.mod`, `go.sum`: pinned Apache-2.0 `arran4/golang-ical v0.3.8` parser.
- `internal/slaproxy/holiday_dataset.go`: bounded local ICS/manifest loading,
  restricted explicit-date profile, immutable dates and deterministic semantic hash.
- `internal/slaproxy/config.go`, `evaluator.go`: per-policy dataset selection,
  mutual exclusion, canonical-path cache, coverage checks and policy versions.
- `internal/slaproxy/contracts.go`, `holidays.go`, `metrics.go`, `server.go`:
  compatible provider wiring, bounded dataset metric labels and rolling readiness.
- `internal/slaproxy/holiday_dataset_test.go`: configuration, parser, manifest,
  coverage, civil-date/DST/window/horizon, hashing, cache and fuzz tests.
- `internal/slaproxy/holiday_dataset_integration_test.go`: durable restart,
  epoch activation, stale workers, changed-policy holds, historical resolution,
  dispatcher authorization, byte-only changes and exact-semantics restoration.
- `internal/slaproxy/testdata/holidays/`: real SE and DE/BY 2020–2040 ICS pairs,
  generation README and `.gitattributes` preserving checksum-sensitive CRLF bytes.
- `tools/holidays/{generate.py,test_generate.py,requirements.txt,README.md}`:
  pinned offline exporter, explicit-date normalization, atomic version-directory
  installation, reproducibility tests and verified CLI workflow.
- `docs/examples/alert-sla-proxy-ics.yaml`: small working ICS configuration;
  `docs/examples/alert-sla-proxy.yaml` and `docs/examples/README.md`: selection guidance.
- `docs/sla-proxy/holiday-datasets.md`: generation, manifests, installation,
  coverage, updates, rollback, security and durable holds.
- `README.md`, `docs/README.md`, `docs/development.md`, and
  `docs/sla-proxy/{configuration,getting-started,concepts,operations,reference}.md`:
  navigation, field reference, operator guidance and dependency rationale.
- `docs/validate.py`: validates actual example ICS/manifest pairs through Go's
  loader and checks offline helper syntax without importing Python dependencies.

## Compatibility and operational decisions

Subsequent maintainer-approved cleanup removes the former country/subdivision YAML
fields, country adapter, provider interface and `rickar/cal` dependency. Public
holidays now require ICS; explicit dates still work alone. The metric now has only
a `result` label. Policy serialization is simplified and existing policy hashes
change; a reused durable namespace requires its usual higher-epoch procedure.
The legacy compatibility statements below describe the initial implementation.

Legacy country/subdivision configuration, provider callers and legacy policy hashes
remain compatible. ICS mode adds countries through files/YAML without compiled
country imports. The running binary requires neither Python nor network access.
No durable schema, occurrence identity, webhook contract or sink semantics changed.

Absolute local `.ics` paths are required. Sidecars append `.json` to the resolved
file path. Dataset dates and declared coverage determine identity; paths, provenance,
event order and duplicate events do not. Explicit additions retain reason precedence.
Startup and readiness require coverage from yesterday through policy-local today +
evaluation horizon + 3 days. Evaluation independently rejects uncovered dates.

Changed dates **or coverage** require an explicit higher policy epoch. Existing
affected bindings retain their original decisions and become `policy_changed`
holds; they are never silently reevaluated with new data. Exact semantic restoration
at a still higher epoch can resume compatible work while its coverage remains valid.
There is no persistent historical dataset store or automatic migration. This is the
specification's permitted safe-hold strategy. No deliberate contract deviations.

## Validation

Executed successfully on the final implementation:

| Command/check | Result |
|---|---|
| `go test ./...` | Passed |
| `go test -race ./internal/slaproxy/...` | Passed |
| `go test -tags=integration ./internal/slaproxy/...` | Passed against local Valkey |
| `go vet ./...` | Passed |
| `python3 docs/validate.py --examples` | Passed; Markdown links, shell/JSON/Python syntax, YAML/ICS/manifest loaders and alertload profiles |
| `/tmp/sla-ics-generator/bin/python -B -m unittest discover -s tools/holidays -v` | Passed, 3 offline generator tests |
| `git diff --check` and `git diff --cached --check` | Passed |
| Staged ICS bytes SHA-256 against staged manifests | Passed for both fixtures |

Additional checks: `govulncheck ./...` reported no vulnerabilities; a three-second
four-worker `FuzzHolidayDatasetICS` run passed 76,539 executions. Regenerating the
Swedish fixture with the pinned helper produced matching ICS and manifest bytes.
The installed `holidays-ics --help` was inspected; documented flags were exercised
for both SE and DE/BY. No required validation command was left unrun.

## Remaining limitations

Datasets have finite coverage and require an operator-managed refresh and restart.
Coverage extensions can hold existing unresolved work; an expired original dataset
may prevent restoration. Existing at-least-once delivery ambiguity remains.
There is no automatic download, reload, redrive or calendar migration.

Only the documented explicit all-day profile is supported; recurrence, date-time
events and raw exporter DURATION require offline normalization. Checksums establish
pair consistency, not legal accuracy or authenticity. Operators must review source
coverage, regional applicability and future legislation. The parser is pre-1.0;
dependency maintenance risks and defensive limits are documented in the
[developer guide](development.md#ics-dependency-and-offline-dataset-tooling).

Pre-existing untracked specifications and unrelated artifacts are excluded from
these commits.
