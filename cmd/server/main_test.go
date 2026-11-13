package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestShutdownOTel(t *testing.T) {
	for _, mode := range []string{"success", "error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			sentinel := errors.New("flush failed")
			called := 0
			start := time.Now()
			err := shutdownOTel(logger, func(ctx context.Context) error {
				called++
				if _, ok := ctx.Deadline(); !ok {
					t.Error("shutdown context has no bound")
				}
				switch mode {
				case "error":
					return sentinel
				case "timeout":
					<-ctx.Done()
					return ctx.Err()
				default:
					return nil
				}
			}, 10*time.Millisecond)
			if called != 1 {
				t.Fatal("shutdown not called once")
			}
			switch mode {
			case "success":
				if err != nil || !strings.Contains(logs.String(), "shutdown completed") {
					t.Fatal(err, logs.String())
				}
			case "error":
				if !errors.Is(err, sentinel) {
					t.Fatal(err)
				}
			case "timeout":
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
					t.Fatal("shutdown unbounded", err)
				}
			}
			if mode != "success" && !strings.Contains(logs.String(), `"operation":"shutdown"`) {
				t.Fatal("missing shutdown failure context")
			}
		})
	}
}

func TestStartupValkeyFailureRunsOTelCleanup(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.SetError("ERR unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStartupFailureHelper$")
	cmd.Env = append(os.Environ(), "ALERTRECEIVER_STARTUP_FAILURE_TEST=1", "REDIS_ADDR="+mr.Addr(), "REDIS_PASSWORD=", "REDIS_DB=0", "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf", "OTEL_EXPORTER_OTLP_HEADERS=", "OTEL_EXPORTER_OTLP_LOGS_HEADERS=", "OTEL_RESOURCE_ATTRIBUTES=")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("startup subprocess: %v\n%s", err, output)
	}
	text := string(output)
	failure := strings.Index(text, "Valkey ping failed")
	cleanup := strings.Index(text, "OpenTelemetry shutdown completed")
	if failure < 0 || cleanup <= failure {
		t.Fatalf("startup error bypassed provider cleanup: %s", text)
	}
}

func TestStartupFailureHelper(t *testing.T) {
	if os.Getenv("ALERTRECEIVER_STARTUP_FAILURE_TEST") != "1" {
		return
	}
	if code := run(); code != 1 {
		t.Fatalf("startup failure returned %d", code)
	}
}

func TestPortBindingFailureExitsNonzeroAndRunsCleanup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPortBindingFailureHelper$")
	cmd.Env = append(os.Environ(), "ALERTRECEIVER_BIND_FAILURE_TEST=1", "ADDRESS="+listener.Addr().String(), "REDIS_ADDR="+mr.Addr(), "REDIS_PASSWORD=", "REDIS_DB=0", "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf", "OTEL_EXPORTER_OTLP_HEADERS=", "OTEL_EXPORTER_OTLP_LOGS_HEADERS=", "OTEL_RESOURCE_ATTRIBUTES=")
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("binding failure should exit 1, got %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, `"operation":"serve"`) || !strings.Contains(text, "address already in use") {
		t.Fatalf("listener failure not logged: %s", text)
	}
	if !strings.Contains(text, "OpenTelemetry shutdown completed") {
		t.Fatalf("listener failure bypassed cleanup: %s", text)
	}
	if strings.Contains(text, "shutting down gracefully") {
		t.Fatalf("listener failure reported as graceful shutdown: %s", text)
	}
}

func TestPortBindingFailureHelper(t *testing.T) {
	if os.Getenv("ALERTRECEIVER_BIND_FAILURE_TEST") != "1" {
		return
	}
	os.Exit(run())
}
