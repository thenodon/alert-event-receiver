package processor

import "github.com/andersh/alertmanager_event_receiver/internal/models"

type Outcome string

const (
	Indeterminate       Outcome = "indeterminate"
	InvalidNotification Outcome = "invalid_notification"
	SupersededFiring    Outcome = "superseded_firing"
	StaleResolved       Outcome = "stale_resolved"
	Opened              Outcome = "opened"
	RepeatedFiring      Outcome = "repeated_firing"
	Closed              Outcome = "closed"
	RepeatedResolved    Outcome = "repeated_resolved"
	Reopened            Outcome = "reopened"
	StaleFiring         Outcome = "stale_firing"
	ResolvedWithoutOpen Outcome = "resolved_without_open_state"
)

type EventEmissionStatus string

const (
	EmissionNotRequired EventEmissionStatus = "not_required"
	EmissionSuppressed  EventEmissionStatus = "suppressed"
	EmissionAttempted   EventEmissionStatus = "attempted"
	EmissionAccepted    EventEmissionStatus = "accepted"
	EmissionFailed      EventEmissionStatus = "failed"
)

// ProcessingResult describes lifecycle detection independently of delivery.
// TransitionOccurred means a lifecycle transition was detected, even if emission
// was suppressed or failed. Indeterminate and InvalidNotification carry no
// authoritative lifecycle decision; callers must inspect Outcome and error.
// Event is nil when there was no transition.
// Accepted means only that EventEmitter.Emit returned nil, not durable delivery.
// Attempted is the intermediate status while calling the emitter.
type ProcessingResult struct {
	Outcome             Outcome
	TransitionOccurred  bool
	Event               *models.LifecycleEvent
	EventEmissionStatus EventEmissionStatus
}
