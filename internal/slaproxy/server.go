package slaproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Service owns an independent configuration, registry and HTTP lifecycle.
// It has no dependency on receiver processing, receiver telemetry or Valkey.
type Service struct {
	cfg       Config
	logger    *slog.Logger
	metrics   *metrics
	enricher  NoopEnricher
	evaluator PolicyEvaluator
	now       func() time.Time
	ready     atomic.Bool
	handler   http.Handler
}

func New(cfg Config, logger *slog.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, fmt.Errorf("logger is required")
	}
	s := &Service{cfg: cfg, logger: logger, metrics: newMetrics(cfg)}
	engine, err := NewCalendarEngine(cfg, observedHolidays{provider: CountryHolidays{}, metrics: s.metrics})
	if err != nil {
		return nil, err
	}
	s.evaluator, s.now = engine, time.Now
	s.ready.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.WebhookPath, s.webhook)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mode":"observe_only","ready":true}` + "\n"))
	})
	s.handler = mux
	return s, nil
}

func (s *Service) Handler() http.Handler { return s.handler }

func (s *Service) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		s.ready.Store(false)
		return fmt.Errorf("listen: %w", err)
	}
	return s.Serve(ctx, listener)
}

// Serve owns listener. Cancellation stops readiness and drains existing requests
// with a fresh bounded context; unexpected listener errors remain failures.
func (s *Service) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{
		Handler: s.handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
	s.logger.Warn("SLA proxy observation mode: alerts are not persisted or forwarded",
		"component", "sla_proxy", "mode", "observe_only", "calendar_evaluation", true, "delivery_enforced", false)
	s.logger.Info("starting HTTP server", "component", "http", "operation", "serve", "addr", listener.Addr().String())
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var serveErr error
	returned := false
	select {
	case serveErr = <-done:
		returned = true
	case <-ctx.Done():
	}
	s.ready.Store(false)
	s.logger.Info("stopping HTTP server", "component", "http", "operation", "shutdown")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	if !returned {
		select {
		case serveErr = <-done:
		case <-shutdownCtx.Done():
			serveErr = shutdownCtx.Err()
		}
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if err := errors.Join(serveErr, shutdownErr); err != nil {
		s.logger.Error("HTTP server stopped with error", "component", "http", "operation", "shutdown", "error", err.Error())
		return err
	}
	s.logger.Info("HTTP shutdown completed", "component", "http", "operation", "shutdown")
	return nil
}
