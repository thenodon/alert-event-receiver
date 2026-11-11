package alertload

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"
)

func integrationConfig() Config {
	c := testConfig()
	c.Rate = 20
	c.Duration = 100 * time.Millisecond
	return c
}

func TestPhaseOrderingAndExpectedOutcomes(t *testing.T) {
	for _, scenario := range []string{"firing", "lifecycle", "duplicates", "orphans"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			var payloads []Payload
			states := map[string]string{}
			observed := map[string]uint64{}
			var closedDurations []time.Duration
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Error("invalid HTTP contract")
				}
				var p Payload
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
					http.Error(w, "invalid", 400)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				payloads = append(payloads, p)
				for _, a := range p.Alerts {
					if a.Status != p.Status || a.Fingerprint == "" || a.StartsAt.IsZero() {
						t.Error("invalid homogeneous batch")
					}
					key := a.Fingerprint + "/" + a.StartsAt.Format(time.RFC3339Nano)
					switch {
					case a.Status == "firing" && states[key] == "":
						observed["opened"]++
						states[key] = "firing"
					case a.Status == "firing" && states[key] == "firing":
						observed["repeated_firing"]++
					case a.Status == "resolved" && states[key] == "firing":
						observed["closed"]++
						states[key] = "closed"
						closedDurations = append(closedDurations, a.EndsAt.Sub(a.StartsAt))
					case a.Status == "resolved" && states[key] == "closed":
						observed["repeated_resolved"]++
					case a.Status == "resolved" && states[key] == "":
						observed["resolved_without_open_state"]++
						states[key] = "closed"
					default:
						t.Errorf("unexpected lifecycle order: %+v", a)
					}
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			c := integrationConfig()
			c.URL = server.URL
			c.Scenario = scenario
			c.ResolveAfter = 24 * time.Hour
			c.BatchSize = 3
			c.Workers = 4
			report, err := Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if report.Failed {
				t.Fatalf("unexpected failed run: %+v", report)
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(observed, report.AcceptedOutcomes) {
				t.Fatalf("expected outcomes differ: observed=%v report=%v", observed, report.AcceptedOutcomes)
			}
			if uint64(len(payloads)) != report.SentRequests || report.AcceptedAlerts != report.SentRequests*uint64(c.BatchSize) {
				t.Fatal("HTTP/alert accounting differs")
			}
			n := uint64(c.RequestsPerPhase() * c.BatchSize)
			wantClosed := uint64(0)
			if scenario == "lifecycle" || scenario == "duplicates" {
				wantClosed = n
			}
			if report.ExpectedDurationHistogram.Count != wantClosed || uint64(len(closedDurations)) != wantClosed {
				t.Fatal("unexpected closure observations")
			}
			for _, duration := range closedDurations {
				if duration != c.ResolveAfter {
					t.Fatal("duration used wall-clock time")
				}
			}
			if scenario == "duplicates" && report.DuplicateCandidates != 2*n {
				t.Fatal("duplicates created unexpected closure counts")
			}
			if scenario == "orphans" && (report.OrphanCandidates != n || report.ExpectedDurationHistogram.Count != 0) {
				t.Fatal("orphan affected histogram")
			}
			if report.PeakInflight > c.Workers || report.PeakInflight < 1 {
				t.Fatal("worker limit violated")
			}
			for _, p := range report.Phases {
				if p.HTTPLatency.Count != p.SentRequests || p.PayloadBytes.Count != p.SentRequests || p.QueueWait.P95 < 0 {
					t.Fatal("incomplete bounded distributions")
				}
			}
			var data bytes.Buffer
			if err := WriteJSON(&data, report); err != nil {
				t.Fatal(err)
			}
			var decoded Report
			if err := json.Unmarshal(data.Bytes(), &decoded); err != nil || decoded.SchemaVersion != 1 {
				t.Fatal("invalid JSON report", err)
			}
		})
	}
}

func TestOnlySuccessfulFiringsAreResolved(t *testing.T) {
	for _, status := range []int{400, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var mu sync.Mutex
			var requests []Payload
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p Payload
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				requests = append(requests, p)
				mu.Unlock()
				if p.Status == "firing" && p.Alerts[0].Labels["load_occurrence"] != "0" {
					http.Error(w, "private-token", status)
					return
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			c := integrationConfig()
			c.URL = server.URL
			c.MaxFailureRate = 1
			report, err := Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 3 || report.Failed || report.MatchedClosureCandidates != uint64(c.BatchSize) {
				t.Fatalf("eligibility failed: %+v requests=%d", report, len(requests))
			}
			for _, p := range requests {
				if p.Status == "resolved" && p.Alerts[0].Labels["load_occurrence"] != "0" {
					t.Fatal("resolved failed/ambiguous firing")
				}
			}
			if status == 500 && report.UnknownAlerts != uint64(c.BatchSize) {
				t.Fatal("5xx not marked unknown")
			}
			if status == 400 && report.Phases[0].RejectedAlerts != uint64(c.BatchSize) {
				t.Fatal("4xx not counted")
			}
			var data bytes.Buffer
			_ = WriteJSON(&data, report)
			if bytes.Contains(data.Bytes(), []byte("private-token")) {
				t.Fatal("retained response body secret")
			}
		})
	}
}

func TestTimeoutNetworkAndRedirectFailures(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
		defer server.Close()
		c := integrationConfig()
		c.URL = server.URL
		c.Timeout = 5 * time.Millisecond
		report, err := Run(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Failed || report.Phases[0].Timeouts == 0 || report.UnknownAlerts == 0 || report.MatchedClosureCandidates != 0 {
			t.Fatalf("timeout hidden: %+v", report)
		}
	})
	t.Run("transport", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		endpoint := server.URL
		server.Close()
		c := integrationConfig()
		c.URL = endpoint
		c.Scenario = "firing"
		report, err := Run(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Failed || report.Phases[0].NetworkErrors == 0 || report.UnknownAlerts == 0 {
			t.Fatal("network failure hidden")
		}
	})
	t.Run("redirect not followed", func(t *testing.T) {
		var mu sync.Mutex
		followed := 0
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { mu.Lock(); followed++; mu.Unlock() }))
		defer target.Close()
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/private-token", http.StatusTemporaryRedirect)
		}))
		defer redirect.Close()
		c := integrationConfig()
		c.URL = redirect.URL
		c.Scenario = "firing"
		report, err := Run(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if followed != 0 || !report.Failed || report.Phases[0].ResponseClasses["3xx"] == 0 {
			t.Fatal("redirect safety failed")
		}
	})
}

func TestSaturationDrainAndCancellation(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "queue saturation", true: "interrupt"}[interrupt], func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			entered := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			c := integrationConfig()
			c.URL = server.URL
			c.Scenario = "firing"
			c.Rate = 10000
			c.Duration = 50 * time.Millisecond
			c.MaxPending = 1
			c.Workers = 1
			c.DrainTimeout = 5 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Report, 1)
			go func() {
				report, err := Run(ctx, c)
				if err != nil {
					t.Error(err)
				}
				done <- report
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				unblock()
				t.Fatal("worker did not start")
			}
			if interrupt {
				cancel()
			}
			select {
			case report := <-done:
				unblock()
				if !report.Failed || !report.Phases[0].DrainTimedOut || report.PeakInflight != 1 {
					t.Fatalf("unbounded workers/drain: %+v", report)
				}
				if report.Interrupted != interrupt {
					t.Fatal("interrupt status incorrect")
				}
				if !interrupt && report.Phases[0].QueueFull == 0 {
					t.Fatal("open loop silently blocked on saturated queue")
				}
				p := report.Phases[0]
				if p.OfferedRequests != p.ScheduledRequests+p.QueueFull+p.MissedDeadline || p.ScheduledRequests != p.SentRequests+p.Abandoned {
					t.Fatal("lost request accounting")
				}
			case <-time.After(2 * time.Second):
				unblock()
				t.Fatal("run did not terminate after bounded drain")
			}
		})
	}
}

func TestFailureThresholdAndDropPolicy(t *testing.T) {
	c := testConfig()
	c.MaxFailureRate = .5
	c.FailOnDrop = false
	r := newReport(c)
	r.Phases = []PhaseReport{{SentRequests: 2, FailedRequests: 1, QueueFull: 1}}
	r.finish(c, time.Now())
	if r.Failed {
		t.Fatal("threshold equality or optional drop policy failed")
	}
	c.MaxFailureRate = .49
	r = newReport(c)
	r.Phases = []PhaseReport{{SentRequests: 2, FailedRequests: 1}}
	r.finish(c, time.Now())
	if !r.Failed {
		t.Fatal("failure threshold ignored")
	}
	c.MaxFailureRate = 1
	c.FailOnDrop = true
	r = newReport(c)
	r.Phases = []PhaseReport{{QueueFull: 1}}
	r.finish(c, time.Now())
	if !r.Failed {
		t.Fatal("queue overflow did not fail run")
	}
}
