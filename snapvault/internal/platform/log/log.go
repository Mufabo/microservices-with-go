// Package log builds the structured logger used by every Snapvault service.
package log

import (
	"log/slog"
	"os"
)

// New returns a JSON logger tagged with the service name. Set LOG_LEVEL=debug
// to include debug records. JSON to stdout is what Cloud Logging ingests
// automatically on GKE, so there is no logging agent to install.
func New(service string) *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler).With("service", service)
}
