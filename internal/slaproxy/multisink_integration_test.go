//go:build integration

package slaproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

func multiConfig(t *testing.T, f *valkeyFixture, url string) Config {
	c := deliveryConfig(t, f)
	c.Delivery.Dispatcher.DeliveryTimeout = 500 * time.Millisecond
	c.Delivery.Dispatcher.ClaimLease = 3 * time.Second
	c.Delivery.Sinks = []SinkConfig{{ID: "servicenow", Type: "http", HTTP: &HTTPSinkConfig{URL: url, Timeout: 500 * time.Millisecond}}, {ID: "audit", Type: "log"}}
	return c
}
func sinkBinding(t testing.TB, row *DurableOccurrence, sink string) *DurableBinding {
	t.Helper()
	for _, b := range row.Bindings {
		if b.SinkID == sink {
			return b
		}
	}
	t.Fatalf("missing sink %s", sink)
	return nil
}
func dispatchSink(t *testing.T, s *Service, id, sink string) {
	t.Helper()
	row := deliveryRow(t, s, id)
	b := sinkBinding(t, row, sink)
	if err := s.dispatcher.runOne(context.Background(), deliveryWorkID(id, b.ID)); err != nil {
		t.Fatal(err)
	}
}
func TestMultiSinkIndependentRetriesAndOrdering(t *testing.T) {
	for _, failed := range []string{"servicenow", "audit", "both"} {
		t.Run(failed, func(t *testing.T) {
			f := realValkey(t)
			var httpCalls atomic.Int64
			var mu sync.Mutex
			var requests []models.WebhookPayload
			var keys []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p models.WebhookPayload
				if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
					t.Error(err)
				}
				mu.Lock()
				requests = append(requests, p)
				keys = append(keys, r.Header.Get("Idempotency-Key"))
				mu.Unlock()
				n := httpCalls.Add(1)
				if (failed == "servicenow" || failed == "both") && n == 1 {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(202)
			}))
			defer server.Close()
			cfg := multiConfig(t, f, server.URL)
			s := deliveryService(t, cfg, nil)
			var auditCalls atomic.Int64
			audit := &controlledSink{call: func(context.Context, DeliveryEvent) error {
				if auditCalls.Add(1) == 1 && (failed == "audit" || failed == "both") {
					return errors.New("temporary")
				}
				return nil
			}}
			s.dispatcher.sinks["audit"] = audit
			row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
			if len(row.Bindings) != 2 {
				t.Fatal("missing frozen bindings")
			}
			for _, sink := range []string{"servicenow", "audit"} {
				dispatchSink(t, s, row.ID, sink)
			}
			first := deliveryRow(t, s, row.ID)
			for _, sink := range []string{"servicenow", "audit"} {
				b := sinkBinding(t, first, sink)
				failedHere := failed == sink || failed == "both"
				if (b.Intents["firing"].Receipt == nil) != failedHere {
					t.Fatal("non-independent receipt", sink, b)
				}
			}
			// Duplicate webhook and rediscovery must not resend the successful sink.
			acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
			for _, sink := range []string{"servicenow", "audit"} {
				b := sinkBinding(t, first, sink)
				if b.Intents["firing"].Receipt == nil {
					waitPast(t, b.Intents["firing"].NextAttemptAt)
				}
				dispatchSink(t, s, row.ID, sink)
			}
			acceptTest(t, s.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:30:00+01:00")
			for _, sink := range []string{"servicenow", "audit"} {
				dispatchSink(t, s, row.ID, sink)
			}
			acceptTest(t, s.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:30:00+01:00")
			for _, sink := range []string{"servicenow", "audit"} {
				dispatchSink(t, s, row.ID, sink)
			}
			final := deliveryRow(t, s, row.ID)
			for _, b := range final.Bindings {
				if b.Intents["firing"].Receipt == nil || b.Intents["resolved"].Receipt == nil || b.Claim != nil {
					t.Fatal("missing ordered receipts", b)
				}
			}
			events := audit.snapshot()
			if len(events) != 2 || events[0].Action != "firing" || events[1].Action != "resolved" {
				t.Fatal(events)
			}
			mu.Lock()
			defer mu.Unlock()
			want := 2
			if failed == "servicenow" || failed == "both" {
				want = 3
				if keys[0] != keys[1] {
					t.Fatal("unstable retry identity")
				}
			}
			if len(requests) != want || requests[len(requests)-1].Status != "resolved" {
				t.Fatal("HTTP retry/ordering", requests)
			}
			for _, p := range requests {
				if len(p.Alerts) != 1 || p.Alerts[0].Fingerprint != row.Fingerprint {
					t.Fatal("invalid single-alert payload")
				}
			}
		})
	}
}

func TestMultiSinkBlockedHTTPResolutionAndOtherSinkProgress(t *testing.T) {
	f := realValkey(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p models.WebhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p.Status == "firing" {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		mu.Lock()
		actions = append(actions, p.Status)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()
	cfg := multiConfig(t, f, server.URL)
	cfg.Delivery.Dispatcher.DeliveryTimeout = 2 * time.Second
	cfg.Delivery.Sinks[0].HTTP.Timeout = 2 * time.Second
	cfg.Delivery.Dispatcher.ClaimLease = 4 * time.Second
	first, second := deliveryService(t, cfg, nil), deliveryService(t, cfg, nil)
	audit := &controlledSink{}
	first.dispatcher.sinks["audit"] = audit
	second.dispatcher.sinks["audit"] = audit
	row := acceptTest(t, first.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	bid := sinkBinding(t, row, "servicenow").ID
	done := make(chan error, 1)
	go func() { done <- first.dispatcher.runOne(context.Background(), deliveryWorkID(row.ID, bid)) }()
	await(t, entered)
	// The audit sink can commit while the HTTP operation owns its separate claim.
	dispatchSink(t, second, row.ID, "audit")
	firing := deliveryRow(t, second, row.ID)
	if sinkBinding(t, firing, "audit").Intents["firing"].Receipt == nil || sinkBinding(t, firing, "servicenow").Claim == nil {
		t.Fatal("slow sink blocks independent delivery")
	}
	acceptTest(t, second.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:30:00+01:00")
	dispatchSink(t, second, row.ID, "audit")
	dispatchSink(t, second, row.ID, "servicenow") // Active HTTP claim prevents resolution overtaking.
	if len(audit.snapshot()) != 2 {
		t.Fatal("resolution waited for unrelated HTTP sink")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	dispatchSink(t, second, row.ID, "servicenow")
	closed := deliveryRow(t, second, row.ID)
	for _, b := range closed.Bindings {
		if b.Intents["firing"].Receipt == nil || b.Intents["resolved"].Receipt == nil {
			t.Fatal(b)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(actions, ",") != "firing,resolved" {
		t.Fatal(actions)
	}
}

func TestMultiSinkCatalogFreezeAndChanges(t *testing.T) {
	f := realValkey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer server.Close()
	cfg := multiConfig(t, f, server.URL)
	cfg.Delivery.Sinks = cfg.Delivery.Sinks[:1]
	s := deliveryService(t, cfg, nil)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	pending := pendingSnapshot(t, s.durable)
	added := multiConfig(t, f, server.URL)
	added.Durable.PolicyEpoch++
	newer := deliveryService(t, added, nil)
	for _, id := range []string{row.ID, pending.ID} {
		var pendingWork []string
		if err := newer.durable.maintenance(context.Background(), &pendingWork); err != nil {
			t.Fatal(err)
		}
		old := deliveryRow(t, newer, id)
		if len(old.Bindings) != 1 || sinkBinding(t, old, "servicenow") == nil {
			t.Fatal("historical sink backfill")
		}
	}
	fresh := alertAt(t, "firing", "sla_id", "2026-01-05T08:31:00+01:00")
	fresh.Fingerprint = "new-catalog"
	accepted, err := newer.durable.accept(context.Background(), fresh, modelsPayload(fresh))
	if err != nil || len(accepted.Occurrence.Bindings) != 2 {
		t.Fatal("new sink not bound", err)
	}
	for _, kind := range []string{"remove", "endpoint", "type", "auth-env"} {
		t.Run(kind, func(t *testing.T) {
			bad := multiConfig(t, f, server.URL)
			bad.Durable.PolicyEpoch = 3
			switch kind {
			case "remove":
				bad.Delivery.Sinks = bad.Delivery.Sinks[:1]
			case "endpoint":
				bad.Delivery.Sinks[0].HTTP.URL += "/changed"
			case "type":
				bad.Delivery.Sinks[0] = SinkConfig{ID: "servicenow", Type: "log"}
			case "auth-env":
				bad.Delivery.Sinks[0].HTTP.BasicAuth.Username = "${TEST_MULTI_USER}"
				bad.Delivery.Sinks[0].HTTP.BasicAuth.Password = "${TEST_MULTI_PASSWORD}"
				t.Setenv("TEST_MULTI_USER", "user")
				t.Setenv("TEST_MULTI_PASSWORD", "password")
			}
			changed, err := New(bad, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			if err == nil {
				_ = changed.Close()
				t.Fatal("incompatible destination activated")
			}
			if err := newer.durable.store.health(context.Background()); err != nil {
				t.Fatal("failed activation altered catalog", err)
			}
		})
	}
	dispatchSink(t, newer, row.ID, "servicenow")
	if sinkBinding(t, deliveryRow(t, newer, row.ID), "servicenow").Intents["firing"].Receipt == nil {
		t.Fatal("outstanding delivery lost on addition")
	}
}

func TestMultiSinkHTTPReceiptOutageAndRestart(t *testing.T) {
	f := realValkey(t)
	var stop atomic.Bool
	stop.Store(true)
	var mu sync.Mutex
	var ids []string
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p models.WebhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		ids = append(ids, r.Header.Get("Idempotency-Key"))
		actions = append(actions, p.Status)
		mu.Unlock()
		if stop.Swap(false) {
			f.stop()
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	cfg := multiConfig(t, f, server.URL)
	cfg.Delivery.Dispatcher.DeliveryTimeout = 100 * time.Millisecond
	cfg.Delivery.Sinks[0].HTTP.Timeout = 100 * time.Millisecond
	cfg.Delivery.Dispatcher.ClaimLease = 500 * time.Millisecond
	s := deliveryService(t, cfg, nil)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	b := sinkBinding(t, row, "servicenow")
	if err := s.dispatcher.runOne(context.Background(), deliveryWorkID(row.ID, b.ID)); err == nil {
		t.Fatal("receipt acknowledged during outage")
	}
	f.start()
	restarted := deliveryService(t, cfg, nil)
	persisted := deliveryRow(t, restarted, row.ID)
	b = sinkBinding(t, persisted, "servicenow")
	if b.Intents["firing"].Receipt != nil || b.Claim == nil {
		t.Fatal("ambiguous send forgotten")
	}
	acceptTest(t, restarted.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:30:00+01:00")
	waitPast(t, b.Claim.Until)
	dispatchSink(t, restarted, row.ID, "servicenow")
	dispatchSink(t, restarted, row.ID, "servicenow")
	dispatchSink(t, restarted, row.ID, "audit")
	final := deliveryRow(t, restarted, row.ID)
	if sinkBinding(t, final, "servicenow").Intents["resolved"].Receipt == nil || sinkBinding(t, final, "audit").Intents["firing"].Receipt != nil {
		t.Fatal("per-sink recovery ordering lost")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(actions, ",") != "firing,firing,resolved" || ids[0] != ids[1] {
		t.Fatal(actions, ids)
	}
}

func TestMultiSinkPendingActivationCancellationAndPolicyHold(t *testing.T) {
	for _, mode := range []string{"activate", "early-resolution", "policy-change"} {
		t.Run(mode, func(t *testing.T) {
			f := realValkey(t)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
			defer server.Close()
			cfg := multiConfig(t, f, server.URL)
			s := deliveryService(t, cfg, nil)
			audit := &controlledSink{}
			s.dispatcher.sinks["audit"] = audit
			row := pendingSnapshot(t, s.durable)
			for _, sink := range []string{"servicenow", "audit"} {
				dispatchSink(t, s, row.ID, sink)
			}
			if calls.Load() != 0 || len(audit.snapshot()) != 0 {
				t.Fatal("delivered before eligibility")
			}
			switch mode {
			case "early-resolution":
				acceptTest(t, s.durable, "resolved", "2026-01-05T07:30:00+01:00", "2026-01-05T07:45:00+01:00")
			case "policy-change":
				cfg.Durable.PolicyEpoch++
				p := cfg.SLADefinitions[0].SLAs["business"]
				p.Schedule = map[string][]string{"monday": {"09:00-17:00"}}
				cfg.SLADefinitions[0].SLAs["business"] = p
				s = deliveryService(t, cfg, nil)
				s.dispatcher.sinks["audit"] = audit
				var pending []string
				if err := s.durable.maintenance(context.Background(), &pending); err != nil {
					t.Fatal(err)
				}
			default:
				if err := s.durable.runOne(context.Background(), row.ID); err != nil {
					t.Fatal(err)
				}
			}
			for _, sink := range []string{"servicenow", "audit"} {
				dispatchSink(t, s, row.ID, sink)
			}
			expected := int64(0)
			if mode == "activate" {
				expected = 1
			}
			if calls.Load() != expected || int64(len(audit.snapshot())) != expected {
				t.Fatal("unexpected deliveries", mode, calls.Load(), audit.snapshot())
			}
			final := deliveryRow(t, s, row.ID)
			for _, b := range final.Bindings {
				if mode == "activate" && b.Intents["firing"].Receipt == nil {
					t.Fatal("missing receipt")
				}
				if mode == "policy-change" && b.HoldReason != "policy_changed" {
					t.Fatal("missing policy hold")
				}
				if mode == "early-resolution" && (b.Eligibility != EligibilityIneligible || !b.deliveryDueAt(final).IsZero()) {
					t.Fatal("early resolution remained actionable")
				}
			}
		})
	}
}

func TestMultiSinkBatchPersistsIndependentOriginalPayloads(t *testing.T) {
	f := realValkey(t)
	var mu sync.Mutex
	var received []models.WebhookPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p models.WebhookPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		received = append(received, p)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer server.Close()
	s := deliveryService(t, multiConfig(t, f, server.URL), nil)
	first := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00.123456789+01:00")
	first.Fingerprint = "one"
	first.Annotations = map[string]string{"summary": "first-only"}
	second := first
	second.Fingerprint = "two"
	second.Annotations = map[string]string{"summary": "second-only"}
	payload := modelsPayload(first)
	payload.Alerts = []models.Alert{first, second}
	payload.GroupKey = "original-group"
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if response := post(s, raw); response.Code != 200 {
		t.Fatal(response.Body)
	}
	restarted := deliveryService(t, s.cfg, nil)
	for _, a := range []models.Alert{first, second} {
		dispatchSink(t, restarted, occurrenceKeyID(a), "servicenow")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatal(received)
	}
	for i, p := range received {
		want := payload.Alerts[i]
		if len(p.Alerts) != 1 || p.Alerts[0].Fingerprint != want.Fingerprint || p.Alerts[0].Annotations["summary"] != want.Annotations["summary"] || !p.Alerts[0].StartsAt.Equal(want.StartsAt) || p.GroupKey != "original-group" {
			t.Fatal("cross-alert reconstruction", p)
		}
	}
}

func TestMultiSinkConcurrentInstancesAndIndexRepair(t *testing.T) {
	f := realValkey(t)
	var requests atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		w.WriteHeader(200)
	})
	firstHTTP := httptest.NewServer(handler)
	defer firstHTTP.Close()
	secondHTTP := httptest.NewServer(handler)
	defer secondHTTP.Close()
	cfg := multiConfig(t, f, firstHTTP.URL)
	cfg.Durable.Scheduler.OperationTimeout = time.Second
	cfg.Durable.Scheduler.LeaseDuration = 3 * time.Second
	cfg.Delivery.Dispatcher.ClaimLease = 5 * time.Second
	cfg.Delivery.Sinks = append(cfg.Delivery.Sinks, SinkConfig{ID: "second-http", Type: "http", HTTP: &HTTPSinkConfig{URL: secondHTTP.URL, Timeout: 500 * time.Millisecond}})
	services := []*Service{deliveryService(t, cfg, nil), deliveryService(t, cfg, nil)}
	audit := &controlledSink{}
	for _, s := range services {
		s.dispatcher.sinks["audit"] = audit
	}
	var ids []string
	var works []string
	for n := range 8 {
		a := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
		a.Fingerprint = string(rune('a' + n))
		accepted, err := services[0].durable.accept(context.Background(), a, modelsPayload(a))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, accepted.Occurrence.ID)
		for bid := range accepted.Occurrence.Bindings {
			works = append(works, deliveryWorkID(accepted.Occurrence.ID, bid))
		}
	}
	var wg sync.WaitGroup
	failures := make(chan error, len(works)*2)
	for n, work := range works {
		for instance := range 2 {
			wg.Go(func() { failures <- services[(n+instance)%2].dispatcher.runOne(context.Background(), work) })
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 16 || len(audit.snapshot()) != 8 {
		t.Fatal("missing or duplicate independent deliveries", requests.Load(), len(audit.snapshot()))
	}
	for _, id := range ids {
		for _, binding := range deliveryRow(t, services[0], id).Bindings {
			if binding.Intents["firing"].Receipt == nil || binding.Claim != nil {
				t.Fatal("missing durable receipt")
			}
		}
	}
	// Repair cross-sink orphan membership without touching the authoritative row.
	store := services[0].durable.store
	ctx := context.Background()
	row := deliveryRow(t, services[0], ids[0])
	work := deliveryWorkID(row.ID, sinkBinding(t, row, "servicenow").ID)
	before := store.client.Get(ctx, store.rowKey(row.ID)).Val()
	if err := store.client.SAdd(ctx, store.key("delivery-pending:audit"), work).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.cleanOrphan(ctx, "delivery-pending:audit", work); err != nil {
		t.Fatal(err)
	}
	if store.client.SIsMember(ctx, store.key("delivery-pending:audit"), work).Val() || store.client.Get(ctx, store.rowKey(row.ID)).Val() != before {
		t.Fatal("cross-sink orphan repair failed or rewrote source")
	}
}

func TestMultiSinkWrongTypeIndexStopsBeforeHTTP(t *testing.T) {
	f := realValkey(t)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	cfg := multiConfig(t, f, server.URL)
	s := deliveryService(t, cfg, nil)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	store := s.durable.store
	ctx := context.Background()
	before := store.client.Get(ctx, store.rowKey(row.ID)).Val()
	key := store.key("delivery-pending:servicenow")
	if err := store.client.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.client.Set(ctx, key, "wrong-type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	bid := sinkBinding(t, row, "servicenow").ID
	if err := s.dispatcher.runOne(ctx, deliveryWorkID(row.ID, bid)); !errors.Is(err, ErrIndexSchema) {
		t.Fatal("corruption not quarantined", err)
	}
	if calls.Load() != 0 || store.client.Get(ctx, store.rowKey(row.ID)).Val() != before {
		t.Fatal("corrupt index allowed partial state/delivery")
	}
}

func TestMultiSinkHTTPShutdownCancellationRecovery(t *testing.T) {
	f := realValkey(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	first.Store(true)
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if first.Swap(false) {
			close(entered)
			<-release
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	cfg := multiConfig(t, f, server.URL)
	s := deliveryService(t, cfg, nil)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	bid := sinkBinding(t, row, "servicenow").ID
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.dispatcher.runOne(ctx, deliveryWorkID(row.ID, bid)) }()
	await(t, entered)
	cancel()
	err := <-done
	close(release)
	if err != nil {
		t.Fatal("cancellation recovery not persisted", err)
	}
	failed := sinkBinding(t, deliveryRow(t, s, row.ID), "servicenow").Intents["firing"]
	if failed.Receipt != nil || failed.AuthorizedAt == nil || failed.PermanentFailure || failed.NextAttemptAt.IsZero() {
		t.Fatal("cancelled request discarded", failed)
	}
	restarted := deliveryService(t, cfg, nil)
	waitPast(t, failed.NextAttemptAt)
	dispatchSink(t, restarted, row.ID, "servicenow")
	if r := sinkBinding(t, deliveryRow(t, restarted, row.ID), "servicenow").Intents["firing"].Receipt; r == nil || r.Attempts != 2 {
		t.Fatal("cancelled delivery not recovered", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatal("recovery identity changed", keys)
	}
}

func TestMultiSinkMissingRegistryCannotRedirectOutstandingWork(t *testing.T) {
	f := realValkey(t)
	cfg := deliveryConfig(t, f)
	s := deliveryService(t, cfg, nil)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	store := s.durable.store
	ctx := context.Background()
	before := store.client.Get(ctx, store.rowKey(row.ID)).Val()
	if err := store.client.Del(ctx, store.destinationKey()).Err(); err != nil {
		t.Fatal(err)
	}
	cfg.Durable.PolicyEpoch++
	cfg.Delivery.Sinks[0].Type = "http"
	cfg.Delivery.Sinks[0].HTTP = &HTTPSinkConfig{URL: "https://different.example/webhook", Timeout: 10 * time.Millisecond}
	if candidate, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
		_ = candidate.Close()
		t.Fatal("missing registry allowed destination replacement")
	} else if !strings.Contains(err.Error(), store.destinationKey()) || !strings.Contains(err.Error(), "policy_epoch does not repair") || !strings.Contains(err.Error(), "durable.valkey.namespace") {
		t.Fatal("missing actionable registry diagnostic", err)
	}
	if store.client.Exists(ctx, store.destinationKey()).Val() != 0 || store.client.Get(ctx, store.rowKey(row.ID)).Val() != before {
		t.Fatal("failed registry activation overwrote authoritative state")
	}
}
