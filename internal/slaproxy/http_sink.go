package slaproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/models"
)

const maxHTTPResponseBody = 64 << 10
const maxRetryAfter = 24 * time.Hour

type HTTPSink struct {
	config             HTTPSinkConfig
	client             *http.Client
	logger             *slog.Logger
	metrics            *deliveryMetrics
	username, password string
}

func newDeliveryHTTPClient(workers int) (*http.Client, *http.Transport) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxResponseHeaderBytes = 64 << 10
	transport.MaxIdleConns = max(100, workers*4)
	transport.MaxIdleConnsPerHost = max(2, workers*2)
	// HTTP/1 with a non-rewindable body prevents net/http's transparent replay of
	// a POST carrying Idempotency-Key. The dispatcher owns every retry attempt.
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, transport
}
func newHTTPSink(cfg HTTPSinkConfig, client *http.Client, logger *slog.Logger, metrics *deliveryMetrics) (*HTTPSink, error) {
	sink := &HTTPSink{config: cfg, client: client, logger: logger, metrics: metrics}
	if cfg.BasicAuth.Username != "" || cfg.BasicAuth.Password != "" {
		var userErr, passwordErr error
		sink.username, userErr = resolveCredential(cfg.BasicAuth.Username)
		sink.password, passwordErr = resolveCredential(cfg.BasicAuth.Password)
		if userErr != nil || passwordErr != nil || sink.username == "" || sink.password == "" || strings.ContainsAny(sink.username, ":\r\n") || strings.ContainsAny(sink.password, "\r\n") {
			return nil, errors.New("HTTP Basic Authentication credentials are missing or invalid")
		}
	}
	return sink, nil
}

// singleAlertPayload uses only this occurrence's frozen notification. Empty map
// containers are normalized for the wire format; no labels or annotations are
// derived from other alerts or injected by SLA processing.
func singleAlertPayload(e DeliveryEvent) models.WebhookPayload {
	p := e.Notification.Metadata
	a := e.Notification.Alert
	a.Status = e.Action
	p.Status = e.Action
	p.Alerts = []models.Alert{a}
	if p.GroupLabels == nil {
		p.GroupLabels = map[string]string{}
	}
	if p.CommonLabels == nil {
		p.CommonLabels = map[string]string{}
	}
	if p.CommonAnnotations == nil {
		p.CommonAnnotations = map[string]string{}
	}
	if p.Alerts[0].Annotations == nil {
		p.Alerts[0].Annotations = map[string]string{}
	}
	return p
}
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > uint64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return min(maxRetryAfter, max(time.Duration(0), date.Sub(now)))
	}
	return 0
}
func (s *HTTPSink) Deliver(parent context.Context, e DeliveryEvent) (result error) {
	payload, err := json.Marshal(singleAlertPayload(e))
	if err != nil {
		return &DeliveryError{Permanent: true, Cause: errors.New("webhook encoding failed")}
	}
	ctx, cancel := context.WithTimeout(parent, s.config.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.URL, bytes.NewReader(payload))
	if err != nil {
		return &DeliveryError{Permanent: true, Cause: errors.New("invalid webhook request")}
	}
	request.GetBody = nil // Never allow automatic replay by the shared transport.
	request.Header.Set("Content-Type", "application/json")
	if s.config.IdempotencyKey == nil || *s.config.IdempotencyKey {
		request.Header.Set("Idempotency-Key", e.IntentID)
	}
	if s.username != "" {
		request.SetBasicAuth(s.username, s.password)
	}
	began := time.Now()
	status := 0
	defer func() {
		elapsed := time.Since(began).Seconds()
		outcome := "success"
		if result != nil {
			outcome = deliveryFailure(result)
		}
		class := "network_error"
		if status > 0 {
			class = strconv.Itoa(status/100) + "xx"
		}
		if s.metrics != nil {
			s.metrics.httpRequests.WithLabelValues(e.SinkID, class, outcome).Inc()
			s.metrics.httpDuration.WithLabelValues(e.SinkID).Observe(elapsed)
		}
		if s.logger != nil {
			s.logger.InfoContext(parent, "SLA HTTP request completed", "component", "sla_sink", "sink_id", e.SinkID, "sink_type", "http", "intent_id", e.IntentID, "occurrence_id", e.OccurrenceID, "binding_id", e.BindingID, "action", e.Action, "attempt", e.Attempt, "http_status", status, "duration_seconds", elapsed, "outcome", outcome)
		}
	}()
	response, err := s.client.Do(request)
	if err != nil {
		// Discard url.Error and transport details: URLs and proxy/authentication data
		// can contain secrets. Cancellation/timeout remain retryable ambiguous calls.
		return &DeliveryError{Cause: errors.New("HTTP transport, cancellation or timeout failure")}
	}
	defer response.Body.Close()
	status = response.StatusCode
	// Drain a bounded amount for keep-alive reuse. API acceptance is determined by
	// status, not by an oversized, truncated or discarded response body.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxHTTPResponseBody+1))
	if status >= 200 && status < 300 {
		return nil
	}
	failure := &DeliveryError{HTTPStatus: status, Permanent: !(status == 408 || status == 429 || (status >= 500 && status < 600)), Cause: errors.New("HTTP destination rejected the request")}
	if status == 429 || status == 503 {
		failure.RetryAfter = retryAfter(response.Header.Get("Retry-After"), time.Now())
	}
	return failure
}
