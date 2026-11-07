package httpapi

import (
	"errors"
	"net/http"

	"campaign-assistant/backend/internal/auth"
)

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	User auth.User `json:"user"`
}

func handleSignup(d Deps, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in credentials
		if !decodeJSON(w, r, &in, maxJSONBody) {
			return
		}
		u, token, err := d.Auth.Signup(r.Context(), in.Email, in.Password)
		switch {
		case errors.Is(err, auth.ErrInvalidEmail), errors.Is(err, auth.ErrWeakPassword):
			writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		case errors.Is(err, auth.ErrEmailTaken):
			writeError(w, http.StatusConflict, "email_taken", err.Error())
		case err != nil:
			writeDBError(w, r, d, err, "account")
		default:
			setSessionCookie(w, token, d.Auth.SessionTTLValue(), secure)
			writeJSON(w, http.StatusCreated, userResponse{u})
		}
	}
}

func handleLogin(d Deps, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in credentials
		if !decodeJSON(w, r, &in, maxJSONBody) {
			return
		}
		u, token, err := d.Auth.Login(r.Context(), in.Email, in.Password)
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "invalid_credentials", err.Error())
		case err != nil:
			writeDBError(w, r, d, err, "account")
		default:
			setSessionCookie(w, token, d.Auth.SessionTTLValue(), secure)
			writeJSON(w, http.StatusOK, userResponse{u})
		}
	}
}

func handleLogout(d Deps, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if err := d.Auth.Logout(r.Context(), c.Value); err != nil {
				writeDBError(w, r, d, err, "session")
				return
			}
		}
		clearSessionCookie(w, secure)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleMagicLink(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Email string `json:"email"`
		}
		if !decodeJSON(w, r, &in, maxJSONBody) {
			return
		}
		err := d.Auth.SendMagicLink(r.Context(), in.Email)
		if errors.Is(err, auth.ErrInvalidEmail) {
			writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
			return
		}
		if err != nil {
			writeDBError(w, r, d, err, "sign-in link")
			return
		}
		// Same answer whether or not the account existed.
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent"})
	}
}

func handleMagicLinkVerify(d Deps, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Token string `json:"token"`
		}
		if !decodeJSON(w, r, &in, maxJSONBody) {
			return
		}
		u, token, err := d.Auth.VerifyMagicLink(r.Context(), in.Token)
		if errors.Is(err, auth.ErrInvalidLink) {
			writeError(w, http.StatusUnauthorized, "invalid_link", err.Error())
			return
		}
		if err != nil {
			writeDBError(w, r, d, err, "sign-in link")
			return
		}
		setSessionCookie(w, token, d.Auth.SessionTTLValue(), secure)
		writeJSON(w, http.StatusOK, userResponse{u})
	}
}

func handleMe(w http.ResponseWriter, _ *http.Request, u auth.User) {
	writeJSON(w, http.StatusOK, userResponse{u})
}
