package slaproxy

import "github.com/prometheus/client_golang/prometheus"

type metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	latency  *prometheus.HistogramVec
	alerts   *prometheus.CounterVec
	selected *prometheus.CounterVec
	skipped  *prometheus.CounterVec
}

func newMetrics(cfg Config) *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sla_proxy_notifications_received_total", Help: "Webhook requests by HTTP response status class, including invalid requests.",
		}, []string{"status"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "sla_proxy_webhook_duration_seconds", Help: "Observation webhook request duration in seconds.", Buckets: prometheus.DefBuckets,
		}, []string{"status"}),
		alerts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sla_proxy_alerts_received_total", Help: "Alerts in fully validated webhook batches, including selected and skipped alerts, repeats and stale notifications.",
		}, []string{"status"}),
		selected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sla_proxy_alerts_selected_total", Help: "Observe-only selections by configured policy pair and alert status; not policy eligibility, persistence or delivery.",
		}, []string{"sla_label", "sla_value", "status"}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sla_proxy_alerts_skipped_total", Help: "Valid alerts skipped for missing SLA selection labels or unknown SLA identifiers.",
		}, []string{"reason"}),
	}
	m.registry.MustRegister(m.requests, m.latency, m.alerts, m.selected, m.skipped,
		prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	for _, status := range []string{"2xx", "4xx", "5xx"} {
		m.requests.WithLabelValues(status)
	}
	for _, status := range []string{"firing", "resolved"} {
		m.alerts.WithLabelValues(status)
		for _, def := range cfg.SLADefinitions {
			for name := range def.SLAs {
				m.selected.WithLabelValues(def.SLALabel, name, status)
			}
		}
	}
	for _, reason := range []string{"missing_sla_label", "unknown_sla"} {
		m.skipped.WithLabelValues(reason)
	}
	return m
}
