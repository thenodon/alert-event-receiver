package alertload

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	receivermodels "github.com/andersh/alertmanager_event_receiver/internal/models"
)

func testConfig() Config {
	c := DefaultConfig()
	c.RunID = "test-run"
	c.Rate = 200
	c.Duration = 30 * time.Millisecond
	c.Timeout = time.Second
	c.DrainTimeout = time.Second
	c.Workers = 3
	c.BatchSize = 2
	return c
}

func TestPayloadDeterminismAndReceiverWireModel(t *testing.T) {
	c := testConfig()
	c.BaseTime = time.Date(2026, 11, 9, 10, 0, 0, 123456789, time.FixedZone("local", 3600))
	a := PayloadFor(c, 7, "firing")
	b := PayloadFor(c, 7, "firing")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("generation is not deterministic")
	}
	c.BaseTime = c.BaseTime.UTC()
	if !reflect.DeepEqual(a, PayloadFor(c, 7, "firing")) {
		t.Fatal("timezone offset changed payload")
	}
	resolved := PayloadFor(c, 7, "resolved")
	for i, alert := range a.Alerts {
		r := resolved.Alerts[i]
		if r.Fingerprint != alert.Fingerprint || !r.StartsAt.Equal(alert.StartsAt) || r.EndsAt == nil || r.EndsAt.Sub(r.StartsAt) != c.ResolveAfter {
			t.Fatal("lost occurrence identity or synthetic timing")
		}
		if r.Labels["load_run"] != c.RunID || r.Labels["load_occurrence"] == "" {
			t.Fatal("missing isolation labels")
		}
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	var receiver receivermodels.WebhookPayload
	if err := json.Unmarshal(data, &receiver); err != nil {
		t.Fatal(err)
	}
	if receiver.Version != "4" || receiver.Status != "resolved" || receiver.GroupKey != "alertload" || len(receiver.Alerts) != c.BatchSize {
		t.Fatalf("wire mismatch: %+v", receiver)
	}
	for i, alert := range receiver.Alerts {
		if alert.Fingerprint != resolved.Alerts[i].Fingerprint || !alert.StartsAt.Equal(resolved.Alerts[i].StartsAt) || !alert.EndsAt.Equal(*resolved.Alerts[i].EndsAt) || alert.Status != receiver.Status {
			t.Fatal("receiver cannot decode generated occurrence")
		}
	}
	seen := map[string]bool{}
	for _, run := range []string{"run-a", "run-b"} {
		for _, seed := range []int64{1, 2} {
			c.RunID = run
			c.Seed = seed
			for n := 0; n < 5; n++ {
				for _, alert := range PayloadFor(c, n, "firing").Alerts {
					if seen[alert.Fingerprint] {
						t.Fatal("fingerprint collision")
					}
					seen[alert.Fingerprint] = true
				}
			}
		}
	}
}

func TestDurationBoundaryExpectations(t *testing.T) {
	for _, seconds := range []float64{0, 299, 300, 301, 3600, 86400, 604800, 604801} {
		c := testConfig()
		c.ResolveAfter = time.Duration(seconds * float64(time.Second))
		p := PayloadFor(c, 0, "resolved")
		for _, alert := range p.Alerts {
			if alert.EndsAt.Sub(alert.StartsAt).Seconds() != seconds {
				t.Fatal("synthetic duration changed")
			}
		}
		h := expectedHistogram(seconds, 3)
		if h.Count != 3 || h.Sum != seconds*3 || h.Buckets["+Inf"] != 3 {
			t.Fatal("incorrect expected count/sum")
		}
		for _, bound := range []float64{300, 3600, 86400, 604800} {
			want := uint64(0)
			if seconds <= bound {
				want = 3
			}
			if h.Buckets[formatBound(bound)] != want {
				t.Fatalf("seconds=%v bucket=%v got=%v want=%v", seconds, bound, h.Buckets[formatBound(bound)], want)
			}
		}
	}
}

func TestBoundedRecorderQuantiles(t *testing.T) {
	var r recorder
	for i := 1; i <= 10000; i++ {
		r.add(float64(i))
	}
	d := r.summary(1)
	if d.Count != 10000 || d.Max != 10000 || d.Mean != 5000.5 {
		t.Fatalf("incorrect recorder: %+v", d)
	}
	for _, pair := range [][2]float64{{d.P50, 5000}, {d.P90, 9000}, {d.P95, 9500}, {d.P99, 9900}} {
		if pair[0] < pair[1] || pair[0] > pair[1]*1.05+1 {
			t.Fatalf("quantile outside bounded-bin tolerance: %v", pair)
		}
	}
	var empty recorder
	if empty.summary(1) != (Distribution{}) {
		t.Fatal("empty histogram has nonzero values")
	}
	empty.add(0)
	if empty.summary(1).Max != 0 || empty.summary(1).P95 != 0 {
		t.Fatal("zero samples mishandled")
	}
}
