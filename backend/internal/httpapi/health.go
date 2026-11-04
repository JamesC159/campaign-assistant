package httpapi

import (
	"context"
	"net/http"
	"time"
)

const dbPingTimeout = 2 * time.Second

// HealthResponse is the body of GET /api/health. Keep it in sync with api/openapi.yaml.
type HealthResponse struct {
	Status   string          `json:"status"` // "ok" | "degraded"
	Version  string          `json:"version"`
	Env      string          `json:"env"`
	Provider string          `json:"provider"`
	Database ComponentHealth `json:"database"`
}

// ComponentHealth is the health of one dependency.
type ComponentHealth struct {
	Status    string `json:"status"` // "up" | "down"
	LatencyMS int64  `json:"latency_ms"`
}

func handleLiveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func handleReadiness(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if db := checkDB(r.Context(), d); db.Status != "up" {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ready\n"))
	}
}

func handleHealth(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp := HealthResponse{
			Status:   "ok",
			Version:  d.Version,
			Env:      string(d.Env),
			Provider: string(d.Provider),
			Database: checkDB(r.Context(), d),
		}
		code := http.StatusOK
		if resp.Database.Status != "up" {
			resp.Status = "degraded"
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, code, resp)
	}
}

func checkDB(ctx context.Context, d Deps) ComponentHealth {
	ctx, cancel := context.WithTimeout(ctx, dbPingTimeout)
	defer cancel()
	start := time.Now()
	err := d.DB.Ping(ctx)
	h := ComponentHealth{Status: "up", LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		h.Status = "down"
		d.Log.WarnContext(ctx, "database health check failed", "err", err)
	}
	return h
}
