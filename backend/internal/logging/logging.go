// Package logging builds the application's structured logger.
package logging

import (
	"io"
	"log/slog"

	"campaign-assistant/backend/internal/config"
)

// New returns a JSON logger in prod (for log aggregation) and a human-readable
// text logger in dev.
func New(env config.Env, level slog.Level, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if env == config.EnvProd {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
