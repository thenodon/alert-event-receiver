package state

import (
	"context"
	"time"
)

// AlertState holds the working state for one alert instance stored in Redis.
type AlertState struct {
	Status        string
	FirstFiringAt time.Time
	LastSeenAt    time.Time
	StartsAt      time.Time
	EndsAt        time.Time
	Alertname     string
	Labels        map[string]string
}

// Store is the interface for alert state operations.
// Store calls do not make a read/decide/write sequence atomic.
// Concurrent processing of the same fingerprint can race.
type Store interface {
	// GetState returns current state for the given fingerprint, or nil if not found.
	GetState(ctx context.Context, fingerprint string) (*AlertState, error)

	// SetFiring creates or overwrites the open state for a new firing cycle.
	SetFiring(ctx context.Context, fingerprint string, s AlertState) error

	// UpdateLastSeen updates only the last_seen_at timestamp.
	UpdateLastSeen(ctx context.Context, fingerprint string, at time.Time) error

	// SetClosed replaces the complete occurrence with a closed tombstone and TTL.
	SetClosed(ctx context.Context, fingerprint string, s AlertState, ttl time.Duration) error

	// CheckAndSetIdempotency atomically sets an idempotency key if not present.
	// Returns true if the key was newly set (event is not a duplicate).
	CheckAndSetIdempotency(ctx context.Context, key string, ttl time.Duration) (bool, error)
}
