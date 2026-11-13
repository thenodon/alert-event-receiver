package slaproxy

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Sink performs one delivery operation. Persistence, retries and ordering belong
// to the dispatcher. Implementations must honor context cancellation and keep
// the stable IntentID for downstream idempotency.
type Sink interface {
	Deliver(context.Context, DeliveryEvent) error
}

type DeliveryEvent struct {
	IntentID      string
	OccurrenceID  string
	BindingID     string
	SinkID        string
	Action        string
	Policy        PolicyRef
	PolicyVersion string
	Notification  Notification
	StartsAt      time.Time
	EndsAt        *time.Time
	EligibleAt    time.Time
	Attempt       uint64
}

// DeliveryError classifies a sink failure. The dispatcher never logs its raw
// message/cause, because a transport or handler error may contain secrets.
// Ordinary errors are retryable; permanent errors require operator intervention.
type DeliveryError struct {
	Permanent bool
	Cause     error
}

func (e *DeliveryError) Error() string { return "sink delivery failed" }
func (e *DeliveryError) Unwrap() error { return e.Cause }

type DeliveryReceipt struct {
	IntentID     string         `json:"intent_id"`
	SinkID       string         `json:"sink_id"`
	CommittedAt  time.Time      `json:"committed_at"`
	DeliveredAt  time.Time      `json:"delivered_at"`
	Attempts     uint64         `json:"attempts"`
	Fence        uint64         `json:"fence"`
	StateVersion uint64         `json:"state_version"`
	Status       DeliveryStatus `json:"status"`
}

type DeliveryClaim struct {
	Token     string    `json:"token"`
	Fence     uint64    `json:"fence"`
	BindingID string    `json:"binding_id"`
	IntentID  string    `json:"intent_id"`
	Action    string    `json:"action"`
	Until     time.Time `json:"until"`
	Sending   bool      `json:"sending"`
}

// LogSink calls Handler.Handle directly so reported write errors reach delivery
// retry handling. slog.Logger.Info discards such errors. Success does not imply
// fsync, downstream collection or durability of a custom asynchronous handler.
type LogSink struct {
	Handler slog.Handler
	Now     func() time.Time // Optional clock for deterministic tests; defaults to time.Now.
}

func (s LogSink) Deliver(ctx context.Context, e DeliveryEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Handler == nil || !s.Handler.Enabled(ctx, slog.LevelInfo) {
		return errors.New("delivery log handler unavailable or INFO disabled")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	deliveredAt := now().UTC()
	r := slog.NewRecord(deliveredAt, slog.LevelInfo, "SLA alert delivered", 0)
	r.AddAttrs(slog.String("component", "sla_sink"), slog.String("sink_id", e.SinkID), slog.String("sink_type", "log"),
		slog.String("action", e.Action), slog.String("occurrence_id", e.OccurrenceID), slog.String("binding_id", e.BindingID), slog.String("intent_id", e.IntentID),
		slog.String("sla_label", e.Policy.SLALabel), slog.String("sla_value", e.Policy.SLAValue), slog.String("policy_version", e.PolicyVersion),
		slog.Time("delivery_at", deliveredAt), slog.Time("starts_at", e.StartsAt.UTC()), slog.Time("eligible_at", e.EligibleAt.UTC()), slog.Uint64("delivery_attempt", e.Attempt))
	if e.EndsAt != nil {
		r.AddAttrs(slog.Time("ends_at", e.EndsAt.UTC()))
	}
	return s.Handler.Handle(ctx, r)
}
