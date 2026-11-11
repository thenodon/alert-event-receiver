package alertload

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type MetricCheck struct {
	Metric   string  `json:"metric"`
	Expected float64 `json:"expected"`
	Observed float64 `json:"observed"`
	Matches  bool    `json:"matches"`
}
type MetricsComparison struct {
	Status  string             `json:"status"`
	Reasons []string           `json:"reasons"`
	Deltas  map[string]float64 `json:"counter_deltas"`
	Checks  []MetricCheck      `json:"checks"`
}
type BackendCheck struct {
	Status              string            `json:"status"`
	Reason              string            `json:"reason,omitempty"`
	Expected            uint64            `json:"expected"`
	Observed            uint64            `json:"observed"`
	ExpectedTransitions map[string]uint64 `json:"expected_transitions"`
	ObservedTransitions map[string]uint64 `json:"observed_transitions"`
	Note                string            `json:"note"`
}
type snapshot map[string]float64

// Verification uses bounded read-only HTTP calls, no redirects or response/error
// text in reports. One baseline and one final scrape/query; no polling/retries.
func verificationRead(ctx context.Context, c Config, endpoint string, form url.Values) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.CheckTimeout)
	defer cancel()
	transport := &http.Transport{DisableCompression: true, MaxResponseHeaderBytes: 64 << 10, TLSHandshakeTimeout: c.CheckTimeout}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: c.CheckTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	method := http.MethodGet
	var body io.Reader
	if form != nil {
		method = http.MethodPost
		body = strings.NewReader(form.Encode())
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, errors.New("cannot construct verification request")
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("verification request failed or timed out")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("verification endpoint returned non-2xx status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("invalid or oversized verification response")
	}
	return data, nil
}

var trackedMetrics = map[string]string{
	"alertreceiver_webhook_requests_total":        "status",
	"alertreceiver_events_generated_total":        "transition",
	"alertreceiver_events_emitted_total":          "transition",
	"alertreceiver_processing_outcomes_total":     "outcome",
	"alertreceiver_alert_duration_seconds_count":  "",
	"alertreceiver_alert_duration_seconds_sum":    "",
	"alertreceiver_alert_duration_seconds_bucket": "le",
	"alertreceiver_redis_errors_total":            "operation",
	"alertreceiver_emit_errors_total":             "transition",
	"alertreceiver_otlp_export_requests_total":    "protocol,result",
	"process_start_time_seconds":                  "",
}

func metricKey(name string, labels map[string]string) string {
	keys := strings.Split(trackedMetrics[name], ",")
	if len(keys) == 1 && keys[0] == "" {
		return name
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}
func safeMetricLabel(key, value string) bool {
	switch key {
	case "status":
		return value == "2xx" || value == "4xx" || value == "5xx"
	case "transition":
		return value == "firing" || value == "resolved"
	case "protocol":
		return value == "grpc" || value == "http/protobuf"
	case "result":
		return value == "success" || value == "error"
	case "operation":
		switch value {
		case "get_state", "set_firing", "set_closed", "update_last_seen", "setnx_idemp", "idempotency":
			return true
		}
	case "outcome":
		switch value {
		case "opened", "closed", "reopened", "superseded_firing", "repeated_firing", "repeated_resolved", "stale_firing", "stale_resolved", "resolved_without_open_state", "indeterminate", "invalid_notification":
			return true
		}
	case "le":
		if value == "+Inf" {
			return true
		}
		v, err := strconv.ParseFloat(value, 64)
		return err == nil && !math.IsInf(v, 0) && !math.IsNaN(v) && v >= 0
	}
	return false
}
func parseLabels(text string) (map[string]string, error) {
	labels := map[string]string{}
	for strings.TrimSpace(text) != "" {
		text = strings.TrimSpace(text)
		key, rest, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || len(rest) == 0 || rest[0] != '"' || len(labels) >= 32 {
			return nil, errors.New("invalid metric labels")
		}
		end := 1
		for end < len(rest) {
			if rest[end] == '\\' {
				end += 2
				continue
			}
			if rest[end] == '"' {
				break
			}
			end++
		}
		if end >= len(rest) {
			return nil, errors.New("invalid quoted metric label")
		}
		value, err := strconv.Unquote(rest[:end+1])
		if err != nil {
			return nil, errors.New("invalid metric label encoding")
		}
		if _, exists := labels[key]; exists {
			return nil, errors.New("duplicate metric label")
		}
		labels[key] = value
		text = strings.TrimSpace(rest[end+1:])
		if text != "" {
			if text[0] != ',' {
				return nil, errors.New("invalid metric label separator")
			}
			text = text[1:]
		}
	}
	return labels, nil
}
func parseSnapshot(data []byte) (snapshot, error) {
	result := snapshot{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		nameEnd := strings.IndexAny(line, "{ \t")
		if nameEnd < 0 {
			continue
		}
		name := line[:nameEnd]
		keys, tracked := trackedMetrics[name]
		if !tracked {
			continue
		}
		rest := strings.TrimSpace(line[nameEnd:])
		labels := map[string]string{}
		if strings.HasPrefix(rest, "{") {
			// Find the closing brace outside quoted strings (label values may contain }).
			quoted, escaped := false, false
			end := -1
			for i := 1; i < len(rest); i++ {
				ch := rest[i]
				if escaped {
					escaped = false
					continue
				}
				if ch == '\\' && quoted {
					escaped = true
					continue
				}
				if ch == '"' {
					quoted = !quoted
				}
				if ch == '}' && !quoted {
					end = i
					break
				}
			}
			if end < 0 {
				return nil, errors.New("invalid metric sample")
			}
			var err error
			labels, err = parseLabels(rest[1:end])
			if err != nil {
				return nil, err
			}
			rest = strings.TrimSpace(rest[end+1:])
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil, errors.New("missing metric value")
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, errors.New("invalid metric value")
		}
		filtered := map[string]string{}
		if keys != "" {
			for _, key := range strings.Split(keys, ",") {
				v := labels[key]
				if !safeMetricLabel(key, v) {
					return nil, errors.New("unexpected verification metric label")
				}
				if key == "le" && v != "+Inf" {
					f, _ := strconv.ParseFloat(v, 64)
					v = formatBound(f)
				}
				filtered[key] = v
			}
		}
		if len(result) >= 4096 {
			return nil, errors.New("too many verification series")
		}
		key := metricKey(name, filtered)
		sum := result[key] + value
		if math.IsInf(sum, 0) {
			return nil, errors.New("metric aggregation overflow")
		}
		result[key] = sum
	}
	if scanner.Err() != nil {
		return nil, errors.New("invalid/oversized metric line")
	}
	if _, ok := result["alertreceiver_alert_duration_seconds_count"]; !ok {
		return nil, errors.New("receiver duration histogram missing from scrape")
	}
	return result, nil
}
func scrape(ctx context.Context, c Config) (snapshot, error) {
	data, err := verificationRead(ctx, c, c.MetricsURL, nil)
	if err != nil {
		return nil, err
	}
	return parseSnapshot(data)
}

func compareMetrics(before, after snapshot, r Report) MetricsComparison {
	result := MetricsComparison{Status: "matched", Reasons: []string{}, Deltas: map[string]float64{}, Checks: []MetricCheck{}}
	for k, v := range before {
		end, exists := after[k]
		if (!exists && v != 0) || (exists && end < v) {
			result.Reasons = append(result.Reasons, "counter reset or series disappeared")
			break
		}
	}
	if a, ok := before["process_start_time_seconds"]; ok && after["process_start_time_seconds"] != a {
		result.Reasons = append(result.Reasons, "receiver restarted")
	}
	for k, v := range after {
		if k == "process_start_time_seconds" {
			continue
		}
		result.Deltas[k] = v - before[k]
	}
	if r.FailedRequests > 0 || r.DroppedRequests > 0 || r.Interrupted || r.AbandonedRequests > 0 {
		result.Reasons = append(result.Reasons, "failed, dropped or interrupted traffic makes lifecycle expectations uncertain")
	}
	if len(result.Reasons) > 0 {
		result.Status = "inconclusive"
		return result
	}
	check := func(key string, expected float64) {
		observed := result.Deltas[key]
		match := math.Abs(observed-expected) <= 1e-8*math.Max(1, math.Abs(expected))
		result.Checks = append(result.Checks, MetricCheck{key, expected, observed, match})
		if !match {
			result.Status = "mismatch"
		}
	}
	check("alertreceiver_webhook_requests_total{status=2xx}", float64(r.SuccessfulRequests))
	check("alertreceiver_webhook_requests_total{status=4xx}", 0)
	check("alertreceiver_webhook_requests_total{status=5xx}", 0)
	outcomes := []string{"opened", "closed", "reopened", "superseded_firing", "repeated_firing", "repeated_resolved", "stale_firing", "stale_resolved", "resolved_without_open_state", "indeterminate", "invalid_notification"}
	for _, outcome := range outcomes {
		check("alertreceiver_processing_outcomes_total{outcome="+outcome+"}", float64(r.AcceptedOutcomes[outcome]))
	}
	expectedEvents := map[string]uint64{
		"firing":   r.AcceptedOutcomes["opened"] + r.AcceptedOutcomes["reopened"] + r.AcceptedOutcomes["superseded_firing"],
		"resolved": r.AcceptedOutcomes["closed"] + r.AcceptedOutcomes["resolved_without_open_state"],
	}
	for _, transition := range []string{"firing", "resolved"} {
		expected := expectedEvents[transition]
		check("alertreceiver_events_generated_total{transition="+transition+"}", float64(expected))
		check("alertreceiver_events_emitted_total{transition="+transition+"}", float64(expected))
	}
	check("alertreceiver_alert_duration_seconds_count", float64(r.ExpectedDurationHistogram.Count))
	check("alertreceiver_alert_duration_seconds_sum", r.ExpectedDurationHistogram.Sum)
	for _, le := range sortedKeys(r.ExpectedDurationHistogram.Buckets) {
		check("alertreceiver_alert_duration_seconds_bucket{le="+le+"}", float64(r.ExpectedDurationHistogram.Buckets[le]))
	}
	// Exposed failures are measured separately from correctness checks. SDK
	// submission/export counters do not establish backend delivery.
	return result
}
func queryBackend(ctx context.Context, c Config, r Report) BackendCheck {
	result := BackendCheck{Status: "error", ExpectedTransitions: map[string]uint64{"firing": r.AcceptedOutcomes["opened"] + r.AcceptedOutcomes["reopened"] + r.AcceptedOutcomes["superseded_firing"], "resolved": r.AcceptedOutcomes["closed"] + r.AcceptedOutcomes["resolved_without_open_state"]}, ObservedTransitions: map[string]uint64{}, Note: "Single query-time visibility/count check, not a durable persistence or exactly-once guarantee. Use a fresh run ID and a retention-compatible synthetic time range."}
	for _, v := range result.ExpectedTransitions {
		result.Expected += v
	}
	if r.FailedRequests > 0 || r.DroppedRequests > 0 || r.Interrupted || r.AbandonedRequests > 0 {
		result.Status = "inconclusive"
		result.Reason = "uncertain workload; backend query skipped"
		return result
	}
	query := fmt.Sprintf(`event.kind:="alert_transition" alert.label.load_run:=%q | stats by (alert.transition) count() total`, c.RunID)
	data, err := verificationRead(ctx, c, c.VictoriaLogsURL, url.Values{"query": {query}})
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	for rows := 0; ; rows++ {
		var row map[string]json.RawMessage
		err := decoder.Decode(&row)
		if err == io.EOF {
			break
		}
		if err != nil || rows >= 2 {
			result.Reason = "invalid backend count response"
			return result
		}
		var transition string
		if json.Unmarshal(row["alert.transition"], &transition) != nil || (transition != "firing" && transition != "resolved") {
			result.Reason = "invalid backend transition count"
			return result
		}
		if _, exists := result.ObservedTransitions[transition]; exists {
			result.Reason = "duplicate backend count row"
			return result
		}
		raw := string(row["total"])
		var text string
		if json.Unmarshal(row["total"], &text) == nil {
			raw = text
		}
		count, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || count > 1_000_000_000_000 {
			result.Reason = "invalid backend count"
			return result
		}
		result.ObservedTransitions[transition] = count
		result.Observed += count
	}
	result.Status = "matched"
	for transition, expected := range result.ExpectedTransitions {
		if result.ObservedTransitions[transition] != expected {
			result.Status = "mismatch"
		}
	}
	return result
}
func verify(ctx context.Context, c Config, r *Report, baseline snapshot) {
	if c.MetricsURL == "" && c.VictoriaLogsURL == "" {
		return
	}
	started := time.Now()
	defer func() { r.VerificationSeconds = time.Since(started).Seconds() }()
	if c.SettleTime > 0 {
		timer := time.NewTimer(c.SettleTime)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	if ctx.Err() != nil {
		if !r.Interrupted {
			r.FailureReasons = append(r.FailureReasons, "interrupted during verification")
		}
		r.Interrupted = true
		r.Failed = true
		if c.MetricsURL != "" {
			r.MetricsComparison = &MetricsComparison{Status: "inconclusive", Reasons: []string{"verification interrupted"}}
		}
		if c.VictoriaLogsURL != "" {
			result := queryBackend(ctx, c, *r)
			r.BackendCheck = &result
		}
		return
	}
	if c.MetricsURL != "" {
		after, err := scrape(ctx, c)
		if err != nil {
			r.MetricsComparison = &MetricsComparison{Status: "error", Reasons: []string{err.Error()}}
		} else {
			comparison := compareMetrics(baseline, after, *r)
			r.MetricsComparison = &comparison
		}
		if r.MetricsComparison.Status != "matched" {
			r.FailureReasons = append(r.FailureReasons, "receiver metrics comparison "+r.MetricsComparison.Status)
		}
	}
	if c.VictoriaLogsURL != "" {
		result := queryBackend(ctx, c, *r)
		r.BackendCheck = &result
		if result.Status != "matched" {
			r.FailureReasons = append(r.FailureReasons, "VictoriaLogs visibility check "+result.Status)
		}
	}
	if ctx.Err() != nil && !r.Interrupted {
		r.Interrupted = true
		r.FailureReasons = append(r.FailureReasons, "interrupted during verification")
	}
	r.Failed = len(r.FailureReasons) > 0
}
