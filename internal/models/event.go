package models

import "time"

// Transition values.
const (
	TransitionFiring   = "firing"
	TransitionResolved = "resolved"
)

// StateWriteResult values recorded on each emitted event.
const (
	StateOpened              = "opened"                    // new firing cycle
	StateReopened            = "reopened"                  // firing after a closed cycle
	StateClosed              = "closed"                    // normal resolved with matching open state
	StateResolvedWithoutOpen = "resolved_without_open_state" // orphan resolved
	StateAlreadyFiring       = "already_firing"            // suppressed duplicate firing
)

// LifecycleEvent is the normalised alert transition event emitted as an OTel log record.
type LifecycleEvent struct {
	EventKind    string `json:"event_kind"`
	EventVersion int    `json:"event_version"`

	Transition     string `json:"transition"`
	Status         string `json:"status"`
	Fingerprint    string `json:"fingerprint"`      // = alert.Fingerprint from webhook
	FingerprintSrc string `json:"fingerprint_source"` // sorted label names used for the fingerprint

	Alertname   string            `json:"alertname"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`

	StartsAt        time.Time `json:"startsAt"`
	EndsAt          time.Time `json:"endsAt,omitempty"`
	DurationSeconds float64   `json:"duration_seconds"`

	GeneratorURL  string `json:"generatorURL,omitempty"`
	AMGroupKey    string `json:"am_group_key,omitempty"`
	AMReceiver    string `json:"am_receiver,omitempty"`
	AMExternalURL string `json:"am_external_url,omitempty"`

	StateWriteResult string `json:"state_write_result"`
	OrphanReason     string `json:"orphan_reason,omitempty"`
}

