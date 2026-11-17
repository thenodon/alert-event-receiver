"""Offline generator tests; run in the pinned generator environment, not Go runtime."""
import datetime as dt
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("holiday_generator", Path(__file__).with_name("generate.py"))
generator = importlib.util.module_from_spec(spec)
spec.loader.exec_module(generator)


class GeneratorTests(unittest.TestCase):
    def test_duration_ranges_and_recurring_rejection(self):
        raw = b"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nDTSTART;VALUE=DATE:20261224\r\nDURATION:P3D\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
        encoded, dates = generator.explicit_calendar(raw, 2026, 2026)
        self.assertEqual(dates, frozenset(dt.date(2026, 12, day) for day in (24, 25, 26)))
        self.assertNotIn(b"DURATION", encoded)
        self.assertIn(b"DTEND;VALUE=DATE:20261227", encoded)
        for property in (b"RRULE:FREQ=YEARLY", b"RDATE;VALUE=DATE:20270101"):
            with self.assertRaises(ValueError):
                generator.explicit_calendar(raw.replace(b"END:VEVENT", property + b"\r\nEND:VEVENT"), 2026, 2026)

    def test_reproducible_pairs_and_atomic_new_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            pairs = []
            for suffix in ("one", "two"):
                target = Path(directory) / suffix
                command = [sys.executable, str(Path(generator.__file__)), "--country", "SE", "--first-year", "2026", "--last-year", "2027", "--output-dir", str(target)]
                result = subprocess.run(command, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                data = (target / "holidays.ics").read_bytes()
                metadata = json.loads((target / "holidays.ics.json").read_text())
                self.assertEqual(metadata["ics_sha256"], hashlib.sha256(data).hexdigest())
                self.assertEqual((metadata["first_year"], metadata["last_year"]), (2026, 2027))
                pairs.append((data, metadata))
                again = subprocess.run(command, capture_output=True, text=True)
                self.assertNotEqual(again.returncode, 0)
                self.assertEqual(data, (target / "holidays.ics").read_bytes())
            self.assertEqual(pairs[0], pairs[1])
            self.assertFalse(any(p.name.startswith(".holiday-stage-") for p in Path(directory).iterdir()))

    def test_unsupported_years_do_not_get_a_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "invalid"
            result = subprocess.run([sys.executable, str(Path(generator.__file__)), "--country", "SE", "--first-year", "1900", "--last-year", "2026", "--output-dir", str(target)], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("provider supports years", result.stderr)
            self.assertFalse(target.exists())


if __name__ == "__main__":
    unittest.main()
