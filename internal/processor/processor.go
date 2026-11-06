package processor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
	"github.com/andersh/alertmanager_event_receiver/internal/state"
	"github.com/andersh/alertmanager_event_receiver/internal/telemetry"
)

// EventEmitter emits a lifecycle event to the log provider.
type EventEmitter interface {
	Emit(ctx context.Context, event models.LifecycleEvent) error
}

// Processor handles alert state transitions for individual alert instances.
type Processor struct {
	store          state.Store
	emitter        EventEmitter
	closedStateTTL time.Duration
	idempTTL       time.Duration
	logger         *slog.Logger
}

// New creates a Processor with the given dependencies.
func New(store state.Store, emitter EventEmitter, closedTTL, idempTTL time.Duration, logger *slog.Logger) *Processor {
	return &Processor{
		store:          store,
		emitter:        emitter,
		closedStateTTL: closedTTL,
		idempTTL:       idempTTL,
		logger:         logger,
	}
}

// Process handles a single alert from an Alertmanager webhook payload.
// groupKey, receiver and externalURL come from the outer webhook payload.
func (p *Processor) Process(ctx context.Context, alert models.Alert, groupKey, receiver, externalURL string) (ProcessingResult, error) {
	fingerprintSrc := state.FingerprintSource(alert.Labels)

	if alert.Status == models.TransitionFiring {
		return p.onFiring(ctx, alert, fingerprintSrc, groupKey, receiver, externalURL)
	}
	return p.onResolved(ctx, alert, fingerprintSrc, groupKey, receiver, externalURL)
}

func (p *Processor) onFiring(
	ctx context.Context,
	alert models.Alert,
	fingerprintSrc, groupKey, receiver, externalURL string,
) (ProcessingResult, error) {
	current, err := p.store.GetState(ctx, alert.Fingerprint)
	if err != nil {
		p.logger.ErrorContext(ctx, "state read failed",
			slog.String("fingerprint", alert.Fingerprint), slog.Any("error", err))
		telemetry.RedisErrors.WithLabelValues("get_state").Inc()
		// Continue: absence of state is treated as "none" — safe to open.
	}

	// Transition: open ──firing──> open (no new event, just refresh last_seen_at)
	if current != nil && current.Status == "firing" {
		if err := p.store.UpdateLastSeen(ctx, alert.Fingerprint, time.Now()); err != nil {
			p.logger.WarnContext(ctx, "failed to update last_seen_at",
				slog.String("fingerprint", alert.Fingerprint), slog.Any("error", err))
			telemetry.RedisErrors.WithLabelValues("update_last_seen").Inc()
		}
		telemetry.StateWritesTotal.WithLabelValues(models.StateAlreadyFiring).Inc()
		return ProcessingResult{Outcome: RepeatedFiring, EventEmissionStatus: EmissionNotRequired}, nil
	}

	// Transitions:
	//   none    ──firing──> open  (stateResult = opened)
	//   closed  ──firing──> open  (stateResult = reopened)
	// A closed occurrence can only reopen with a known, strictly newer startsAt.
	if current != nil && current.Status == "closed" &&
		(alert.StartsAt.IsZero() || current.StartsAt.IsZero() || !alert.StartsAt.After(current.StartsAt)) {
		return ProcessingResult{Outcome: StaleFiring, EventEmissionStatus: EmissionNotRequired}, nil
	}

	stateResult := models.StateOpened
	if current != nil && current.Status == "closed" {
		stateResult = models.StateReopened
	}
	telemetry.StateWritesTotal.WithLabelValues(stateResult).Inc()

	firstFiring := alert.StartsAt
	if firstFiring.IsZero() {
		firstFiring = time.Now()
	}
	now := time.Now()

	if err := p.store.SetFiring(ctx, alert.Fingerprint, state.AlertState{
		Status:        "firing",
		FirstFiringAt: firstFiring,
		LastSeenAt:    now,
		StartsAt:      alert.StartsAt,
		Alertname:     alert.Labels["alertname"],
		Labels:        alert.Labels,
	}); err != nil {
		p.logger.ErrorContext(ctx, "state write failed",
			slog.String("fingerprint", alert.Fingerprint), slog.Any("error", err))
		telemetry.RedisErrors.WithLabelValues("set_firing").Inc()
		// Continue to emit the event even if state write failed.
	}

	event := models.LifecycleEvent{
		EventKind:        "alert_transition",
		EventVersion:     1,
		Transition:       models.TransitionFiring,
		Status:           models.TransitionFiring,
		Fingerprint:      alert.Fingerprint,
		FingerprintSrc:   fingerprintSrc,
		Alertname:        alert.Labels["alertname"],
		Labels:           alert.Labels,
		Annotations:      alert.Annotations,
		StartsAt:         alert.StartsAt,
		DurationSeconds:  0,
		GeneratorURL:     alert.GeneratorURL,
		AMGroupKey:       groupKey,
		AMReceiver:       receiver,
		AMExternalURL:    externalURL,
		StateWriteResult: stateResult,
	}

	outcome := Opened
	if stateResult == models.StateReopened {
		outcome = Reopened
	}
	return p.emit(ctx, alert, event, outcome)
}

func (p *Processor) onResolved(
	ctx context.Context,
	alert models.Alert,
	fingerprintSrc, groupKey, receiver, externalURL string,
) (ProcessingResult, error) {
	current, err := p.store.GetState(ctx, alert.Fingerprint)
	if err != nil {
		p.logger.ErrorContext(ctx, "state read failed",
			slog.String("fingerprint", alert.Fingerprint), slog.Any("error", err))
		telemetry.RedisErrors.WithLabelValues("get_state").Inc()
	}

	if current != nil && current.Status == "closed" {
		return ProcessingResult{Outcome: RepeatedResolved, EventEmissionStatus: EmissionNotRequired}, nil
	}

	// Determine start time: prefer stored first_firing_at, fall back to payload.
	var startTime time.Time
	if current != nil && !current.FirstFiringAt.IsZero() {
		startTime = current.FirstFiringAt
	} else if !alert.StartsAt.IsZero() {
		startTime = alert.StartsAt
	} else {
		startTime = time.Now()
	}

	endTime := alert.EndsAt
	if endTime.IsZero() {
		endTime = time.Now()
	}

	duration := endTime.Sub(startTime).Seconds()
	if duration < 0 {
		duration = 0
	}

	// Determine state result and orphan status.
	stateResult := models.StateClosed
	orphanReason := ""
	if current == nil {
		stateResult = models.StateResolvedWithoutOpen
		orphanReason = "open_state_missing"
		telemetry.ResolvedOrphansTotal.Inc()
		p.logger.WarnContext(ctx, "resolved event received without matching open state",
			slog.String("fingerprint", alert.Fingerprint),
			slog.String("alertname", alert.Labels["alertname"]),
		)
	}
	telemetry.StateWritesTotal.WithLabelValues(stateResult).Inc()

	// Preserve payload occurrence timing on an orphan tombstone using the existing store API.
	if current == nil {
		if err := p.store.SetFiring(ctx, alert.Fingerprint, state.AlertState{
			Status: "firing", FirstFiringAt: startTime, LastSeenAt: time.Now(),
			StartsAt: alert.StartsAt, Alertname: alert.Labels["alertname"], Labels: alert.Labels,
		}); err != nil {
			p.logger.ErrorContext(ctx, "orphan occurrence state write failed", slog.Any("error", err))
			telemetry.RedisErrors.WithLabelValues("set_firing").Inc()
		}
	}

	// Write closed tombstone regardless — prevents duplicate orphan events too.
	if err := p.store.SetClosed(ctx, alert.Fingerprint, p.closedStateTTL); err != nil {
		p.logger.ErrorContext(ctx, "state close failed",
			slog.String("fingerprint", alert.Fingerprint), slog.Any("error", err))
		telemetry.RedisErrors.WithLabelValues("set_closed").Inc()
	}

	// Prefer labels from stored state (more reliable for long-running alerts).
	labels := alert.Labels
	if current != nil && len(current.Labels) > 0 {
		labels = current.Labels
	}

	event := models.LifecycleEvent{
		EventKind:        "alert_transition",
		EventVersion:     1,
		Transition:       models.TransitionResolved,
		Status:           models.TransitionResolved,
		Fingerprint:      alert.Fingerprint,
		FingerprintSrc:   fingerprintSrc,
		Alertname:        labels["alertname"],
		Labels:           labels,
		Annotations:      alert.Annotations,
		StartsAt:         startTime,
		EndsAt:           endTime,
		DurationSeconds:  duration,
		GeneratorURL:     alert.GeneratorURL,
		AMGroupKey:       groupKey,
		AMReceiver:       receiver,
		AMExternalURL:    externalURL,
		StateWriteResult: stateResult,
		OrphanReason:     orphanReason,
	}

	outcome := Closed
	if current == nil {
		outcome = ResolvedWithoutOpen
	}
	return p.emit(ctx, alert, event, outcome)
}

// emit applies delivery deduplication only after lifecycle processing has completed.
func (p *Processor) emit(ctx context.Context, alert models.Alert, event models.LifecycleEvent, outcome Outcome) (ProcessingResult, error) {
	result := ProcessingResult{Outcome: outcome, TransitionOccurred: true, Event: &event, EventEmissionStatus: EmissionAttempted}
	// Build the idempotency timestamp.
	// For firing: use startsAt (stable across repeated AM notifications for the same cycle).
	// For resolved: use endsAt if set, otherwise fall back to now.
	var idempTime time.Time
	switch alert.Status {
	case models.TransitionFiring:
		idempTime = alert.StartsAt
		if idempTime.IsZero() {
			idempTime = time.Now()
		}
	default:
		idempTime = alert.EndsAt
		if idempTime.IsZero() {
			idempTime = time.Now()
		}
	}

	idempKey := state.IdempotencyKey(alert.Fingerprint, alert.Status, idempTime)
	isNew, err := p.store.CheckAndSetIdempotency(ctx, idempKey, p.idempTTL)
	if err != nil {
		// Log and continue: on idempotency failure we prefer emitting over silently dropping.
		p.logger.ErrorContext(ctx, "idempotency check failed — processing anyway",
			slog.String("fingerprint", alert.Fingerprint),
			slog.String("transition", alert.Status),
			slog.Any("error", err),
		)
		telemetry.RedisErrors.WithLabelValues("idempotency").Inc()
	} else if !isNew {
		p.logger.InfoContext(ctx, "lifecycle event emission suppressed",
			slog.String("fingerprint", alert.Fingerprint),
			slog.String("transition", alert.Status),
		)
		telemetry.DuplicatesDropped.Inc()
		result.EventEmissionStatus = EmissionSuppressed
		return result, nil
	}
	if err == nil && isNew {
		telemetry.IdempotencyKeysCreatedTotal.WithLabelValues(alert.Status).Inc()
	}

	if err := p.emitter.Emit(ctx, event); err != nil {
		result.EventEmissionStatus = EmissionFailed
		telemetry.EmitErrors.WithLabelValues(event.Transition).Inc()
		return result, fmt.Errorf("emit %s event: %w", event.Transition, err)
	}
	result.EventEmissionStatus = EmissionAccepted
	telemetry.EventsEmitted.WithLabelValues(event.Transition).Inc()
	return result, nil
}
