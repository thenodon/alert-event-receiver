package alertload

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"time"
)

type Rates struct {
	OfferedRequests    float64 `json:"offered_requests_per_second"`
	OfferedAlerts      float64 `json:"offered_alerts_per_second"`
	SentRequests       float64 `json:"sent_requests_per_second"`
	CompletedRequests  float64 `json:"completed_requests_per_second"`
	SuccessfulRequests float64 `json:"successful_requests_per_second"`
	SentAlerts         float64 `json:"sent_alerts_per_second"`
	AcceptedAlerts     float64 `json:"http_accepted_alerts_per_second"`
}
type PhaseReport struct {
	Name               string            `json:"phase"`
	Transition         string            `json:"transition"`
	ExpectedOutcome    string            `json:"conditional_expected_outcome"`
	PlannedRequests    uint64            `json:"planned_requests"`
	EligibleRequests   uint64            `json:"eligible_requests"`
	OfferedRequests    uint64            `json:"offered_requests"`
	ScheduledRequests  uint64            `json:"scheduled_requests"`
	SentRequests       uint64            `json:"sent_requests"`
	CompletedRequests  uint64            `json:"completed_requests"`
	SuccessfulRequests uint64            `json:"successful_requests"`
	FailedRequests     uint64            `json:"failed_requests"`
	QueueFull          uint64            `json:"not_scheduled_queue_full"`
	MissedDeadline     uint64            `json:"not_scheduled_deadline"`
	NotOffered         uint64            `json:"not_offered_interrupted"`
	Abandoned          uint64            `json:"abandoned_before_send"`
	ConstructionErrors uint64            `json:"construction_errors"`
	SentAlerts         uint64            `json:"alerts_sent"`
	AcceptedAlerts     uint64            `json:"alerts_http_accepted"`
	UnknownAlerts      uint64            `json:"alerts_unknown"`
	RejectedAlerts     uint64            `json:"alerts_http_rejected"`
	ResponseClasses    map[string]uint64 `json:"http_response_classes"`
	Timeouts           uint64            `json:"timeouts"`
	NetworkErrors      uint64            `json:"network_errors"`
	CanceledRequests   uint64            `json:"canceled_requests"`
	PeakInflight       int               `json:"peak_inflight"`
	SchedulingSeconds  float64           `json:"scheduling_seconds"`
	DrainSeconds       float64           `json:"drain_seconds"`
	WallSeconds        float64           `json:"wall_seconds"`
	DrainTimedOut      bool              `json:"drain_timed_out"`
	HTTPLatency        Distribution      `json:"http_latency_seconds"`
	QueueWait          Distribution      `json:"queue_wait_seconds"`
	Construction       Distribution      `json:"construction_seconds"`
	SchedulingLag      Distribution      `json:"scheduling_lag_seconds"`
	PayloadBytes       Distribution      `json:"payload_bytes"`
	Rates              Rates             `json:"rates"`
}

type Report struct {
	SchemaVersion             int               `json:"schema_version"`
	Config                    Config            `json:"config"`
	Endpoint                  string            `json:"endpoint_origin"`
	GoVersion                 string            `json:"go_version"`
	StartedAt                 time.Time         `json:"started_at"`
	EndedAt                   time.Time         `json:"ended_at"`
	Duration                  string            `json:"scheduling_window_per_phase"`
	Timeout                   string            `json:"request_timeout"`
	DrainTimeout              string            `json:"drain_timeout"`
	ResolveAfter              string            `json:"synthetic_resolve_after"`
	WallSeconds               float64           `json:"wall_seconds"`
	SchedulingSeconds         float64           `json:"scheduling_seconds"`
	Interrupted               bool              `json:"interrupted"`
	Failed                    bool              `json:"failed"`
	FailureReasons            []string          `json:"failure_reasons"`
	Warning                   string            `json:"expectation_warning"`
	TargetAlertsPerSecond     float64           `json:"target_alerts_per_second"`
	PlannedMaxRequests        uint64            `json:"planned_max_requests"`
	OccurrencesPlanned        uint64            `json:"occurrences_planned"`
	OccurrencesSubmitted      uint64            `json:"occurrences_submitted"`
	OccurrencesHTTPAccepted   uint64            `json:"occurrences_http_accepted"`
	PlannedMaxAlerts          uint64            `json:"planned_max_alerts"`
	OfferedRequests           uint64            `json:"offered_requests"`
	ScheduledRequests         uint64            `json:"scheduled_requests"`
	SentRequests              uint64            `json:"sent_requests"`
	CompletedRequests         uint64            `json:"completed_requests"`
	SuccessfulRequests        uint64            `json:"successful_requests"`
	FailedRequests            uint64            `json:"failed_requests"`
	DroppedRequests           uint64            `json:"dropped_by_generator_requests"`
	AbandonedRequests         uint64            `json:"abandoned_before_send_requests"`
	SentAlerts                uint64            `json:"alerts_sent"`
	AcceptedAlerts            uint64            `json:"alerts_http_accepted"`
	UnknownAlerts             uint64            `json:"alerts_unknown"`
	PeakInflight              int               `json:"peak_inflight"`
	Rates                     Rates             `json:"rates"`
	TransitionsSent           map[string]uint64 `json:"transitions_sent"`
	PlannedOutcomes           map[string]uint64 `json:"eligible_expected_outcomes"`
	AcceptedOutcomes          map[string]uint64 `json:"http_accepted_expected_outcomes"`
	MatchedClosureCandidates  uint64            `json:"matched_closure_candidates"`
	DuplicateCandidates       uint64            `json:"duplicate_candidates"`
	OrphanCandidates          uint64            `json:"orphan_resolution_candidates"`
	ExpectedDurationHistogram ExpectedHistogram `json:"conditional_expected_duration_histogram"`
	Phases                    []PhaseReport     `json:"phases"`
}

func newReport(c Config) Report {
	return Report{
		SchemaVersion:         1,
		Config:                c,
		Endpoint:              c.Endpoint(),
		GoVersion:             runtime.Version(),
		StartedAt:             time.Now().UTC(),
		Duration:              c.Duration.String(),
		Timeout:               c.Timeout.String(),
		DrainTimeout:          c.DrainTimeout.String(),
		ResolveAfter:          c.ResolveAfter.String(),
		FailureReasons:        []string{},
		Warning:               "Expected outcomes assume isolated clean receiver state and successful state operations. HTTP 2xx is not proof of lifecycle correctness, OTLP export, or backend persistence. Failed/time-out deliveries can have mutated state; no retries are performed.",
		TargetAlertsPerSecond: c.Rate * float64(c.BatchSize),
		TransitionsSent:       make(map[string]uint64),
		PlannedOutcomes:       make(map[string]uint64),
		AcceptedOutcomes:      make(map[string]uint64),
		Phases:                []PhaseReport{},
	}
}
func rate(n uint64, seconds float64) float64 {
	if seconds <= 0 {
		return 0
	}
	return float64(n) / seconds
}
func rates(offered, sent, completed, successful, alerts, accepted uint64, batch int, scheduling, wall float64) Rates {
	return Rates{OfferedRequests: rate(offered, scheduling), OfferedAlerts: rate(offered*uint64(batch), scheduling), SentRequests: rate(sent, wall), CompletedRequests: rate(completed, wall), SuccessfulRequests: rate(successful, wall), SentAlerts: rate(alerts, wall), AcceptedAlerts: rate(accepted, wall)}
}
func (r *Report) finish(c Config, start time.Time) {
	r.OccurrencesPlanned = uint64(c.RequestsPerPhase()) * uint64(c.BatchSize)
	if len(r.Phases) > 0 {
		r.OccurrencesSubmitted = r.Phases[0].SentAlerts
		r.OccurrencesHTTPAccepted = r.Phases[0].AcceptedAlerts
	}
	r.EndedAt = time.Now().UTC()
	r.WallSeconds = time.Since(start).Seconds()
	for _, p := range r.Phases {
		r.OfferedRequests += p.OfferedRequests
		r.ScheduledRequests += p.ScheduledRequests
		r.SentRequests += p.SentRequests
		r.CompletedRequests += p.CompletedRequests
		r.SuccessfulRequests += p.SuccessfulRequests
		r.FailedRequests += p.FailedRequests
		r.DroppedRequests += p.QueueFull + p.MissedDeadline
		r.AbandonedRequests += p.Abandoned
		r.SentAlerts += p.SentAlerts
		r.AcceptedAlerts += p.AcceptedAlerts
		r.UnknownAlerts += p.UnknownAlerts
		r.PeakInflight = max(r.PeakInflight, p.PeakInflight)
		r.SchedulingSeconds += p.SchedulingSeconds
		r.TransitionsSent[p.Transition] += p.SentAlerts
		r.PlannedOutcomes[p.ExpectedOutcome] += p.EligibleRequests * uint64(c.BatchSize)
		r.AcceptedOutcomes[p.ExpectedOutcome] += p.AcceptedAlerts
		if p.DrainTimedOut {
			r.FailureReasons = append(r.FailureReasons, "phase drain timeout: "+p.Name)
		}
		if p.ConstructionErrors > 0 {
			r.FailureReasons = append(r.FailureReasons, "request construction failed")
		}
	}
	r.MatchedClosureCandidates = r.AcceptedOutcomes["closed"]
	r.DuplicateCandidates = r.AcceptedOutcomes["repeated_firing"] + r.AcceptedOutcomes["repeated_resolved"]
	r.OrphanCandidates = r.AcceptedOutcomes["resolved_without_open_state"]
	r.ExpectedDurationHistogram = expectedHistogram(c.ResolveAfter.Seconds(), r.MatchedClosureCandidates)
	r.Rates = rates(r.OfferedRequests, r.SentRequests, r.CompletedRequests, r.SuccessfulRequests, r.SentAlerts, r.AcceptedAlerts, c.BatchSize, r.SchedulingSeconds, r.WallSeconds)
	if r.Interrupted {
		r.FailureReasons = append(r.FailureReasons, "interrupted")
	}
	if r.SentRequests > 0 && float64(r.FailedRequests)/float64(r.SentRequests) > c.MaxFailureRate {
		r.FailureReasons = append(r.FailureReasons, "failed request fraction exceeds max-failure-rate")
	}
	if c.FailOnDrop && r.DroppedRequests > 0 {
		r.FailureReasons = append(r.FailureReasons, "generator dropped scheduled traffic")
	}
	r.Failed = len(r.FailureReasons) > 0
}
func formatBound(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
func WriteJSON(w io.Writer, r Report) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}
func WriteSummary(w io.Writer, r Report) error {
	if _, err := fmt.Fprintf(w, "alertload run=%s scenario=%s endpoint=%s seed=%d base=%s\n", r.Config.RunID, r.Config.Scenario, r.Endpoint, r.Config.Seed, r.Config.BaseTime.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	for _, p := range r.Phases {
		if _, err := fmt.Fprintf(w, "  phase=%s offered=%d scheduled=%d sent=%d completed=%d 2xx=%d failed=%d queue_full=%d deadline_missed=%d abandoned=%d schedule=%.3fs drain=%.3fs HTTP p50/p90/p95/p99/max=%.6f/%.6f/%.6f/%.6f/%.6fs queue_p95=%.6fs peak_inflight=%d\n", p.Name, p.OfferedRequests, p.ScheduledRequests, p.SentRequests, p.CompletedRequests, p.SuccessfulRequests, p.FailedRequests, p.QueueFull, p.MissedDeadline, p.Abandoned, p.SchedulingSeconds, p.DrainSeconds, p.HTTPLatency.P50, p.HTTPLatency.P90, p.HTTPLatency.P95, p.HTTPLatency.P99, p.HTTPLatency.Max, p.QueueWait.P95, p.PeakInflight); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "    http_classes=%v timeouts=%d network_errors=%d canceled=%d construction_p95=%.6fs payload_mean/p95=%.0f/%.0f bytes\n", p.ResponseClasses, p.Timeouts, p.NetworkErrors, p.CanceledRequests, p.Construction.P95, p.PayloadBytes.Mean, p.PayloadBytes.P95); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "target=%.3f req/s (%.3f alerts/s); offered=%.3f req/s (%.3f alerts/s); achieved=%.3f sent req/s (%.3f alerts/s), %.3f completed req/s\nrequests planned_max=%d offered=%d scheduled=%d sent=%d completed=%d failed=%d dropped=%d alerts_sent=%d HTTP_accepted=%d unknown=%d wall=%.3fs\n", r.Config.Rate, r.TargetAlertsPerSecond, r.Rates.OfferedRequests, r.Rates.OfferedAlerts, r.Rates.SentRequests, r.Rates.SentAlerts, r.Rates.CompletedRequests, r.PlannedMaxRequests, r.OfferedRequests, r.ScheduledRequests, r.SentRequests, r.CompletedRequests, r.FailedRequests, r.DroppedRequests, r.SentAlerts, r.AcceptedAlerts, r.UnknownAlerts, r.WallSeconds); err != nil {
		return err
	}
	for _, key := range sortedKeys(r.AcceptedOutcomes) {
		if _, err := fmt.Fprintf(w, "  conditional_expected_%s=%d alerts\n", key, r.AcceptedOutcomes[key]); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "matched_closure_candidates=%d expected_duration=%.3fs result_failed=%v interrupted=%v\n%s\n", r.MatchedClosureCandidates, r.Config.ResolveAfter.Seconds(), r.Failed, r.Interrupted, r.Warning)
	return err
}
