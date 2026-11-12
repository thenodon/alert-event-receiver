package slaproxy

import (
	"context"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

// PolicyRef identifies a policy within its independent label namespace.
type PolicyRef struct {
	SLALabel string
	SLAValue string
}

// OccurrenceID retains Alertmanager's identity, without recomputing fingerprints.
// Compare StartsAt with time.Time.Equal; serialize as UTC RFC3339Nano.
type OccurrenceID struct {
	Fingerprint string
	StartsAt    time.Time
}

// Decision is calendar eligibility at the supplied instant, not alert lifecycle
// eligibility. NextEvaluationAt is the first eligible instant at or after it.
type Decision struct {
	Eligible         bool
	Reason           string
	NextEvaluationAt time.Time
	PolicyVersion    string
	// SearchUntil is exclusive, and set on horizon exhaustion when coverage is
	// known: no eligible instant exists in [evaluation time, SearchUntil).
	SearchUntil time.Time
}

// PolicyEvaluator observes eligibility, without persisting or delivering alerts.
type PolicyEvaluator interface {
	Evaluate(ctx context.Context, input PolicyInput, policy PolicyRef, sinkID string, at time.Time) (Decision, error)
}

// PolicyInput separates optional enrichment metadata from original alert labels.
type PolicyInput struct {
	Alert      models.Alert
	Enrichment Enrichment
}

// HolidayProvider receives a civil date: only Year/Month/Day are meaningful.
// It must not reinterpret that date by converting its time zone. Implementations
// validate coverage at startup and must support concurrent evaluation.
type HolidayProvider interface {
	Version() string
	Validate(country, subdivision string) error
	IsHoliday(ctx context.Context, localDate time.Time, country, subdivision string) (bool, error)
}

// PendingOccurrence is a read model, not a durable delivery state machine.
type PendingOccurrence struct {
	Alert            models.Alert
	Policy           PolicyRef
	SinkID           string
	LastSeenAt       time.Time
	NextEvaluationAt time.Time
}

// PendingState is deliberately read-only. Atomic acceptance, mutation, claims,
// fencing and not-found semantics must be designed before Phase 3 implementation.
type PendingState interface {
	Lookup(ctx context.Context, occurrence OccurrenceID, sinkID string) (*PendingOccurrence, error)
}

// Scheduler implementations must define durable recovery/ownership in Phase 3.
type Scheduler interface {
	Run(context.Context) error
}

// DeliveryReceipt describes sink acceptance only, never workflow completion.
type DeliveryReceipt struct {
	Accepted bool
}

// DownstreamDelivery preserves the original webhook metadata and alerts. Its
// payload, idempotency, durable intent and ordering contracts need Phase 4 review.
// This interface has no implementation and is never invoked in Phases 1 or 2.
type DownstreamDelivery interface {
	Deliver(ctx context.Context, sinkID, idempotencyKey string, payload models.WebhookPayload) (DeliveryReceipt, error)
}

// Enrichment is separate metadata; it must not replace original labels/identity.
type Enrichment map[string]string

type Enricher interface {
	Enrich(context.Context, models.Alert) (Enrichment, error)
}

type NoopEnricher struct{}

func (NoopEnricher) Enrich(context.Context, models.Alert) (Enrichment, error) {
	return nil, nil
}

// Select uses the first nonempty configured label. Unknown values never fall
// through to a later definition. The reference is retained for diagnostics.
func (c Config) Select(alert models.Alert) (PolicyRef, string) {
	for _, def := range c.SLADefinitions {
		value := alert.Labels[def.SLALabel]
		if value == "" {
			continue
		}
		ref := PolicyRef{SLALabel: def.SLALabel, SLAValue: value}
		if _, exists := def.SLAs[value]; !exists {
			return ref, "unknown_sla"
		}
		return ref, "selected"
	}
	return PolicyRef{}, "missing_sla_label"
}
