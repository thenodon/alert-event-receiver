# Offline holiday dataset generator

Use the [operator guide](../../docs/sla-proxy/holiday-datasets.md) for setup, exact
CLI syntax, generation, manifest contract and safe installation. This tool is
not part of the running Go service and is never invoked by it.

Tests in the isolated pinned environment:

```bash
/tmp/sla-holiday-tools/bin/python -B -m unittest discover -s tools/holidays -v
```

The helper invokes `holidays.generate_ics` with public categories, validates the
provider's advertised years, uses the Python `icalendar` parser for all-day
normalization, checks exported effective dates against the provider API, and emits
an adjacent SHA-bound manifest. No hand-written RFC parser or country rules are
implemented here. Output goes to a new directory by atomic rename; install or
switch complete version directories, not individual live files.

Provenance and license sources:

- [python-holidays 0.108](https://pypi.org/project/holidays/0.108/) — MIT.
- [icalendar 7.3.0](https://pypi.org/project/icalendar/7.3.0/) — BSD-2-Clause.

These are offline build/preparation dependencies. Regenerate for future legal
changes, review provider limitations, and keep older immutable versions for
rollback. Do not treat a provider's future predictions as a legal guarantee.
