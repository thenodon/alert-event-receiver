#!/usr/bin/env python3
"""Generate a fresh firing/resolved pair for the documentation examples."""
import datetime
import json
import pathlib
import sys
import uuid


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: python3 docs/examples/make-test-webhooks.py OUTPUT_PREFIX")
    prefix = pathlib.Path(sys.argv[1])
    prefix.parent.mkdir(parents=True, exist_ok=True)
    end = datetime.datetime.now(datetime.timezone.utc)
    start = end - datetime.timedelta(seconds=2)
    fingerprint = uuid.uuid4().hex
    for action in ("firing", "resolved"):
        alert = {
            "status": action,
            "fingerprint": fingerprint,
            "startsAt": start.isoformat().replace("+00:00", "Z"),
            "labels": {"alertname": "DocumentationDemo", "service": "documentation-demo", "sla_id": "demo"},
            "annotations": {"summary": "Local documentation test"},
        }
        if action == "resolved":
            alert["endsAt"] = end.isoformat().replace("+00:00", "Z")
        payload = {"version": "4", "receiver": "documentation-demo", "status": action, "alerts": [alert]}
        path = pathlib.Path(str(prefix) + "-" + action + ".json")
        path.write_text(json.dumps(payload) + "\n")
        print(path)
    print("fingerprint:", fingerprint)


if __name__ == "__main__":
    main()
