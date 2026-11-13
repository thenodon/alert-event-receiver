package slaproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
)

// Service owns an independent configuration, registry and HTTP lifecycle.
// Receiver processing/telemetry are independent. Valkey is used only when durable processing is enabled.
type Service struct {
	cfg        Config
	logger     *slog.Logger
	metrics    *metrics
	enricher   NoopEnricher
	evaluator  PolicyEvaluator
	now        func() time.Time
	ready      atomic.Bool
	handler    http.Handler
	durable    *DurableManager
	dispatcher *DeliveryDispatcher
	closeOnce  sync.Once
	closeErr   error
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
	if cfg.Durable.Enabled {
		password := ""
		if name := cfg.Durable.Valkey.PasswordEnv; name != "" {
			var exists bool
			password, exists = os.LookupEnv(name)
			if !exists || password == "" {
				return nil, fmt.Errorf("durable Valkey password environment variable must be set")
			}
		}
		options := &redis.Options{Addr: cfg.Durable.Valkey.Address, Username: cfg.Durable.Valkey.Username, Password: password,
			DB: cfg.Durable.Valkey.Database, MaxRetries: -1, ContextTimeoutEnabled: true, DialTimeout: cfg.Durable.Scheduler.OperationTimeout,
			ReadTimeout: cfg.Durable.Scheduler.OperationTimeout, WriteTimeout: cfg.Durable.Scheduler.OperationTimeout}
		if cfg.Durable.Valkey.TLS {
			options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		client := redis.NewClient(options)
		store := newDurableStore(client, cfg, engine, s.metrics)
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Durable.Scheduler.OperationTimeout)
		err := client.Ping(ctx).Err()
		if err == nil {
			err = store.initSchema(ctx)
			if err == nil {
				err = store.activate(ctx)
			}
			if err == nil && cfg.Delivery.Enabled {
				err = store.activateSink(ctx)
			}
		}
		cancel()
		if err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("initialize SLA state store: %w", err)
		}
		s.durable = &DurableManager{store: store, engine: engine, cfg: cfg, logger: logger}
		if cfg.Delivery.Enabled {
			s.dispatcher = &DeliveryDispatcher{manager: s.durable, sink: LogSink{Handler: logger.Handler()}, metrics: s.metrics.delivery}
		}
	}
	s.ready.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc(cfg.WebhookPath, s.webhook)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if s.durable != nil {
			ctx, cancel := context.WithTimeout(r.Context(), cfg.Durable.Scheduler.OperationTimeout)
			defer cancel()
			if err := s.durable.store.health(ctx); err != nil {
				http.Error(w, "state store not ready", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = fmt.Fprintf(w, "{\"mode\":%q,\"ready\":true}\n", s.mode())
	})
	s.handler = mux
	return s, nil
}

func (s *Service) Handler() http.Handler { return s.handler }

func (s *Service) mode() string {
	if s.cfg.Delivery.Enabled {
		return "delivery_enabled"
	}
	if s.durable != nil {
		return "durable_state_only"
	}
	return "observe_only"
}
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		if s.durable != nil {
			s.closeErr = s.durable.store.client.Close()
		}
	})
	return s.closeErr
}

func (s *Service) ListenAndServe(ctx context.Context) error {
	defer s.Close()
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
	defer s.Close()
	writeTimeout := 15 * time.Second
	if s.durable != nil {
		writeTimeout = max(writeTimeout, s.cfg.Durable.Scheduler.OperationTimeout+time.Second)
	}
	server := &http.Server{
		Handler: s.handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: writeTimeout,
		IdleTimeout: 60 * time.Second,
	}
	s.logger.Info("SLA proxy starting",
		"component", "sla_proxy", "mode", s.mode(), "calendar_evaluation", true, "delivery_enabled", s.cfg.Delivery.Enabled)
	runtimeCtx, stopRuntime := context.WithCancel(ctx)
	defer stopRuntime()
	var runtimeDone chan error
	if s.durable != nil {
		runtimeDone = make(chan error, 1)
		go func() { runtimeDone <- s.runDurable(runtimeCtx) }()
	}
	s.logger.Info("starting HTTP server", "component", "http", "operation", "serve", "addr", listener.Addr().String())
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var serveErr error
	var runtimeErr error
	runtimeReturned := false
	returned := false
	select {
	case serveErr = <-done:
		returned = true
	case <-ctx.Done():
	case runtimeErr = <-runtimeDone:
		runtimeReturned = true
	}
	s.ready.Store(false)
	stopRuntime()
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
	if runtimeDone != nil && !runtimeReturned {
		select {
		case runtimeErr = <-runtimeDone:
		case <-shutdownCtx.Done():
			runtimeErr = shutdownCtx.Err()
		}
	}
	if err := errors.Join(serveErr, shutdownErr, runtimeErr); err != nil {
		s.logger.Error("HTTP server stopped with error", "component", "http", "operation", "shutdown", "error", err.Error())
		return err
	}
	s.logger.Info("HTTP shutdown completed", "component", "http", "operation", "shutdown")
	return nil
}

func (s *Service) runDurable(ctx context.Context) error {
	if s.dispatcher == nil {
		return s.durable.Run(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- s.durable.Run(ctx) }()
	go func() { done <- s.dispatcher.Run(ctx) }()
	first := <-done
	cancel()
	second := <-done
	return errors.Join(first, second)
}
