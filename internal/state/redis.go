package state

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/telemetry"
	"github.com/redis/go-redis/v9"
)

const (
	stateKeyPrefix = "alertstate:"
	idempKeyPrefix = "alertidemp:"
)

// RedisStore implements Store using Redis / Valkey.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore creates a RedisStore backed by the given client.
func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client}
}

func stateKey(fingerprint string) string { return stateKeyPrefix + fingerprint }

func recordRedisOp(operation, result string, started time.Time) {
	telemetry.RedisOpsTotal.WithLabelValues(operation, result).Inc()
	telemetry.RedisOpDuration.WithLabelValues(operation).Observe(time.Since(started).Seconds())
	if result == "error" {
		telemetry.RedisErrors.WithLabelValues(operation).Inc()
	}
}

// GetState retrieves alert state by fingerprint. Returns nil if not found.
func (r *RedisStore) GetState(ctx context.Context, fingerprint string) (*AlertState, error) {
	started := time.Now()
	key := stateKey(fingerprint)
	vals, err := r.client.HGetAll(ctx, key).Result()
	if err != nil {
		recordRedisOp("get_state", "error", started)
		return nil, fmt.Errorf("redis HGetAll %s: %w", key, err)
	}
	if len(vals) == 0 {
		recordRedisOp("get_state", "ok", started)
		return nil, nil
	}

	s := &AlertState{
		Status:    vals["status"],
		Alertname: vals["alertname"],
		Labels:    make(map[string]string),
	}
	s.FirstFiringAt, _ = time.Parse(time.RFC3339, vals["first_firing_at"])
	s.LastSeenAt, _ = time.Parse(time.RFC3339, vals["last_seen_at"])
	s.StartsAt, _ = time.Parse(time.RFC3339, vals["starts_at"])

	// Labels are stored with a "label." prefix to avoid collisions with state fields.
	for k, v := range vals {
		if strings.HasPrefix(k, "label.") {
			s.Labels[strings.TrimPrefix(k, "label.")] = v
		}
	}
	recordRedisOp("get_state", "ok", started)
	return s, nil
}

// SetFiring stores an open firing state and removes any existing TTL.
func (r *RedisStore) SetFiring(ctx context.Context, fingerprint string, s AlertState) error {
	started := time.Now()
	key := stateKey(fingerprint)
	prevStatus, err := r.client.HGet(ctx, key, "status").Result()
	if err != nil && err != redis.Nil {
		recordRedisOp("set_firing", "error", started)
		return fmt.Errorf("redis HGet %s status: %w", key, err)
	}
	fields := map[string]interface{}{
		"status":          "firing",
		"first_firing_at": s.FirstFiringAt.UTC().Format(time.RFC3339),
		"last_seen_at":    s.LastSeenAt.UTC().Format(time.RFC3339),
		"starts_at":       s.StartsAt.UTC().Format(time.RFC3339),
		"alertname":       s.Alertname,
	}
	for k, v := range s.Labels {
		fields["label."+k] = v
	}
	if err := r.client.HSet(ctx, key, fields).Err(); err != nil {
		recordRedisOp("set_firing", "error", started)
		return fmt.Errorf("redis HSet %s: %w", key, err)
	}
	// Persist removes the TTL so open alerts don't expire.
	if err := r.client.Persist(ctx, key).Err(); err != nil {
		recordRedisOp("set_firing", "error", started)
		return fmt.Errorf("redis Persist %s: %w", key, err)
	}

	switch prevStatus {
	case "closed":
		telemetry.StateEntries.WithLabelValues("closed").Dec()
		telemetry.StateEntries.WithLabelValues("firing").Inc()
	case "firing":
		// No net change.
	default:
		telemetry.StateEntries.WithLabelValues("firing").Inc()
	}
	recordRedisOp("set_firing", "ok", started)
	return nil
}

// UpdateLastSeen updates only the last_seen_at field.
func (r *RedisStore) UpdateLastSeen(ctx context.Context, fingerprint string, at time.Time) error {
	started := time.Now()
	key := stateKey(fingerprint)
	if err := r.client.HSet(ctx, key, "last_seen_at", at.UTC().Format(time.RFC3339)).Err(); err != nil {
		recordRedisOp("update_last_seen", "error", started)
		return err
	}
	recordRedisOp("update_last_seen", "ok", started)
	return nil
}

// SetClosed marks the alert closed and applies a TTL tombstone.
func (r *RedisStore) SetClosed(ctx context.Context, fingerprint string, ttl time.Duration) error {
	started := time.Now()
	key := stateKey(fingerprint)
	prevStatus, err := r.client.HGet(ctx, key, "status").Result()
	if err != nil && err != redis.Nil {
		recordRedisOp("set_closed", "error", started)
		return fmt.Errorf("redis HGet %s status: %w", key, err)
	}
	if err := r.client.HSet(ctx, key, "status", "closed").Err(); err != nil {
		recordRedisOp("set_closed", "error", started)
		return fmt.Errorf("redis HSet closed %s: %w", key, err)
	}
	if err := r.client.Expire(ctx, key, ttl).Err(); err != nil {
		recordRedisOp("set_closed", "error", started)
		return err
	}

	switch prevStatus {
	case "firing":
		telemetry.StateEntries.WithLabelValues("firing").Dec()
		telemetry.StateEntries.WithLabelValues("closed").Inc()
	case "closed":
		// No net change.
	default:
		telemetry.StateEntries.WithLabelValues("closed").Inc()
	}
	telemetry.ClosedTTLSeconds.Observe(ttl.Seconds())
	recordRedisOp("set_closed", "ok", started)
	return nil
}

// CheckAndSetIdempotency atomically sets the idempotency key if absent.
// Returns true if the key was newly set (not a duplicate delivery).
func (r *RedisStore) CheckAndSetIdempotency(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	started := time.Now()
	full := idempKeyPrefix + key
	set, err := r.client.SetNX(ctx, full, "1", ttl).Result()
	if err != nil {
		telemetry.RedisIdempSetNXTotal.WithLabelValues("error").Inc()
		recordRedisOp("setnx_idemp", "error", started)
		return false, fmt.Errorf("redis SetNX %s: %w", full, err)
	}
	if set {
		telemetry.RedisIdempSetNXTotal.WithLabelValues("set").Inc()
		recordRedisOp("setnx_idemp", "ok", started)
	} else {
		telemetry.RedisIdempSetNXTotal.WithLabelValues("exists").Inc()
		recordRedisOp("setnx_idemp", "duplicate", started)
	}
	return set, nil
}

// IdempotencyKey builds a stable idempotency key for a transition.
// For firing: t = alert.StartsAt. For resolved: t = alert.EndsAt (or now).
func IdempotencyKey(fingerprint, transition string, t time.Time) string {
	return fmt.Sprintf("%s:%s:%d", fingerprint, transition, t.Unix())
}

// FingerprintSource builds the fingerprint_source string from a label map,
// recording which label names were part of the Alertmanager fingerprint.
func FingerprintSource(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "+")
}

