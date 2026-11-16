package slaproxy

import (
	"context"
	"strings"

	"github.com/redis/go-redis/v9"
)

// cleanOrphan removes only a member whose authoritative occurrence/binding is
// absent. WATCH of that occurrence fences concurrent creation or replacement.
func (s *DurableStore) cleanOrphan(ctx context.Context, index, member string) error {
	id, bid := member, ""
	delivery := strings.HasPrefix(index, "delivery-")
	valid := true
	if delivery {
		id, bid, valid = parseDeliveryWork(member)
	}
	keys := []string{s.schemaKey(), s.key("policies")}
	if valid {
		keys = append(keys, s.rowKey(id))
	}
	for attempt := 0; attempt < 64; attempt++ {
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			if err := s.checkSchema(ctx, tx); err != nil {
				return err
			}
			if valid {
				data, err := tx.Get(ctx, s.rowKey(id)).Bytes()
				if err != nil && err != redis.Nil {
					return err
				}
				if err == nil {
					row, err := decodeOccurrence(data, id)
					if err != nil {
						return err
					}
					if !delivery {
						return nil
					}
					if binding := row.Bindings[bid]; binding != nil {
						belongs := true
						for _, prefix := range []string{"delivery-pending:", "delivery-inflight:", "delivery-failed:"} {
							if strings.HasPrefix(index, prefix) && strings.TrimPrefix(index, prefix) != binding.SinkID {
								belongs = false
							}
						}
						if belongs {
							return nil
						}
					}
				}
			}
			commands, err := tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				key := s.key(index)
				if index == "due" || index == "retention" || index == "delivery-due" {
					p.ZRem(ctx, key, member)
				} else {
					p.SRem(ctx, key, member)
				}
				return nil
			})
			return s.checkedExec(ctx, commands, err)
		}, keys...)
		if err != redis.TxFailedErr {
			return err
		}
	}
	return redis.TxFailedErr
}
