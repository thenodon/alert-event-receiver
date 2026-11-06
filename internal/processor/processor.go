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

// Process handles every notification before emission deduplication. A result with
// Outcome Indeterminate is not an authoritative lifecycle decision.
func (p *Processor) Process(ctx context.Context, alert models.Alert, groupKey, receiver, externalURL string) (ProcessingResult, error) {
	if err := validate(alert); err != nil {
		return ProcessingResult{Outcome: InvalidNotification, EventEmissionStatus: EmissionNotRequired}, err
	}
	current, err := p.store.GetState(ctx, alert.Fingerprint)
	if err != nil {
		return p.stateFailure(ctx, "get_state", err)
	}
	if current != nil && (current.StartsAt.IsZero() || (current.Status != "firing" && current.Status != "closed")) {
		return p.stateFailure(ctx, "get_state", fmt.Errorf("invalid stored occurrence"))
	}
	if current != nil && alert.StartsAt.Before(current.StartsAt) {
		outcome := StaleFiring
		if alert.Status == models.TransitionResolved {
			outcome = StaleResolved
		}
		return noTransition(outcome), nil
	}
	same := current != nil && alert.StartsAt.Equal(current.StartsAt)
	if same && current.Status == "closed" {
		if alert.Status == models.TransitionResolved {
			return noTransition(RepeatedResolved), nil
		}
		return noTransition(StaleFiring), nil
	}
	if same && alert.Status == models.TransitionFiring {
		if err := p.store.UpdateLastSeen(ctx, alert.Fingerprint, time.Now()); err != nil {
			return p.stateFailure(ctx, "update_last_seen", err)
		}
		telemetry.StateWritesTotal.WithLabelValues(models.StateAlreadyFiring).Inc()
		return noTransition(RepeatedFiring), nil
	}
	now := time.Now()
	next := state.AlertState{Status: alert.Status, StartsAt: alert.StartsAt,
		FirstFiringAt: alert.StartsAt, LastSeenAt: now,
		Alertname: alert.Labels["alertname"], Labels: alert.Labels}
	event := models.LifecycleEvent{
		EventKind: "alert_transition", EventVersion: 1,
		Transition: alert.Status, Status: alert.Status, Fingerprint: alert.Fingerprint,
		FingerprintSrc: state.FingerprintSource(alert.Labels), Alertname: next.Alertname,
		Labels: alert.Labels, Annotations: alert.Annotations, StartsAt: alert.StartsAt,
		GeneratorURL: alert.GeneratorURL, AMGroupKey: groupKey,
		AMReceiver: receiver, AMExternalURL: externalURL,
	}
	outcome := Opened
	if alert.Status == models.TransitionFiring {
		event.StateWriteResult = models.StateOpened
		if current != nil {
			if current.Status == "closed" {
				outcome = Reopened
				event.StateWriteResult = models.StateReopened
			} else {
				outcome = SupersededFiring
			}
		}
		if err := p.store.SetFiring(ctx, alert.Fingerprint, next); err != nil {
			return p.stateFailure(ctx, "set_firing", err)
		}
	} else {
		outcome = ResolvedWithoutOpen
		event.StateWriteResult = models.StateResolvedWithoutOpen
		event.OrphanReason = "open_state_missing"
		if same {
			outcome = Closed
			event.StateWriteResult = models.StateClosed
			event.OrphanReason = ""
			next.FirstFiringAt = current.FirstFiringAt
			next.LastSeenAt = current.LastSeenAt
			if len(current.Labels) > 0 {
				next.Labels = current.Labels
				next.Alertname = current.Alertname
			}
		}
		next.Status = "closed"
		next.EndsAt = alert.EndsAt
		event.EndsAt = alert.EndsAt
		event.DurationSeconds = alert.EndsAt.Sub(alert.StartsAt).Seconds()
		event.Labels = next.Labels
		event.Alertname = next.Alertname
		if err := p.store.SetClosed(ctx, alert.Fingerprint, next, p.closedStateTTL); err != nil {
			return p.stateFailure(ctx, "set_closed", err)
		}
		if !same {
			telemetry.ResolvedOrphansTotal.Inc()
		}
	}
	telemetry.StateWritesTotal.WithLabelValues(event.StateWriteResult).Inc()
	return p.emit(ctx, alert, event, outcome)
}

func validate(alert models.Alert) error {
	if alert.Status != models.TransitionFiring && alert.Status != models.TransitionResolved {
		return fmt.Errorf("invalid alert status %q", alert.Status)
	}
	if alert.Fingerprint == "" {
		return fmt.Errorf("fingerprint is required")
	}
	if alert.StartsAt.IsZero() {
		return fmt.Errorf("startsAt is required")
	}
	if _, err := alert.StartsAt.MarshalText(); err != nil {
		return fmt.Errorf("invalid startsAt: %w", err)
	}
	if alert.Status == models.TransitionResolved {
		if alert.EndsAt.IsZero() {
			return fmt.Errorf("endsAt is required for resolved alerts")
		}
		if _, err := alert.EndsAt.MarshalText(); err != nil {
			return fmt.Errorf("invalid endsAt: %w", err)
		}
		if alert.EndsAt.Before(alert.StartsAt) {
			return fmt.Errorf("endsAt must be >= startsAt")
		}
	}
	return nil
}

func noTransition(outcome Outcome) ProcessingResult {
	return ProcessingResult{Outcome: outcome, EventEmissionStatus: EmissionNotRequired}
}

func (p *Processor) stateFailure(ctx context.Context, operation string, err error) (ProcessingResult, error) {
	p.logger.ErrorContext(ctx, "required state operation failed", slog.String("operation", operation), slog.Any("error", err))
	return noTransition(Indeterminate), fmt.Errorf("%s: %w", operation, err)
}

// emit applies delivery deduplication only after lifecycle processing has completed.
func (p *Processor) emit(ctx context.Context, alert models.Alert, event models.LifecycleEvent, outcome Outcome) (ProcessingResult, error) {
	result := ProcessingResult{Outcome: outcome, TransitionOccurred: true, Event: &event, EventEmissionStatus: EmissionAttempted}
	idempKey := state.IdempotencyKey(alert.Fingerprint, alert.Status, alert.StartsAt)
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
