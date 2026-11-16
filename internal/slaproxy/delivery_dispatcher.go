package slaproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	mathrand "math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

type DeliveryDispatcher struct {
	manager *DurableManager
	sinks   map[string]Sink
	metrics *deliveryMetrics
}

func (d *DeliveryDispatcher) operation(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d.manager.cfg.Durable.Scheduler.OperationTimeout)
}
func (d *DeliveryDispatcher) claim(ctx context.Context, work string) (*DurableOccurrence, error) {
	id, bid, valid := parseDeliveryWork(work)
	if !valid {
		return nil, ErrClaimBusy
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	m := d.manager
	reclaimed := false
	sinkID := "unknown"
	row, err := m.store.claimBinding(ctx, id, bid, func(row *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		if row == nil || row.Bindings[bid] == nil {
			return nil, ErrClaimBusy
		}
		b := row.Bindings[bid]
		sinkID = b.SinkID
		if b.Claim != nil && b.Claim.Until.After(now) {
			return nil, ErrClaimBusy
		}
		reclaimed = b.Claim != nil
		b.Claim = nil
		m.syncPolicies(row, now)
		m.refreshDelivery(row)
		for _, action := range []string{"firing", "resolved"} {
			i := b.Intents[action]
			if !deliveryActionable(row, b, i) || i.NextAttemptAt.After(now) {
				continue
			}
			cfg, found := m.cfg.sinkConfig(b.SinkID)
			if !found || cfg.fingerprint() != b.DestinationFingerprint || d.sinks[b.SinkID] == nil {
				i.PermanentFailure = true
				i.LastFailure = "sink_identity_mismatch"
				m.refreshDelivery(row)
				return row, nil
			}
			if b.DeliveryFence == math.MaxUint64 {
				return nil, errors.New("delivery fence exhausted")
			}
			b.DeliveryFence++
			b.Claim = &DeliveryClaim{Token: hex.EncodeToString(token[:]), Fence: b.DeliveryFence, BindingID: bid, IntentID: i.ID, Action: action, Until: now.Add(m.cfg.Delivery.Dispatcher.ClaimLease)}
			return row, nil
		}
		return row, nil
	})
	result := "claimed"
	if errors.Is(err, ErrClaimBusy) {
		result = "busy"
	} else if err != nil {
		result = "error"
	} else if row == nil || row.Bindings[bid].Claim == nil {
		result = "inactive"
	} else if reclaimed {
		result = "reclaimed"
	}
	d.metrics.claims.WithLabelValues(sinkID, result).Inc()
	return row, err
}

// authorize is the send linearization point. Only a committed authorization may
// reach the sink. It is separate from claiming so resolution can cancel queued
// work while another worker is waiting to validate it.
func (d *DeliveryDispatcher) authorize(ctx context.Context, claimed *DurableOccurrence, bid string) (*DurableOccurrence, error) {
	m := d.manager
	c := claimed.Bindings[bid].Claim
	return m.store.updateDelivery(ctx, claimed.ID, c, func(row *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		m.syncPolicies(row, now)
		m.refreshDelivery(row)
		b := row.Bindings[c.BindingID]
		i := b.Intents[c.Action]
		if !deliveryActionable(row, b, i) {
			b.Claim = nil
			return row, nil
		}
		if b.Claim.Until.Sub(now) < m.cfg.Delivery.Dispatcher.DeliveryTimeout+2*m.cfg.Durable.Scheduler.OperationTimeout {
			b.Claim = nil
			i.NextAttemptAt = now.Add(m.cfg.Delivery.Dispatcher.RetryInitial)
			return row, nil
		}
		if i.Attempts == math.MaxUint64 {
			return nil, errors.New("delivery attempts exhausted")
		}
		if i.Payload == nil {
			if c.Action == "resolved" {
				i.Payload = row.Resolution
			} else {
				i.Payload = row.LastFiring
				if i.Payload == nil && row.Original != nil && row.Original.Alert.Status == "firing" {
					i.Payload = row.Original
				}
			}
		}
		if i.Payload == nil {
			i.PermanentFailure = true
			i.LastFailure = "payload_unavailable"
			b.Claim = nil
			m.refreshDelivery(row)
			return row, nil
		}
		i.Attempts++
		if i.AuthorizedAt == nil {
			i.AuthorizedAt = utcPointer(now)
		}
		b.Claim.Sending = true
		return row, nil
	})
}
func deliveryEvent(row *DurableOccurrence, bid string) DeliveryEvent {
	c := row.Bindings[bid].Claim
	b := row.Bindings[c.BindingID]
	i := b.Intents[c.Action]
	return DeliveryEvent{IntentID: i.ID, OccurrenceID: row.ID, BindingID: b.ID, SinkID: b.SinkID, Action: i.Action, Policy: b.Policy, PolicyVersion: b.PolicyVersion,
		Notification: *i.Payload, StartsAt: row.StartsAt, EndsAt: row.EndsAt, EligibleAt: b.Calendar.NextEvaluationAt, Attempt: i.Attempts}
}
func deliveryFailure(err error) string {
	var failure *DeliveryError
	if errors.As(err, &failure) && failure.Permanent {
		return "permanent_failure"
	}
	return "temporary_failure"
}
func (d *DeliveryDispatcher) retryDelay(attempt uint64) time.Duration {
	cfg := d.manager.cfg.Delivery.Dispatcher
	return deliveryRetryDelay(cfg.RetryInitial, cfg.RetryMax, attempt, mathrand.Float64())
}

func deliveryRetryDelay(initial, maximum time.Duration, attempt uint64, jitter float64) time.Duration {
	delay := initial
	for attempt > 1 && delay < maximum {
		if delay > maximum/2 {
			delay = maximum
			break
		}
		delay *= 2
		attempt--
	}
	lower, upper := delay, min(maximum, delay+delay/4)
	// Keep jitter at the cap too, instead of clipping every failed occurrence
	// to the same maximum deadline. Never go below configured retry_initial.
	if lower == upper {
		lower = max(initial, delay-delay/5)
	}
	return lower + time.Duration(float64(upper-lower)*jitter)
}

func (d *DeliveryDispatcher) finish(ctx context.Context, row *DurableOccurrence, e DeliveryEvent, sinkErr error, deliveredAt time.Time) error {
	m := d.manager
	c := row.Bindings[e.BindingID].Claim
	_, err := m.store.updateDelivery(ctx, row.ID, c, func(latest *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		if !latest.Bindings[c.BindingID].Claim.Sending {
			return nil, ErrFenced
		}
		b := latest.Bindings[c.BindingID]
		i := b.Intents[c.Action]
		b.Claim = nil
		if sinkErr == nil {
			i.Receipt = &DeliveryReceipt{IntentID: i.ID, SinkID: b.SinkID, DeliveredAt: deliveredAt, CommittedAt: now, Attempts: i.Attempts, Fence: c.Fence, StateVersion: latest.Version + 1, Status: Delivered}
			i.Ready = false
			i.Payload = nil
			i.Canceled = false
			i.LastFailure = ""
			i.NextAttemptAt = time.Time{}
			if c.Action == "firing" {
				b.FiringDeliveredAt = utcPointer(deliveredAt)
			}
		} else {
			i.LastFailure = deliveryFailure(sinkErr)
			i.PermanentFailure = i.LastFailure == "permanent_failure"
			delay := d.retryDelay(i.Attempts)
			var failure *DeliveryError
			if errors.As(sinkErr, &failure) {
				delay = max(delay, failure.RetryAfter)
			}
			i.NextAttemptAt = now.Add(delay)
		}
		// Read/merge the latest resolution, never the pre-send snapshot. This creates
		// the ordered resolution intent when firing succeeded during a resolve race.
		m.advance(latest, now)
		return latest, nil
	})
	result := "committed"
	if errors.Is(err, ErrFenced) {
		result = "fenced"
		d.metrics.claims.WithLabelValues(e.SinkID, "fenced").Inc()
	} else if err != nil {
		result = "error"
	}
	d.metrics.commits.WithLabelValues(e.SinkID, result).Inc()
	if err == nil && sinkErr == nil {
		d.metrics.receipts.WithLabelValues(e.SinkID, e.Action).Inc()
		if e.Action == "firing" {
			d.metrics.eligibilityDelay.WithLabelValues(e.SinkID).Observe(max(0, deliveredAt.Sub(e.EligibleAt).Seconds()))
		}
		m.logger.Info("SLA delivery receipt committed", "component", "sla_delivery", "operation", "receipt", "occurrence_id", e.OccurrenceID, "binding_id", e.BindingID, "intent_id", e.IntentID, "action", e.Action, "sink_id", e.SinkID, "delivery_attempt", e.Attempt)
	}
	return err
}
func (d *DeliveryDispatcher) runOne(ctx context.Context, id string) error {
	_, bid, valid := parseDeliveryWork(id)
	if !valid {
		return d.manager.store.cleanOrphan(ctx, "delivery-due", id)
	}
	op, stop := d.operation(ctx)
	claimed, err := d.claim(op, id)
	stop()
	if errors.Is(err, ErrClaimBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	if claimed == nil || claimed.Bindings[bid].Claim == nil {
		return nil
	}
	op, stop = d.operation(ctx)
	authorized, err := d.authorize(op, claimed, bid)
	stop()
	if errors.Is(err, ErrFenced) {
		d.metrics.claims.WithLabelValues(claimed.Bindings[bid].SinkID, "fenced").Inc()
		return nil
	}
	if err != nil {
		return err
	}
	if authorized == nil || authorized.Bindings[bid].Claim == nil {
		return nil
	}
	event := deliveryEvent(authorized, bid)
	if event.Attempt > 1 {
		d.metrics.retries.WithLabelValues(event.SinkID, event.Action).Inc()
	}
	// No Valkey transaction or pooled WATCH connection is held during sink I/O.
	if d.manager.store.quarantined.Load() {
		return ErrIndexSchema
	}
	op, stop = d.operation(ctx)
	budget, err := d.remainingLease(op, authorized.ID, authorized.Bindings[bid].Claim)
	stop()
	if err != nil {
		return err
	}
	call, stopCall := context.WithTimeout(ctx, budget)
	began := time.Now()
	sinkErr := d.sinks[event.SinkID].Deliver(call, event)
	deliveredAt := time.Now().UTC()
	stopCall()
	d.metrics.duration.WithLabelValues(event.SinkID, event.Action).Observe(time.Since(began).Seconds())
	result := "success"
	if sinkErr != nil {
		result = deliveryFailure(sinkErr)
	}
	d.metrics.attempts.WithLabelValues(event.SinkID, event.Action, result).Inc()
	if sinkErr != nil {
		d.manager.logger.Warn("SLA sink delivery failed", "component", "sla_delivery", "operation", "deliver", "intent_id", event.IntentID, "sink_id", event.SinkID, "action", event.Action, "sink_type", authorized.Bindings[bid].SinkType, "result", result, "delivery_attempt", event.Attempt)
	}
	// A completed sink operation may still commit during bounded shutdown.
	op, stop = d.operation(context.WithoutCancel(ctx))
	defer stop()
	err = d.finish(op, authorized, event, sinkErr, deliveredAt)
	if err != nil {
		d.manager.logger.Error("SLA delivery outcome commit not confirmed", "component", "sla_delivery", "operation", "receipt", "intent_id", event.IntentID, "sink_id", event.SinkID, "fenced", errors.Is(err, ErrFenced))
	}
	return err
}

func (d *DeliveryDispatcher) poll(ctx context.Context) (ids []string, err error) {
	defer func() { err = d.manager.store.readError(ctx, err) }()
	s := d.manager.store
	if err := s.health(ctx); err != nil {
		return nil, err
	}
	ids, err = s.due(ctx, "delivery-due", d.manager.cfg.Delivery.Dispatcher.BatchSize)
	if err != nil {
		return nil, err
	}
	now, err := s.client.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	count, err := s.client.ZCount(ctx, s.key("delivery-due"), "-inf", strconv.FormatInt(now.UnixMilli(), 10)).Result()
	if err != nil {
		return nil, err
	}
	d.metrics.ready.Set(float64(count))
	count, err = s.client.SCard(ctx, s.key("delivery-claimed")).Result()
	if err != nil {
		return nil, err
	}
	d.metrics.claimed.Set(float64(count))
	count, err = s.client.SCard(ctx, s.key("delivery-held")).Result()
	if err != nil {
		return nil, err
	}
	d.metrics.held.Set(float64(count))
	for _, sink := range d.manager.cfg.Delivery.Sinks {
		for _, entry := range []struct {
			prefix string
			gauge  *prometheus.GaugeVec
		}{{"delivery-pending:", d.metrics.pendingBySink}, {"delivery-inflight:", d.metrics.inflightBySink}, {"delivery-failed:", d.metrics.heldBySink}} {
			n, err := s.client.SCard(ctx, s.key(entry.prefix+sink.ID)).Result()
			if err != nil {
				return nil, err
			}
			entry.gauge.WithLabelValues(sink.ID).Set(float64(n))
		}
	}

	return ids, nil
}

func (d *DeliveryDispatcher) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg := d.manager.cfg.Delivery.Dispatcher
	jobs := make(chan string, cfg.BatchSize)
	fatal := make(chan error, 1)
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	for range cfg.Workers {
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-jobs:
					if err := d.runOne(ctx, id); err != nil {
						d.metrics.errors.WithLabelValues("dispatch").Inc()
						if errors.Is(err, ErrPolicyEpoch) || errors.Is(err, ErrIndexSchema) {
							select {
							case fatal <- err:
							default:
							}
							return
						}
						// Bound per-worker failures as well as persisted per-intent retry delays.
						select {
						case <-ctx.Done():
							return
						case <-time.After(cfg.RetryInitial):
						}
					}
				}
			}
		})
	}
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	nextPoll := time.Time{}
	failures := uint64(1)
	// Poll immediately at startup; recovery does not wait for an in-memory timer.
	ready := make(chan time.Time, 1)
	ready <- time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fatal:
			return err
		case <-ticker.C:
		case <-ready:
		}
		if time.Now().Before(nextPoll) {
			continue
		}
		op, stop := d.operation(ctx)
		ids, err := d.poll(op)
		stop()
		if err != nil {
			d.metrics.errors.WithLabelValues("poll").Inc()
			d.manager.logger.Error("SLA delivery polling failed", "component", "sla_delivery", "operation", "poll")
			if errors.Is(err, ErrPolicyEpoch) || errors.Is(err, ErrIndexSchema) {
				return err
			}
			nextPoll = time.Now().Add(d.retryDelay(failures))
			failures++
			continue
		}
		nextPoll = time.Time{}
		failures = 1
		for _, id := range ids {
			select {
			case jobs <- id:
			default:
			}
		}
	}
}

// Check both the token and server-clock lease immediately before external I/O.
// Subtract round-trip time and reserve one state-store operation for the receipt.
func (d *DeliveryDispatcher) remainingLease(ctx context.Context, id string, claim *DeliveryClaim) (time.Duration, error) {
	s := d.manager.store
	began := time.Now()
	var token *redis.StringCmd
	var ttl *redis.DurationCmd
	var now *redis.TimeCmd
	_, err := s.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		token = p.Get(ctx, s.deliveryClaimKey(id, claim.BindingID))
		ttl = p.PTTL(ctx, s.deliveryClaimKey(id, claim.BindingID))
		now = p.Time(ctx)
		return nil
	})
	if err != nil && err != redis.Nil {
		return 0, s.readError(ctx, err)
	}
	value, err := token.Result()
	if err != nil && err != redis.Nil {
		return 0, err
	}
	remaining, err := ttl.Result()
	if err != nil {
		return 0, err
	}
	serverNow, err := now.Result()
	if err != nil {
		return 0, err
	}
	remaining = min(remaining, claim.Until.Sub(serverNow)) - time.Since(began) - s.cfg.Scheduler.OperationTimeout
	if value != claim.Token || remaining <= 0 {
		return 0, ErrFenced
	}
	return min(remaining, d.manager.cfg.Delivery.Dispatcher.DeliveryTimeout), nil
}
