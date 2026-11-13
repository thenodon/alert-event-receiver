//go:build integration

package slaproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func rawOccurrence(t *testing.T, s *DurableStore, id string) *DurableOccurrence {
	t.Helper()
	data, err := s.client.Get(context.Background(), s.rowKey(id)).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var row DurableOccurrence
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	return &row
}

func TestIndependentOccurrenceUpdatesDoNotConflict(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "independent", true: "same_occurrence"}[same], func(t *testing.T) {
			f := realValkey(t)
			cfg := integrationConfig(t, f)
			managers := []*DurableManager{manager(t, cfg), manager(t, cfg)}
			first := acceptTest(t, managers[0], "firing", "2026-01-05T08:30:00+01:00", "")
			second := first
			if !same {
				second = acceptTest(t, managers[1], "firing", "2026-01-05T08:31:00+01:00", "")
			}
			ids := []string{first.ID, second.ID}
			hook := &benchmarkCommands{}
			for _, m := range managers {
				m.store.client.AddHook(hook)
			}
			arrived := make(chan struct{}, 2)
			release := make(chan struct{})
			outcomes := make(chan error, 2)
			var workers sync.WaitGroup
			for i, m := range managers {
				workers.Go(func() {
					var once sync.Once
					_, err := m.store.update(context.Background(), ids[i], func(row *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
						once.Do(func() { arrived <- struct{}{}; <-release })
						row.Fence++
						return row, nil
					})
					outcomes <- err
				})
			}
			for range 2 {
				select {
				case <-arrived:
				case <-time.After(3 * time.Second):
					t.Fatal("overlapping transactions not reached")
				}
			}
			close(release)
			workers.Wait()
			close(outcomes)
			for err := range outcomes {
				if err != nil {
					t.Fatal(err)
				}
			}
			if same {
				if hook.retries.Load() == 0 {
					t.Fatal("same occurrence failed to conflict")
				}
				row := rawOccurrence(t, managers[0].store, first.ID)
				if row.Version != first.Version+2 || row.Fence != first.Fence+2 {
					t.Fatal("lost same-occurrence update", row)
				}
			} else {
				if hook.retries.Load() != 0 {
					t.Fatal("independent occurrence false conflict", hook.retries.Load())
				}
				for i, m := range managers {
					row := rawOccurrence(t, m.store, ids[i])
					if row.Version != 2 || row.Fence != 1 {
						t.Fatal(row)
					}
				}
			}
		})
	}
}

func TestSchedulerAndDeliveryIndependentTransactions(t *testing.T) {
	f := realValkey(t)
	cfg := deliveryConfig(t, f)
	sink := &controlledSink{}
	first, second := deliveryService(t, cfg, sink), deliveryService(t, cfg, sink)
	pending := pendingSnapshot(t, first.durable)
	ready := acceptTest(t, second.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	hooks := &benchmarkCommands{}
	first.durable.store.client.AddHook(hooks)
	second.durable.store.client.AddHook(hooks)
	outcomes := make(chan error, 2)
	go func() { outcomes <- first.durable.runOne(context.Background(), pending.ID) }()
	go func() { outcomes <- second.dispatcher.runOne(context.Background(), ready.ID) }()
	for range 2 {
		if err := <-outcomes; err != nil {
			t.Fatal(err)
		}
	}
	if hooks.retries.Load() != 0 {
		t.Fatal("scheduler/delivery false conflict", hooks.retries.Load())
	}
	if onlyBinding(t, deliveryRow(t, first, pending.ID)).Eligibility != EligibilityEligible {
		t.Fatal("scheduler did not commit")
	}
	if onlyBinding(t, deliveryRow(t, second, ready.ID)).Intents["firing"].Receipt == nil {
		t.Fatal("delivery did not commit")
	}
	dispatch(t, first, pending.ID)
	if len(sink.snapshot()) != 2 {
		t.Fatal(sink.snapshot())
	}
}

type beforeRepairExec struct {
	once   atomic.Bool
	before func()
}

func (h *beforeRepairExec) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (h *beforeRepairExec) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h *beforeRepairExec) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		transaction := false
		recordWrite := false
		for _, cmd := range commands {
			transaction = transaction || cmd.Name() == "multi"
			recordWrite = recordWrite || cmd.Name() == "set"
		}
		if transaction && !recordWrite && h.once.CompareAndSwap(false, true) {
			h.before()
		}
		return next(ctx, commands)
	}
}
func TestDeliveryInvalidatesStaleIndexOnlyRepair(t *testing.T) {
	f := realValkey(t)
	cfg := deliveryConfig(t, f)
	sink := &controlledSink{}
	repair, deliver := deliveryService(t, cfg, sink), deliveryService(t, cfg, sink)
	row := acceptTest(t, repair.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	ctx := context.Background()
	if err := repair.durable.store.client.Del(ctx, repair.durable.store.key("delivery-ready")).Err(); err != nil {
		t.Fatal(err)
	}
	var receiptJSON string
	hook := &beforeRepairExec{before: func() {
		dispatch(t, deliver, row.ID)
		receiptJSON = deliver.durable.store.client.Get(ctx, deliver.durable.store.rowKey(row.ID)).Val()
	}}
	repair.durable.store.client.AddHook(hook)
	_, err := repair.durable.store.reconcile(ctx, row.ID, func(current *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) { return current, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !hook.once.Load() {
		t.Fatal("repair EXEC not reached")
	}
	if after := repair.durable.store.client.Get(ctx, repair.durable.store.rowKey(row.ID)).Val(); after != receiptJSON {
		t.Fatal("repair rewrote delivery receipt")
	}
	if repair.durable.store.client.SIsMember(ctx, repair.durable.store.key("delivery-ready"), row.ID).Val() {
		t.Fatal("stale repair restored ready work")
	}
	if r := onlyBinding(t, deliveryRow(t, deliver, row.ID)).Intents["firing"].Receipt; r == nil {
		t.Fatal("receipt lost")
	}
}

func TestPolicyChangeDuringOccurrenceTransaction(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	old := manager(t, cfg)
	row := pendingSnapshot(t, old)
	before := old.store.client.Get(context.Background(), old.store.rowKey(row.ID)).Val()
	var once sync.Once
	var current *DurableManager
	_, err := old.store.update(context.Background(), row.ID, func(candidate *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
		once.Do(func() { cfg.Durable.PolicyEpoch++; current = manager(t, cfg) })
		candidate.Fence++
		return candidate, nil
	})
	if !errors.Is(err, ErrPolicyEpoch) {
		t.Fatal("old policy transaction committed", err)
	}
	if after := old.store.client.Get(context.Background(), old.store.rowKey(row.ID)).Val(); after != before {
		t.Fatal("stale policy overwrote state")
	}
	if err := current.store.health(context.Background()); err != nil {
		t.Fatal("policy change incorrectly quarantined schema", err)
	}
}

func TestDestinationChangeDuringOccurrenceTransaction(t *testing.T) {
	f := realValkey(t)
	s := deliveryService(t, deliveryConfig(t, f), &controlledSink{})
	ctx := context.Background()
	row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	before := s.durable.store.client.Get(ctx, s.durable.store.rowKey(row.ID)).Val()
	var once sync.Once
	_, err := s.durable.store.update(ctx, row.ID, func(candidate *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
		once.Do(func() {
			if err := s.durable.store.client.Set(ctx, s.durable.store.destinationKey(), "changed", 0).Err(); err != nil {
				t.Fatal(err)
			}
		})
		candidate.Fence++
		return candidate, nil
	})
	if !errors.Is(err, ErrPolicyEpoch) {
		t.Fatal("destination identity change did not fence", err)
	}
	if after := s.durable.store.client.Get(ctx, s.durable.store.rowKey(row.ID)).Val(); after != before {
		t.Fatal("stale destination commit")
	}
}

func TestSchemaMaintenanceFencePreventsPartialCommit(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	old := manager(t, cfg)
	ctx := context.Background()
	row := pendingSnapshot(t, old)
	before := old.store.client.Get(ctx, old.store.rowKey(row.ID)).Val()
	var once sync.Once
	setFence := func(state, generation string) {
		value, _ := json.Marshal(indexSchema{1, state, generation})
		if err := old.store.client.Set(ctx, old.store.schemaKey(), value, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	_, err := old.store.update(ctx, row.ID, func(candidate *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
		once.Do(func() {
			setFence("maintenance", "repair-new")
			if err := old.store.client.Del(ctx, old.store.key("due")).Err(); err != nil {
				t.Fatal(err)
			}
			if err := old.store.client.Set(ctx, old.store.key("due"), "wrong", 0).Err(); err != nil {
				t.Fatal(err)
			}
		})
		candidate.Fence++
		return candidate, nil
	})
	if !errors.Is(err, ErrIndexSchema) {
		t.Fatal(err)
	}
	if after := old.store.client.Get(ctx, old.store.rowKey(row.ID)).Val(); after != before {
		t.Fatal("coordinated schema change partially committed")
	}
	if _, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))); !errors.Is(err, ErrIndexSchema) {
		t.Fatal("maintenance fence bypassed", err)
	}
	if err := old.store.client.Del(ctx, old.store.key("due")).Err(); err != nil {
		t.Fatal(err)
	}
	setFence("ready", "repaired-new-generation")
	repaired := manager(t, cfg)
	var pending []string
	if err := repaired.maintenance(ctx, &pending); err != nil {
		t.Fatal(err)
	}
	if score := repaired.store.client.ZScore(ctx, repaired.store.key("due"), row.ID).Val(); score != deadlineScore(row.dueAt()) {
		t.Fatal("missing schedule not recovered", score)
	}
	if err := old.store.health(ctx); !errors.Is(err, ErrIndexSchema) {
		t.Fatal("old process resumed without restart", err)
	}
}

func TestWrongTypePreflightStopsAllDeliveryInstances(t *testing.T) {
	f := realValkey(t)
	cfg := deliveryConfig(t, f)
	sink := &controlledSink{}
	first, second := deliveryService(t, cfg, sink), deliveryService(t, cfg, sink)
	ctx := context.Background()
	a := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
	row, err := first.durable.accept(ctx, a, modelsPayload(a))
	if err != nil {
		t.Fatal(err)
	}
	before := first.durable.store.client.Get(ctx, first.durable.store.rowKey(row.Occurrence.ID)).Val()
	if err := first.durable.store.client.Del(ctx, first.durable.store.key("delivery-due")).Err(); err != nil {
		t.Fatal(err)
	}
	if err := first.durable.store.client.Set(ctx, first.durable.store.key("delivery-due"), "wrong", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if w := post(first, testPayload(t, a)); w.Code != 503 {
		t.Fatal("corrupt index acknowledged", w.Code, w.Body)
	}
	if after := first.durable.store.client.Get(ctx, first.durable.store.rowKey(row.Occurrence.ID)).Val(); after != before {
		t.Fatal("preflight violation committed state")
	}
	if err := second.dispatcher.runOne(ctx, row.Occurrence.ID); !errors.Is(err, ErrIndexSchema) {
		t.Fatal("peer dispatched after quarantine", err)
	}
	if len(sink.snapshot()) != 0 {
		t.Fatal("sink called in corrupt namespace")
	}
	if err := second.durable.store.health(ctx); !errors.Is(err, ErrIndexSchema) {
		t.Fatal(err)
	}
}

func TestClaimExpirationDuringTransactionStillFences(t *testing.T) {
	for _, delivery := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduler", true: "delivery"}[delivery], func(t *testing.T) {
			f := realValkey(t)
			ctx := context.Background()
			if delivery {
				s := deliveryService(t, deliveryConfig(t, f), &controlledSink{})
				row := acceptTest(t, s.durable, "firing", "2026-01-05T08:30:00+01:00", "")
				claimed, err := s.dispatcher.claim(ctx, row.ID)
				if err != nil {
					t.Fatal(err)
				}
				before := s.durable.store.client.Get(ctx, s.durable.store.rowKey(row.ID)).Val()
				_, err = s.durable.store.updateDelivery(ctx, row.ID, claimed.DeliveryClaim, func(candidate *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
					waitPast(t, claimed.DeliveryClaim.Until)
					candidate.Fence++
					return candidate, nil
				})
				if !errors.Is(err, ErrFenced) {
					t.Fatal("expired delivery commit", err)
				}
				if after := s.durable.store.client.Get(ctx, s.durable.store.rowKey(row.ID)).Val(); after != before {
					t.Fatal("expired worker changed record")
				}
			} else {
				m := manager(t, integrationConfig(t, f))
				row := pendingSnapshot(t, m)
				claimed, err := m.claim(ctx, row.ID)
				if err != nil {
					t.Fatal(err)
				}
				before := m.store.client.Get(ctx, m.store.rowKey(row.ID)).Val()
				_, err = m.store.updateClaim(ctx, row.ID, claimed.Claim, func(candidate *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
					waitPast(t, claimed.Claim.Until)
					candidate.Fence++
					return candidate, nil
				})
				if !errors.Is(err, ErrFenced) {
					t.Fatal("expired scheduler commit", err)
				}
				if after := m.store.client.Get(ctx, m.store.rowKey(row.ID)).Val(); after != before {
					t.Fatal("expired scheduler changed record")
				}
			}
		})
	}
}

func TestStartupRejectsExistingIndexCorruption(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	client := redis.NewClient(&redis.Options{Addr: f.address})
	defer client.Close()
	if err := client.Set(context.Background(), "sla:{sla}:pending", "bad type", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))); !errors.Is(err, ErrIndexSchema) {
		t.Fatal("startup accepted corrupt schema", err)
	}
	if value := client.Get(context.Background(), "sla:{sla}:policies").Val(); value != "" {
		t.Fatal("startup activated policies despite schema failure")
	}
}

func TestIndependentDeliveryAcrossInstancesHasNoFalseConflicts(t *testing.T) {
	f := realValkey(t)
	cfg := deliveryConfig(t, f)
	services := []*Service{deliveryService(t, cfg, nil), deliveryService(t, cfg, nil)}
	ctx := context.Background()
	ids := make([]string, 12)
	for i := range ids {
		a := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
		a.Fingerprint = fmt.Sprintf("independent-%d", i)
		accepted, err := services[0].durable.accept(ctx, a, modelsPayload(a))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = accepted.Occurrence.ID
	}
	hooks := &benchmarkCommands{}
	for _, s := range services {
		s.durable.store.client.AddHook(hooks)
	}
	start := make(chan struct{})
	outcomes := make(chan error, 2)
	var workers sync.WaitGroup
	for instance, s := range services {
		workers.Go(func() {
			<-start
			for i := instance; i < len(ids); i += len(services) {
				if err := s.dispatcher.runOne(ctx, ids[i]); err != nil {
					outcomes <- err
					return
				}
			}
			outcomes <- nil
		})
	}
	close(start)
	workers.Wait()
	close(outcomes)
	for err := range outcomes {
		if err != nil {
			t.Fatal(err)
		}
	}
	if hooks.retries.Load() != 0 {
		t.Fatal("independent dispatchers conflicted on shared indexes", hooks.retries.Load())
	}
	for _, id := range ids {
		row := deliveryRow(t, services[0], id)
		b := onlyBinding(t, row)
		if row.Version != 4 || b.Delivery != Delivered || b.Intents["firing"].Receipt == nil || b.Intents["firing"].Receipt.Attempts != 1 {
			t.Fatal("missing/duplicate delivery", row)
		}
	}
	store := services[0].durable.store
	if store.client.ZCard(ctx, store.key("delivery-due")).Val() != 0 || store.client.SCard(ctx, store.key("delivery-claimed")).Val() != 0 || store.client.SCard(ctx, store.key("delivery-ready")).Val() != 0 {
		t.Fatal("completed delivery indexes inconsistent")
	}
	if got := store.client.SCard(ctx, store.key("unresolved")).Val(); got != int64(len(ids)) {
		t.Fatal("independent lifecycle index lost updates", got)
	}
}

func TestQuarantineMarkerFailureStillStopsLocalProcessing(t *testing.T) {
	f := realValkey(t)
	cfg := integrationConfig(t, f)
	m := manager(t, cfg)
	ctx := context.Background()
	row := acceptTest(t, m, "firing", "2026-01-05T08:30:00+01:00", "")
	before := m.store.client.Get(ctx, m.store.rowKey(row.ID)).Val()
	if err := m.store.client.Set(ctx, m.store.key("pending"), "wrong", 0).Err(); err != nil {
		t.Fatal(err)
	}
	// Model an ACL fault that prevents publishing the quarantine marker.
	if err := m.store.client.Do(ctx, "ACL", "SETUSER", "default", "-set").Err(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.store.client.Do(ctx, "ACL", "SETUSER", "default", "+set").Err() }()
	a := alertAt(t, "firing", "sla_id", "2026-01-05T08:30:00+01:00")
	_, err := m.accept(ctx, a, modelsPayload(a))
	if !errors.Is(err, ErrIndexSchema) || !strings.Contains(err.Error(), "marker not confirmed") {
		t.Fatal("marker failure hidden", err)
	}
	if err := m.store.health(ctx); !errors.Is(err, ErrIndexSchema) {
		t.Fatal("local processing continued without durable marker", err)
	}
	if after := m.store.client.Get(ctx, m.store.rowKey(row.ID)).Val(); after != before {
		t.Fatal("preflight failure changed occurrence")
	}
	if current := m.store.client.Get(ctx, m.store.schemaKey()).Val(); !readyIndexSchema(current) {
		t.Fatal("test did not exercise failed publication", current)
	}
}
