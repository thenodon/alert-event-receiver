//go:build integration

package slaproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func withDataset(cfg Config, path string) Config {
	// Config contains maps; never mutate a live manager's configuration in tests.
	definitions := make([]Definition, len(cfg.SLADefinitions))
	for i, def := range cfg.SLADefinitions {
		definitions[i] = Definition{SLALabel: def.SLALabel, SLAs: make(map[string]Policy, len(def.SLAs))}
		for name, p := range def.SLAs {
			p.Holidays = &Holidays{Dataset: datasetPath(path)}
			definitions[i].SLAs[name] = p
		}
	}
	cfg.SLADefinitions = definitions
	return cfg
}

func TestDatasetDurablePendingEpochRecoveryAndHolds(t *testing.T) {
	for _, finish := range []string{"restore", "resolve"} {
		t.Run(finish, func(t *testing.T) {
			f := realValkey(t)
			original := icsCalendar(icsEvent("20260106", ""))
			path := writeDataset(t, original, 2020, 2040)
			cfg := withDataset(integrationConfig(t, f), path)
			old := manager(t, cfg)
			pending := pendingSnapshot(t, old)
			pinned := onlyBinding(t, pending).Calendar
			// New data would postpone the first opportunity from Monday to the next week.
			saveDataset(t, path, icsCalendar(icsEvent("20260105", "")), 2020, 2040)
			if service, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
				service.Close()
				t.Fatal("dataset changed without an epoch")
			}
			changed := cfg
			changed.Durable.PolicyEpoch++
			current := manager(t, changed)
			d, err := current.engine.Evaluate(context.Background(), PolicyInput{}, PolicyRef{"sla_id", "business"}, "", pending.StartsAt)
			if err != nil || d.NextEvaluationAt.Equal(pinned.NextEvaluationAt) {
				t.Fatal("changed dataset did not alter policy", d, err)
			}
			// Disk-backed restart, before reconciliation has had an opportunity to hold it.
			f.restart()
			current = manager(t, changed)
			if err := current.runOne(context.Background(), pending.ID); err != nil {
				t.Fatal(err)
			}
			held, err := current.store.load(context.Background(), pending.ID)
			if err != nil {
				t.Fatal(err)
			}
			binding := onlyBinding(t, held)
			if binding.HoldReason != "policy_changed" || binding.Eligibility != EligibilityPending || !binding.NextEvaluationAt.IsZero() || binding.PolicyVersion != pinned.PolicyVersion || !binding.Calendar.NextEvaluationAt.Equal(pinned.NextEvaluationAt) {
				t.Fatal("pending work silently reinterpreted", binding)
			}
			if _, err := old.claim(context.Background(), pending.ID); !errors.Is(err, ErrPolicyEpoch) {
				t.Fatal("old worker not fenced", err)
			}
			if finish == "resolve" {
				closed := acceptTest(t, current, "resolved", "2026-01-05T07:30:00+01:00", "2026-01-05T08:15:00+01:00")
				b := onlyBinding(t, closed)
				if closed.Lifecycle != LifecycleResolved || b.HoldReason != "policy_changed" || b.Eligibility != EligibilityEligible || b.Intents["firing"] != nil {
					t.Fatal("resolution lost pinned historical semantics", b)
				}
			} else {
				saveDataset(t, path, original, 2020, 2040)
				restored := cfg
				restored.Durable.PolicyEpoch = changed.Durable.PolicyEpoch + 1
				resumed := manager(t, restored)
				var scan []string
				if err := resumed.maintenance(context.Background(), &scan); err != nil {
					t.Fatal(err)
				}
				if err := resumed.runOne(context.Background(), pending.ID); err != nil {
					t.Fatal(err)
				}
				loaded, err := resumed.store.load(context.Background(), pending.ID)
				if err != nil {
					t.Fatal(err)
				}
				b := onlyBinding(t, loaded)
				if b.HoldReason != "" || b.Eligibility != EligibilityEligible || b.PolicyVersion != pinned.PolicyVersion || b.Intents["firing"] == nil {
					t.Fatal("exact snapshot restoration did not release hold", b)
				}
			}
		})
	}
}

func TestDatasetNonsemanticRestartAndCoverageExtension(t *testing.T) {
	f := realValkey(t)
	original := icsCalendar(icsEvent("20260106", ""))
	path := writeDataset(t, original, 2020, 2040)
	cfg := withDataset(integrationConfig(t, f), path)
	old := manager(t, cfg)
	pending := pendingSnapshot(t, old)
	saveDataset(t, path, icsCalendar(icsEvent("20260106", ""), icsEvent("20260106", "")), 2020, 2040)
	current := manager(t, cfg) // No epoch bump for a byte-only equivalent file.
	if err := current.runOne(context.Background(), pending.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := current.store.load(context.Background(), pending.ID)
	if err != nil || onlyBinding(t, loaded).HoldReason != "" {
		t.Fatal(loaded, err)
	}
	saveDataset(t, path, original, 2020, 2041)
	if service, err := New(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil))); err == nil {
		service.Close()
		t.Fatal("coverage extension bypassed epoch")
	}
	cfg.Durable.PolicyEpoch++
	extended := manager(t, cfg)
	var scan []string
	if err := extended.maintenance(context.Background(), &scan); err != nil {
		t.Fatal(err)
	}
	loaded, err = extended.store.load(context.Background(), pending.ID)
	if err != nil || onlyBinding(t, loaded).HoldReason != "policy_changed" {
		t.Fatal("coverage extension did not hold pinned work", err)
	}
}

func TestDatasetChangeBlocksUnsentDelivery(t *testing.T) {
	f := realValkey(t)
	path := writeDataset(t, icsCalendar(icsEvent("20260106", "")), 2020, 2040)
	cfg := withDataset(deliveryConfig(t, f), path)
	sink := &controlledSink{}
	old := deliveryService(t, cfg, sink)
	row := acceptTest(t, old.durable, "firing", "2026-01-05T08:30:00+01:00", "")
	intent := onlyBinding(t, row).Intents["firing"].ID
	saveDataset(t, path, icsCalendar(icsEvent("20260105", "")), 2020, 2040)
	cfg.Durable.PolicyEpoch++
	current := deliveryService(t, cfg, sink)
	// Dispatcher claim/authorization must synchronize versions before I/O, even
	// before the eventual namespace reconciliation has seen the record.
	dispatch(t, current, row.ID)
	held := deliveryRow(t, current, row.ID)
	b := onlyBinding(t, held)
	if len(sink.snapshot()) != 0 || b.HoldReason != "policy_changed" || b.Intents["firing"].ID != intent || b.Intents["firing"].Receipt != nil {
		t.Fatal("changed dataset sent or rewrote intent", b)
	}
}
