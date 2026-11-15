package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
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
	client       *redis.Client
	cfg          DurableConfig
	prefix       string
	registry     policyRegistry
	metrics      *metrics
	catalog      map[string]string
	catalogValue string
	schema       string
	quarantined  atomic.Bool
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
	catalog := config.sinkCatalog()
	catalogJSON, _ := json.Marshal(catalog)
	digest := tupleID(string(encoded), string(catalogJSON), fmt.Sprint(config.Delivery.Enabled), "multi-sink-v1")
	return &DurableStore{client: client, cfg: cfg, prefix: cfg.Valkey.Namespace + ":{sla}:", registry: policyRegistry{cfg.PolicyEpoch, digest}, metrics: metrics, catalog: catalog, catalogValue: string(catalogJSON)}
}

func (s *DurableStore) key(suffix string) string  { return s.prefix + suffix }
func (s *DurableStore) rowKey(id string) string   { return s.key("occ:" + id) }
func (s *DurableStore) claimKey(id string) string { return s.key("claim:" + id) }

func (s *DurableStore) health(ctx context.Context) error {
	if err := s.checkSchema(ctx, s.client); err != nil {
		return err
	}
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
	catalog, err := s.client.Get(ctx, s.destinationKey()).Result()
	if err != nil {
		return err
	}
	if catalog != s.catalogValue {
		return ErrPolicyEpoch
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
	policyKey, catalogKey := s.key("policies"), s.destinationKey()
	for attempt := 0; attempt < 32; attempt++ {
		err = s.client.Watch(ctx, func(tx *redis.Tx) error {
			if err := s.checkSchema(ctx, tx); err != nil {
				return err
			}
			data, err := tx.Get(ctx, policyKey).Bytes()
			if err != nil && err != redis.Nil {
				return err
			}
			policyPresent := err == nil
			if policyPresent {
				var old policyRegistry
				if err := json.Unmarshal(data, &old); err != nil {
					return err
				}
				if old.Epoch > s.registry.Epoch {
					return ErrPolicyEpoch
				}
				if old.Epoch == s.registry.Epoch && old.Digest != s.registry.Digest {
					return fmt.Errorf("configuration changed at active epoch; increase durable.policy_epoch")
				}
			}
			current, err := tx.Get(ctx, catalogKey).Result()
			if err != nil && err != redis.Nil {
				return err
			}
			if policyPresent != (err == nil) {
				return fmt.Errorf("incomplete configuration registry; operator repair required")
			}
			if err == nil {
				var registered map[string]string
				if err := json.Unmarshal([]byte(current), &registered); err != nil || len(registered) == 0 {
					return fmt.Errorf("invalid destination registry")
				}
				for id, fingerprint := range registered {
					if s.catalog[id] != fingerprint {
						return fmt.Errorf("registered sink %s changed or was removed; outstanding bindings cannot be redirected or discarded", id)
					}
				}
			}
			encoded, _ := json.Marshal(s.registry)
			if policyPresent && current == s.catalogValue {
				old, _ := tx.Get(ctx, policyKey).Result()
				if old == string(encoded) {
					return nil
				}
			}
			commands, err := tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, catalogKey, s.catalogValue, 0)
				p.Set(ctx, policyKey, encoded, 0)
				return nil
			})
			return s.checkedExec(ctx, commands, err)
		}, policyKey, catalogKey, s.schemaKey())
		if err != redis.TxFailedErr {
			return err
		}
	}
	return fmt.Errorf("configuration activation contention")
}

func (s *DurableStore) load(ctx context.Context, id string) (row *DurableOccurrence, err error) {
	defer func() { s.record("load", err) }()
	if err := s.checkSchema(ctx, s.client); err != nil {
		return nil, err
	}
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
	if row.FormatVersion != occurrenceFormatVersion || row.ID != id || row.Version == 0 || row.StartsAt.IsZero() || len(row.Bindings) == 0 ||
		(row.Lifecycle != LifecycleFiring && row.Lifecycle != LifecycleResolved) ||
		(row.Lifecycle == LifecycleResolved && (row.EndsAt == nil || row.CompletedAt == nil || row.CompactAt == nil || row.DeleteAt == nil)) {
		return nil, fmt.Errorf("corrupt occurrence %s", id)
	}

	for key, b := range row.Bindings {
		if b == nil || b.ID != key || key != bindingKeyID(id, b.Policy, b.SinkID) || b.Intents == nil || b.PolicyVersion == "" || b.Calendar.PolicyVersion != b.PolicyVersion ||
			(!b.Calendar.NextEvaluationAt.IsZero() && b.Calendar.NextEvaluationAt.Before(row.StartsAt)) {
			return nil, fmt.Errorf("corrupt occurrence binding %s", id)
		}
		if (b.SinkType != "log" && b.SinkType != "http") || b.DestinationFingerprint == "" {
			return nil, fmt.Errorf("corrupt binding destination")
		}
		if c := b.Claim; c != nil {
			if c.Token == "" || c.Fence == 0 || c.BindingID != b.ID || c.Until.IsZero() || b.Intents[c.Action] == nil || b.Intents[c.Action].ID != c.IntentID {
				return nil, fmt.Errorf("corrupt binding delivery claim")
			}
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
	return &row, nil
}

// update retries optimistic conflicts but not ambiguous network failures. A
// caller retries its idempotent lifecycle operation after an unacknowledged EXEC.
func (s *DurableStore) update(ctx context.Context, id string, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (committed *DurableOccurrence, err error) {
	return s.updateClaim(ctx, id, nil, mutate)
}

func (s *DurableStore) updateClaim(ctx context.Context, id string, required *SchedulerClaim, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (*DurableOccurrence, error) {
	return s.updateRecord(ctx, id, required, nil, "", mutate, false)
}

// reconcile distinguishes an occurrence change from index-only repair. An
// unchanged record retains its exact version, timestamps and claim/token TTL.
func (s *DurableStore) reconcile(ctx context.Context, id string, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (*DurableOccurrence, error) {
	return s.updateRecord(ctx, id, nil, nil, "", mutate, true)
}

func (s *DurableStore) updateRecord(ctx context.Context, id string, required *SchedulerClaim, delivery *DeliveryClaim, bindingID string, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error), reconcile bool) (committed *DurableOccurrence, err error) {
	defer func() { s.record("update", err) }()
	types := s.indexTypes()
	for key, kind := range map[string]string{s.rowKey(id): "string", s.claimKey(id): "string", s.key("policies"): "string", s.destinationKey(): "string"} {
		types[key] = kind
	}
	keys := []string{s.rowKey(id), s.claimKey(id), s.key("policies"), s.schemaKey(), s.destinationKey()}
	if bindingID != "" {
		key := s.deliveryClaimKey(id, bindingID)
		keys = append(keys, key)
		types[key] = "string"
	}

	for attempt := 0; attempt < 64; attempt++ {
		err = s.client.Watch(ctx, func(tx *redis.Tx) error {
			if err := s.checkSchema(ctx, tx); err != nil {
				return err
			}
			if err := s.checkTypes(ctx, tx, types); err != nil {
				return err
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
			catalog, err := tx.Get(ctx, s.destinationKey()).Result()
			if err != nil {
				return err
			}
			if catalog != s.catalogValue {
				return ErrPolicyEpoch
			}
			previousIndexes := s.occurrenceIndexes(id, old)
			previousClaims := bindingClaims(old)

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
				token, err := tx.Get(ctx, s.deliveryClaimKey(id, bindingID)).Result()
				if err != nil && err != redis.Nil {
					return err
				}
				var current *DeliveryClaim
				if old != nil && old.Bindings[bindingID] != nil {
					current = old.Bindings[bindingID].Claim
				}
				if current == nil || token != delivery.Token || current.Token != delivery.Token || current.Fence != delivery.Fence || current.IntentID != delivery.IntentID || !current.Until.After(now) {
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
			if next != nil && next.FormatVersion != occurrenceFormatVersion {
				return fmt.Errorf("unsupported occurrence format")
			}
			nextClaims := bindingClaims(next)
			for bid, claim := range nextClaims {
				previous, found := previousClaims[bid]
				if (!found || !sameDeliveryToken(previous, claim)) && bid != bindingID {
					return fmt.Errorf("delivery claim mutation requires binding-scoped update")
				}
			}
			for bid := range previousClaims {
				if _, found := nextClaims[bid]; !found && bid != bindingID {
					return fmt.Errorf("delivery claim removal requires binding-scoped update")
				}
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
			// Claim tokens and the schema fence remain WATCHed. Shared index
			// types are preflighted under the schema ownership contract; runtime
			// command errors quarantine the namespace (EXEC has no rollback).
			if s.quarantined.Load() {
				return ErrIndexSchema
			}
			commands, err := tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				queueIndexChanges(ctx, p, previousIndexes, s.occurrenceIndexes(id, next))
				queueBindingTokens(ctx, p, s, id, previousClaims, nextClaims)
				if next == nil {
					p.Del(ctx, s.claimKey(id))
					p.Del(ctx, s.rowKey(id))
					return nil
				}
				if next.Claim == nil {
					p.Del(ctx, s.claimKey(id))
				} else {
					p.Set(ctx, s.claimKey(id), next.Claim.Token, leaseTTL)
				}
				p.Set(ctx, s.rowKey(id), encoded, 0)
				return nil
			})
			err = s.checkedExec(ctx, commands, err)
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
	if err := s.checkSchema(ctx, s.client); err != nil {
		return nil, err
	}
	now, err := s.client.Time(ctx).Result()
	if err != nil {
		return nil, err
	}
	ids, err = s.client.ZRangeArgs(ctx, redis.ZRangeArgs{Key: s.key(index), Start: "-inf", Stop: now.UnixMilli(), ByScore: true, Count: int64(limit)}).Result()
	return ids, s.readError(ctx, err)
}

func (s *DurableStore) deliveryClaimKey(id, bindingID string) string {
	return s.key("delivery-claim:" + deliveryWorkID(id, bindingID))
}
func (s *DurableStore) destinationKey() string { return s.key("destinations") }
func (s *DurableStore) updateDelivery(ctx context.Context, id string, claim *DeliveryClaim, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (*DurableOccurrence, error) {
	return s.updateRecord(ctx, id, nil, claim, claim.BindingID, mutate, false)
}
func (s *DurableStore) claimBinding(ctx context.Context, id, bindingID string, mutate func(*DurableOccurrence, time.Time) (*DurableOccurrence, error)) (*DurableOccurrence, error) {
	return s.updateRecord(ctx, id, nil, nil, bindingID, mutate, true)
}
