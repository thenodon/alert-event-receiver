package slaproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

type LifecycleStatus string

const (
	LifecycleFiring   LifecycleStatus = "firing"
	LifecycleResolved LifecycleStatus = "resolved"
)

type DeliveryStatus string

const (
	NotDelivered    DeliveryStatus = "not_delivered"
	PendingDelivery DeliveryStatus = "pending_delivery"
	Delivered       DeliveryStatus = "delivered"
	DeliveryFailed  DeliveryStatus = "delivery_failed"
)

// Notification preserves the original alert and all group metadata without
// duplicating the entire batch's other alerts into every occurrence.
type Notification struct {
	Alert    models.Alert          `json:"alert"`
	Metadata models.WebhookPayload `json:"metadata"` // Alerts is nil.
}

type DeliveryIntent struct {
	ID                     string           `json:"id"`
	Action                 string           `json:"action"`
	PayloadReference       string           `json:"payload_reference"`
	IdempotencyKey         string           `json:"idempotency_key"`
	Ready                  bool             `json:"ready"`
	RequiresFiringDelivery bool             `json:"requires_firing_delivery"`
	Canceled               bool             `json:"canceled"`
	CreatedAt              time.Time        `json:"created_at"`
	Attempts               uint64           `json:"attempts,omitempty"`
	AuthorizedAt           *time.Time       `json:"authorized_at,omitempty"`
	NextAttemptAt          time.Time        `json:"next_attempt_at,omitempty"`
	PermanentFailure       bool             `json:"permanent_failure,omitempty"`
	LastFailure            string           `json:"last_failure,omitempty"`
	Payload                *Notification    `json:"payload,omitempty"`
	Receipt                *DeliveryReceipt `json:"receipt,omitempty"`
}

type DurableBinding struct {
	ID               string                     `json:"id"`
	SinkType         string                     `json:"sink_type,omitempty"`
	Policy           PolicyRef                  `json:"policy"`
	SinkID           string                     `json:"sink_id"`
	PolicyVersion    string                     `json:"policy_version"`
	Calendar         Decision                   `json:"calendar"`
	Eligibility      EligibilityStatus          `json:"eligibility"`
	Delivery         DeliveryStatus             `json:"delivery"`
	HoldReason       string                     `json:"hold_reason,omitempty"`
	NextEvaluationAt time.Time                  `json:"next_evaluation_at"`
	RetryCount       int                        `json:"retry_count"`
	Intents          map[string]*DeliveryIntent `json:"intents"`
	// Successful firing evidence; populated only after the sink succeeds.
	FiringDeliveredAt *time.Time `json:"firing_delivered_at,omitempty"`
}

type SchedulerClaim struct {
	Token   string    `json:"token"`
	Fence   uint64    `json:"fence"`
	Until   time.Time `json:"until"`
	Version uint64    `json:"version"`
}

type DurableOccurrence struct {
	ID                       string          `json:"id"`
	Fingerprint              string          `json:"fingerprint"`
	StartsAt                 time.Time       `json:"starts_at"`
	Lifecycle                LifecycleStatus `json:"lifecycle"`
	EndsAt                   *time.Time      `json:"ends_at,omitempty"`
	Original                 *Notification   `json:"original,omitempty"`
	LastFiring               *Notification   `json:"last_firing,omitempty"`
	Resolution               *Notification   `json:"resolution,omitempty"`
	LastReceivedFiringAt     *time.Time      `json:"last_received_firing_at,omitempty"`
	LastReceivedResolutionAt *time.Time      `json:"last_received_resolution_at,omitempty"`
	// Webhooks are trusted lifecycle events, not independent activity verification.
	LastVerifiedActiveAt *time.Time                 `json:"last_verified_active_at,omitempty"`
	LastEvaluatedAt      *time.Time                 `json:"last_evaluated_at,omitempty"`
	Bindings             map[string]*DurableBinding `json:"bindings"`
	Version              uint64                     `json:"version"`
	Fence                uint64                     `json:"fence"`
	Claim                *SchedulerClaim            `json:"claim,omitempty"`
	CreatedAt            time.Time                  `json:"created_at"`
	UpdatedAt            time.Time                  `json:"updated_at"`
	CompletedAt          *time.Time                 `json:"completed_at,omitempty"`
	CompactAt            *time.Time                 `json:"compact_at,omitempty"`
	DeleteAt             *time.Time                 `json:"delete_at,omitempty"`
	Compacted            bool                       `json:"compacted"`
	ResolutionConflict   bool                       `json:"resolution_conflict"`
	DeliveryClaim        *DeliveryClaim             `json:"delivery_claim,omitempty"`
	DeliveryFence        uint64                     `json:"delivery_fence,omitempty"`
}

func tupleID(values ...string) string {
	data, _ := json.Marshal(values)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func occurrenceKeyID(a models.Alert) string {
	return tupleID(a.Fingerprint, a.StartsAt.UTC().Format(time.RFC3339Nano))
}
func bindingKeyID(id string, ref PolicyRef, sink string) string {
	return tupleID(id, ref.SLALabel, ref.SLAValue, sink)
}

func (o *DurableOccurrence) dueAt() time.Time {
	if o.Claim != nil {
		return o.Claim.Until
	}
	var due time.Time
	for _, b := range o.Bindings {
		if b.HoldReason == "" && !b.NextEvaluationAt.IsZero() && (due.IsZero() || b.NextEvaluationAt.Before(due)) {
			due = b.NextEvaluationAt
		}
	}
	return due
}
func (o *DurableOccurrence) retentionAt() time.Time {
	if o.CompletedAt == nil || o.outstandingDelivery() {
		return time.Time{}
	}
	if !o.Compacted {
		return *o.CompactAt
	}
	return *o.DeleteAt
}
