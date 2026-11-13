package slaproxy

import "github.com/prometheus/client_golang/prometheus"

type deliveryMetrics struct {
	attempts         *prometheus.CounterVec
	receipts         *prometheus.CounterVec
	commits          *prometheus.CounterVec
	retries          *prometheus.CounterVec
	claims           *prometheus.CounterVec
	errors           *prometheus.CounterVec
	duration         *prometheus.HistogramVec
	eligibilityDelay *prometheus.HistogramVec
	ready            prometheus.Gauge
	claimed          prometheus.Gauge
	held             prometheus.Gauge
}

func newDeliveryMetrics(reg *prometheus.Registry) *deliveryMetrics {
	m := &deliveryMetrics{
		attempts:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "sla_proxy_delivery_attempts_total", Help: "Sink calls by configured sink, action and result; success is sink acceptance, not a durable receipt."}, []string{"sink_id", "action", "result"}),
		receipts:         prometheus.NewCounterVec(prometheus.CounterOpts{Name: "sla_proxy_delivery_receipts_total", Help: "Successful firing/resolved receipts committed durably; excludes repeated sink calls without a new receipt."}, []string{"sink_id", "action"}),
		commits:          prometheus.NewCounterVec(prometheus.CounterOpts{Name: "sla_proxy_delivery_receipt_commits_total", Help: "Delivery outcome persistence results, including ambiguous connection failures."}, []string{"result"}),
		retries:          prometheus.NewCounterVec(prometheus.CounterOpts{Name: "sla_proxy_delivery_retries_total", Help: "Sink calls after the first authorized attempt, including crash-window retries."}, []string{"sink_id", "action"}),
		claims:           prometheus.NewCounterVec(prometheus.CounterOpts{Name: "sla_proxy_delivery_claims_total", Help: "Delivery claim outcomes, including expired claims reclaimed and fenced outcomes."}, []string{"result"}),
		errors:           prometheus.NewCounterVec(prometheus.CounterOpts{Name: "sla_proxy_delivery_errors_total", Help: "Dispatcher operational errors by bounded operation; raw sink errors are not logged."}, []string{"operation"}),
		duration:         prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "sla_proxy_delivery_duration_seconds", Help: "Duration of individual sink calls, regardless of receipt persistence.", Buckets: prometheus.DefBuckets}, []string{"sink_id", "action"}),
		eligibilityDelay: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "sla_proxy_delivery_eligibility_delay_seconds", Help: "First eligibility to firing sink success, observed after receipt commit.", Buckets: []float64{1, 5, 30, 60, 300, 900, 3600, 21600, 86400}}, []string{"sink_id"}),
		ready:            prometheus.NewGauge(prometheus.GaugeOpts{Name: "sla_proxy_delivery_ready_occurrences", Help: "Namespace-wide due delivery work, including expired claims eligible for recovery; sampled when polling. Do not sum replicas."}),
		claimed:          prometheus.NewGauge(prometheus.GaugeOpts{Name: "sla_proxy_delivery_claimed_occurrences", Help: "Namespace-wide persisted delivery claims, including expired claims not yet recovered; sampled when polling. Do not sum replicas."}),
	}
	m.held = prometheus.NewGauge(prometheus.GaugeOpts{Name: "sla_proxy_delivery_held_occurrences", Help: "Namespace-wide occurrences with permanent delivery or preparation failures; retained for operator intervention. Do not sum replicas."})
	reg.MustRegister(m.attempts, m.receipts, m.commits, m.retries, m.claims, m.errors, m.duration, m.eligibilityDelay, m.ready, m.claimed, m.held)
	return m
}
