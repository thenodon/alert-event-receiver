package slaproxy

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type occurrenceIndex struct {
	key, member     string
	sorted, present bool
	score           float64
}

func (s *DurableStore) occurrenceIndexes(id string, row *DurableOccurrence) []occurrenceIndex {
	indexes := []occurrenceIndex{{key: s.key("due"), member: id, sorted: true}, {key: s.key("retention"), member: id, sorted: true}}
	for _, name := range []string{"unresolved", "pending", "held", "intent-index"} {
		indexes = append(indexes, occurrenceIndex{key: s.key(name), member: id})
	}
	if row == nil {
		return indexes
	}
	for i, at := range []time.Time{row.dueAt(), row.retentionAt()} {
		indexes[i].present = !at.IsZero()
		if !at.IsZero() {
			indexes[i].score = deadlineScore(at)
		}
	}
	indexes[2].present = row.Lifecycle == LifecycleFiring
	for _, b := range row.Bindings {
		indexes[3].present = indexes[3].present || (row.Lifecycle == LifecycleFiring && b.Eligibility == EligibilityPending)
		indexes[4].present = indexes[4].present || b.HoldReason != ""
		for _, i := range b.Intents {
			indexes[5].present = indexes[5].present || (i.Ready && !i.Canceled)
		}
		member := deliveryWorkID(id, b.ID)
		at := b.deliveryDueAt(row)
		due := occurrenceIndex{key: s.key("delivery-due"), member: member, sorted: true, present: !at.IsZero()}
		if !at.IsZero() {
			due.score = deadlineScore(at)
		}
		queued := b.Claim == nil && !at.IsZero()
		claimed := b.Claim != nil
		held := b.deliveryHeld()
		indexes = append(indexes, due,
			occurrenceIndex{key: s.key("delivery-ready"), member: member, present: queued},
			occurrenceIndex{key: s.key("delivery-claimed"), member: member, present: claimed},
			occurrenceIndex{key: s.key("delivery-held"), member: member, present: held},
			occurrenceIndex{key: s.key("delivery-pending:" + b.SinkID), member: member, present: queued},
			occurrenceIndex{key: s.key("delivery-inflight:" + b.SinkID), member: member, present: claimed},
			occurrenceIndex{key: s.key("delivery-failed:" + b.SinkID), member: member, present: held})
	}
	return indexes
}
func (index occurrenceIndex) queue(ctx context.Context, p redis.Pipeliner) {
	if index.sorted {
		if index.present {
			p.ZAdd(ctx, index.key, redis.Z{Score: index.score, Member: index.member})
		} else {
			p.ZRem(ctx, index.key, index.member)
		}
	} else {
		setMember(ctx, p, index.key, index.member, index.present)
	}
}
func queueIndexChanges(ctx context.Context, p redis.Pipeliner, old, next []occurrenceIndex) {
	desired := make(map[string]occurrenceIndex, len(old)+len(next))
	for _, index := range old {
		index.present = false
		desired[index.key+"\x00"+index.member] = index
	}
	for _, index := range next {
		desired[index.key+"\x00"+index.member] = index
	}
	for _, index := range desired {
		index.queue(ctx, p)
	}
}
func bindingClaims(row *DurableOccurrence) map[string]DeliveryClaim {
	claims := make(map[string]DeliveryClaim)
	if row != nil {
		for id, b := range row.Bindings {
			if b.Claim != nil {
				claims[id] = *b.Claim
			}
		}
	}
	return claims
}
func sameDeliveryToken(a, b DeliveryClaim) bool {
	return a.Token == b.Token && a.Fence == b.Fence && a.Until.Equal(b.Until)
}
func queueBindingTokens(ctx context.Context, p redis.Pipeliner, s *DurableStore, id string, old, next map[string]DeliveryClaim) {
	for bid, claim := range next {
		before, found := old[bid]
		if !found || !sameDeliveryToken(before, claim) {
			p.SetArgs(ctx, s.deliveryClaimKey(id, bid), claim.Token, redis.SetArgs{ExpireAt: claim.Until})
		}
	}
	for bid := range old {
		if _, found := next[bid]; !found {
			p.Del(ctx, s.deliveryClaimKey(id, bid))
		}
	}
}
func (s *DurableStore) repairIndexes(ctx context.Context, tx *redis.Tx, id string, row *DurableOccurrence) error {
	indexes := s.occurrenceIndexes(id, row)
	scores := make([]*redis.FloatCmd, len(indexes))
	members := make([]*redis.BoolCmd, len(indexes))
	tokens := make(map[string]*redis.IntCmd)
	_, err := tx.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, index := range indexes {
			if index.sorted {
				scores[i] = p.ZScore(ctx, index.key, index.member)
			} else {
				members[i] = p.SIsMember(ctx, index.key, index.member)
			}
		}
		if row == nil || row.Claim == nil {
			key := s.claimKey(id)
			tokens[key] = p.Exists(ctx, key)
		}
		if row != nil {
			for bid, b := range row.Bindings {
				if b.Claim == nil {
					key := s.deliveryClaimKey(id, bid)
					tokens[key] = p.Exists(ctx, key)
				}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return s.readError(ctx, err)
	}
	var changes []occurrenceIndex
	for i, index := range indexes {
		if index.sorted {
			score, err := scores[i].Result()
			if err != nil && err != redis.Nil {
				return s.readError(ctx, err)
			}
			present := err == nil
			if present != index.present || (present && score != index.score) {
				changes = append(changes, index)
			}
		} else {
			present, err := members[i].Result()
			if err != nil {
				return s.readError(ctx, err)
			}
			if present != index.present {
				changes = append(changes, index)
			}
		}
	}
	var removeTokens []string
	for key, exists := range tokens {
		n, err := exists.Result()
		if err != nil {
			return err
		}
		if n > 0 {
			removeTokens = append(removeTokens, key)
		}
	}
	if len(changes) == 0 && len(removeTokens) == 0 {
		return nil
	}
	if s.quarantined.Load() {
		return ErrIndexSchema
	}
	commands, err := tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
		for _, index := range changes {
			index.queue(ctx, p)
		}
		for _, key := range removeTokens {
			p.Del(ctx, key)
		}
		return nil
	})
	return s.checkedExec(ctx, commands, err)
}
