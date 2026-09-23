// Package logging configures the application's structured logger and
// provides helpers for carrying a request-scoped logger through a context.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

type ctxKey struct{}

// Config controls how the logger is built.
type Config struct {
	// Level is one of "debug", "info", "warn" or "error". Defaults to "info".
	Level string
	// Format is "json", "pretty" or "text". When empty it is "pretty" if the
	// output is a terminal and "json" otherwise, which gives readable local
	// logs and parseable production ones without either having to be
	// configured.
	Format string
}

// ConfigFromEnv reads the logger configuration from LOG_LEVEL and LOG_FORMAT.
func ConfigFromEnv() Config {
	return Config{
		Level:  os.Getenv("LOG_LEVEL"),
		Format: os.Getenv("LOG_FORMAT"),
	}
}

// New builds a *slog.Logger that writes to w using cfg.
func New(w io.Writer, cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}

	var handler slog.Handler
	switch {
	case strings.EqualFold(cfg.Format, "pretty"):
		handler = NewPrettyHandler(w, opts)
	case strings.EqualFold(cfg.Format, "text"):
		handler = slog.NewTextHandler(w, opts)
	case strings.EqualFold(cfg.Format, "json"):
		handler = slog.NewJSONHandler(w, opts)
	case shouldColor(w):
		// Unset and attached to a terminal: someone is watching.
		handler = NewPrettyHandler(w, opts)
	default:
		handler = slog.NewJSONHandler(w, opts)
	}

	return slog.New(handler)
}

func parseLevel(s string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return level
}

// WithContext returns a copy of ctx carrying logger.
func WithContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, logger)
}

// FromContext returns the logger stored in ctx, or slog.Default() if none.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}
