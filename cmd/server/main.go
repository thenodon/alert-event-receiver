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

	logger := telemetry.NewLogger()

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", slog.Any("error", err))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Initialise OTel log SDK.
	// Endpoint and credentials come from standard OTel env vars (OTEL_EXPORTER_OTLP_ENDPOINT, etc.).
	otelShutdown, err := telemetry.InitOTel(ctx, cfg.OTelServiceName)
	if err != nil {
		logger.Error("failed to initialise OTel", slog.Any("error", err))
		os.Exit(1)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(shutCtx); err != nil {
			logger.Error("OTel shutdown error", slog.Any("error", err))
		}
	}()

	// Connect to Redis / Valkey.
	redisClient := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	if err := redisClient.Ping(ctx).Err(); err != nil {
		logger.Error("Redis ping failed", slog.String("addr", cfg.RedisAddr), slog.Any("error", err))
		os.Exit(1)
	}
	logger.Info("Redis connected", slog.String("addr", cfg.RedisAddr))

	// Wire dependencies.
	store := state.NewRedisStore(redisClient)
	emitter := telemetry.NewOTelEmitter("alert-event-receiver", version)
	proc := processor.New(store, emitter, cfg.ClosedStateTTL, cfg.IdempotencyTTL, logger)

	// HTTP routes.
	mux := http.NewServeMux()
	mux.Handle("POST /webhook", webhook.NewHandler(proc, logger))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:         cfg.Address,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("server started", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", slog.Any("error", err))
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down gracefully")

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Error("HTTP shutdown error", slog.Any("error", err))
	}
}
