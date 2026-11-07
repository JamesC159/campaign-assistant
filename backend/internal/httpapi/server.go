// Package httpapi is the HTTP layer: routing, middleware and handlers.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"campaign-assistant/backend/internal/auth"
	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/mail"
	"campaign-assistant/backend/internal/notify"
	"campaign-assistant/backend/internal/store"
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

	Store   *store.Store
	Auth    *auth.Service
	Events  *notify.Broker // live session status; nil disables push (clients still get the current status)
	Mailbox *mail.Mailbox  // mock mailbox, exposed at /dev/mailbox in dev; nil otherwise
}

// NewHandler builds the full HTTP handler with middleware applied.
// Middleware order (outermost first): request ID → access log → panic recovery
// → cross-origin protection → session auth → routes.
func NewHandler(d Deps) http.Handler {
	mux := http.NewServeMux()
	secure := d.Env == config.EnvProd

	// Infrastructure probes (load balancer / orchestrator).
	mux.HandleFunc("GET /healthz", handleLiveness)
	mux.HandleFunc("GET /readyz", handleReadiness(d))
	mux.HandleFunc("GET /api/health", handleHealth(d))

	if d.Store != nil && d.Auth != nil {
		authLimit := newIPLimiter(20, 10)
		mux.HandleFunc("POST /api/auth/signup", authLimit.wrap(handleSignup(d, secure)))
		mux.HandleFunc("POST /api/auth/login", authLimit.wrap(handleLogin(d, secure)))
		mux.HandleFunc("POST /api/auth/logout", handleLogout(d, secure))
		mux.HandleFunc("POST /api/auth/magic-link", authLimit.wrap(handleMagicLink(d)))
		mux.HandleFunc("POST /api/auth/magic-link/verify", authLimit.wrap(handleMagicLinkVerify(d, secure)))
		mux.HandleFunc("GET /api/me", authed(handleMe))

		mux.HandleFunc("GET /api/campaigns", authed(handleListCampaigns(d)))
		mux.HandleFunc("POST /api/campaigns", authed(handleCreateCampaign(d)))
		mux.HandleFunc("GET /api/campaigns/{campaign}", authed(handleGetCampaign(d)))
		mux.HandleFunc("PATCH /api/campaigns/{campaign}", authed(handleRenameCampaign(d)))
		mux.HandleFunc("DELETE /api/campaigns/{campaign}", authed(handleDeleteCampaign(d)))
		mux.HandleFunc("POST /api/campaigns/{campaign}/members", authed(handleAddMember(d)))
		mux.HandleFunc("DELETE /api/campaigns/{campaign}/members/{user}", authed(handleRemoveMember(d)))
		mux.HandleFunc("GET /api/campaigns/{campaign}/canon", authed(handleGetCanon(d)))
		mux.HandleFunc("GET /api/campaigns/{campaign}/recaps", authed(handleListPartyRecaps(d)))

		mux.HandleFunc("GET /api/campaigns/{campaign}/sessions", authed(handleListSessions(d)))
		mux.HandleFunc("POST /api/campaigns/{campaign}/sessions", authed(handleCreateSession(d)))
		mux.HandleFunc("GET /api/sessions/{session}", authed(handleGetSession(d)))
		mux.HandleFunc("GET /api/sessions/{session}/proposal", authed(handleGetProposal(d)))
		mux.HandleFunc("POST /api/sessions/{session}/approve", authed(handleApproveSession(d)))
		mux.HandleFunc("POST /api/sessions/{session}/reprocess", authed(handleReprocessSession(d)))
		mux.HandleFunc("GET /api/sessions/{session}/events", authed(handleSessionEvents(d)))
	}

	if d.Env == config.EnvDev && d.Mailbox != nil {
		mux.HandleFunc("GET /dev/mailbox", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"messages": d.Mailbox.Messages()})
		})
	}

	// Anything else under /api/ gets a JSON 404 instead of the mux's plain-text one.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	// Rejects cross-site browser requests to state-changing endpoints (Sec-Fetch-Site / Origin checks).
	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request rejected")
	}))

	var h http.Handler = mux
	h = withAuth(d.Auth, h)
	h = csrf.Handler(h)
	h = recoverPanics(d.Log, h)
	h = accessLog(d.Log, h)
	h = requestID(h)
	return h
}
