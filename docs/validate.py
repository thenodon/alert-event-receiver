#!/usr/bin/env python3
"""Check repository documentation without modifying application source.

--examples uses Go's overlay mechanism to run temporary configuration-loader tests.
No runtime service, sink, database or remote endpoint is initialized by this script.
"""
import argparse
import ast
import collections
import json
import pathlib
import re
import subprocess
import sys
import tempfile
import urllib.parse

ROOT = pathlib.Path(__file__).resolve().parent.parent


def fences(text):
    return re.findall(r"^```([^\n]*)\n(.*?)^```\s*$", text, re.M | re.S)


def anchors(text):
    text = re.sub(r"^```.*?^```\s*$", "", text, flags=re.M | re.S)
    counts = collections.Counter()
    result = set(re.findall(r'<(?:a|span)[^>]*\bid=["\']([^"\']+)', text))
    for heading in re.findall(r"^#{1,6}\s+(.+?)(?:\s+#+)?$", text, re.M):
        slug = re.sub(r"[^\w\- ]", "", heading.lower()).replace(" ", "-")
        count = counts[slug]
        counts[slug] += 1
        result.add(slug + (f"-{count}" if count else ""))
    return result


def check_documents():
    errors = []
    documents = [ROOT / "README.md", *sorted((ROOT / "docs").rglob("*.md"))]
    link_count = 0
    for path in documents:
        text = path.read_text()
        for target in re.findall(r"!?\[[^\]]*\]\(([^)]+)\)", text):
            target = target.strip().split(' "', 1)[0].strip("<>")
            url = urllib.parse.urlsplit(target)
            if url.scheme or url.netloc:
                continue
            link_count += 1
            destination = path.parent / urllib.parse.unquote(url.path) if url.path else path
            if not destination.exists():
                errors.append(f"{path.relative_to(ROOT)}: missing {target}")
            elif url.fragment and destination.suffix == ".md":
                if urllib.parse.unquote(url.fragment) not in anchors(destination.read_text()):
                    errors.append(f"{path.relative_to(ROOT)}: missing anchor {target}")
        for language, body in fences(text):
            try:
                if language.strip() == "json":
                    json.loads(body)
                elif language.strip() in ("bash", "sh", "shell"):
                    result = subprocess.run(["bash", "-n"], input=body, text=True, capture_output=True)
                    if result.returncode:
                        raise ValueError(result.stderr.strip())
            except ValueError as error:
                errors.append(f"{path.relative_to(ROOT)}: invalid {language} block: {error}")
        # Repository-relative paths mentioned in commands must exist. Ignore glob
        # patterns, generated output paths and the historical proposed-layout prose.
        for language, body in fences(text):
            if language.strip() not in ("bash", "sh", "shell"):
                continue
            for name in re.findall(r"(?<![\w/])(?:docs|cmd|internal)/[\w./-]+", body):
                if name.endswith("/..."):
                    name = name[:-4]
                if not (ROOT / name).exists():
                    errors.append(f"{path.relative_to(ROOT)}: command references missing {name}")
    for path in [*(ROOT / "docs").rglob("*.py"), *(ROOT / "tools/holidays").glob("*.py")]:
        try:
            ast.parse(path.read_text(), filename=str(path))
        except SyntaxError as error:
            errors.append(str(error))
    env = ROOT / "docs/examples/event-receiver.env"
    result = subprocess.run(["bash", "-n", str(env)], capture_output=True, text=True)
    if result.returncode:
        errors.append(result.stderr.strip())
    if errors:
        raise RuntimeError("\n".join(errors))
    print(f"Checked {len(documents)} Markdown files and {link_count} local links; JSON, shell and Python syntax passed.")


def check_examples():
    source_sla = r'''package slaproxy
import (
    "os"
    "path/filepath"
    "strings"
    "testing"
)
func TestDocumentationExamples(t *testing.T) {
    paths, err := filepath.Glob("../../docs/examples/*.yaml")
    if err != nil || len(paths) == 0 { t.Fatal("no examples", err) }
    for _, path := range paths {
        t.Run(filepath.Base(path), func(t *testing.T) {
            f, err := os.Open(path)
            if err != nil { t.Fatal(err) }
            defer f.Close()
            cfg, err := LoadConfig(f)
            if err != nil { t.Fatal(err) }
            // The documented absolute example install prefix is substituted only
            // in this offline test. Production configs are never rewritten.
            for _, def := range cfg.SLADefinitions {
                for _, policy := range def.SLAs {
                    if policy.Holidays != nil && policy.Holidays.Dataset != nil {
                        value := *policy.Holidays.Dataset
                        const prefix = "/etc/alert-sla-proxy/example-holidays/"
                        if !strings.HasPrefix(value, prefix) { t.Fatal("unknown example dataset prefix", value) }
                        actual, err := filepath.Abs(filepath.Join("testdata", "holidays", strings.TrimPrefix(value, prefix)))
                        if err != nil { t.Fatal(err) }
                        *policy.Holidays.Dataset = actual
                    }
                }
            }
            if _, err = NewCalendarEngine(cfg, CountryHolidays{}); err != nil { t.Fatal(err) }
        })
    }
}
'''
    source_load = r'''package alertload
import (
    "reflect"
    "testing"
)
func TestDocumentationExamples(t *testing.T) {
    c := DefaultConfig()
    c.Scenario = "mixed"
    c.ScenarioFile = "../../docs/examples/alertload-mixed.json"
    first, err := workloads(c)
    if err != nil { t.Fatal(err) }
    c.ScenarioFile = "../../docs/examples/alertload-mixed.csv"
    second, err := workloads(c)
    if err != nil { t.Fatal(err) }
    if !reflect.DeepEqual(first, second) { t.Fatal("CSV and JSON profiles differ") }
}
'''
    with tempfile.TemporaryDirectory(prefix="documentation-check-") as directory:
        directory = pathlib.Path(directory)
        replacements = {}
        for package, source in (("slaproxy", source_sla), ("alertload", source_load)):
            actual = directory / (package + "_test.go")
            actual.write_text(source)
            virtual = ROOT / "internal" / package / "z_documentation_examples_test.go"
            if virtual.exists():
                raise RuntimeError(f"overlay path already exists: {virtual}")
            replacements[str(virtual)] = str(actual)
        overlay = directory / "overlay.json"
        overlay.write_text(json.dumps({"Replace": replacements}))
        subprocess.run(["go", "test", "-overlay", str(overlay), "./internal/slaproxy", "./internal/alertload",
                        "-run", "^TestDocumentationExamples$", "-count=1", "-v"], cwd=ROOT, check=True)
    print("All SLA YAML/ICS/manifest examples and equivalent alertload profiles passed actual Go loaders.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--examples", action="store_true", help="also validate examples using temporary Go overlay tests")
    args = parser.parse_args()
    check_documents()
    if args.examples:
        check_examples()


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, subprocess.CalledProcessError) as error:
        print(error, file=sys.stderr)
        sys.exit(1)
