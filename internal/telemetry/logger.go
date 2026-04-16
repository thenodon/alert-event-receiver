package telemetry

import (
	"log/slog"
	"os"
)

// NewLogger returns a structured JSON logger writing to stdout.
// Log level defaults to INFO and can be adjusted by extending this function
// if a LOG_LEVEL env var is added to config in future.
func NewLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

