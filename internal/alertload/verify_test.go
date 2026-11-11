package alertload

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScrapeAndBackendComparisonOverHTTP(t *testing.T) {
	var mu sync.Mutex
	var requests, firing, resolved uint64
	const seconds = 301.0
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		requests++
		if p.Status == "firing" {
			firing += uint64(len(p.Alerts))
		} else {
			resolved += uint64(len(p.Alerts))
		}
		mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "# HELP unrelated ignored\nprocess_start_time_seconds 123\nalertreceiver_webhook_requests_total{status=\"2xx\"} %d\nalertreceiver_processing_outcomes_total{outcome=\"opened\"} %d\nalertreceiver_processing_outcomes_total{outcome=\"closed\"} %d\n", requests+7, firing, resolved)
		fmt.Fprintf(w, "alertreceiver_alert_duration_seconds_count %d\nalertreceiver_alert_duration_seconds_sum %g\n", resolved, float64(resolved)*seconds)
		for name, value := range map[string]uint64{"firing": firing, "resolved": resolved} {
			fmt.Fprintf(w, "alertreceiver_events_generated_total{transition=%q} %d\nalertreceiver_events_emitted_total{transition=%q} %d\n", name, value, name, value)
		}
		for le, count := range expectedHistogram(seconds, resolved).Buckets {
			label := le
			if le != "+Inf" {
				label += ".0"
			}
			fmt.Fprintf(w, "alertreceiver_alert_duration_seconds_bucket{le=%q} %d\n", label, count)
		}
	})
	mux.HandleFunc("/select/logsql/query", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Error("backend query must POST")
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if !strings.Contains(r.Form.Get("query"), `alert.label.load_run:="test-run"`) || !strings.Contains(r.Form.Get("query"), "stats by (alert.transition)") {
			t.Errorf("unsafe/incomplete query: %s", r.Form.Get("query"))
		}
		mu.Lock()
		defer mu.Unlock()
		encoder := json.NewEncoder(w)
		_ = encoder.Encode(map[string]any{"alert.transition": "firing", "total": fmt.Sprint(firing)})
		_ = encoder.Encode(map[string]any{"alert.transition": "resolved", "total": resolved})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := integrationConfig()
	c.URL = server.URL + "/webhook"
	c.MetricsURL = server.URL + "/metrics"
	c.ComparisonIsolated = true
	c.VictoriaLogsURL = server.URL + "/select/logsql/query"
	c.SettleTime = 0
	c.ResolveAfter = 301 * time.Second
	report, err := Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed || report.MetricsComparison == nil || report.MetricsComparison.Status != "matched" || report.BackendCheck == nil || report.BackendCheck.Status != "matched" {
		t.Fatalf("verification failed: %+v metrics=%+v backend=%+v", report, report.MetricsComparison, report.BackendCheck)
	}
	if report.BackendCheck.Expected != report.AcceptedAlerts || report.BackendCheck.Observed != report.AcceptedAlerts {
		t.Fatal("backend count incorrect")
	}
	if report.MetricsComparison.Deltas["alertreceiver_webhook_requests_total{status=2xx}"] != float64(report.SentRequests) {
		t.Fatal("baseline not subtracted")
	}
}

func TestPrometheusParsingAndComparisonLimits(t *testing.T) {
	body := `# TYPE alertreceiver_alert_duration_seconds histogram
alertreceiver_alert_duration_seconds_count 2
alertreceiver_alert_duration_seconds_sum 600
alertreceiver_alert_duration_seconds_bucket{le="300.0",ignored="brace}quote\""} 2
unrelated{credential="private-token"} 123
`
	got, err := parseSnapshot([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got["alertreceiver_alert_duration_seconds_bucket{le=300}"] != 2 {
		t.Fatal("bucket boundary not normalized")
	}
	for key := range got {
		if strings.Contains(key, "private-token") {
			t.Fatal("unrelated data retained")
		}
	}
	for _, invalid := range []string{
		"other_metric 1\n",
		"alertreceiver_alert_duration_seconds_count NaN\n",
		"alertreceiver_alert_duration_seconds_count 0\nalertreceiver_processing_outcomes_total{outcome=\"private-token\"} 1\n",
		"alertreceiver_alert_duration_seconds_count 0\nalertreceiver_alert_duration_seconds_bucket{le=\"300\" 1\n",
	} {
		if _, err := parseSnapshot([]byte(invalid)); err == nil || strings.Contains(err.Error(), "private-token") {
			t.Fatal("bad scrape accepted or leaked error content", err)
		}
	}
	c := testConfig()
	r := newReport(c)
	r.finish(c, time.Now())
	before := snapshot{"process_start_time_seconds": 1, "alertreceiver_alert_duration_seconds_count": 0}
	after := snapshot{"process_start_time_seconds": 2, "alertreceiver_alert_duration_seconds_count": 0}
	if result := compareMetrics(before, after, r); result.Status != "inconclusive" {
		t.Fatal("restart not detected")
	}
	before = snapshot{"alertreceiver_alert_duration_seconds_count": 10}
	after = snapshot{"alertreceiver_alert_duration_seconds_count": 2}
	if result := compareMetrics(before, after, r); result.Status != "inconclusive" {
		t.Fatal("counter reset not detected")
	}
	r.FailedRequests = 1
	if result := compareMetrics(snapshot{}, snapshot{}, r); result.Status != "inconclusive" {
		t.Fatal("ambiguous workload got authoritative comparison")
	}
	r.FailedRequests = 0
	r.SuccessfulRequests = 1
	if result := compareMetrics(snapshot{}, snapshot{}, r); result.Status != "mismatch" {
		t.Fatal("metric mismatch hidden")
	}
}

func TestVerificationErrorsAndSafety(t *testing.T) {
	c := integrationConfig()
	c.MetricsURL = "http://127.0.0.1:9011/metrics"
	if c.Validate() == nil {
		t.Fatal("aggregate comparison lacked isolation acknowledgement")
	}
	c.ComparisonIsolated = true
	c.MetricsURL = "https://example.com/metrics"
	if c.Validate() == nil {
		t.Fatal("remote scrape bypassed safety")
	}
	c.MetricsURL = ""
	c.VictoriaLogsURL = "http://user:private-token@localhost/query"
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("backend credentials accepted or leaked")
	}
	var requests int
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.Error(w, "private-token", 500)
	}))
	defer server.Close()
	c = integrationConfig()
	c.URL = server.URL + "/webhook"
	c.MetricsURL = server.URL + "/metrics"
	c.ComparisonIsolated = true
	c.SettleTime = 0
	report, err := Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Failed || report.SentRequests != 0 || report.MetricsComparison == nil || report.MetricsComparison.Status != "error" {
		t.Fatal("baseline failure did not stop load")
	}
	mu.Lock()
	if requests != 1 {
		t.Errorf("baseline failure sent other traffic: %d", requests)
	}
	mu.Unlock()
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "private-token") {
		t.Fatal("verification body leaked")
	}
}

func TestBackendMismatchMalformedAndAmbiguous(t *testing.T) {
	for _, reply := range []string{`{"alert.transition":"firing","total":"0"}`, `not json`, `{"alert.transition":"firing","total":"private-token"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, reply) }))
		c := testConfig()
		c.VictoriaLogsURL = server.URL
		r := newReport(c)
		r.AcceptedOutcomes["opened"] = 2
		result := queryBackend(context.Background(), c, r)
		server.Close()
		if result.Status == "matched" || strings.Contains(result.Reason, "private-token") {
			t.Fatal("backend mismatch hidden or leaked")
		}
	}
	c := testConfig()
	r := newReport(c)
	r.UnknownAlerts = 1
	r.FailedRequests = 1
	if result := queryBackend(context.Background(), c, r); result.Status != "inconclusive" {
		t.Fatal("ambiguous delivery asserted backend success")
	}
}

func TestDifferentProfileDurationsAggregate(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/profiles.json"
	if err := os.WriteFile(path, []byte(`{"profiles":[{"scenario":"lifecycle","weight":1,"resolve_after":"299s"},{"scenario":"reopen","weight":1,"resolve_after":"301s"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := integrationConfig()
	c.Scenario = "mixed"
	c.ScenarioFile = path
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	c.URL = server.URL
	report, err := Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	want := expectedHistogram(299, uint64(c.BatchSize))
	want.merge(expectedHistogram(301, 2*uint64(c.BatchSize)))
	if !reflect.DeepEqual(report.ExpectedDurationHistogram, want) {
		t.Fatalf("profile-specific duration lost: %v want %v", report.ExpectedDurationHistogram, want)
	}
}
