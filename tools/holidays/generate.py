#!/usr/bin/env python3
"""Export explicit public holidays and an integrity/coverage manifest offline.

Install requirements.txt in a separate environment. The output is an immutable,
new version directory installed by one rename; switch a directory symlink to
activate a complete pair, then restart the Go service. No runtime download/reload.
"""
import argparse
import datetime as dt
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

import holidays
from icalendar import Calendar, Event

PINNED_HOLIDAYS = "0.108"
PINNED_ICALENDAR = "7.3.0"
MAX_BYTES = 8 << 20


def explicit_calendar(raw, first_year, last_year):
    """Use a real ICS parser, emit canonical one-day events, deduplicate dates.

    The exporter may group adjacent days using DURATION. Intersect its generated
    date ranges with the requested years (observed holidays may spill over).
    Preserve explicit civil dates, never convert them through a time zone.
    """
    if not raw or len(raw) > MAX_BYTES:
        raise ValueError("empty or oversized exporter output")
    source = Calendar.from_ical(raw)
    dates = set()
    lower, upper = dt.date(first_year, 1, 1), dt.date(last_year + 1, 1, 1)
    for event in source.walk("VEVENT"):
        if any(key in event for key in ("RRULE", "RDATE", "EXDATE", "RECURRENCE-ID")):
            raise ValueError("recurring exporter output is unsupported")
        start = event.decoded("DTSTART")
        if not isinstance(start, dt.date) or isinstance(start, dt.datetime):
            raise ValueError("exporter must use all-day dates")
        if "DTEND" in event and "DURATION" in event:
            raise ValueError("exporter supplied both end and duration")
        end = event.decoded("DTEND") if "DTEND" in event else start + event.decoded("DURATION", dt.timedelta(days=1))
        if not isinstance(end, dt.date) or isinstance(end, dt.datetime):
            raise ValueError("invalid all-day end")
        if "DURATION" in event:
            duration = event.decoded("DURATION")
            if not isinstance(duration, dt.timedelta) or duration.seconds or duration.microseconds:
                raise ValueError("exporter duration must contain whole civil days")
        if not 1 <= (end - start).days <= 3660:
            raise ValueError("invalid exporter event range")
        current = max(start, lower)
        while current < min(end, upper):
            dates.add(current)
            current += dt.timedelta(days=1)
    if not dates or len(dates) > 200000:
        raise ValueError("empty or excessive generated date set")
    calendar = Calendar()
    calendar.add("prodid", "-//alert-sla-proxy//Explicit holidays v1//EN")
    calendar.add("version", "2.0")
    calendar.add("calscale", "GREGORIAN")
    for date in sorted(dates):
        event = Event()
        event.add("uid", f"{date:%Y%m%d}@alert-sla-proxy-holidays")
        # Stable metadata makes repeated generation byte-reproducible as well.
        event.add("dtstamp", dt.datetime(2000, 1, 1, tzinfo=dt.timezone.utc))
        event.add("dtstart", date)
        event.add("dtend", date + dt.timedelta(days=1))
        event.add("summary", "Public holiday")
        calendar.add_component(event)
    encoded = calendar.to_ical()
    if len(encoded) > MAX_BYTES:
        raise ValueError("normalized output exceeds size limit")
    return encoded, frozenset(dates)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--country", required=True)
    parser.add_argument("--subdivision", default="")
    parser.add_argument("--first-year", type=int, required=True)
    parser.add_argument("--last-year", type=int, required=True)
    parser.add_argument("--output-dir", type=Path, required=True, help="new immutable version directory; must not exist")
    parser.add_argument("--name", default="holidays.ics", help="basename for the .ics file")
    args = parser.parse_args()
    if not 1 <= args.first_year <= args.last_year <= 9998:
        parser.error("years must satisfy 1 <= first <= last <= 9998")
    if Path(args.name).name != args.name or not args.name.endswith(".ics"):
        parser.error("--name must be an .ics basename")
    for package, expected in (("holidays", PINNED_HOLIDAYS), ("icalendar", PINNED_ICALENDAR)):
        if importlib.metadata.version(package) != expected:
            parser.error(f"install requirements.txt; expected {package}=={expected}")
    # Some providers silently return no dates outside their documented years.
    # Never manufacture a coverage assertion for those unsupported years.
    source = holidays.country_holidays(args.country, subdiv=args.subdivision or None,
                                      categories="public", expand=False)
    if args.first_year < source.start_year or args.last_year > source.end_year:
        parser.error(f"provider supports years {source.start_year}-{source.end_year}")
    expected = holidays.country_holidays(args.country, subdiv=args.subdivision or None,
                                        categories="public", expand=False,
                                        years=range(args.first_year, args.last_year + 1))
    expected_dates = frozenset(date for date in expected if args.first_year <= date.year <= args.last_year)
    destination = args.output_dir.absolute()
    if destination.exists():
        parser.error("output directory already exists; generate a new version")
    destination.parent.mkdir(parents=True, exist_ok=True)
    stage = Path(tempfile.mkdtemp(prefix=".holiday-stage-", dir=destination.parent))
    try:
        raw_file = stage / "export.ics"
        command = [sys.executable, "-m", "holidays.generate_ics", args.country,
                   "--years", f"{args.first_year}-{args.last_year}",
                   "--categories", "public", "--language", "en_US", "--output", str(raw_file)]
        if args.subdivision:
            command += ["--subdiv", args.subdivision]
        subprocess.run(command, check=True)
        data, dates = explicit_calendar(raw_file.read_bytes(), args.first_year, args.last_year)
        if dates != expected_dates:
            raise ValueError("exported dates do not match the requested provider calendar")
        raw_file.unlink()
        output = stage / args.name
        output.write_bytes(data)
        manifest = {"schema_version": 1, "format": "ics-holiday-dataset",
                    "country": args.country, "subdivision": args.subdivision,
                    "first_year": args.first_year, "last_year": args.last_year,
                    "source": "python-holidays", "source_version": PINNED_HOLIDAYS,
                    "ics_sha256": hashlib.sha256(data).hexdigest()}
        (stage / (args.name + ".json")).write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
        # Files are public holiday data, not secrets. Keep the directory searchable
        # by a separate unprivileged service user; operator ACLs may be stricter.
        output.chmod(0o644)
        (stage / (args.name + ".json")).chmod(0o644)
        stage.chmod(0o755)
        for file in stage.iterdir():
            with file.open("rb") as handle:
                os.fsync(handle.fileno())
        if destination.exists():
            raise ValueError("output directory appeared during generation")
        os.rename(stage, destination)
        print(f"Installed {len(dates)} explicit dates in {destination / args.name}")
    finally:
        if stage.exists():
            shutil.rmtree(stage)


if __name__ == "__main__":
    main()
