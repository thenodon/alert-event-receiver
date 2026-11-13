//go:build integration

package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type controlledSink struct {
	mu     sync.Mutex
	events []DeliveryEvent
	call   func(context.Context, DeliveryEvent) error
}

func (s *controlledSink) Deliver(ctx context.Context, e DeliveryEvent) error {
	if s.call != nil {
		if err := s.call(ctx, e); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}
func (s *controlledSink) snapshot() []DeliveryEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DeliveryEvent(nil), s.events...)
}
func deliveryConfig(t *testing.T, f *valkeyFixture) Config {
	c := integrationConfig(t, f)
	c.Delivery.Enabled = true
	c.Delivery.Dispatcher.PollInterval = 10 * time.Millisecond
	c.Delivery.Dispatcher.Workers = 2
	c.Delivery.Dispatcher.BatchSize = 10
	c.Delivery.Dispatcher.ClaimLease = 400 * time.Millisecond
	c.Delivery.Dispatcher.DeliveryTimeout = 50 * time.Millisecond
	c.Delivery.Dispatcher.RetryInitial = 30 * time.Millisecond
	c.Delivery.Dispatcher.RetryMax = 100 * time.Millisecond
	return c
}
func deliveryService(t *testing.T, c Config, sink Sink) *Service {
	t.Helper()
	s, err := New(c, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if sink != nil {
		s.dispatcher.sink = sink
	}
	return s
}
func deliveryRow(t *testing.T, s *Service, id string) *DurableOccurrence {
	t.Helper()
	row, err := s.durable.store.load(context.Background(), id)
	if err != nil || row == nil {
		t.Fatal(row, err)
	}
	return row
}
func dispatch(t *testing.T, s *Service, id string) {
	t.Helper()
	if err := s.dispatcher.runOne(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
func waitPast(t *testing.T, until time.Time) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(until.Add(2 * time.Millisecond)) {
		if time.Now().After(deadline) {
			t.Fatal("wait deadline exceeded")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func await(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("test sink not reached")
	}
}

func TestDeliveryLoggingSinkAndOrderedReceipts(t *testing.T) {
	f := realValkey(t)
	cfg := deliveryConfig(t, f)
	var output bytes.Buffer
	s, err := New(cfg, slog.New(slog.NewJSONHandler(&output, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	dispatch(t, s, row.ID)
	fired := deliveryRow(t, s, row.ID)
	b := onlyBinding(t, fired)
	r := b.Intents["firing"].Receipt
	if r == nil || b.FiringDeliveredAt == nil || b.Delivery != Delivered || r.IntentID != b.Intents["firing"].ID || r.Attempts != 1 || r.Fence == 0 || r.StateVersion == 0 {
		t.Fatal("missing firing receipt", b)
	}
	// Repeated firing notifications update lifecycle metadata but never resend.
	acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	dispatch(t, s, row.ID)
	acceptTest(t, s.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:30:00+01:00")
	dispatch(t, s, row.ID)
	acceptTest(t, s.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:30:00+01:00")
	dispatch(t, s, row.ID)
	closed := deliveryRow(t, s, row.ID)
	b = onlyBinding(t, closed)
	if closed.Lifecycle != LifecycleResolved || b.Eligibility != EligibilityEligible || b.Delivery != Delivered || b.Intents["resolved"].Receipt == nil {
		t.Fatal(b)
	}
	for _, action := range []string{"firing", "resolved"} {
		if got := counter(t, s, "sla_proxy_delivery_receipts_total", map[string]string{"sink_id": "default", "action": action}); got != 1 {
			t.Fatal("duplicate/missing success receipt metric", action, got)
		}
	}
	var actions []string
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var log map[string]any
		if err := json.Unmarshal([]byte(line), &log); err != nil {
			t.Fatal(err)
		}
		if log["msg"] == "SLA alert delivered" {
			actions = append(actions, log["action"].(string))
		}
	}
	if strings.Join(actions, ",") != "firing,resolved" {
		t.Fatal(actions)
	}
}

func TestDeliveryPendingEligibilityAndEarlyResolution(t *testing.T) {
	for _, resolve := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduler_opens", true: "resolved_before_opening"}[resolve], func(t *testing.T) {
			f := realValkey(t)
			sink := &controlledSink{}
			s := deliveryService(t, deliveryConfig(t, f), sink)
			row := pendingSnapshot(t, s.durable)
			// Snapshot represents historical receipt before opening, not a delayed firing
			// incorrectly classified as permanently outside SLA.
			dispatch(t, s, row.ID)
			if len(sink.snapshot()) != 0 {
				t.Fatal("pending fired early")
			}
			if resolve {
				acceptTest(t, s.durable, "resolved", "2026-01-05T07:30:00+01:00", "2026-01-05T07:45:00+01:00")
			} else {
				if err := s.durable.runOne(context.Background(), row.ID); err != nil {
					t.Fatal(err)
				}
			}
			dispatch(t, s, row.ID)
			want := 1
			if resolve {
				want = 0
			}
			if len(sink.snapshot()) != want {
				t.Fatal(sink.snapshot())
			}
		})
	}
}

func TestDeliveryResolutionBeforeAuthorizationCancelsClaim(t *testing.T) {
	f := realValkey(t)
	sink := &controlledSink{}
	s := deliveryService(t, deliveryConfig(t, f), sink)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	claimed, err := s.dispatcher.claim(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T09:00:00+01:00")
	canceled, err := s.dispatcher.authorize(context.Background(), claimed)
	if err != nil || canceled.DeliveryClaim != nil || onlyBinding(t, canceled).Intents["firing"].AuthorizedAt != nil {
		t.Fatal(canceled, err)
	}
	dispatch(t, s, row.ID)
	if len(sink.snapshot()) != 0 {
		t.Fatal("canceled firing or standalone resolution delivered")
	}
}

func TestDeliveryResolutionDuringSinkAndCompetingDispatcher(t *testing.T) {
	f := realValkey(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	sink := &controlledSink{call: func(ctx context.Context, e DeliveryEvent) error {
		if e.Action == "firing" {
			once.Do(func() { close(entered) })
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}}
	cfg := deliveryConfig(t, f)
	cfg.Delivery.Dispatcher.DeliveryTimeout = time.Second
	cfg.Delivery.Dispatcher.ClaimLease = 2 * time.Second
	s := deliveryService(t, cfg, sink)
	other := deliveryService(t, cfg, sink)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	done := make(chan error, 1)
	go func() { done <- s.dispatcher.runOne(context.Background(), row.ID) }()
	await(t, entered)
	dispatch(t, other, row.ID) // Another process cannot own the active claim.
	acceptTest(t, other.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:30:00+01:00")
	// No resolution may overtake a firing whose successful receipt is pending.
	dispatch(t, other, row.ID)
	close(release)
	if err := <-done; err != nil {
		t.Fatal("resolution fenced successful firing receipt", err)
	}
	dispatch(t, other, row.ID)
	events := sink.snapshot()
	if len(events) != 2 || events[0].Action != "firing" || events[1].Action != "resolved" {
		t.Fatal(events)
	}
	closed := deliveryRow(t, s, row.ID)
	b := onlyBinding(t, closed)
	if closed.Lifecycle != LifecycleResolved || b.Intents["firing"].Receipt == nil || b.Intents["resolved"].Receipt == nil {
		t.Fatal(closed)
	}
}

func TestDeliveryClaimExpiryRecoveryAndFencing(t *testing.T) {
	f := realValkey(t)
	sink := &controlledSink{}
	cfg := deliveryConfig(t, f)
	s := deliveryService(t, cfg, sink)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	old, err := s.dispatcher.claim(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, err = s.dispatcher.authorize(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process loss after durable authorization; a fresh service recovers.
	restarted := deliveryService(t, cfg, sink)
	waitPast(t, old.DeliveryClaim.Until)
	dispatch(t, restarted, row.ID)
	current := deliveryRow(t, restarted, row.ID)
	r := onlyBinding(t, current).Intents["firing"].Receipt
	if r == nil || r.Attempts != 2 || r.Fence <= old.DeliveryClaim.Fence {
		t.Fatal(r)
	}
	if err := s.dispatcher.finish(context.Background(), old, deliveryEvent(old), nil, time.Now().UTC()); !errors.Is(err, ErrFenced) {
		t.Fatal("expired owner committed", err)
	}
	if len(sink.snapshot()) != 1 {
		t.Fatal(sink.snapshot())
	}
}

func TestDeliveryClaimedCrashBeforeAuthorization(t *testing.T) {
	f := realValkey(t)
	sink := &controlledSink{}
	cfg := deliveryConfig(t, f)
	s := deliveryService(t, cfg, sink)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	old, err := s.dispatcher.claim(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted := deliveryService(t, cfg, sink)
	waitPast(t, old.DeliveryClaim.Until)
	dispatch(t, restarted, row.ID)
	receipt := onlyBinding(t, deliveryRow(t, restarted, row.ID)).Intents["firing"].Receipt
	if receipt == nil || receipt.Attempts != 1 {
		t.Fatal(receipt)
	}
}

func TestDeliverySuccessWithStoreOutageAndResolutionRecovery(t *testing.T) {
	f := realValkey(t)
	var once sync.Once
	sink := &controlledSink{call: func(context.Context, DeliveryEvent) error { once.Do(f.stop); return nil }}
	cfg := deliveryConfig(t, f)
	s := deliveryService(t, cfg, sink)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	if err := s.dispatcher.runOne(context.Background(), row.ID); err == nil {
		t.Fatal("outcome acknowledged during outage")
	}
	if len(sink.snapshot()) != 1 {
		t.Fatal("sink was not invoked")
	}
	f.start()
	restarted := deliveryService(t, cfg, sink)
	acceptTest(t, restarted.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:30:00+01:00")
	persisted := deliveryRow(t, restarted, row.ID)
	if onlyBinding(t, persisted).Intents["firing"].Receipt != nil {
		t.Fatal("invented receipt")
	}
	waitPast(t, persisted.DeliveryClaim.Until)
	dispatch(t, restarted, row.ID)
	dispatch(t, restarted, row.ID)
	events := sink.snapshot()
	if len(events) != 3 || events[0].Action != "firing" || events[1].Action != "firing" || events[2].Action != "resolved" || events[0].IntentID != events[1].IntentID {
		t.Fatal(events)
	}
	if onlyBinding(t, deliveryRow(t, restarted, row.ID)).Intents["resolved"].Receipt == nil {
		t.Fatal("resolution continuity lost")
	}
}

func TestDeliveryTransientAndPermanentFailures(t *testing.T) {
	f := realValkey(t)
	var mu sync.Mutex
	calls := 0
	sink := &controlledSink{call: func(context.Context, DeliveryEvent) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return errors.New("do-not-log-this-secret")
		}
		return nil
	}}
	s := deliveryService(t, deliveryConfig(t, f), sink)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	dispatch(t, s, row.ID)
	failed := deliveryRow(t, s, row.ID)
	i := onlyBinding(t, failed).Intents["firing"]
	if i.Receipt != nil || i.Attempts != 1 || i.NextAttemptAt.IsZero() || onlyBinding(t, failed).Delivery != DeliveryFailed {
		t.Fatal(i)
	}
	dispatch(t, s, row.ID)
	mu.Lock()
	gotCalls := calls
	mu.Unlock()
	if gotCalls != 1 {
		t.Fatal("retry hot loop", gotCalls)
	}
	waitPast(t, i.NextAttemptAt)
	dispatch(t, s, row.ID)
	if r := onlyBinding(t, deliveryRow(t, s, row.ID)).Intents["firing"].Receipt; r == nil || r.Attempts != 2 {
		t.Fatal(r)
	}
	permanent := &controlledSink{call: func(context.Context, DeliveryEvent) error {
		return &DeliveryError{Permanent: true, Cause: errors.New("secret")}
	}}
	s.dispatcher.sink = permanent
	other := alertAt(t, "firing", "sla_id", "2026-01-05T08:31:00+01:00")
	other.Fingerprint = "permanent"
	accepted, err := s.durable.accept(context.Background(), other, modelsPayload(other))
	if err != nil {
		t.Fatal(err)
	}
	dispatch(t, s, accepted.Occurrence.ID)
	held := deliveryRow(t, s, accepted.Occurrence.ID)
	if !onlyBinding(t, held).Intents["firing"].PermanentFailure || !held.deliveryDueAt().IsZero() {
		t.Fatal("permanent failure not held", held)
	}
}

func TestDeliveryPolicyHoldAndDestinationPin(t *testing.T) {
	f := realValkey(t)
	sink := &controlledSink{}
	cfg := deliveryConfig(t, f)
	s := deliveryService(t, cfg, sink)
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	changed := deliveryConfig(t, f)
	changed.Durable.PolicyEpoch++
	p := changed.SLADefinitions[0].SLAs["business"]
	p.Schedule = map[string][]string{"monday": {"09:00-17:00"}}
	changed.SLADefinitions[0].SLAs["business"] = p
	newer := deliveryService(t, changed, sink)
	var pending []string
	if err := newer.durable.maintenance(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	dispatch(t, newer, row.ID)
	if len(sink.snapshot()) != 0 || onlyBinding(t, deliveryRow(t, newer, row.ID)).HoldReason != "policy_changed" {
		t.Fatal("pending delivery not held")
	}
	if err := newer.durable.store.client.Set(context.Background(), newer.durable.store.destinationKey(), "future-http", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(changed, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
		t.Fatal("sink identity silently changed")
	}
}

func TestDeliveryDisabledCompatibilityAndServiceShutdown(t *testing.T) {
	f := realValkey(t)
	disabled := integrationConfig(t, f)
	legacy := deliveryService(t, disabled, nil)
	if legacy.dispatcher != nil || legacy.mode() != "durable_state_only" {
		t.Fatal("delivery enabled implicitly")
	}
	row := acceptTest(t, legacy.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	if onlyBinding(t, row).SinkType != "" || onlyBinding(t, row).Intents["firing"].Receipt != nil {
		t.Fatal("disabled behavior changed")
	}
	// Enabling delivery adopts existing Phase 3 intent metadata through maintenance.
	sink := &controlledSink{}
	enabled := deliveryConfig(t, f)
	if candidate, err := New(enabled, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
		_ = candidate.Close()
		t.Fatal("delivery format activated without a new epoch")
	}
	enabled.Durable.PolicyEpoch++
	s := deliveryService(t, enabled, sink)
	var pending []string
	if err := s.durable.maintenance(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, listener) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(sink.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dispatcher did not shut down")
	}
	if len(sink.snapshot()) != 1 {
		t.Fatal("persisted intent not recovered on startup")
	}
}

func TestDeliveryRetentionAndNoopLeasePreservation(t *testing.T) {
	f := realValkey(t)
	s := deliveryService(t, deliveryConfig(t, f), &controlledSink{})
	ctx := context.Background()
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	claimed, err := s.dispatcher.claim(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := s.durable.store.rowKey(row.ID)
	before := s.durable.store.client.Get(ctx, key).Val()
	expiry := s.durable.store.client.PExpireTime(ctx, s.durable.store.deliveryClaimKey(row.ID)).Val()
	var pending []string
	for range 2 {
		if err := s.durable.maintenance(ctx, &pending); err != nil {
			t.Fatal(err)
		}
	}
	if after := s.durable.store.client.Get(ctx, key).Val(); after != before {
		t.Fatal("no-op maintenance rewrote delivery claim")
	}
	if after := s.durable.store.client.PExpireTime(ctx, s.durable.store.deliveryClaimKey(row.ID)).Val(); after != expiry {
		t.Fatal("delivery lease renewed")
	}
	authorized, err := s.dispatcher.authorize(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	acceptTest(t, s.durable, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T09:00:00+01:00")
	_, err = s.durable.store.update(ctx, row.ID, func(current *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		current.CompactAt = utcPointer(now.Add(-time.Hour))
		current.DeleteAt = utcPointer(now.Add(-time.Minute))
		return current, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Uncertain authorized delivery retains payload and state even past ordinary
	// completed-retention bounds; it must not disappear while awaiting a receipt.
	if err := s.durable.maintenance(ctx, &pending); err != nil {
		t.Fatal(err)
	}
	retained := deliveryRow(t, s, row.ID)
	if retained.Compacted || onlyBinding(t, retained).Intents["firing"].Payload == nil {
		t.Fatal("pending payload lost")
	}
	if err := s.dispatcher.finish(ctx, authorized, deliveryEvent(authorized), nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	dispatch(t, s, row.ID)
	final := deliveryRow(t, s, row.ID)
	if final.outstandingDelivery() {
		t.Fatal("completed work still pins retention")
	}
	if err := s.durable.maintenance(ctx, &pending); err != nil {
		t.Fatal(err)
	}
	if stored, err := s.durable.store.load(ctx, row.ID); err != nil || stored != nil {
		t.Fatal("completed retention did not resume", stored, err)
	}
}

func TestDeliveryBlockedWorkDoesNotBlockOtherOccurrences(t *testing.T) {
	f := realValkey(t)
	cfg := deliveryConfig(t, f)
	cfg.Delivery.Dispatcher.DeliveryTimeout = time.Second
	cfg.Delivery.Dispatcher.ClaimLease = 2 * time.Second
	entered, otherDelivered := make(chan struct{}), make(chan struct{})
	var enterOnce, otherOnce sync.Once
	sink := &controlledSink{call: func(ctx context.Context, e DeliveryEvent) error {
		if e.Notification.Alert.Fingerprint == "blocked" {
			enterOnce.Do(func() { close(entered) })
			<-ctx.Done()
			return ctx.Err()
		}
		otherOnce.Do(func() { close(otherDelivered) })
		return nil
	}}
	s := deliveryService(t, cfg, sink)
	a := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
	a.Fingerprint = "blocked"
	first, err := s.durable.accept(context.Background(), a, modelsPayload(a))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.dispatcher.Run(ctx) }()
	await(t, entered)
	a.Fingerprint = "independent"
	if _, err := s.durable.accept(context.Background(), a, modelsPayload(a)); err != nil {
		t.Fatal(err)
	}
	await(t, otherDelivered)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked worker did not stop")
	}
	if r := onlyBinding(t, deliveryRow(t, s, first.Occurrence.ID)).Intents["firing"].Receipt; r != nil {
		t.Fatal("failed call got a receipt")
	}
}

func TestDeliveryConcurrentClaimTransactions(t *testing.T) {
	f := realValkey(t)
	cfg := deliveryConfig(t, f)
	sink := &controlledSink{}
	services := []*Service{deliveryService(t, cfg, sink), deliveryService(t, cfg, sink)}
	row := acceptTest(t, services[0].durable, "firing", "2026-01-05T08:30:00+01:00", "")
	barrier := make(chan struct{})
	var workers sync.WaitGroup
	outcomes := make(chan error, 2)
	for _, s := range services {
		workers.Go(func() { <-barrier; _, err := s.dispatcher.claim(context.Background(), row.ID); outcomes <- err })
	}
	close(barrier)
	workers.Wait()
	close(outcomes)
	owners, busy := 0, 0
	for err := range outcomes {
		if err == nil {
			owners++
		} else if errors.Is(err, ErrClaimBusy) {
			busy++
		} else {
			t.Fatal(err)
		}
	}
	if owners != 1 || busy != 1 {
		t.Fatal("concurrent claim ownership", owners, busy)
	}
}

func TestDeliveryDiscoveryRepairWithoutRecordRewrite(t *testing.T) {
	f := realValkey(t)
	sink := &controlledSink{}
	s := deliveryService(t, deliveryConfig(t, f), sink)
	ctx := context.Background()
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	store := s.durable.store
	before := store.client.Get(ctx, store.rowKey(row.ID)).Val()
	if err := store.client.Del(ctx, store.key("delivery-due"), store.key("delivery-ready")).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.client.SAdd(ctx, store.key("delivery-claimed"), row.ID).Err(); err != nil {
		t.Fatal(err)
	}
	if err := store.client.Set(ctx, store.deliveryClaimKey(row.ID), "orphan", time.Second).Err(); err != nil {
		t.Fatal(err)
	}
	var pending []string
	if err := s.durable.maintenance(ctx, &pending); err != nil {
		t.Fatal(err)
	}
	if after := store.client.Get(ctx, store.rowKey(row.ID)).Val(); after != before {
		t.Fatal("index-only delivery repair rewrote record")
	}
	if store.client.Exists(ctx, store.deliveryClaimKey(row.ID)).Val() != 0 {
		t.Fatal("orphan token retained")
	}
	if store.client.SIsMember(ctx, store.key("delivery-claimed"), row.ID).Val() {
		t.Fatal("orphan claimed index retained")
	}
	ids, err := s.dispatcher.poll(ctx)
	if err != nil || len(ids) != 1 || ids[0] != row.ID {
		t.Fatal("ready discovery not recovered", ids, err)
	}
	dispatch(t, s, row.ID)
	if len(sink.snapshot()) != 1 || onlyBinding(t, deliveryRow(t, s, row.ID)).Intents["firing"].Receipt == nil {
		t.Fatal("repaired work not delivered")
	}
}
