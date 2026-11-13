package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	ErrFenced      = errors.New("scheduler claim is expired or fenced")
	ErrPolicyEpoch = errors.New("policy registry changed; restart with the active policy epoch/configuration")
	ErrClaimBusy   = errors.New("occurrence is not due or is already claimed")
)

type policyRegistry struct {
	Epoch  uint64 `json:"epoch"`
	Digest string `json:"digest"`
}

// DurableStore uses only standard commands and WATCH/MULTI/EXEC. State, required
// schedule and intents live in one authoritative JSON document. Secondary indexes
// are transactionally maintained and can be rebuilt from those documents.
type DurableStore struct {
	client          *redis.Client
	cfg             DurableConfig
	prefix          string
	registry        policyRegistry
	metrics         *metrics
	deliveryEnabled bool
	sinkType        string
}

func newDurableStore(client *redis.Client, config Config, engine *CalendarEngine, metrics *metrics) *DurableStore {
	cfg := config.Durable
	versions := make(map[string]string)
	for ref, policy := range engine.policies {
		versions[tupleID(ref.SLALabel, ref.SLAValue)] = policy.version
	}
	order := make([]string, len(config.SLADefinitions))
	for i, definition := range config.SLADefinitions {
		order[i] = definition.SLALabel
	}
	encoded, _ := json.Marshal(struct {
		Versions       map[string]string
		SelectionOrder []string
	}{versions, order})
	digest := tupleID(string(encoded), cfg.SinkID)
	// Activating the delivery record format fences older Phase 3 processes. The
	// disabled digest stays identical to Phase 3 for existing deployments.
	if config.Delivery.Enabled {
		digest = tupleID(digest, "delivery-v1", config.Delivery.Sink.Type)
	}
	return &DurableStore{client: client, cfg: cfg, deliveryEnabled: config.Delivery.Enabled, sinkType: config.Delivery.Sink.Type, prefix: cfg.Valkey.Namespace + ":{sla}:", registry: policyRegistry{cfg.PolicyEpoch, digest}, metrics: metrics}
}

func (s *DurableStore) key(suffix string) string  { return s.prefix + suffix }
func (s *DurableStore) rowKey(id string) string   { return s.key("occ:" + id) }
func (s *DurableStore) claimKey(id string) string { return s.key("claim:" + id) }

func (s *DurableStore) health(ctx context.Context) error {
	data, err := s.client.Get(ctx, s.key("policies")).Bytes()
	if err != nil {
		return err
	}
	var registry policyRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return err
	}
	if registry != s.registry {
		return ErrPolicyEpoch
	}
	if s.deliveryEnabled {
		kind, err := s.client.Get(ctx, s.destinationKey()).Result()
		if err != nil {
			return err
		}
		if kind != s.sinkType {
			return ErrPolicyEpoch
		}
	}
	return nil
}

func (s *DurableStore) record(operation string, err error) {
	result := "success"
	if errors.Is(err, ErrClaimBusy) {
		result = "busy"
	} else if errors.Is(err, ErrFenced) {
		result = "fenced"
	} else if err != nil {
		result = "error"
		s.metrics.storeErrors.WithLabelValues(operation).Inc()
	}
	s.metrics.storeOperations.WithLabelValues(operation, result).Inc()
}

func (s *DurableStore) activate(ctx context.Context) (err error) {
	defer func() { s.record("activate", err) }()
	key := s.key("policies")
	for attempt := 0; attempt < 32; attempt++ {
		err = s.client.Watch(ctx, func(tx *redis.Tx) error {
			data, err := tx.Get(ctx, key).Bytes()
			if err != nil && err != redis.Nil {
				return err
			}
			if err == nil {
				var old policyRegistry
				if err := json.Unmarshal(data, &old); err != nil {
					return err
				}
				if old.Epoch > s.registry.Epoch {
					return ErrPolicyEpoch
				}
				if old.Epoch == s.registry.Epoch {
					if old.Digest != s.registry.Digest {
						return fmt.Errorf("policy configuration differs at epoch %d; increase durable.policy_epoch", old.Epoch)
					}
					return nil
				}
			}
			value, err := json.Marshal(s.registry)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, key, value, 0); return nil })
			return err
		}, key)
		if err != redis.TxFailedErr {
			return err
		}
	}
	return fmt.Errorf("policy activation contention: %w", err)
}

func (s *DurableStore) load(ctx context.Context, id string) (row *DurableOccurrence, err error) {
	defer func() { s.record("load", err) }()
	data, err := s.client.Get(ctx, s.rowKey(id)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeOccurrence(data, id)
}

func decodeOccurrence(data []byte, id string) (*DurableOccurrence, error) {
	var row DurableOccurrence
	if err := json.Unmarshal(data, &row); err != nil {
		return nil, err
	}
	if row.ID != id || row.Version == 0 || row.StartsAt.IsZero() || len(row.Bindings) == 0 ||
		(row.Lifecycle != LifecycleFiring && row.Lifecycle != LifecycleResolved) ||
		(row.Lifecycle == LifecycleResolved && (row.EndsAt == nil || row.CompletedAt == nil || row.CompactAt == nil || row.DeleteAt == nil)) {
		return nil, fmt.Errorf("corrupt occurrence %s", id)
	}
	if c := row.DeliveryClaim; c != nil {
		b := row.Bindings[c.BindingID]
		if c.Token == "" || c.Fence == 0 || c.Until.IsZero() || b == nil || b.Intents[c.Action] == nil || b.Intents[c.Action].ID != c.IntentID {
			return nil, fmt.Errorf("corrupt delivery claim")
		}
	}
	for key, b := range row.Bindings {
		if b == nil || b.ID != key || key != bindingKeyID(id, b.Policy, b.SinkID) || b.Intents == nil || b.PolicyVersion == "" || b.Calendar.PolicyVersion != b.PolicyVersion ||
			(!b.Calendar.NextEvaluationAt.IsZero() && b.Calendar.NextEvaluationAt.Before(row.StartsAt)) {
			return nil, fmt.Errorf("corrupt occurrence binding %s", id)
		}
		switch b.Eligibility {
		case EligibilityPending, EligibilityEligible, EligibilityIneligible, EligibilityUnknown:
		default:
			return nil, fmt.Errorf("invalid durable eligibility")
		}
		switch b.Delivery {
		case NotDelivered, PendingDelivery, Delivered, DeliveryFailed:
		default:
			return nil, fmt.Errorf("invalid durable delivery status")
		}
		for action, intent := range b.Intents {
			if intent == nil {
				return nil, fmt.Errorf("invalid delivery intent")
			}
			if b.SinkType != "" {
				if (action != "firing" && action != "resolved") || intent.Action != action || intent.ID != tupleID(row.ID, b.ID, action) {
					return nil, fmt.Errorf("corrupt delivery identity")
				}
				if intent.AuthorizedAt != nil && ((intent.Payload == nil && intent.Receipt == nil) || intent.Attempts == 0) {
					return nil, fmt.Errorf("corrupt send authorization")
				}
				if r := intent.Receipt; r != nil {
					if r.IntentID != intent.ID || r.SinkID != b.SinkID || r.DeliveredAt.IsZero() || r.Status != Delivered || r.Attempts == 0 {
						return nil, fmt.Errorf("corrupt delivery receipt")
					}
				}
			}
		}
	}
	return &row, nil
}

// update retries optimistic conflicts but not ambiguous network failures. A
// caller retries its idempotent lifecycle operation after an unacknowledged EXEC.
func (s *DurableStore) update(ctx context.Context, id string, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (committed *DurableOccurrence, err error) {
	return s.updateClaim(ctx, id, nil, mutate)
}

func (s *DurableStore) updateClaim(ctx context.Context, id string, required *SchedulerClaim, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (*DurableOccurrence, error) {
	return s.updateRecord(ctx, id, required, nil, mutate, false)
}

// reconcile distinguishes an occurrence change from index-only repair. An
// unchanged record retains its exact version, timestamps and claim/token TTL.
func (s *DurableStore) reconcile(ctx context.Context, id string, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (*DurableOccurrence, error) {
	return s.updateRecord(ctx, id, nil, nil, mutate, true)
}

func (s *DurableStore) updateRecord(ctx context.Context, id string, required *SchedulerClaim, delivery *DeliveryClaim, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error), reconcile bool) (committed *DurableOccurrence, err error) {
	defer func() { s.record("update", err) }()
	types := map[string]string{s.rowKey(id): "string", s.claimKey(id): "string", s.key("policies"): "string",
		s.key("due"): "zset", s.key("retention"): "zset", s.key("unresolved"): "set", s.key("pending"): "set", s.key("held"): "set", s.key("intent-index"): "set"}
	if s.deliveryEnabled {
		for key, kind := range s.deliveryKeyTypes(id) {
			types[key] = kind
		}
	}
	keys := make([]string, 0, len(types))
	for key := range types {
		keys = append(keys, key)
	}
	for attempt := 0; attempt < 64; attempt++ {
		err = s.client.Watch(ctx, func(tx *redis.Tx) error {
			// Keep every type check under WATCH, but send the independent
			// reads in one round trip. No validation is cached or omitted.
			checked := make(map[string]*redis.StatusCmd, len(types))
			_, err := tx.Pipelined(ctx, func(p redis.Pipeliner) error {
				for key := range types {
					checked[key] = p.Type(ctx, key)
				}
				return nil
			})
			if err != nil {
				return err
			}
			for key, command := range checked {
				actual, err := command.Result()
				if err != nil {
					return err
				}
				if actual != "none" && actual != types[key] {
					return fmt.Errorf("unexpected type for SLA store key %s: %s", key, actual)
				}
			}
			data, err := tx.Get(ctx, s.key("policies")).Bytes()
			if err != nil {
				return err
			}
			var registry policyRegistry
			if err := json.Unmarshal(data, &registry); err != nil {
				return err
			}
			if registry != s.registry {
				return ErrPolicyEpoch
			}
			data, err = tx.Get(ctx, s.rowKey(id)).Bytes()
			if err != nil && err != redis.Nil {
				return err
			}
			var old *DurableOccurrence
			if err == nil {
				old, err = decodeOccurrence(data, id)
				if err != nil {
					return err
				}
			}
			deliveryKeys := s.deliveryEnabled || old.hasDelivery()
			if deliveryKeys && !s.deliveryEnabled {
				for key, kind := range s.deliveryKeyTypes(id) {
					if err := tx.Watch(ctx, key).Err(); err != nil {
						return err
					}
					actual, err := tx.Type(ctx, key).Result()
					if err != nil {
						return err
					}
					if actual != "none" && actual != kind {
						return fmt.Errorf("invalid delivery index type")
					}
				}
			}
			if s.deliveryEnabled {
				kind, err := tx.Get(ctx, s.destinationKey()).Result()
				if err != nil {
					return err
				}
				if kind != s.sinkType {
					return ErrPolicyEpoch
				}
			}
			now, err := tx.Time(ctx).Result()
			if err != nil {
				return err
			}
			if required != nil {
				token, err := tx.Get(ctx, s.claimKey(id)).Result()
				if err != nil && err != redis.Nil {
					return err
				}
				if old == nil || old.Claim == nil || token != required.Token || old.Claim.Token != required.Token || old.Claim.Fence != required.Fence || old.Version != required.Version || !old.Claim.Until.After(now) {
					return ErrFenced
				}
			}
			if delivery != nil {
				token, err := tx.Get(ctx, s.deliveryClaimKey(id)).Result()
				if err != nil && err != redis.Nil {
					return err
				}
				if old == nil || old.DeliveryClaim == nil || token != delivery.Token || old.DeliveryClaim.Token != delivery.Token || old.DeliveryClaim.Fence != delivery.Fence || old.DeliveryClaim.IntentID != delivery.IntentID || !old.DeliveryClaim.Until.After(now) {
					return ErrFenced
				}
			}
			var version uint64
			if old != nil {
				version = old.Version
			}
			var previous []byte
			if reconcile {
				previous, err = json.Marshal(old)
				if err != nil {
					return err
				}
			}
			next, err := mutate(old, now.UTC())
			if err != nil {
				return err
			}
			if reconcile {
				candidate, err := json.Marshal(next)
				if err != nil {
					return err
				}
				if bytes.Equal(previous, candidate) {
					// No record write and no lease renewal. Repair only index
					// discrepancies, guarded by the same WATCH/type checks.
					err = s.repairIndexes(ctx, tx, id, next)
					if err == nil {
						committed = next
					}
					return err
				}
			}
			var encoded []byte
			var leaseTTL time.Duration
			if next != nil {
				if version == math.MaxUint64 {
					return fmt.Errorf("occurrence version exhausted")
				}
				next.Version, next.UpdatedAt = version+1, now.UTC()
				if next.Claim != nil && (old == nil || next.Claim.Version == 0) {
					next.Claim.Version = next.Version
				}
				encoded, err = json.Marshal(next)
				if err != nil {
					return err
				}
				if next.Claim != nil {
					leaseTTL = next.Claim.Until.Sub(now)
					if leaseTTL <= 0 {
						return ErrFenced
					}
				}
			}
			// Type checks and all relevant keys are WATCHed, including expiring
			// claim keys. EXEC cannot succeed across token expiry or replacement.
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				if next == nil {
					for _, set := range []string{"unresolved", "pending", "held", "intent-index"} {
						p.SRem(ctx, s.key(set), id)
					}
					p.ZRem(ctx, s.key("due"), id)
					p.ZRem(ctx, s.key("retention"), id)
					if deliveryKeys {
						p.Del(ctx, s.deliveryClaimKey(id))
						p.ZRem(ctx, s.key("delivery-due"), id)
						p.SRem(ctx, s.key("delivery-ready"), id)
						p.SRem(ctx, s.key("delivery-claimed"), id)
						p.SRem(ctx, s.key("delivery-held"), id)
					}
					p.Del(ctx, s.claimKey(id))
					p.Del(ctx, s.rowKey(id))
					return nil
				}
				if deliveryKeys {
					queueScore(ctx, p, s.key("delivery-due"), id, next.deliveryDueAt())
					setMember(ctx, p, s.key("delivery-ready"), id, next.DeliveryClaim == nil && !next.deliveryDueAt().IsZero())
					setMember(ctx, p, s.key("delivery-claimed"), id, next.DeliveryClaim != nil)
					setMember(ctx, p, s.key("delivery-held"), id, next.deliveryHeld())
					if next.DeliveryClaim == nil {
						p.Del(ctx, s.deliveryClaimKey(id))
					} else {
						p.SetArgs(ctx, s.deliveryClaimKey(id), next.DeliveryClaim.Token, redis.SetArgs{ExpireAt: next.DeliveryClaim.Until})
					}
				}
				queueScore(ctx, p, s.key("due"), id, next.dueAt())
				queueScore(ctx, p, s.key("retention"), id, next.retentionAt())
				pending, held, intent := false, false, false
				for _, b := range next.Bindings {
					pending = pending || (next.Lifecycle == LifecycleFiring && b.Eligibility == EligibilityPending)
					held = held || b.HoldReason != ""
					for _, i := range b.Intents {
						intent = intent || (i.Ready && !i.Canceled)
					}
				}
				setMember(ctx, p, s.key("unresolved"), id, next.Lifecycle == LifecycleFiring)
				setMember(ctx, p, s.key("pending"), id, pending)
				setMember(ctx, p, s.key("held"), id, held)
				setMember(ctx, p, s.key("intent-index"), id, intent)
				if next.Claim == nil {
					p.Del(ctx, s.claimKey(id))
				} else {
					p.Set(ctx, s.claimKey(id), next.Claim.Token, leaseTTL)
				}
				// This single document is the source of truth for state, schedule,
				// fencing and intent. No required metadata lives only in an index.
				p.Set(ctx, s.rowKey(id), encoded, 0)
				return nil
			})
			if err == nil {
				committed = next
			}
			return err
		}, keys...)
		if err != redis.TxFailedErr {
			return committed, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt%8+1) * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("state transaction contention: %w", err)
}

// deadlineScore rounds up only a fractional millisecond. Exact timestamps
// remain in the record and are checked again before claiming due work.
func deadlineScore(at time.Time) float64 {
	millis := at.UnixMilli()
	if at.Nanosecond()%int(time.Millisecond) != 0 {
		millis++
	}
	return float64(millis)
}

type occurrenceIndex struct {
	key     string
	sorted  bool
	present bool
	score   float64
}

func (s *DurableStore) occurrenceIndexes(row *DurableOccurrence) []occurrenceIndex {
	indexes := []occurrenceIndex{
		{key: s.key("due"), sorted: true}, {key: s.key("retention"), sorted: true},
		{key: s.key("unresolved")}, {key: s.key("pending")}, {key: s.key("held")}, {key: s.key("intent-index")},
	}
	if s.deliveryEnabled || row.hasDelivery() {
		indexes = append(indexes, occurrenceIndex{key: s.key("delivery-due"), sorted: true}, occurrenceIndex{key: s.key("delivery-ready")}, occurrenceIndex{key: s.key("delivery-claimed")}, occurrenceIndex{key: s.key("delivery-held")})
		if row != nil {
			at := row.deliveryDueAt()
			indexes[6].present = !at.IsZero()
			if !at.IsZero() {
				indexes[6].score = deadlineScore(at)
			}
			indexes[7].present = row.DeliveryClaim == nil && !at.IsZero()
			indexes[8].present = row.DeliveryClaim != nil
			indexes[9].present = row.deliveryHeld()
		}
	}
	if row == nil {
		return indexes
	}
	for i, at := range []time.Time{row.dueAt(), row.retentionAt()} {
		indexes[i].present = !at.IsZero()
		if indexes[i].present {
			indexes[i].score = deadlineScore(at)
		}
	}
	indexes[2].present = row.Lifecycle == LifecycleFiring
	for _, b := range row.Bindings {
		indexes[3].present = indexes[3].present || (row.Lifecycle == LifecycleFiring && b.Eligibility == EligibilityPending)
		indexes[4].present = indexes[4].present || b.HoldReason != ""
		for _, intent := range b.Intents {
			indexes[5].present = indexes[5].present || (intent.Ready && !intent.Canceled)
		}
	}
	return indexes
}

// repairIndexes reads membership in one pipeline. If everything matches it
// sends no MULTI/EXEC at all. An index-only repair WATCHes the occurrence as well
// as the indexes, so a concurrent lifecycle update always invalidates stale repair.
func (s *DurableStore) repairIndexes(ctx context.Context, tx *redis.Tx, id string, row *DurableOccurrence) error {
	indexes := s.occurrenceIndexes(row)
	scores := make(map[string]*redis.FloatCmd)
	members := make(map[string]*redis.BoolCmd)
	var tokenExists *redis.IntCmd
	var deliveryTokenExists *redis.IntCmd
	_, err := tx.Pipelined(ctx, func(p redis.Pipeliner) error {
		for _, index := range indexes {
			if index.sorted {
				scores[index.key] = p.ZScore(ctx, index.key, id)
			} else {
				members[index.key] = p.SIsMember(ctx, index.key, id)
			}
		}
		if row == nil || row.Claim == nil {
			tokenExists = p.Exists(ctx, s.claimKey(id))
		}
		if (s.deliveryEnabled || row.hasDelivery()) && (row == nil || row.DeliveryClaim == nil) {
			deliveryTokenExists = p.Exists(ctx, s.deliveryClaimKey(id))
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	var changes []occurrenceIndex
	for _, index := range indexes {
		if index.sorted {
			score, err := scores[index.key].Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				return err
			}
			present := err == nil
			if present != index.present || (present && score != index.score) {
				changes = append(changes, index)
			}
		} else {
			present, err := members[index.key].Result()
			if err != nil {
				return err
			}
			if present != index.present {
				changes = append(changes, index)
			}
		}
	}
	removeToken := false
	if tokenExists != nil {
		n, err := tokenExists.Result()
		if err != nil {
			return err
		}
		removeToken = n != 0
	}
	removeDeliveryToken := false
	if deliveryTokenExists != nil {
		n, err := deliveryTokenExists.Result()
		if err != nil {
			return err
		}
		removeDeliveryToken = n != 0
	}
	if len(changes) == 0 && !removeToken && !removeDeliveryToken {
		return nil
	}
	_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for _, index := range changes {
			if index.sorted {
				if index.present {
					p.ZAdd(ctx, index.key, redis.Z{Score: index.score, Member: id})
				} else {
					p.ZRem(ctx, index.key, id)
				}
			} else {
				setMember(ctx, p, index.key, id, index.present)
			}
		}
		if removeToken {
			p.Del(ctx, s.claimKey(id))
		}
		if removeDeliveryToken {
			p.Del(ctx, s.deliveryClaimKey(id))
		}
		return nil
	})
	return err
}

func queueScore(ctx context.Context, p redis.Pipeliner, key, id string, at time.Time) {
	if at.IsZero() {
		p.ZRem(ctx, key, id)
		return
	}
	// Index rounding cannot advance a nanosecond-precision calendar deadline.
	p.ZAdd(ctx, key, redis.Z{Score: deadlineScore(at), Member: id})
}
func setMember(ctx context.Context, p redis.Pipeliner, key, id string, include bool) {
	if include {
		p.SAdd(ctx, key, id)
	} else {
		p.SRem(ctx, key, id)
	}
}

func (s *DurableStore) due(ctx context.Context, index string, limit int) (ids []string, err error) {
	defer func() { s.record("due", err) }()
	now, err := s.client.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	return s.client.ZRangeArgs(ctx, redis.ZRangeArgs{Key: s.key(index), Start: "-inf", Stop: now.UnixMilli(), ByScore: true, Count: int64(limit)}).Result()
}

func (s *DurableStore) deliveryClaimKey(id string) string { return s.key("delivery-claim:" + id) }
func (s *DurableStore) destinationKey() string            { return s.key("destination:" + tupleID(s.cfg.SinkID)) }
func (s *DurableStore) deliveryKeyTypes(id string) map[string]string {
	return map[string]string{s.deliveryClaimKey(id): "string", s.destinationKey(): "string", s.key("delivery-due"): "zset", s.key("delivery-ready"): "set", s.key("delivery-claimed"): "set", s.key("delivery-held"): "set"}
}
func (s *DurableStore) updateDelivery(ctx context.Context, id string, claim *DeliveryClaim, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (*DurableOccurrence, error) {
	return s.updateRecord(ctx, id, nil, claim, mutate, false)
}

// A destination's implementation is an immutable pin, separate from policy
// epochs. A future sink change needs an explicit reviewed migration.
func (s *DurableStore) activateSink(ctx context.Context) error {
	key := s.destinationKey()
	for attempt := 0; attempt < 32; attempt++ {
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			kind, err := tx.Get(ctx, key).Result()
			if err != nil && err != redis.Nil {
				return err
			}
			if err == nil {
				if kind != s.sinkType {
					return fmt.Errorf("logical sink already pinned to another implementation; migration required")
				}
				return nil
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, key, s.sinkType, 0); return nil })
			return err
		}, key)
		if err != redis.TxFailedErr {
			return err
		}
	}
	return fmt.Errorf("sink activation contention")
}
