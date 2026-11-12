package slaproxy

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type metrics struct {
	registry     *prometheus.Registry
	requests     *prometheus.CounterVec
	latency      *prometheus.HistogramVec
	alerts       *prometheus.CounterVec
	selected     *prometheus.CounterVec
	skipped      *prometheus.CounterVec
	calendar     *prometheus.CounterVec
	eligibility  *prometheus.CounterVec
	policyErrors *prometheus.CounterVec
	holidays     *prometheus.CounterVec
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
		calendar: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sla_proxy_calendar_classifications_total", Help: "Successful per-notification calendar classifications at startsAt, not lifecycle eligibility or delivery.",
		}, []string{"sla_label", "sla_value", "classification", "reason"}),
		eligibility: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sla_proxy_alert_eligibility_observations_total", Help: "Per-notification lifecycle evidence status, including pending, unknown and errors; not current activity verification or delivery.",
		}, []string{"sla_label", "sla_value", "alert_status", "eligibility_status", "reason"}),
		policyErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sla_proxy_policy_errors_total", Help: "Failed evaluator calls by bounded reason; one call per selected policy pair and exact startsAt instant per webhook.",
		}, []string{"reason"}),
		holidays: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sla_proxy_holiday_evaluations_total", Help: "Country calendar checks, including future dates searched, by configured country and result; not alert counts.",
		}, []string{"country", "result"}),
	}
	m.registry.MustRegister(m.requests, m.latency, m.alerts, m.selected, m.skipped,
		m.calendar, m.eligibility, m.policyErrors, m.holidays,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
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
	for _, reason := range []string{"calendar_error", "unknown_policy", "invalid_evaluation_time", "cancelled", "evaluation_error"} {
		m.policyErrors.WithLabelValues(reason)
	}
	return m
}
