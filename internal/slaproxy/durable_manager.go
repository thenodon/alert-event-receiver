package slaproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

type DurableManager struct {
	store         *DurableStore
	engine        *CalendarEngine
	cfg           Config
	logger        *slog.Logger
	scanCursor    uint64 // Owned by the scheduler's single maintenance loop.
	indexCursor   uint64
	indexPosition int
	indexPending  []string
}

type Acceptance struct {
	Occurrence *DurableOccurrence `json:"occurrence,omitempty"`
	Skipped    string             `json:"skipped,omitempty"`
	Policies   []PolicyRef        `json:"-"`
}

func notification(a models.Alert, p models.WebhookPayload) *Notification {
	p.Alerts = nil
	return &Notification{Alert: a, Metadata: p}
}

func (m *DurableManager) accept(ctx context.Context, a models.Alert, payload models.WebhookPayload) (Acceptance, error) {
	id := occurrenceKeyID(a)
	ref, selection := m.cfg.Select(a)
	var refs []PolicyRef
	if selection == "selected" {
		refs = []PolicyRef{ref}
	}
	existing, err := m.store.load(ctx, id)
	if err != nil {
		return Acceptance{Policies: refs}, err
	}
	if existing != nil {
		refs = nil
		for _, b := range existing.Bindings {
			refs = append(refs, b.Policy)
		}
	}
	if existing == nil && selection != "selected" {
		return Acceptance{Skipped: selection}, nil
	}
	var before *DurableOccurrence
	row, err := m.store.update(ctx, id, func(row *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		before = transitionSnapshot(row)
		if row != nil && row.DeleteAt != nil && !row.DeleteAt.After(now) {
			row = nil
		}
		if row == nil {
			if selection != "selected" {
				return nil, fmt.Errorf("occurrence disappeared during resolution acceptance")
			}
			d, err := m.engine.Evaluate(ctx, PolicyInput{Alert: a}, ref, m.cfg.Durable.SinkID, a.StartsAt)
			if err != nil {
				m.store.metrics.policyErrors.WithLabelValues(evaluationErrorReason(err)).Inc()
				return nil, err
			}
			bid := bindingKeyID(id, ref, m.cfg.Durable.SinkID)
			row = &DurableOccurrence{ID: id, Fingerprint: a.Fingerprint, StartsAt: a.StartsAt.UTC(), Lifecycle: LifecycleFiring,
				Original: notification(a, payload), CreatedAt: now, Bindings: map[string]*DurableBinding{bid: {ID: bid, Policy: ref, SinkID: m.cfg.Durable.SinkID,
					PolicyVersion: d.PolicyVersion, Calendar: d, Eligibility: EligibilityUnknown, Delivery: NotDelivered, Intents: map[string]*DeliveryIntent{}}}}
		}
		if row.Fingerprint != a.Fingerprint || !row.StartsAt.Equal(a.StartsAt) {
			return nil, fmt.Errorf("occurrence identity mismatch")
		}
		row.Claim = nil // Any lifecycle notification invalidates outstanding work.
		if a.Status == "resolved" {
			row.LastReceivedResolutionAt = utcPointer(now)
			if row.Lifecycle != LifecycleResolved {
				row.Lifecycle, row.EndsAt, row.CompletedAt = LifecycleResolved, utcPointer(a.EndsAt), utcPointer(now)
				row.CompactAt, row.DeleteAt = utcPointer(now.Add(m.cfg.Durable.Retention.CompletedPayload)), utcPointer(now.Add(m.cfg.Durable.Retention.Tombstone))
				row.Resolution = notification(a, payload)
			} else if !row.EndsAt.Equal(a.EndsAt) {
				row.ResolutionConflict = true
			}
		} else {
			row.LastReceivedFiringAt = utcPointer(now)
			if row.Lifecycle == LifecycleFiring && !row.Compacted {
				row.LastFiring = notification(a, payload)
			}
		}
		m.advance(row, now)
		return row, nil
	})
	if err != nil {
		return Acceptance{Policies: refs}, err
	}
	m.logTransition(before, row, "webhook")
	refs = nil
	for _, b := range row.Bindings {
		refs = append(refs, b.Policy)
	}
	return Acceptance{Occurrence: row, Policies: refs}, nil
}

func transitionSnapshot(row *DurableOccurrence) *DurableOccurrence {
	if row == nil {
		return nil
	}
	copy := *row
	copy.Bindings = make(map[string]*DurableBinding, len(row.Bindings))
	for id, b := range row.Bindings {
		binding := *b
		copy.Bindings[id] = &binding
	}
	return &copy
}

// advance trusts Alertmanager's webhook lifecycle. Absence of a received resolve
// means firing in known state, not independently verified underlying activity.
func (m *DurableManager) advance(row *DurableOccurrence, now time.Time) {
	m.syncPolicies(row, now)
	for _, b := range row.Bindings {
		b.NextEvaluationAt = time.Time{}
		first := b.Calendar.NextEvaluationAt
		if row.Lifecycle == LifecycleResolved {
			a := models.Alert{Status: "resolved", StartsAt: row.StartsAt, EndsAt: *row.EndsAt}
			b.Eligibility, _ = alertEligibility(a, b.Calendar, now)
			// Phase 4 must decide retroactive firing. Unconsumed firing intent
			// becomes non-actionable once a resolution is known.
			if i := b.Intents["firing"]; i != nil {
				i.Ready, i.Canceled = false, true
			}
			b.Delivery = NotDelivered
			if b.Eligibility == EligibilityEligible || b.FiringDeliveredAt != nil {
				i := m.intent(row, b, "resolved", now)
				i.RequiresFiringDelivery = true
				i.Ready = row.ResolutionConflict == false && b.HoldReason == "" && b.FiringDeliveredAt != nil
				if i.Ready {
					b.Delivery = PendingDelivery
				}
			} else if i := b.Intents["resolved"]; i != nil {
				i.Ready, i.Canceled = false, true
			}
			continue
		}
		if b.HoldReason != "" {
			for _, i := range b.Intents {
				i.Ready = false
			}
			continue // Preserve historical eligibility while held.
		}
		if first.IsZero() {
			b.Eligibility, b.HoldReason = EligibilityUnknown, "search_horizon_exhausted"
			continue
		}
		if first.After(now) {
			b.Eligibility, b.NextEvaluationAt = EligibilityPending, first.UTC()
			continue
		}
		b.Eligibility = EligibilityEligible
		if b.FiringDeliveredAt != nil {
			continue
		} // Preserve future receipt-derived state; never create a receipt here.
		i := m.intent(row, b, "firing", now)
		i.Ready, i.Canceled = true, false
		b.Delivery = PendingDelivery
	}
}

func (m *DurableManager) syncPolicies(row *DurableOccurrence, now time.Time) {
	for _, b := range row.Bindings {
		oldHold := b.HoldReason
		policy, exists := m.engine.policies[b.Policy]
		b.HoldReason = ""
		if !exists {
			b.HoldReason = "policy_removed"
		} else if policy.version != b.PolicyVersion {
			b.HoldReason = "policy_changed"
		}
		if b.SinkID != m.cfg.Durable.SinkID {
			b.HoldReason = "sink_changed"
		}
		if row.ResolutionConflict {
			b.HoldReason = "resolution_conflict"
		}
		if b.Calendar.NextEvaluationAt.IsZero() && b.HoldReason == "" && row.Lifecycle == LifecycleFiring {
			b.HoldReason = "search_horizon_exhausted"
		}
		if b.HoldReason != "" {
			b.NextEvaluationAt = time.Time{}
			for _, i := range b.Intents {
				i.Ready = false
			}
		} else if oldHold != "" && row.Lifecycle == LifecycleFiring {
			b.NextEvaluationAt = now // Restoring exactly the pinned version queues work, not migration.
		}
	}
}

func (m *DurableManager) intent(row *DurableOccurrence, b *DurableBinding, action string, now time.Time) *DeliveryIntent {
	if i := b.Intents[action]; i != nil {
		return i
	}
	id := tupleID(row.ID, b.ID, action)
	reference := "last_firing"
	if action == "resolved" {
		reference = "resolution"
	}
	i := &DeliveryIntent{ID: id, Action: action, PayloadReference: reference, IdempotencyKey: id, CreatedAt: now}
	b.Intents[action] = i
	return i
}

func (m *DurableManager) claim(ctx context.Context, id string) (*DurableOccurrence, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	row, err := m.store.update(ctx, id, func(row *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		if row == nil || row.dueAt().IsZero() || row.dueAt().After(now) {
			return nil, ErrClaimBusy
		}
		if row.Lifecycle == LifecycleResolved {
			return nil, ErrClaimBusy
		}
		row.Fence++
		row.Claim = &SchedulerClaim{Token: hex.EncodeToString(token[:]), Fence: row.Fence, Until: now.Add(m.cfg.Durable.Scheduler.LeaseDuration)}
		return row, nil
	})
	return row, err
}

func (m *DurableManager) complete(ctx context.Context, claimed *DurableOccurrence) (*DurableOccurrence, error) {
	return m.store.updateClaim(ctx, claimed.ID, claimed.Claim, func(row *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		row.Claim, row.LastEvaluatedAt = nil, utcPointer(now)
		m.advance(row, now)
		return row, nil
	})
}

func (m *DurableManager) logTransition(before, after *DurableOccurrence, reason string) {
	if after == nil {
		return
	}
	from := "missing"
	if before != nil {
		from = string(before.Lifecycle)
	}
	if from != string(after.Lifecycle) {
		m.store.metrics.transitions.WithLabelValues(from, string(after.Lifecycle), reason).Inc()
	}
	for id, b := range after.Bindings {
		var old *DurableBinding
		if before != nil {
			old = before.Bindings[id]
		}
		if old != nil && old.Eligibility == b.Eligibility && old.HoldReason == b.HoldReason && before.Lifecycle == after.Lifecycle {
			continue
		}
		m.logger.Info("SLA occurrence transition", "component", "sla_lifecycle", "operation", reason, "occurrence_id", after.ID,
			"fingerprint", after.Fingerprint, "starts_at", after.StartsAt, "sla_label", b.Policy.SLALabel, "sla_value", b.Policy.SLAValue, "sink_id", b.SinkID,
			"lifecycle", after.Lifecycle, "eligibility", b.Eligibility, "delivery", b.Delivery, "hold_reason", b.HoldReason,
			"next_evaluation_at", b.NextEvaluationAt, "state_version", after.Version)
	}
}
