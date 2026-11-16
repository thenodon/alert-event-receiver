package alertload

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestWeightedPlanDeterministic(t *testing.T) {
	c := testConfig()
	c.Scenario = "mixed"
	c.Mix = "firing=1,lifecycle=2,orphans=1"
	c.Rate = 100
	c.Duration = 100 * time.Millisecond
	a, profiles, err := buildPlan(c)
	if err != nil {
		t.Fatal(err)
	}
	b, again, err := buildPlan(c)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(profiles, again) || len(a) != 2 {
		t.Fatal("nondeterministic profile allocation")
	}
	counts := map[string]int{}
	for i := 0; i < c.RequestsPerPhase(); i++ {
		x, ok := a[0].at(i, true)
		y, ok2 := b[0].at(i, true)
		if !ok || !ok2 || x.profile != y.profile || !reflect.DeepEqual(payloadForStep(c, i, x), payloadForStep(c, i, y)) {
			t.Fatal("nondeterministic planned payload")
		}
		counts[x.profile]++
	}
	if counts["firing"] != 3 || counts["lifecycle"] != 5 || counts["orphans"] != 2 {
		t.Fatalf("largest remainder allocation: %v", counts)
	}
	c.Seed++
	other, _, _ := buildPlan(c)
	same := true
	for i := 0; i < c.RequestsPerPhase(); i++ {
		x, _ := a[0].at(i, true)
		y, _ := other[0].at(i, true)
		same = same && x.profile == y.profile
	}
	if same {
		t.Fatal("seed did not shuffle cohort assignment")
	}
}

func TestScenarioFiles(t *testing.T) {
	dir := t.TempDir()
	jsonFile := filepath.Join(dir, "profiles.json")
	csvFile := filepath.Join(dir, "profiles.csv")
	if err := os.WriteFile(jsonFile, []byte(`{"profiles":[{"scenario":"reopen","weight":1,"resolve_after":"0s"},{"scenario":"lifecycle","weight":2,"resolve_after":"301s"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(csvFile, []byte("scenario,weight,resolve_after\nlifecycle,2,301s\nreopen,1,0s\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := ParseConfig([]string{"-run-id", "file-test", "-scenario-file", jsonFile}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.ScenarioFile = csvFile
	pa, wa, err := buildPlan(a)
	if err != nil {
		t.Fatal(err)
	}
	pb, wb, err := buildPlan(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wa, wb) {
		t.Fatalf("file formats differ: %v %v", wa, wb)
	}
	for i := 0; i < a.RequestsPerPhase(); i++ {
		x, _ := pa[0].at(i, true)
		y, _ := pb[0].at(i, true)
		if !reflect.DeepEqual(payloadForStep(a, i, x), payloadForStep(b, i, y)) {
			t.Fatal("file formats changed payload")
		}
	}
	for _, body := range []string{
		`{"profiles":[]}`, `{"profiles":[{"scenario":"unknown","weight":1}]}`, `{"profiles":[{"scenario":"firing","weight":0}]}`,
		`{"profiles":[{"scenario":"firing","weight":1},{"scenario":"firing","weight":2}]}`,
		`{"profiles":[{"scenario":"lifecycle","weight":1,"resolve_after":"-1s"}]}`,
		`{"profiles":[{"scenario":"firing","weight":1,"secret":"private-token"}]}`, `{"profiles":[]} {}`,
	} {
		if err := os.WriteFile(jsonFile, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := a.Validate(); err == nil {
			t.Fatalf("accepted invalid scenario: %s", body)
		}
	}
	if _, err := ParseConfig([]string{"-scenario-file", csvFile, "-mix", "firing=1"}, io.Discard); err == nil {
		t.Fatal("ambiguous weights accepted")
	}
	if _, err := ParseConfig([]string{"-scenario-file", csvFile, "-scenario", "firing"}, io.Discard); err == nil {
		t.Fatal("scenario file silently ignored")
	}
	c := testConfig()
	c.Scenario = "mixed"
	c.Mix = "firing=1,firing=2"
	if c.Validate() == nil {
		t.Fatal("duplicate mix profile accepted")
	}
}

// This test server models only the externally documented ordering rules; it
// does not import receiver processor/state implementation packages.
func TestPhaseTwoProfilesOverHTTP(t *testing.T) {
	for _, scenario := range []string{"stale", "reopen", "supersede", "mixed"} {
		t.Run(scenario, func(t *testing.T) {
			type state struct {
				start  time.Time
				closed bool
			}
			states := map[string]state{}
			outcomes := map[string]uint64{}
			var histogram ExpectedHistogram
			histogram = expectedHistogram(0, 0)
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p Payload
				if json.NewDecoder(r.Body).Decode(&p) != nil {
					t.Error("invalid payload")
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, a := range p.Alerts {
					current, exists := states[a.Fingerprint]
					outcome := ""
					switch {
					case exists && a.StartsAt.Before(current.start):
						if a.Status == "firing" {
							outcome = "stale_firing"
						} else {
							outcome = "stale_resolved"
						}
					case exists && a.StartsAt.Equal(current.start) && current.closed:
						if a.Status == "firing" {
							outcome = "stale_firing"
						} else {
							outcome = "repeated_resolved"
						}
					case exists && a.StartsAt.Equal(current.start) && a.Status == "firing":
						outcome = "repeated_firing"
					case a.Status == "firing":
						outcome = "opened"
						if exists {
							if current.closed {
								outcome = "reopened"
							} else {
								outcome = "superseded_firing"
							}
						}
						states[a.Fingerprint] = state{a.StartsAt, false}
					case a.Status == "resolved":
						outcome = "resolved_without_open_state"
						if exists && a.StartsAt.Equal(current.start) {
							outcome = "closed"
							histogram.observe(a.EndsAt.Sub(a.StartsAt).Seconds(), 1)
						}
						states[a.Fingerprint] = state{a.StartsAt, true}
					}
					if a.Status != p.Status {
						t.Error("heterogeneous notification")
					}
					outcomes[outcome]++
				}
				w.WriteHeader(204)
			}))
			defer server.Close()
			c := integrationConfig()
			c.Scenario = scenario
			c.URL = server.URL
			c.ResolveAfter = 5 * time.Minute
			c.BatchSize = 2
			if scenario == "mixed" {
				c.Rate = 80
				c.Duration = 100 * time.Millisecond
				c.Mix = "firing=1,lifecycle=1,duplicates=1,orphans=1,stale=1,reopen=1,supersede=1"
			}
			report, err := Run(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if report.Failed {
				t.Fatalf("failed run: %+v", report)
			}
			mu.Lock()
			defer mu.Unlock()
			// The report includes zero-valued cohort profiles too, so compare values.
			for outcome, count := range outcomes {
				if report.AcceptedOutcomes[outcome] != count {
					t.Fatalf("%s got=%v want=%v", outcome, report.AcceptedOutcomes, outcomes)
				}
			}
			for outcome, count := range report.AcceptedOutcomes {
				if outcomes[outcome] != count {
					t.Fatalf("extra expected outcome %s", outcome)
				}
			}
			if !reflect.DeepEqual(histogram, report.ExpectedDurationHistogram) {
				t.Fatalf("duration expectations differ: %+v vs %+v", histogram, report.ExpectedDurationHistogram)
			}
			n := uint64(c.RequestsPerPhase() * c.BatchSize)
			if scenario == "reopen" && (outcomes["closed"] != 2*n || outcomes["reopened"] != n) {
				t.Fatal("reopen did not preserve two distinct occurrences")
			}
			if scenario == "supersede" && (outcomes["superseded_firing"] != n || outcomes["stale_resolved"] != n) {
				t.Fatal("supersede profile incorrect")
			}
			var data bytes.Buffer
			if err := WriteJSON(&data, report); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPhaseTwoFailureGatesAllDependentSteps(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.Error(w, "ambiguous state failure", 500)
	}))
	defer server.Close()
	c := integrationConfig()
	c.URL = server.URL
	c.Scenario = "reopen"
	report, err := Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != c.RequestsPerPhase() || report.MatchedClosureCandidates != 0 || report.UnknownAlerts == 0 {
		t.Fatal("failed predecessors were reused")
	}
	for _, phase := range report.Phases[1:] {
		if phase.SentRequests != 0 {
			t.Fatal("dependent phase sent after failed firing")
		}
	}
}

func TestScenarioFileSizeBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte(" "), (1<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	c.Scenario = "mixed"
	c.ScenarioFile = path
	if err := c.Validate(); err == nil {
		t.Fatal(fmt.Sprint("oversized scenario accepted"))
	}
}
