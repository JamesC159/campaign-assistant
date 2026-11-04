package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"campaign-assistant/backend/internal/config"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func newTestHandler(t *testing.T, db Pinger) (http.Handler, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	h := NewHandler(Deps{
		Log:      slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DB:       db,
		Env:      config.EnvDev,
		Provider: config.ProviderMock,
		Version:  "test",
	})
	return h, &logs
}

func get(h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthOK(t *testing.T) {
	h, _ := newTestHandler(t, fakeDB{})
	rec := get(h, "/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Database.Status != "up" || body.Provider != "mock" || body.Version != "test" {
		t.Fatalf("unexpected body: %+v", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("health must not be cached")
	}
}

func TestHealthDegradedWhenDBDown(t *testing.T) {
	h, _ := newTestHandler(t, fakeDB{err: errors.New("connection refused")})
	rec := get(h, "/api/health")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	var body HealthResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Status != "degraded" || body.Database.Status != "down" {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestProbes(t *testing.T) {
	up, _ := newTestHandler(t, fakeDB{})
	down, _ := newTestHandler(t, fakeDB{err: errors.New("nope")})

	if rec := get(down, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("liveness must not depend on the DB, got %d", rec.Code)
	}
	if rec := get(up, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("readyz with DB up = %d", rec.Code)
	}
	if rec := get(down, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz with DB down = %d", rec.Code)
	}
}

func TestUnknownAPIRouteIsJSON404(t *testing.T) {
	h, _ := newTestHandler(t, fakeDB{})
	rec := get(h, "/api/does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	var body ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "not_found" {
		t.Fatalf("want JSON not_found error, got %q (%v)", rec.Body.String(), err)
	}
}

func TestRequestID(t *testing.T) {
	h, logs := newTestHandler(t, fakeDB{})

	rec := get(h, "/api/health", RequestIDHeader, "abc-123")
	if got := rec.Header().Get(RequestIDHeader); got != "abc-123" {
		t.Errorf("valid incoming ID should be reused, got %q", got)
	}
	if !strings.Contains(logs.String(), "request_id=abc-123") {
		t.Errorf("request ID missing from access log: %s", logs.String())
	}

	rec = get(h, "/api/health", RequestIDHeader, "bad id with spaces\n")
	if got := rec.Header().Get(RequestIDHeader); got == "" || strings.Contains(got, " ") {
		t.Errorf("invalid incoming ID should be replaced, got %q", got)
	}
}

func TestRecoverPanics(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	h := requestID(accessLog(log, recoverPanics(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))))

	rec := get(h, "/anything")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("panic details must not leak to the client")
	}
	if !strings.Contains(logs.String(), "boom") {
		t.Error("panic should be logged")
	}
}
