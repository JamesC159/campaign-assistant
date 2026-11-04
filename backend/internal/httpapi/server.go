// Package httpapi is the HTTP layer: routing, middleware and handlers.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"campaign-assistant/backend/internal/config"
)

// Pinger reports whether the database is reachable. *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the handler dependencies, injected so tests can supply fakes.
type Deps struct {
	Log      *slog.Logger
	DB       Pinger
	Env      config.Env
	Provider config.Provider
	Version  string
}

// NewHandler builds the full HTTP handler with middleware applied.
// Middleware order (outermost first): request ID → access log → panic recovery → routes.
func NewHandler(d Deps) http.Handler {
	mux := http.NewServeMux()

	// Infrastructure probes (load balancer / orchestrator).
	mux.HandleFunc("GET /healthz", handleLiveness)
	mux.HandleFunc("GET /readyz", handleReadiness(d))

	// Public API.
	mux.HandleFunc("GET /api/health", handleHealth(d))

	// Anything else under /api/ gets a JSON 404 instead of the mux's plain-text one.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	var h http.Handler = mux
	h = recoverPanics(d.Log, h)
	h = accessLog(d.Log, h)
	h = requestID(h)
	return h
}
