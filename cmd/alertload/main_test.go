package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andersh/alertmanager_event_receiver/internal/alertload"
)

func TestCLIAndJSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "report.json")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-url", server.URL, "-scenario", "lifecycle", "-rate", "20", "-duration", "100ms", "-resolve-after", "604801s", "-seed", "42", "-run-id", "cli-test", "-output", path}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report alertload.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 1 || report.Config.Seed != 42 || report.MatchedClosureCandidates != 2 || report.ExpectedDurationHistogram.Buckets["+Inf"] != 2 || report.ExpectedDurationHistogram.Buckets["604800"] != 0 {
		t.Fatal("JSON report mismatch")
	}
	if !strings.Contains(stdout.String(), "target=") || !strings.Contains(stdout.String(), "alerts/s") || !strings.Contains(stdout.String(), "p50/p90/p95/p99/max") {
		t.Fatal("incomplete terminal summary")
	}
}
func TestCLIValidationAndHelp(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"-help"}, 0}, {[]string{"-rate", "0"}, 2}, {[]string{"-scenario", "unknown"}, 2}, {[]string{"-url", "http://user:private-token@localhost"}, 2}, {[]string{"-url", "https://example.com"}, 2},
	} {
		var out, stderr bytes.Buffer
		if code := run(context.Background(), tc.args, &out, &stderr); code != tc.code {
			t.Fatalf("args=%v code=%d want=%d", tc.args, code, tc.code)
		}
		if strings.Contains(stderr.String(), "private-token") {
			t.Fatal("CLI error leaked credential")
		}
	}
}
func TestCLIInterruptedReport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, stderr bytes.Buffer
	path := filepath.Join(t.TempDir(), "interrupted.json")
	if code := run(ctx, []string{"-run-id", "interrupted", "-output", path}, &out, &stderr); code != 130 {
		t.Fatalf("interrupt code=%d", code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report alertload.Report
	_ = json.Unmarshal(data, &report)
	if !report.Interrupted {
		t.Fatal("interrupted report missing")
	}
}

func TestCLIPhaseTwoScenarioFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	dir := t.TempDir()
	scenario := filepath.Join(dir, "profiles.json")
	output := filepath.Join(dir, "report.json")
	if err := os.WriteFile(scenario, []byte(`{"profiles":[{"scenario":"reopen","weight":1,"resolve_after":"299s"},{"scenario":"lifecycle","weight":1,"resolve_after":"301s"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-url", server.URL, "-scenario-file", scenario, "-rate", "20", "-duration", "100ms", "-run-id", "phase2-cli", "-output", output}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var report alertload.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Config.Scenario != "mixed" || report.MatchedClosureCandidates != 3 || len(report.Workloads) != 2 || report.ExpectedDurationHistogram.Buckets["300"] != 2 {
		t.Fatal("scenario file CLI lost per-profile durations")
	}
	if !strings.Contains(stdout.String(), "synthetic_duration=4m59s") || strings.Contains(stdout.String(), "expected_duration=600") {
		t.Fatal("terminal summary misrepresented profile durations")
	}
}
