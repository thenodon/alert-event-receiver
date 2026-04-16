package config_test

import (
	"testing"
	"time"

	"github.com/andersh/alertmanager_event_receiver/internal/config"
)

func TestDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != ":9011" {
		t.Errorf("expected address :9011, got %s", cfg.Address)
	}
	if cfg.ClosedStateTTL != 24*time.Hour {
		t.Errorf("expected 24h closed TTL, got %v", cfg.ClosedStateTTL)
	}
	if cfg.IdempotencyTTL != 7*24*time.Hour {
		t.Errorf("expected 168h idempotency TTL, got %v", cfg.IdempotencyTTL)
	}
	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("expected localhost:6379, got %s", cfg.RedisAddr)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("ADDRESS", "127.0.0.1:8080")
	t.Setenv("REDIS_ADDR", "valkey:6379")
	t.Setenv("REDIS_DB", "2")
	t.Setenv("CLOSED_STATE_TTL", "48h")
	t.Setenv("OTEL_SERVICE_NAME", "my-receiver")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != "127.0.0.1:8080" {
		t.Errorf("expected 127.0.0.1:8080, got %s", cfg.Address)
	}
	if cfg.RedisAddr != "valkey:6379" {
		t.Errorf("expected valkey:6379, got %s", cfg.RedisAddr)
	}
	if cfg.RedisDB != 2 {
		t.Errorf("expected DB 2, got %d", cfg.RedisDB)
	}
	if cfg.ClosedStateTTL != 48*time.Hour {
		t.Errorf("expected 48h, got %v", cfg.ClosedStateTTL)
	}
	if cfg.OTelServiceName != "my-receiver" {
		t.Errorf("expected my-receiver, got %s", cfg.OTelServiceName)
	}
}

func TestAddressSupportsColonOnly(t *testing.T) {
	t.Setenv("ADDRESS", ":8080")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != ":8080" {
		t.Errorf("expected :8080, got %s", cfg.Address)
	}
}

func TestInvalidRedisDB(t *testing.T) {
	t.Setenv("REDIS_DB", "notanumber")
	_, err := config.Load()
	if err == nil {
		t.Error("expected error for invalid REDIS_DB")
	}
}

