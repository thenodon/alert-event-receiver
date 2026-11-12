//go:build integration

package slaproxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/redis/go-redis/v9"
)

type valkeyFixture struct {
	t                    *testing.T
	address, dir, binary string
	cmd                  *exec.Cmd
}

func realValkey(t *testing.T) *valkeyFixture {
	t.Helper()
	binary := os.Getenv("SLA_PROXY_VALKEY_SERVER")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("valkey-server")
		if err != nil {
			t.Fatal("real integration tests require valkey-server or SLA_PROXY_VALKEY_SERVER")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	f := &valkeyFixture{t: t, address: address, dir: t.TempDir(), binary: binary}
	f.start()
	t.Cleanup(f.stop)
	return f
}
func (f *valkeyFixture) start() {
	_, port, _ := net.SplitHostPort(f.address)
	f.cmd = exec.Command(f.binary, "--bind", "127.0.0.1", "--port", port, "--dir", f.dir,
		"--save", "", "--appendonly", "yes", "--appendfsync", "always", "--logfile", filepath.Join(f.dir, "valkey.log"))
	f.cmd.Stdout, f.cmd.Stderr = io.Discard, io.Discard
	if err := f.cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	c := redis.NewClient(&redis.Options{Addr: f.address, MaxRetries: -1, DialTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if err := c.Ping(ctx).Err(); err == nil {
			// Prove that all production operations work without any scripting
			// permission, including after AOF/server restart.
			if err := c.Do(ctx, "ACL", "SETUSER", "default", "-eval", "-evalsha", "-eval_ro", "-evalsha_ro", "-script").Err(); err != nil {
				f.t.Fatal(err)
			}
			return
		}
		select {
		case <-ctx.Done():
			data, _ := os.ReadFile(filepath.Join(f.dir, "valkey.log"))
			f.t.Fatalf("Valkey did not start: %s", data)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func (f *valkeyFixture) stop() {
	if f.cmd != nil {
		_ = f.cmd.Process.Kill()
		_ = f.cmd.Wait()
		f.cmd = nil
	}
}
func (f *valkeyFixture) restart() { f.stop(); f.start() }

func integrationConfig(t *testing.T, f *valkeyFixture) Config {
	c := testConfig(t)
	c.Durable.Enabled, c.Durable.Valkey.Address = true, f.address
	c.Durable.Scheduler.PollInterval, c.Durable.Scheduler.ReconcileInterval = 10*time.Millisecond, 100*time.Millisecond
	c.Durable.Scheduler.OperationTimeout, c.Durable.Scheduler.LeaseDuration = 100*time.Millisecond, 300*time.Millisecond
	c.Durable.Scheduler.RetryInitial, c.Durable.Scheduler.RetryMax = 20*time.Millisecond, 200*time.Millisecond
	c.Durable.Scheduler.Workers, c.Durable.Scheduler.BatchSize = 2, 10
	return c
}
func manager(t *testing.T, cfg Config) *DurableManager {
	t.Helper()
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.durable
}
func acceptTest(t *testing.T, m *DurableManager, status, start, end string) *DurableOccurrence {
	t.Helper()
	a := alertAt(t, status, "sla_id", start)
	if end != "" {
		a.EndsAt = instant(t, end)
	}
	row, err := m.accept(context.Background(), a, modelsPayload(a))
	if err != nil || row.Occurrence == nil {
		t.Fatal(err, row)
	}
	return row.Occurrence
}
func modelsPayload(a models.Alert) models.WebhookPayload {
	return models.WebhookPayload{Version: "4", Receiver: "sla", Status: a.Status, Alerts: []models.Alert{a}}
}
func onlyBinding(t *testing.T, row *DurableOccurrence) *DurableBinding {
	t.Helper()
	if len(row.Bindings) != 1 {
		t.Fatal(row)
	}
	for _, b := range row.Bindings {
		return b
	}
	return nil
}

// Seed a legitimate historical pending snapshot, as if accepted at 07:35 before
// a process outage. The real store/transaction then recovers overdue work now.
func pendingSnapshot(t *testing.T, m *DurableManager) *DurableOccurrence {
	t.Helper()
	a := alertAt(t, "firing", "sla_id", "2026-01-05T07:30:00+01:00")
	ref := PolicyRef{"sla_id", "business"}
	d, err := m.engine.Evaluate(context.Background(), PolicyInput{}, ref, "", a.StartsAt)
	if err != nil {
		t.Fatal(err)
	}
	id := occurrenceKeyID(a)
	bid := bindingKeyID(id, ref, m.cfg.Durable.SinkID)
	row := &DurableOccurrence{ID: id, Fingerprint: a.Fingerprint, StartsAt: a.StartsAt.UTC(), Lifecycle: LifecycleFiring,
		Original: notification(a, modelsPayload(a)), LastFiring: notification(a, modelsPayload(a)), CreatedAt: a.StartsAt.UTC(),
		Bindings: map[string]*DurableBinding{bid: {ID: bid, Policy: ref, SinkID: m.cfg.Durable.SinkID, PolicyVersion: d.PolicyVersion, Calendar: d, Intents: map[string]*DeliveryIntent{}, Delivery: NotDelivered}}}
	m.advance(row, a.StartsAt.Add(5*time.Minute))
	committed, err := m.store.update(context.Background(), id, func(*DurableOccurrence, time.Time) (*DurableOccurrence, error) { return row, nil })
	if err != nil {
		t.Fatal(err)
	}
	if onlyBinding(t, committed).Eligibility != EligibilityPending {
		t.Fatal("bad pending fixture")
	}
	return committed
}

func TestValkeyLifecycleAndRetrySafety(t *testing.T) {
	f := realValkey(t)
	m := manager(t, integrationConfig(t, f))
	row := acceptTest(t, m, "firing", "2026-01-05T08:30:00+01:00", "")
	first := onlyBinding(t, row).Intents["firing"].ID
	duplicate := acceptTest(t, m, "firing", "2026-01-05T08:30:00+01:00", "")
	if duplicate.ID != row.ID || onlyBinding(t, duplicate).Intents["firing"].ID != first || duplicate.LastVerifiedActiveAt != nil {
		t.Fatal(duplicate)
	}
	closed := acceptTest(t, m, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T17:30:00+01:00")
	b := onlyBinding(t, closed)
	if closed.Lifecycle != LifecycleResolved || b.Eligibility != EligibilityEligible || b.Delivery == Delivered || b.Intents["firing"].Ready || b.Intents["resolved"].Ready || !b.Intents["resolved"].RequiresFiringDelivery {
		t.Fatal(closed, b)
	}
	late := acceptTest(t, m, "firing", "2026-01-05T08:30:00+01:00", "")
	repeated := acceptTest(t, m, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T17:30:00+01:00")
	if late.Lifecycle != LifecycleResolved || !repeated.DeleteAt.Equal(*closed.DeleteAt) {
		t.Fatal(late, repeated)
	}
	orphan := acceptTest(t, m, "resolved", "2026-01-05T07:30:00+01:00", "2026-01-05T07:45:00+01:00")
	if onlyBinding(t, orphan).Eligibility != EligibilityIneligible || onlyBinding(t, orphan).Intents["firing"] != nil {
		t.Fatal(orphan)
	}
	late = acceptTest(t, m, "firing", "2026-01-05T07:30:00+01:00", "")
	if late.Lifecycle != LifecycleResolved {
		t.Fatal("orphan resolution reopened")
	}
	newer := acceptTest(t, m, "firing", "2026-01-05T08:30:00.000000001+01:00", "")
	if newer.ID == row.ID {
		t.Fatal("nanosecond occurrence collapsed")
	}
	// A conflicting repeated resolution is durably held, never reopened.
	conflict := acceptTest(t, m, "resolved", "2026-01-05T08:30:00+01:00", "2026-01-05T18:00:00+01:00")
	if !conflict.ResolutionConflict || onlyBinding(t, conflict).HoldReason != "resolution_conflict" || !conflict.EndsAt.Equal(*closed.EndsAt) {
		t.Fatal(conflict)
	}
}

func TestValkeyConcurrentUpdatesAndSchedulers(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	m := manager(t, cfg)
	other := manager(t, cfg)
	row := pendingSnapshot(t, m)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Go(func() {
			a := alertAt(t, "firing", "sla_id", "2026-01-05T07:30:00+01:00")
			if _, err := other.accept(context.Background(), a, modelsPayload(a)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Go(func() {
		a := alertAt(t, "resolved", "sla_id", "2026-01-05T07:30:00+01:00")
		a.EndsAt = instant(t, "2026-01-05T07:45:00+01:00")
		if _, err := m.accept(context.Background(), a, modelsPayload(a)); err != nil {
			t.Error(err)
		}
	})
	wg.Wait()
	latest, err := m.store.load(context.Background(), row.ID)
	if err != nil || latest.Lifecycle != LifecycleResolved || onlyBinding(t, latest).Eligibility != EligibilityIneligible {
		t.Fatal(latest, err)
	}
	if n := m.store.client.ZCard(context.Background(), m.store.key("due")).Val(); n != 0 {
		t.Fatal("resolved work still scheduled", n)
	}
	// Two independent connections compete for the same overdue occurrence.
	cfg.Durable.Valkey.Namespace = "claims"
	m, other = manager(t, cfg), manager(t, cfg)
	row = pendingSnapshot(t, m)
	results := make(chan *DurableOccurrence, 2)
	for _, worker := range []*DurableManager{m, other} {
		worker := worker
		wg.Go(func() {
			claimed, err := worker.claim(context.Background(), row.ID)
			if err != nil && !errors.Is(err, ErrClaimBusy) {
				t.Error(err)
			}
			results <- claimed
		})
	}
	wg.Wait()
	close(results)
	claims := 0
	for claimed := range results {
		if claimed != nil {
			claims++
			if _, err := m.complete(context.Background(), claimed); err != nil {
				t.Fatal(err)
			}
		}
	}
	if claims != 1 {
		t.Fatal("concurrent ownership", claims)
	}
	latest, err = m.store.load(context.Background(), row.ID)
	if err != nil || onlyBinding(t, latest).Eligibility != EligibilityEligible || !onlyBinding(t, latest).Intents["firing"].Ready {
		t.Fatal(latest, err)
	}
}

func TestValkeyExpiredClaimsAndResolutionFence(t *testing.T) {
	f := realValkey(t)
	m := manager(t, integrationConfig(t, f))
	row := pendingSnapshot(t, m)
	old, err := m.claim(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Real key expiry: no mocked clock or distributed-lock emulation.
	deadline := time.Now().Add(2 * time.Second)
	for m.store.client.Exists(context.Background(), m.store.claimKey(row.ID)).Val() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = m.complete(context.Background(), old); !errors.Is(err, ErrFenced) {
		t.Fatal("expired worker committed", err)
	}
	newClaim, err := m.claim(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newClaim.Fence <= old.Fence {
		t.Fatal("fence did not advance")
	}
	if _, err = m.complete(context.Background(), old); !errors.Is(err, ErrFenced) {
		t.Fatal("old owner overwrote new owner", err)
	}
	// A resolution invalidates even an otherwise live claim.
	acceptTest(t, m, "resolved", "2026-01-05T07:30:00+01:00", "2026-01-05T07:45:00+01:00")
	if _, err = m.complete(context.Background(), newClaim); !errors.Is(err, ErrFenced) {
		t.Fatal("scheduler overwrote resolution", err)
	}
}

func TestValkeyAOFRestartAndIndexReconciliation(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	m := manager(t, cfg)
	row := pendingSnapshot(t, m)
	f.restart()
	restarted := manager(t, cfg)
	loaded, err := restarted.store.load(context.Background(), row.ID)
	if err != nil || loaded == nil {
		t.Fatal("pending lost across AOF restart", err)
	}
	// Missing secondary indexes are repaired from authoritative row metadata.
	if err := restarted.store.client.Del(context.Background(), restarted.store.key("due"), restarted.store.key("pending"), restarted.store.key("unresolved")).Err(); err != nil {
		t.Fatal(err)
	}
	var pending []string
	if err := restarted.maintenance(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	if n := restarted.store.client.ZCard(context.Background(), restarted.store.key("due")).Val(); n != 1 {
		t.Fatal("due not recovered", n)
	}
	if err := restarted.runOne(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err = restarted.store.load(context.Background(), row.ID)
	if err != nil || onlyBinding(t, loaded).Eligibility != EligibilityEligible {
		t.Fatal(loaded, err)
	}
}

func TestValkeyPolicyHoldsAndRemovedPolicyResolution(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	m := manager(t, cfg)
	row := pendingSnapshot(t, m)
	changed := cfg
	changed.Durable.PolicyEpoch++
	p := changed.SLADefinitions[0].SLAs["business"]
	p.Timezone = "UTC"
	changed.SLADefinitions[0].SLAs["business"] = p
	current := manager(t, changed)
	var pending []string
	if err := current.maintenance(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	loaded, err := current.store.load(context.Background(), row.ID)
	if err != nil || onlyBinding(t, loaded).HoldReason != "policy_changed" || !loaded.dueAt().IsZero() {
		t.Fatal(loaded, err)
	}
	if _, err := m.claim(context.Background(), row.ID); !errors.Is(err, ErrPolicyEpoch) {
		t.Fatal("old configuration could commit", err)
	}
	changed.Durable.PolicyEpoch++
	changed.SLADefinitions[0].SLAs = map[string]Policy{"other": fullDayPolicy()}
	removed := manager(t, changed)
	closed := acceptTest(t, removed, "resolved", "2026-01-05T07:30:00+01:00", "2026-01-05T07:45:00+01:00")
	if closed.Lifecycle != LifecycleResolved || onlyBinding(t, closed).HoldReason != "policy_removed" {
		t.Fatal(closed)
	}
	changed.Durable.PolicyEpoch++
	changed.SLADefinitions = nil // Explicit drain mode still accepts known resolutions.
	drain := manager(t, changed)
	closed = acceptTest(t, drain, "resolved", "2026-01-05T07:30:00+01:00", "2026-01-05T07:45:00+01:00")
	if closed.Lifecycle != LifecycleResolved {
		t.Fatal(closed)
	}
}

func TestValkeyRetentionAndTypeSafety(t *testing.T) {
	f := realValkey(t)
	m := manager(t, integrationConfig(t, f))
	row := acceptTest(t, m, "resolved", "2026-01-05T07:30:00+01:00", "2026-01-05T07:45:00+01:00")
	_, err := m.store.update(context.Background(), row.ID, func(r *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		r.CompactAt = utcPointer(now.Add(-time.Second))
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var pending []string
	if err := m.maintenance(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	compacted, err := m.store.load(context.Background(), row.ID)
	if err != nil || !compacted.Compacted || compacted.Original != nil || compacted.Resolution != nil {
		t.Fatal(compacted, err)
	}
	late := acceptTest(t, m, "firing", "2026-01-05T07:30:00+01:00", "")
	if late.Lifecycle != LifecycleResolved || late.LastFiring != nil {
		t.Fatal("tombstone reopened or regrew payload", late)
	}
	_, err = m.store.update(context.Background(), row.ID, func(r *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		r.DeleteAt = utcPointer(now.Add(-time.Second))
		return r, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.maintenance(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	if r, err := m.store.load(context.Background(), row.ID); err != nil || r != nil {
		t.Fatal("expired tombstone retained", r, err)
	}
	// Wrong types are detected before queuing an EXEC; no lifecycle commit.
	a := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
	if err := m.store.client.Set(context.Background(), m.store.key("due"), "corrupt", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.accept(context.Background(), a, modelsPayload(a)); err == nil {
		t.Fatal("wrong index type accepted")
	}
	if n := m.store.client.Exists(context.Background(), m.store.rowKey(occurrenceKeyID(a))).Val(); n != 0 {
		t.Fatal("state committed despite failed preflight")
	}
}

func TestValkeyWebhookAcknowledgementAndAvailability(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
	w := post(s, testPayload(t, a, a))
	var result DurableObservation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Persisted || result.PersistedCount != 2 || result.Forwarded {
		t.Fatal(w.Code, w.Body)
	}
	f.stop()
	w = post(s, testPayload(t, a))
	if w.Code != 503 {
		t.Fatal("store failure acknowledged", w.Code, w.Body)
	}
	f.start()
	w = post(s, testPayload(t, a, a))
	if w.Code != 200 {
		t.Fatal("retry after restart failed", w.Code, w.Body)
	}
	row, err := s.durable.store.load(context.Background(), occurrenceKeyID(a))
	if err != nil || len(onlyBinding(t, row).Intents) != 1 {
		t.Fatal(row, err)
	}
}

func TestValkeyIndependentPolicyNamespaces(t *testing.T) {
	f := realValkey(t)
	m := manager(t, integrationConfig(t, f))
	for i, label := range []string{"sla_id", "sla_name"} {
		a := alertAt(t, "firing", label, "2026-01-05T08:30:00+01:00")
		a.Fingerprint = "namespace-" + strconv.Itoa(i)
		accepted, err := m.accept(context.Background(), a, modelsPayload(a))
		if err != nil {
			t.Fatal(err)
		}
		if onlyBinding(t, accepted.Occurrence).Policy.SLALabel != label {
			t.Fatal("namespace merged")
		}
	}
	if bindingKeyID("id", PolicyRef{"sla_id", "business"}, "default") == bindingKeyID("id", PolicyRef{"sla_name", "business"}, "default") {
		t.Fatal("binding key collision")
	}
}

func TestValkeyPartialBatchCommitAndValidation(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
	second := first
	second.Fingerprint = "second"
	bad := second
	bad.StartsAt = time.Time{}
	if w := post(s, testPayload(t, first, bad)); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if n := s.durable.store.client.Exists(context.Background(), s.durable.store.rowKey(occurrenceKeyID(first))).Val(); n != 0 {
		t.Fatal("invalid batch partially persisted")
	}
	key := s.durable.store.rowKey(occurrenceKeyID(second))
	if err := s.durable.store.client.Set(context.Background(), key, "not JSON", 0).Err(); err != nil {
		t.Fatal(err)
	}
	w := post(s, testPayload(t, first, second))
	var result DurableObservation
	if w.Code != 503 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Persisted || result.PersistedCount != 1 || result.FailedCount != 1 {
		t.Fatal(w.Code, w.Body)
	}
	if result.Selected != 2 || result.Skipped != 0 {
		t.Fatal("failed acceptance changed batch accounting", result)
	}
	if got := counter(t, s, "sla_proxy_alerts_selected_total", map[string]string{"sla_label": "sla_id", "sla_value": "business", "status": "firing"}); got != 2 {
		t.Fatal("selected metric must include failed acceptance", got)
	}
	if err := s.durable.store.client.Del(context.Background(), key).Err(); err != nil {
		t.Fatal(err)
	}
	w = post(s, testPayload(t, first, second))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	row, err := s.durable.store.load(context.Background(), occurrenceKeyID(first))
	if err != nil || len(onlyBinding(t, row).Intents) != 1 {
		t.Fatal(row, err)
	}
}

func TestValkeySchedulerLoopAndGracefulStop(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	m := manager(t, cfg)
	other := manager(t, cfg)
	row := pendingSnapshot(t, m)
	if _, err := m.claim(context.Background(), row.ID); err != nil {
		t.Fatal(err)
	} // Crashed owner.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- m.Run(ctx) }()
	go func() { done <- other.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	eligible := false
	for time.Now().Before(deadline) {
		current, err := m.store.load(context.Background(), row.ID)
		if err == nil && onlyBinding(t, current).Eligibility == EligibilityEligible {
			eligible = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("scheduler did not stop")
		}
	}
	if !eligible {
		t.Fatal("expired claim stranded pending work")
	}
	current, err := m.store.load(context.Background(), row.ID)
	if err != nil || onlyBinding(t, current).Delivery != PendingDelivery || onlyBinding(t, current).FiringDeliveredAt != nil {
		t.Fatal(current, err)
	}
}

func TestValkeyPolicySelectionOrderRequiresNewEpoch(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	_ = manager(t, cfg)
	if len(cfg.SLADefinitions) < 2 {
		t.Fatal("fixture requires independent definitions")
	}
	cfg.SLADefinitions[0], cfg.SLADefinitions[1] = cfg.SLADefinitions[1], cfg.SLADefinitions[0]
	s, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err == nil {
		_ = s.Close()
		t.Fatal("changed selection order accepted at same epoch")
	}
	cfg.Durable.PolicyEpoch++
	_ = manager(t, cfg)
}

func TestValkeyReconciliationRemovesOrphanMemberships(t *testing.T) {
	f := realValkey(t)
	m := manager(t, integrationConfig(t, f))
	ctx := context.Background()
	for _, index := range []string{"unresolved", "pending", "held", "intent-index"} {
		if err := m.store.client.SAdd(ctx, m.store.key(index), "missing").Err(); err != nil {
			t.Fatal(err)
		}
	}
	// A future index entry must be repairable without waiting for its deadline.
	if err := m.store.client.ZAdd(ctx, m.store.key("due"), redis.Z{Score: float64(time.Now().Add(24 * time.Hour).UnixMilli()), Member: "future-missing"}).Err(); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if err := m.reconcileIndex(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, index := range []string{"unresolved", "pending", "held", "intent-index"} {
		if n := m.store.client.SCard(ctx, m.store.key(index)).Val(); n != 0 {
			t.Fatal(index, n)
		}
	}
	if n := m.store.client.ZCard(ctx, m.store.key("due")).Val(); n != 0 {
		t.Fatal("orphan future schedule retained", n)
	}
}
