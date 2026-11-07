package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/time/rate"

	"campaign-assistant/backend/internal/auth"
)

const (
	sessionCookie = "ca_session"
	maxJSONBody   = 64 << 10 // 64 KiB for ordinary requests
	maxNotesBody  = 2 << 20  // 2 MiB for session notes
)

type userKey struct{}

// currentUser returns the signed-in user, if any.
func currentUser(ctx context.Context) (auth.User, bool) {
	u, ok := ctx.Value(userKey{}).(auth.User)
	return u, ok
}

// withAuth resolves the session cookie (if present) into a user on the context.
func withAuth(svc *auth.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && svc != nil {
			if u, err := svc.Authenticate(r.Context(), c.Value); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authed wraps a handler that needs a signed-in user.
func authed(h func(w http.ResponseWriter, r *http.Request, u auth.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "sign in to continue")
			return
		}
		h(w, r, u)
	}
}

// decodeJSON reads a JSON body into v, rejecting unknown fields, wrong content
// types (which also blocks form-based CSRF) and bodies over limit.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "send a JSON body with Content-Type: application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body is over %d bytes", limit))
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON for this endpoint: "+err.Error())
		}
		return false
	}
	if dec.More() || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be a single JSON object")
		return false
	}
	return true
}

// pathUUID parses a {name} path segment.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such "+name)
		return uuid.Nil, false
	}
	return id, true
}

// writeDBError maps database errors to API errors. Row-level security turns
// "not yours" into "not found" for reads and "forbidden" for writes.
func writeDBError(w http.ResponseWriter, r *http.Request, d Deps, err error, what string) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", what+" not found")
	case errors.As(err, &pgErr) && pgErr.Code == "42501":
		writeError(w, http.StatusForbidden, "forbidden", "you don't have permission to do that")
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeError(w, http.StatusConflict, "conflict", what+" already exists")
	default:
		d.Log.ErrorContext(r.Context(), "database error", "err", err, "request_id", RequestIDFrom(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal", "internal server error")
	}
}

func setSessionCookie(w http.ResponseWriter, token string, ttl time.Duration, secure bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is set in prod; dev serves plain http on 127.0.0.1
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see setSessionCookie
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

// ipLimiter rate-limits requests per client IP (used on auth endpoints).
type ipLimiter struct {
	mu      sync.Mutex
	every   rate.Limit
	burst   int
	clients map[string]*limiterEntry
	sweep   time.Time
}

type limiterEntry struct {
	l    *rate.Limiter
	seen time.Time
}

func newIPLimiter(perMinute, burst int) *ipLimiter {
	return &ipLimiter{every: rate.Limit(float64(perMinute) / 60), burst: burst, clients: map[string]*limiterEntry{}}
}

func (l *ipLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many attempts; try again in a minute")
			return
		}
		next(w, r)
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.sweep) > 10*time.Minute { // forget idle clients so the map can't grow forever
		for k, e := range l.clients {
			if now.Sub(e.seen) > 10*time.Minute {
				delete(l.clients, k)
			}
		}
		l.sweep = now
	}
	e, ok := l.clients[ip]
	if !ok {
		e = &limiterEntry{l: rate.NewLimiter(l.every, l.burst)}
		l.clients[ip] = e
	}
	e.seen = now
	return e.l.Allow()
}

// clientIP is the connection's address. Behind a load balancer this must be
// changed to trust the balancer's forwarded header (not done until deployment).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
