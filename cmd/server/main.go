package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/andersh/alertmanager_event_receiver/internal/config"
	"github.com/andersh/alertmanager_event_receiver/internal/httpsecurity"
	"github.com/andersh/alertmanager_event_receiver/internal/processor"
	"github.com/andersh/alertmanager_event_receiver/internal/state"
	"github.com/andersh/alertmanager_event_receiver/internal/telemetry"
	"github.com/andersh/alertmanager_event_receiver/internal/webhook"
)

// version is set at build time via -ldflags "-X main.version=<value>".
var version = "unknown"

func main() {
	printVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *printVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	os.Exit(run())
}

// run returns only after deferred resource cleanup has completed.
func run() (exitCode int) {
	logger := telemetry.NewLogger()
	telemetry.ConfigureSDKDiagnostics(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "component", "config", "operation", "load", "error", telemetry.SafeError(err))
		return 1
	}

	security, err := httpsecurity.LoadFromEnv()
	if err != nil {
		logger.Error("invalid HTTP security configuration", "component", "http", "operation", "configure", "error", err.Error())
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Initialise OTel log SDK.
	// Endpoint and credentials come from standard OTel env vars (OTEL_EXPORTER_OTLP_ENDPOINT, etc.).
	otelShutdown, err := telemetry.InitOTel(ctx, cfg.OTelServiceName, logger)
	if err != nil {
		logger.Error("failed to initialise OTel", "component", "otel_sdk", "operation", "initialize", "error", telemetry.SafeError(err))
		return 1
	}
	defer func() {
		if err := shutdownOTel(logger, otelShutdown, 5*time.Second); err != nil {
			exitCode = 1
		}
	}()

	// Connect to Redis / Valkey.
	redisClient := redis.NewClient(&redis.Options{
		Addr:                  cfg.RedisAddr,
		Password:              cfg.RedisPassword,
		DB:                    cfg.RedisDB,
		ContextTimeoutEnabled: true,
	})
	defer func() {
		if err := redisClient.Close(); err != nil {
			logger.Error("Valkey close failed", "component", "valkey", "operation", "close", "error", telemetry.SafeError(err))
		}
	}()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Error("Valkey ping failed", "component", "valkey", "operation", "ping", "error", telemetry.SafeError(err))
		return 1
	}
	logger.Info("Redis connected", slog.String("addr", cfg.RedisAddr))

	// Wire dependencies.
	store := state.NewRedisStore(redisClient)
	emitter := telemetry.NewOTelEmitter("alert-event-receiver", version)
	proc := processor.New(store, emitter, cfg.ClosedStateTTL, cfg.IdempotencyTTL, logger)

	srv := newHTTPServer(cfg.Address, security, webhook.NewHandler(proc, logger), func(ctx context.Context) error {
		return redisClient.Ping(ctx).Err()
	})

	serveDone := make(chan error, 1)
	go func() {
		logger.Info("starting HTTP server", "component", "http", "operation", "serve", slog.String("addr", srv.Addr), "scheme", security.Scheme())
		serveDone <- security.ListenAndServe(srv)
	}()

	var serveErr error
	serveReturned := false
	select {
	case serveErr = <-serveDone:
		serveReturned = true
	case <-ctx.Done():
	}
	logger.Info("stopping HTTP server", "component", "http", "operation", "shutdown")

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Error("HTTP shutdown error", "component", "http", "operation", "shutdown", "error", telemetry.SafeError(err))
		_ = srv.Close()
		exitCode = 1
	}
	// Shutdown (or Close on timeout) stops ListenAndServe. Check its result even
	// if a signal arrived concurrently with an unexpected listener failure.
	if !serveReturned {
		select {
		case serveErr = <-serveDone:
		case <-shutCtx.Done():
			logger.Error("HTTP server did not stop within shutdown timeout", "component", "http", "operation", "shutdown", "error", telemetry.SafeError(shutCtx.Err()))
			exitCode = 1
		}
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		logger.Error("server error", "component", "http", "operation", "serve", "error", telemetry.SafeError(serveErr))
		exitCode = 1
	}
	return exitCode
}

// Keep routing and server timeouts common to production and listener tests.
func newHTTPServer(address string, security httpsecurity.Config, webhookHandler http.Handler, pingValkey func(context.Context) error) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("POST /webhook", security.Webhook(webhookHandler))
	mux.Handle("GET /metrics", security.Metrics(promhttp.Handler()))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := pingValkey(ctx); err != nil {
			http.Error(w, "state store not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return &http.Server{
		Addr: address, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func shutdownOTel(logger *slog.Logger, shutdown func(context.Context) error, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	logger.Info("flushing and shutting down OpenTelemetry", "component", "otel_sdk", "operation", "shutdown", "timeout", timeout.String())
	err := shutdown(ctx)
	if err != nil {
		logger.Error("OpenTelemetry shutdown failed", "component", "otel_sdk", "operation", "shutdown", "error", telemetry.SafeError(err))
	} else {
		logger.Info("OpenTelemetry shutdown completed", "component", "otel_sdk", "operation", "shutdown")
	}
	return err
}
