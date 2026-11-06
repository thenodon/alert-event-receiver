package state_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	"github.com/andersh/alertmanager_event_receiver/internal/state"
	"github.com/andersh/alertmanager_event_receiver/internal/telemetry"
)

func newTestStore(t *testing.T) (*state.RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	return state.NewRedisStore(client), mr
}

func TestGetState_NotFound(t *testing.T) {
	store, _ := newTestStore(t)
	s, err := store.GetState(context.Background(), "unknown")
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Error("expected nil for unknown fingerprint")
	}
}

func TestSetFiringAndGetState(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	firingBefore := testutil.ToFloat64(telemetry.StateEntries.WithLabelValues("firing"))
	opsBefore := testutil.ToFloat64(telemetry.RedisOpsTotal.WithLabelValues("set_firing", "ok"))

	err := store.SetFiring(ctx, "fp1", state.AlertState{
		Status:        "firing",
		FirstFiringAt: now,
		LastSeenAt:    now,
		StartsAt:      now,
		Alertname:     "TestAlert",
		Labels:        map[string]string{"alertname": "TestAlert", "severity": "warning"},
	})
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.GetState(ctx, "fp1")
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("expected state, got nil")
	}
	if s.Status != "firing" {
		t.Errorf("expected status=firing, got %s", s.Status)
	}
	if s.Alertname != "TestAlert" {
		t.Errorf("expected alertname=TestAlert, got %s", s.Alertname)
	}
	if s.Labels["severity"] != "warning" {
		t.Errorf("expected severity=warning, got %s", s.Labels["severity"])
	}
	if !s.FirstFiringAt.Equal(now) {
		t.Errorf("first_firing_at mismatch: %v vs %v", s.FirstFiringAt, now)
	}
	if got := testutil.ToFloat64(telemetry.StateEntries.WithLabelValues("firing")) - firingBefore; got != 1 {
		t.Errorf("expected firing gauge delta 1, got %v", got)
	}
	if got := testutil.ToFloat64(telemetry.RedisOpsTotal.WithLabelValues("set_firing", "ok")) - opsBefore; got != 1 {
		t.Errorf("expected set_firing ok ops delta 1, got %v", got)
	}
}

func TestSetClosed_SetsStatusAndTTL(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	_ = store.SetFiring(ctx, "fp2", state.AlertState{
		Status: "firing", FirstFiringAt: now, LastSeenAt: now,
		StartsAt: now, Alertname: "A", Labels: map[string]string{},
	})
	closedBefore := testutil.ToFloat64(telemetry.StateEntries.WithLabelValues("closed"))
	firingBefore := testutil.ToFloat64(telemetry.StateEntries.WithLabelValues("firing"))
	opsBefore := testutil.ToFloat64(telemetry.RedisOpsTotal.WithLabelValues("set_closed", "ok"))
	if err := store.SetClosed(ctx, "fp2", state.AlertState{StartsAt: now, FirstFiringAt: now, EndsAt: now.Add(time.Minute)}, 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	s, _ := store.GetState(ctx, "fp2")
	if s.Status != "closed" {
		t.Errorf("expected closed, got %s", s.Status)
	}
	if ttl := mr.TTL("alertstate:fp2"); ttl <= 0 {
		t.Error("expected positive TTL after SetClosed")
	}
	if got := testutil.ToFloat64(telemetry.StateEntries.WithLabelValues("closed")) - closedBefore; got != 1 {
		t.Errorf("expected closed gauge delta 1, got %v", got)
	}
	if got := testutil.ToFloat64(telemetry.StateEntries.WithLabelValues("firing")) - firingBefore; got != -1 {
		t.Errorf("expected firing gauge delta -1, got %v", got)
	}
	if got := testutil.ToFloat64(telemetry.RedisOpsTotal.WithLabelValues("set_closed", "ok")) - opsBefore; got != 1 {
		t.Errorf("expected set_closed ok ops delta 1, got %v", got)
	}
}

func TestUpdateLastSeen(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	_ = store.SetFiring(ctx, "fp3", state.AlertState{
		Status: "firing", FirstFiringAt: now, LastSeenAt: now,
		StartsAt: now, Alertname: "A", Labels: map[string]string{},
	})
	later := now.Add(5 * time.Minute)
	if err := store.UpdateLastSeen(ctx, "fp3", later); err != nil {
		t.Fatal(err)
	}
	s, _ := store.GetState(ctx, "fp3")
	if !s.LastSeenAt.Equal(later) {
		t.Errorf("last_seen_at not updated: got %v, want %v", s.LastSeenAt, later)
	}
}

func TestCheckAndSetIdempotency_FirstCallSucceeds(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	setBefore := testutil.ToFloat64(telemetry.RedisIdempSetNXTotal.WithLabelValues("set"))

	first, err := store.CheckAndSetIdempotency(ctx, "fp1:firing:12345", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !first {
		t.Error("expected first=true")
	}
	if got := testutil.ToFloat64(telemetry.RedisIdempSetNXTotal.WithLabelValues("set")) - setBefore; got != 1 {
		t.Errorf("expected setnx set delta 1, got %v", got)
	}
}

func TestCheckAndSetIdempotency_DuplicateReturnsFalse(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	_, _ = store.CheckAndSetIdempotency(ctx, "fp1:firing:12345", time.Hour)
	existsBefore := testutil.ToFloat64(telemetry.RedisIdempSetNXTotal.WithLabelValues("exists"))
	second, err := store.CheckAndSetIdempotency(ctx, "fp1:firing:12345", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("expected second=false for duplicate key")
	}
	if got := testutil.ToFloat64(telemetry.RedisIdempSetNXTotal.WithLabelValues("exists")) - existsBefore; got != 1 {
		t.Errorf("expected setnx exists delta 1, got %v", got)
	}
}

func TestIdempotencyKey(t *testing.T) {
	t1 := time.Unix(1700000000, 0)
	key := state.IdempotencyKey("abc", "firing", t1)
	expected := "abc:2023-11-14T22:13:20Z:firing"
	if key != expected {
		t.Errorf("expected %s, got %s", expected, key)
	}
}

func TestFingerprintSource(t *testing.T) {
	labels := map[string]string{
		"severity":  "warning",
		"alertname": "TestAlert",
		"service":   "checkout",
	}
	src := state.FingerprintSource(labels)
	expected := "alertname+service+severity"
	if src != expected {
		t.Errorf("expected %q, got %q", expected, src)
	}
}

func TestClosedStatePreservesPreciseOccurrenceTiming(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 4, 15, 10, 0, 0, 123456789, time.UTC)
	if err := store.SetFiring(ctx, "precise", state.AlertState{FirstFiringAt: at, StartsAt: at, LastSeenAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetClosed(ctx, "precise", state.AlertState{StartsAt: at, FirstFiringAt: at, EndsAt: at.Add(time.Minute)}, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetState(ctx, "precise")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != "closed" || !got.StartsAt.Equal(at) || !got.FirstFiringAt.Equal(at) {
		t.Fatalf("occurrence timing lost: %+v", got)
	}
}

func TestOccurrenceKeyPrecision(t *testing.T) {
	at := time.Date(2026, 4, 15, 10, 0, 0, 123456789, time.UTC)
	key := state.IdempotencyKey("fp", "resolved", at)
	if key != "fp:2026-04-15T10:00:00.123456789Z:resolved" {
		t.Fatalf("unexpected key: %s", key)
	}
	if key == state.IdempotencyKey("fp", "resolved", at.Add(time.Nanosecond)) {
		t.Fatal("nanosecond collision")
	}
	if key != state.IdempotencyKey("fp", "resolved", at.In(time.FixedZone("offset", 7200))) {
		t.Fatal("timezone changed occurrence key")
	}
	if key == state.IdempotencyKey("fp", "firing", at) {
		t.Fatal("transition collision")
	}
	store, mr := newTestStore(t)
	if ok, err := store.CheckAndSetIdempotency(context.Background(), key, time.Hour); err != nil || !ok {
		t.Fatalf("set: %v, %v", ok, err)
	}
	if !mr.Exists("alertidemp:" + key) {
		t.Fatal("wrong Valkey key format")
	}
}

func TestCompleteOccurrenceReplacement(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 4, 15, 10, 0, 0, 123456789, time.UTC)
	old := state.AlertState{StartsAt: at, FirstFiringAt: at, LastSeenAt: at, Labels: map[string]string{"obsolete": "yes"}}
	if err := store.SetFiring(ctx, "fp", old); err != nil {
		t.Fatal(err)
	}
	newer := state.AlertState{StartsAt: at.Add(time.Nanosecond), FirstFiringAt: at.Add(time.Nanosecond), LastSeenAt: at, EndsAt: at.Add(time.Minute), Labels: map[string]string{"new": "yes"}}
	if err := store.SetClosed(ctx, "fp", newer, time.Hour); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetState(ctx, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected closed occurrence, got nil")
		return
	}
	if !got.StartsAt.Equal(newer.StartsAt) || !got.EndsAt.Equal(newer.EndsAt) || got.Labels["obsolete"] != "" || got.Labels["new"] != "yes" || mr.TTL("alertstate:fp") <= 0 {
		t.Fatalf("incorrect closed occurrence: %+v", got)
	}
	newer.StartsAt = newer.EndsAt.Add(time.Nanosecond)
	newer.EndsAt = time.Time{}
	if err := store.SetFiring(ctx, "fp", newer); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetState(ctx, "fp")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected reopened occurrence, got nil")
		return
	}
	if !got.EndsAt.IsZero() || mr.TTL("alertstate:fp") != 0 {
		t.Fatalf("reopen retained closure or TTL: %+v", got)
	}
}

func TestRedisOperationFailures(t *testing.T) {
	store, mr := newTestStore(t)
	ctx := context.Background()
	mr.SetError("ERR unavailable")
	if _, err := store.GetState(ctx, "fp"); err == nil {
		t.Fatal("expected read error")
	}
	if err := store.SetFiring(ctx, "fp", state.AlertState{}); err == nil {
		t.Fatal("expected firing error")
	}
	if err := store.SetClosed(ctx, "fp", state.AlertState{}, time.Hour); err == nil {
		t.Fatal("expected close error")
	}
	if err := store.UpdateLastSeen(ctx, "fp", time.Now()); err == nil {
		t.Fatal("expected refresh error")
	}
}

func TestMalformedStoredOccurrenceFailsRead(t *testing.T) {
	store, mr := newTestStore(t)
	mr.HSet("alertstate:fp", "status", "firing", "starts_at", "malformed")
	if _, err := store.GetState(context.Background(), "fp"); err == nil {
		t.Fatal("expected corrupt state read failure")
	}
}
