# Real generated holiday fixtures

These explicit all-day datasets cover 2020–2040 inclusive. `holidays_se.ics` contains
Swedish public holidays; `holidays_de_by.ics` contains nationwide plus Bavaria's
public holidays as defined by the pinned generator, not all municipality-specific
observances. They are test/example data, not a legal or perpetual-coverage guarantee.

Generated with `holidays==0.108`, `icalendar==7.3.0` and the pinned offline dependencies:

```bash
/tmp/sla-holiday-tools/bin/python tools/holidays/generate.py \
  --country SE --first-year 2020 --last-year 2040 \
  --output-dir /tmp/sla-holidays-se-v1 --name holidays_se.ics
/tmp/sla-holiday-tools/bin/python tools/holidays/generate.py \
  --country DE --subdivision BY --first-year 2020 --last-year 2040 \
  --output-dir /tmp/sla-holidays-de-by-v1 --name holidays_de_by.ics
```

The [operator workflow](../../../../docs/sla-proxy/holiday-datasets.md) includes setup,
normalization, manifest integrity, coverage, and atomic installation. Fixtures and
sidecars are checked into Git so ordinary Go tests and runtime need no Python or
network. Generator names/DTSTAMP are normalized for reproducibility; source name
and package version are retained in manifests. Do not regenerate fixtures silently
when changing dependencies; review effective dates and coverage.
