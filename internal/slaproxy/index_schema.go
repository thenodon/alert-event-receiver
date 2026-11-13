package slaproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrIndexSchema = errors.New("SLA store schema is unavailable or quarantined; operator repair and restart required")

type indexSchema struct {
	Version    int    `json:"version"`
	State      string `json:"state"`
	Generation string `json:"generation"`
}

func (s *DurableStore) schemaKey() string { return s.key("index-schema") }
func readyIndexSchema(data string) bool {
	var schema indexSchema
	return json.Unmarshal([]byte(data), &schema) == nil && schema.Version == 1 && schema.State == "ready" && schema.Generation != ""
}
func (s *DurableStore) indexTypes() map[string]string {
	types := map[string]string{s.key("due"): "zset", s.key("retention"): "zset", s.key("unresolved"): "set", s.key("pending"): "set", s.key("held"): "set", s.key("intent-index"): "set"}
	// Check optional delivery indexes even when delivery is currently disabled.
	for _, name := range []string{"delivery-ready", "delivery-claimed", "delivery-held"} {
		types[s.key(name)] = "set"
	}
	types[s.key("delivery-due")] = "zset"
	return types
}
func (s *DurableStore) initSchema(ctx context.Context) error {
	var generation [16]byte
	if _, err := rand.Read(generation[:]); err != nil {
		return err
	}
	value, _ := json.Marshal(indexSchema{1, "ready", hex.EncodeToString(generation[:])})
	key := s.schemaKey()
	for attempt := 0; attempt < 32; attempt++ {
		err := s.client.Watch(ctx, func(tx *redis.Tx) error {
			current, err := tx.Get(ctx, key).Result()
			if err != nil && err != redis.Nil {
				return err
			}
			if err == nil {
				if !readyIndexSchema(current) {
					return ErrIndexSchema
				}
				s.schema = current
			} else {
				s.schema = string(value)
			}
			// This is schema validation, not a promise that an uncooperative external
			// client cannot corrupt an index immediately afterwards.
			if err := s.checkTypes(ctx, tx, s.indexTypes()); err != nil {
				return err
			}
			if current != "" {
				return nil
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { p.Set(ctx, key, s.schema, 0); return nil })
			return err
		}, key)
		if err != redis.TxFailedErr {
			return err
		}
	}
	return fmt.Errorf("schema initialization contention")
}
func (s *DurableStore) checkSchema(ctx context.Context, cmd redis.Cmdable) error {
	if s.quarantined.Load() {
		return ErrIndexSchema
	}
	current, err := cmd.Get(ctx, s.schemaKey()).Result()
	if err != nil {
		if err == redis.Nil || redis.HasErrorPrefix(err, "WRONGTYPE") {
			s.quarantined.Store(true)
			return ErrIndexSchema
		}
		return err
	}
	// initSchema already validated the immutable expected value. Exact equality
	// fences every state/version/generation change without reparsing on each call.
	if s.schema == "" || current != s.schema {
		s.quarantined.Store(true)
		return ErrIndexSchema
	}
	return nil
}
func (s *DurableStore) checkTypes(ctx context.Context, tx *redis.Tx, types map[string]string) error {
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
			return fmt.Errorf("%w: key %s has type %s, expected %s", s.quarantine(ctx, "type_mismatch"), key, actual, types[key])
		}
	}
	return nil
}

// quarantine is durable best effort plus an immediate, irreversible local stop.
// Do not blindly replace a newer schema generation installed by an operator.
func (s *DurableStore) quarantine(ctx context.Context, reason string) error {
	s.quarantined.Store(true)
	op, stop := context.WithTimeout(context.WithoutCancel(ctx), min(time.Second, s.cfg.Scheduler.OperationTimeout))
	defer stop()
	markerErr := s.client.Watch(op, func(tx *redis.Tx) error {
		current, err := tx.Get(op, s.schemaKey()).Result()
		if err != nil && err != redis.Nil {
			return err
		}
		if err == nil && current != s.schema {
			return nil
		}
		var schema indexSchema
		_ = json.Unmarshal([]byte(s.schema), &schema)
		schema.Version, schema.State = 1, "quarantined"
		if schema.Generation == "" {
			schema.Generation = "repair-required"
		}
		encoded, _ := json.Marshal(schema)
		_, err = tx.TxPipelined(op, func(p redis.Pipeliner) error { p.Set(op, s.schemaKey(), encoded, 0); return nil })
		return err
	}, s.schemaKey())
	if markerErr != nil {
		return fmt.Errorf("%w (%s; durable quarantine marker not confirmed)", ErrIndexSchema, reason)
	}
	return fmt.Errorf("%w (%s)", ErrIndexSchema, reason)
}

// checkedExec inspects every returned command. A server-side runtime error can
// accompany successful sibling writes: never acknowledge it as a commit, never
// retry it as an optimistic conflict, and require schema recovery before sends.
func (s *DurableStore) checkedExec(ctx context.Context, commands []redis.Cmder, err error) error {
	if errors.Is(err, redis.TxFailedErr) {
		return err
	}
	for _, command := range commands {
		var server redis.Error
		if failure := command.Err(); failure != nil && !errors.Is(failure, redis.TxFailedErr) && errors.As(failure, &server) {
			return fmt.Errorf("%w: %s command failed; sibling writes may have committed", s.quarantine(ctx, "exec_command_error"), command.Name())
		}
	}
	var server redis.Error
	if err != nil && errors.As(err, &server) {
		return s.quarantine(ctx, "exec_error")
	}
	return err
}
func (s *DurableStore) readError(ctx context.Context, err error) error {
	if redis.HasErrorPrefix(err, "WRONGTYPE") {
		return s.quarantine(ctx, "index_read_type")
	}
	return err
}
