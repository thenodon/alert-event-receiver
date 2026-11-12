package slaproxy

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

func (m *DurableManager) retryDelay(attempt int) time.Duration {
	delay := m.cfg.Durable.Scheduler.RetryInitial
	for i := 0; i < attempt && delay < m.cfg.Durable.Scheduler.RetryMax; i++ {
		if delay > m.cfg.Durable.Scheduler.RetryMax/2 {
			delay = m.cfg.Durable.Scheduler.RetryMax
			break
		}
		delay *= 2
	}
	delay = time.Duration(float64(delay) * (1 + rand.Float64()*0.25))
	if delay > m.cfg.Durable.Scheduler.RetryMax {
		delay = m.cfg.Durable.Scheduler.RetryMax
	}
	return delay
}

func (m *DurableManager) runOne(ctx context.Context, id string) error {
	opCtx, cancel := context.WithTimeout(ctx, m.cfg.Durable.Scheduler.OperationTimeout)
	defer cancel()
	claimed, err := m.claim(opCtx, id)
	result := "claimed"
	if errors.Is(err, ErrClaimBusy) {
		result = "busy"
	} else if err != nil {
		result = "error"
	}
	m.store.metrics.claims.WithLabelValues(result).Inc()
	if errors.Is(err, ErrClaimBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, b := range claimed.Bindings {
		if !b.NextEvaluationAt.IsZero() {
			m.store.metrics.schedulerLag.Observe(max(0, claimed.UpdatedAt.Sub(b.NextEvaluationAt).Seconds()))
		}
	}
	completed, err := m.complete(opCtx, claimed)
	if err == nil {
		m.store.metrics.schedulerEvaluations.WithLabelValues("committed").Inc()
		m.logTransition(claimed, completed, "scheduler")
		return nil
	}
	if errors.Is(err, ErrFenced) {
		m.store.metrics.schedulerEvaluations.WithLabelValues("fenced").Inc()
		return nil
	}
	// Best effort durable backoff. If the store is unavailable the lease key
	// expires and the due index makes the occurrence reclaimable after restart.
	retryCtx, stop := context.WithTimeout(ctx, m.cfg.Durable.Scheduler.OperationTimeout)
	defer stop()
	_, _ = m.store.updateClaim(retryCtx, id, claimed.Claim, func(row *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
		row.Claim = nil
		for _, b := range row.Bindings {
			if row.Lifecycle == LifecycleFiring && b.HoldReason == "" {
				b.NextEvaluationAt = now.Add(m.retryDelay(b.RetryCount))
				b.RetryCount++
			}
		}
		return row, nil
	})
	return err
}

// maintenance is cursor-based and handles at most BatchSize occurrence records
// per tick, plus a bounded retention batch. SCAN COUNT is a server hint; an
// oversized returned page is buffered and processed on subsequent ticks.
func (m *DurableManager) maintenance(ctx context.Context, pending *[]string) error {
	ids, err := m.store.due(ctx, "retention", m.cfg.Durable.Scheduler.BatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_, err := m.store.update(ctx, id, func(row *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
			if row == nil {
				return nil, nil
			}
			if row.DeleteAt != nil && !row.DeleteAt.After(now) {
				return nil, nil
			}
			if row.CompactAt != nil && !row.CompactAt.After(now) {
				row.Original, row.LastFiring, row.Resolution, row.Compacted = nil, nil, nil, true
			}
			return row, nil
		})
		if err != nil {
			return err
		}
	}
	if len(*pending) == 0 {
		keys, cursor, err := m.store.client.Scan(ctx, m.scanCursor, m.store.key("occ:*"), int64(m.cfg.Durable.Scheduler.BatchSize)).Result()
		if err != nil {
			return err
		}
		m.scanCursor, *pending = cursor, keys
	}
	count := min(len(*pending), m.cfg.Durable.Scheduler.BatchSize)
	for _, key := range (*pending)[:count] {
		id := strings.TrimPrefix(key, m.store.key("occ:"))
		before, err := m.store.load(ctx, id)
		if err != nil {
			return err
		}
		after, err := m.store.update(ctx, id, func(row *DurableOccurrence, now time.Time) (*DurableOccurrence, error) {
			if row == nil {
				return nil, nil
			}
			if row.Claim != nil && row.Claim.Until.After(now) {
				return nil, ErrClaimBusy
			}
			row.Claim = nil
			m.syncPolicies(row, now)
			return row, nil
		})
		if err != nil && !errors.Is(err, ErrClaimBusy) {
			return err
		}
		if err == nil {
			m.logTransition(before, after, "reconcile")
		}
	}
	*pending = (*pending)[count:]
	// Index entries whose records were removed by a completed-retention commit
	// are removed in that same transaction. Missing record entries are harmless
	// and cleaned when discovered by due processing or maintenance below.
	for _, index := range []string{"due", "retention"} {
		ids, err := m.store.due(ctx, index, m.cfg.Durable.Scheduler.BatchSize)
		if err != nil {
			return err
		}
		for _, id := range ids {
			row, err := m.store.load(ctx, id)
			if err != nil {
				return err
			}
			if row == nil {
				if _, err := m.store.update(ctx, id, func(*DurableOccurrence, time.Time) (*DurableOccurrence, error) { return nil, nil }); err != nil {
					return err
				}
			}
		}
	}
	if err := m.reconcileIndex(ctx); err != nil {
		return err
	}
	for set, update := range map[string]func(float64){"unresolved": m.store.metrics.unresolvedOccurrences.Set, "pending": m.store.metrics.pendingOccurrences.Set, "held": m.store.metrics.heldOccurrences.Set} {
		count, err := m.store.client.SCard(ctx, m.store.key(set)).Result()
		if err != nil {
			return err
		}
		update(float64(count))
	}
	return nil
}

// reconcileIndex also removes orphan memberships that have no occurrence row,
// including future scores and ready intents with no scheduling deadline. Rotate
// indexes and buffer oversized SCAN pages to bound work per maintenance tick.
func (m *DurableManager) reconcileIndex(ctx context.Context) error {
	indexes := []string{"unresolved", "pending", "held", "intent-index", "due", "retention"}
	index := indexes[m.indexPosition]
	if len(m.indexPending) == 0 {
		var page []string
		var cursor uint64
		var err error
		if m.indexPosition < 4 {
			page, cursor, err = m.store.client.SScan(ctx, m.store.key(index), m.indexCursor, "*", int64(m.cfg.Durable.Scheduler.BatchSize)).Result()
		} else {
			var scored []string
			scored, cursor, err = m.store.client.ZScan(ctx, m.store.key(index), m.indexCursor, "*", int64(m.cfg.Durable.Scheduler.BatchSize)).Result()
			for i := 0; i < len(scored); i += 2 {
				page = append(page, scored[i])
			}
		}
		if err != nil {
			return err
		}
		m.indexCursor, m.indexPending = cursor, page
	}
	count := min(len(m.indexPending), m.cfg.Durable.Scheduler.BatchSize)
	for _, id := range m.indexPending[:count] {
		// WATCH handles a concurrent creation after the scan. Never delete a live row
		// or disturb an active claim merely to remove an orphan index membership.
		_, err := m.store.update(ctx, id, func(row *DurableOccurrence, _ time.Time) (*DurableOccurrence, error) {
			if row != nil {
				return nil, ErrClaimBusy
			}
			return nil, nil
		})
		if err != nil && !errors.Is(err, ErrClaimBusy) {
			return err
		}
	}
	m.indexPending = m.indexPending[count:]
	if len(m.indexPending) == 0 && m.indexCursor == 0 {
		m.indexPosition = (m.indexPosition + 1) % len(indexes)
	}
	return nil
}

func (m *DurableManager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string, m.cfg.Durable.Scheduler.BatchSize)
	fatal := make(chan error, 1)
	var workers sync.WaitGroup
	defer workers.Wait()
	// Cancel must precede Wait even when returning on a policy-epoch error.
	defer cancel()
	for i := 0; i < m.cfg.Durable.Scheduler.Workers; i++ {
		workers.Go(func() {
			failures := 0
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-jobs:
					if err := m.runOne(ctx, id); err != nil {
						m.store.metrics.schedulerErrors.WithLabelValues("evaluate").Inc()
						m.logger.Error("SLA scheduler evaluation failed", "component", "sla_scheduler", "operation", "evaluate", "occurrence_id", id, "error", err.Error())
						if errors.Is(err, ErrPolicyEpoch) {
							select {
							case fatal <- err:
							default:
							}
							return
						}
						delay := m.retryDelay(failures)
						failures++
						select {
						case <-ctx.Done():
							return
						case <-time.After(delay):
						}
					} else {
						failures = 0
					}
				}
			}
		})
	}
	poll := time.NewTicker(m.cfg.Durable.Scheduler.PollInterval)
	maintain := time.NewTicker(m.cfg.Durable.Scheduler.ReconcileInterval)
	defer poll.Stop()
	defer maintain.Stop()
	var pending []string
	var nextPoll time.Time
	pollFailures := 0
	maintenance := func() error {
		c, stop := context.WithTimeout(ctx, m.cfg.Durable.Scheduler.OperationTimeout)
		defer stop()
		return m.maintenance(c, &pending)
	}
	if err := maintenance(); err != nil {
		m.store.metrics.schedulerErrors.WithLabelValues("reconcile").Inc()
		if errors.Is(err, ErrPolicyEpoch) {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fatal:
			return err
		case <-maintain.C:
			if err := maintenance(); err != nil {
				m.store.metrics.schedulerErrors.WithLabelValues("reconcile").Inc()
				m.logger.Error("SLA state reconciliation failed", "component", "sla_scheduler", "operation", "reconcile", "error", err.Error())
				if errors.Is(err, ErrPolicyEpoch) {
					return err
				}
			}
		case <-poll.C:
			if time.Now().Before(nextPoll) {
				continue
			}
			c, stop := context.WithTimeout(ctx, m.cfg.Durable.Scheduler.OperationTimeout)
			ids, err := m.store.due(c, "due", m.cfg.Durable.Scheduler.BatchSize)
			stop()
			if err != nil {
				m.store.metrics.schedulerErrors.WithLabelValues("poll").Inc()
				nextPoll = time.Now().Add(m.retryDelay(pollFailures))
				pollFailures++
				m.logger.Error("SLA scheduler poll failed", "component", "sla_scheduler", "operation", "poll", "error", err.Error())
				continue
			}
			pollFailures, nextPoll = 0, time.Time{}
			for _, id := range ids {
				select {
				case jobs <- id:
				default:
				}
			}
		}
	}
}
