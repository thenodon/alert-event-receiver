package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RED metrics for the webhook receiver and internal operations.
var (
	// Webhook handler: Rate / Errors / Duration
	WebhookRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "alertreceiver_webhook_requests_total",
		Help: "Total webhook requests received, labelled by HTTP status class (2xx, 4xx, 5xx).",
	}, []string{"status"})

	WebhookDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "alertreceiver_webhook_duration_seconds",
		Help:    "Webhook request processing duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"status"})

	// Event emission
	EventsEmitted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "alertreceiver_events_emitted_total",
		Help: "Total lifecycle events emitted via OTLP, labelled by transition (firing, resolved).",
	}, []string{"transition"})

	EmitErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "alertreceiver_emit_errors_total",
		Help: "Total OTLP emit errors, labelled by transition.",
	}, []string{"transition"})

	// State operations
	RedisErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "alertreceiver_redis_errors_total",
		Help: "Total Redis/Valkey operation errors, labelled by operation.",
	}, []string{"operation"})

	RedisOpsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "alertreceiver_redis_ops_total",
		Help: "Total Redis/Valkey operations, labelled by operation and result.",
	}, []string{"operation", "result"})

	RedisOpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "alertreceiver_redis_op_duration_seconds",
		Help:    "Redis/Valkey operation duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	RedisIdempSetNXTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "alertreceiver_redis_idemp_setnx_total",
		Help: "Total Redis SET NX results for idempotency keys, labelled by result.",
	}, []string{"result"})

	StateWritesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "alertreceiver_state_writes_total",
		Help: "Total alert state transition outcomes, labelled by result.",
	}, []string{"result"})

	StateEntries = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "alertreceiver_state_entries",
		Help: "Current number of Redis/Valkey alert state entries by status, maintained from write-path operations.",
	}, []string{"status"})

	IdempotencyKeysCreatedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "alertreceiver_idempotency_keys_created_total",
		Help: "Total idempotency keys created successfully, labelled by transition.",
	}, []string{"transition"})

	ResolvedOrphansTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "alertreceiver_resolved_orphans_total",
		Help: "Total resolved alerts received without a matching open state.",
	})

	ClosedTTLSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "alertreceiver_closed_ttl_seconds",
		Help:    "TTL applied to closed alert tombstones in Redis/Valkey, in seconds.",
		Buckets: []float64{60, 300, 900, 3600, 21600, 43200, 86400, 172800, 604800},
	})

	// Duplicate suppression
	DuplicatesDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "alertreceiver_duplicates_dropped_total",
		Help: "Total events dropped due to idempotency key match.",
	})
)

