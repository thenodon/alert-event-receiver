package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration sourced from environment variables.
type Config struct {
	// HTTP
	Address string

	// Redis / Valkey
	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	ClosedStateTTL time.Duration // TTL for closed-state tombstone
	IdempotencyTTL time.Duration // TTL for idempotency keys

	// OTel — endpoint/headers/TLS are read from standard OTel env vars by the SDK.
	// See: https://opentelemetry.io/docs/specs/otel/protocol/exporter/
	OTelProtocol    string // OTEL_EXPORTER_OTLP_PROTOCOL: grpc (default) or http/protobuf
	OTelServiceName string // OTEL_SERVICE_NAME
}

// Load reads configuration from environment variables, applying defaults where not set.
func Load() (*Config, error) {
	c := &Config{
		Address:         getEnv("ADDRESS", ":9011"),
		RedisAddr:       getEnv("REDIS_ADDR", "localhost:6379"),
		RedisPassword:   getEnv("REDIS_PASSWORD", ""),
		ClosedStateTTL:  getDuration("CLOSED_STATE_TTL", 24*time.Hour),
		IdempotencyTTL:  getDuration("IDEMPOTENCY_TTL", 7*24*time.Hour),
		OTelProtocol:    getEnv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc"),
		OTelServiceName: getEnv("OTEL_SERVICE_NAME", "alert-event-receiver"),
	}

	var err error
	c.RedisDB, err = strconv.Atoi(getEnv("REDIS_DB", "0"))
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_DB: %w", err)
	}

	return c, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

