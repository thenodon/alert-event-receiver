package slaproxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/andersh/alertmanager_event_receiver/internal/httpsecurity/securitytest"
)

func TestInspectionPhase2(t *testing.T) {
	securitytest.ClearEnv(t)
	cfg := testConfig(t)
	cfg.Durable.Enabled = true
	cfg.Durable.Valkey.Address = miniredis.RunT(t).Addr()
	cfg.Delivery.Enabled = true
	cfg.Delivery.Sinks = []SinkConfig{{ID: "audit", Type: "log"}, {ID: "servicenow", Type: "log"}}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	get := func(path string, want int, result any) {
		t.Helper()
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: HTTP %d, want %d: %s", path, w.Code, want, w.Body)
		}
		if result != nil {
			if err := json.Unmarshal(w.Body.Bytes(), result); err != nil {
				t.Fatal(err)
			}
		}
	}
	alert := testAlert("firing", map[string]string{"sla_id": "business"})
	if w := post(s, testPayload(t, alert)); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body)
	}
	id := occurrenceKeyID(alert)
	ref := PolicyRef{SLALabel: "sla_id", SLAValue: "business"}
	audit := bindingKeyID(id, ref, "audit")
	serviceNow := bindingKeyID(id, ref, "servicenow")

	t.Run("window uses pinned policy and response time", func(t *testing.T) {
		open := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
		closed := open.Add(3 * time.Hour) // Noon in Stockholm.
		s.now = func() time.Time { return open }
		var page inspectionPage[occurrenceListItem]
		get(inspectionPath+"?sla_window=open&sink_id=audit", http.StatusOK, &page)
		if len(page.Items) != 1 || !page.AsOf.Equal(open) || page.Items[0].Bindings[0].SLAWindow != windowOpen || page.Items[0].Bindings[0].SLAWindowReason != "" {
			t.Fatalf("unexpected open page: %+v", page)
		}

		s.now = func() time.Time { return closed }
		get(inspectionPath+"?sla_window=open", http.StatusOK, &page)
		if len(page.Items) != 0 {
			t.Fatal("outside the window matched open")
		}
		var detail occurrenceDetail
		get(inspectionPath+"/"+id, http.StatusOK, &detail)
		if !detail.AsOf.Equal(closed) || detail.Bindings[0].SLAWindow != windowClosed ||
			detail.Bindings[0].Eligibility != EligibilityEligible {
			t.Fatalf("current window changed stored eligibility: %+v", detail)
		}
		get(inspectionPath+"?sla_window=invalid", http.StatusBadRequest, nil)

		policy := s.durable.engine.policies[ref]
		changed := policy
		changed.version = "another-version"
		s.durable.engine.policies[ref] = changed
		get(inspectionPath+"?sla_window=unknown", http.StatusOK, &page)
		s.durable.engine.policies[ref] = policy
		if len(page.Items) != 1 || page.Items[0].Bindings[0].SLAWindow != windowUnknown || page.Items[0].Bindings[0].SLAWindowReason != "policy_changed" {
			t.Fatalf("mismatched pinned policy must be unknown: %+v", page)
		}
		uncovered := policy
		uncovered.dataset = &holidayDataset{firstYear: 2027, lastYear: 2027}
		s.durable.engine.policies[ref] = uncovered
		get(inspectionPath+"?sla_window=unknown", http.StatusOK, &page)
		s.durable.engine.policies[ref] = policy
		if len(page.Items) != 1 || page.Items[0].Bindings[0].SLAWindow != windowUnknown || page.Items[0].Bindings[0].SLAWindowReason != "holiday_coverage_missing" {
			t.Fatalf("uncovered day must be unknown: %+v", page)
		}
	})

	t.Run("delivery work and pagination", func(t *testing.T) {
		s.now = time.Now
		var first struct {
			inspectionPage[deliveryWorkItem]
			DeliveryEnabled bool `json:"delivery_enabled"`
		}
		get(inspectionDeliveryPath+"?action=firing&state=due&limit=1", http.StatusOK, &first)
		if !first.DeliveryEnabled || len(first.Items) != 1 || first.NextCursor == "" || first.Items[0].DueAt == nil ||
			first.Items[0].OccurrenceURL != inspectionPath+"/"+id {
			t.Fatalf("unexpected first delivery page: %+v", first)
		}
		s.now = func() time.Time { return first.AsOf.Add(time.Hour) }
		var second inspectionPage[deliveryWorkItem]
		get(inspectionDeliveryPath+"?action=firing&state=due&cursor="+url.QueryEscape(first.NextCursor), http.StatusOK, &second)
		if len(second.Items) != 1 || second.Items[0].SinkID == first.Items[0].SinkID || second.NextCursor != "" ||
			!second.AsOf.Equal(first.AsOf) {
			t.Fatalf("unexpected second delivery page: %+v", second)
		}
		get(inspectionPath+"?cursor="+url.QueryEscape(first.NextCursor), http.StatusBadRequest, nil)
		get(inspectionDeliveryPath+"?action=resolve", http.StatusBadRequest, nil)
		get(inspectionDeliveryPath+"?state=invalid", http.StatusBadRequest, nil)

		ctx := context.Background()
		claimed, err := s.dispatcher.claim(ctx, deliveryWorkID(id, audit))
		if err != nil {
			t.Fatal(err)
		}
		leaseUntil := claimed.Bindings[audit].Claim.Until
		s.now = func() time.Time { return leaseUntil.Add(-time.Nanosecond) }
		second = inspectionPage[deliveryWorkItem]{}
		get(inspectionDeliveryPath+"?sink_id=audit&state=claimed", http.StatusOK, &second)
		if len(second.Items) != 1 || second.Items[0].LeaseExpiresAt == nil || second.Items[0].NextAttemptAt != nil {
			t.Fatalf("unexpected claimed work: %+v", second)
		}
		s.now = func() time.Time { return leaseUntil }
		second = inspectionPage[deliveryWorkItem]{}
		get(inspectionDeliveryPath+"?state=claim_expired", http.StatusOK, &second)
		if len(second.Items) != 1 || second.Items[0].LeaseExpiresAt == nil || !second.Items[0].LeaseExpiresAt.Equal(leaseUntil) {
			t.Fatalf("unexpected expired claim: %+v", second)
		}

		retryAt := leaseUntil.Add(time.Minute)
		_, err = s.durable.store.updateDelivery(ctx, id, claimed.Bindings[audit].Claim, func(row *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
			row.Bindings[audit].HoldReason = "policy_changed"
			row.Bindings[audit].Claim = nil
			i := row.Bindings[serviceNow].Intents["firing"]
			i.NextAttemptAt, i.LastFailure = retryAt, "temporary_failure"
			return row, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		second = inspectionPage[deliveryWorkItem]{}
		get(inspectionDeliveryPath+"?sink_id=servicenow&state=retry_scheduled", http.StatusOK, &second)
		if len(second.Items) != 1 || second.Items[0].NextAttemptAt == nil || !second.Items[0].NextAttemptAt.Equal(retryAt) || second.Items[0].LeaseExpiresAt != nil {
			t.Fatalf("unexpected retry: %+v", second)
		}
		second = inspectionPage[deliveryWorkItem]{}
		get(inspectionDeliveryPath+"?state=held", http.StatusOK, &second)
		if len(second.Items) != 1 || second.Items[0].HoldReason != "policy_changed" || second.Items[0].SinkID != "audit" {
			t.Fatalf("unexpected held work: %+v", second)
		}
	})
}
