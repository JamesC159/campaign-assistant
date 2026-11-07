package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"campaign-assistant/backend/internal/auth"
	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/canonstore"
	"campaign-assistant/backend/internal/store/dbq"
)

type campaignJSON struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	OwnerID   uuid.UUID `json:"owner_id"`
	Role      string    `json:"role"` // your role: gm | player
	CreatedAt time.Time `json:"created_at"`
}

type memberJSON struct {
	UserID    uuid.UUID `json:"user_id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// errNotGM is returned inside a transaction to abort with 403.
var errNotGM = errors.New("gm only")

// requireGM fails with errNotGM unless the user is a GM of the campaign
// (pgx.ErrNoRows if they can't see it at all).
func requireGM(ctx context.Context, q *dbq.Queries, campaign uuid.UUID) error {
	c, err := q.GetCampaign(ctx, campaign)
	if err != nil {
		return err
	}
	if c.Role != "gm" {
		return errNotGM
	}
	return nil
}

func writeErr(w http.ResponseWriter, r *http.Request, d Deps, err error, what string) {
	if errors.Is(err, errNotGM) {
		writeError(w, http.StatusForbidden, "gm_only", "only the campaign's GM can do that")
		return
	}
	writeDBError(w, r, d, err, what)
}

func validName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	return s, n >= 1 && n <= 200
}

func handleListCampaigns(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		out := []campaignJSON{}
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			rows, err := q.ListMyCampaigns(r.Context())
			for _, c := range rows {
				out = append(out, campaignJSON{ID: c.ID, Name: c.Name, OwnerID: c.OwnerID, Role: c.Role, CreatedAt: c.CreatedAt})
			}
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "campaigns")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"campaigns": out})
	}
}

func handleCreateCampaign(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		var in struct {
			Name  string              `json:"name"`
			Party []canon.PartyMember `json:"party"`
		}
		if !decodeJSON(w, r, &in, maxJSONBody) {
			return
		}
		name, ok := validName(in.Name)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_input", "campaign name must be 1 to 200 characters")
			return
		}
		if len(in.Party) > 12 {
			writeError(w, http.StatusBadRequest, "invalid_input", "a party can have at most 12 player characters")
			return
		}
		for _, pm := range in.Party {
			if _, ok := validName(pm.Name); !ok || len(pm.Aliases) > 10 {
				writeError(w, http.StatusBadRequest, "invalid_input", "each party member needs a name (1-200 characters) and at most 10 aliases")
				return
			}
		}

		var out campaignJSON
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			c, err := q.CreateCampaign(r.Context(), name)
			if err != nil {
				return err
			}
			if err := q.AddMember(r.Context(), dbq.AddMemberParams{CampaignID: c.ID, UserID: u.ID, Role: "gm"}); err != nil {
				return err
			}
			out = campaignJSON{ID: c.ID, Name: c.Name, OwnerID: c.OwnerID, Role: "gm", CreatedAt: c.CreatedAt}
			return canonstore.SeedParty(r.Context(), q, c.ID, in.Party)
		})
		if err != nil {
			writeErr(w, r, d, err, "campaign")
			return
		}
		writeJSON(w, http.StatusCreated, out)
	}
}

func handleGetCampaign(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		var out struct {
			campaignJSON
			Members []memberJSON `json:"members"`
		}
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			c, err := q.GetCampaign(r.Context(), id)
			if err != nil {
				return err
			}
			out.campaignJSON = campaignJSON{ID: c.ID, Name: c.Name, OwnerID: c.OwnerID, Role: c.Role, CreatedAt: c.CreatedAt}
			members, err := q.ListMembers(r.Context(), id)
			out.Members = []memberJSON{}
			for _, m := range members {
				out.Members = append(out.Members, memberJSON{UserID: m.UserID, Email: m.Email, Role: m.Role, CreatedAt: m.CreatedAt})
			}
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "campaign")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleRenameCampaign(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		var in struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &in, maxJSONBody) {
			return
		}
		name, ok := validName(in.Name)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_input", "campaign name must be 1 to 200 characters")
			return
		}
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			if err := requireGM(r.Context(), q, id); err != nil {
				return err
			}
			_, err := q.RenameCampaign(r.Context(), dbq.RenameCampaignParams{ID: id, Name: name})
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "campaign")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": name})
	}
}

func handleDeleteCampaign(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		var deleted int64
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) (err error) {
			deleted, err = q.DeleteCampaign(r.Context(), id) // RLS: only the owner's DELETE matches a row
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "campaign")
			return
		}
		if deleted == 0 {
			writeError(w, http.StatusNotFound, "not_found", "campaign not found, or only its owner can delete it")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleAddMember(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		var in struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if !decodeJSON(w, r, &in, maxJSONBody) {
			return
		}
		if in.Role != "gm" && in.Role != "player" {
			writeError(w, http.StatusBadRequest, "invalid_input", `role must be "gm" or "player"`)
			return
		}
		var member memberJSON
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			if err := requireGM(r.Context(), q, id); err != nil {
				return err
			}
			target, err := q.GetUserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(in.Email)))
			if errors.Is(err, pgx.ErrNoRows) {
				return errNoAccount
			}
			if err != nil {
				return err
			}
			member = memberJSON{UserID: target.ID, Email: target.Email, Role: in.Role, CreatedAt: time.Now()}
			return q.AddMember(r.Context(), dbq.AddMemberParams{CampaignID: id, UserID: target.ID, Role: in.Role})
		})
		if errors.Is(err, errNoAccount) {
			writeError(w, http.StatusNotFound, "no_account", "there is no account with that email yet; ask them to sign up first")
			return
		}
		if err != nil {
			writeErr(w, r, d, err, "member")
			return
		}
		writeJSON(w, http.StatusCreated, member)
	}
}

var errNoAccount = errors.New("no account")

func handleRemoveMember(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		userID, ok := pathUUID(w, r, "user")
		if !ok {
			return
		}
		var n int64
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) (err error) {
			if err := requireGM(r.Context(), q, id); err != nil {
				return err
			}
			n, err = q.RemoveMember(r.Context(), dbq.RemoveMemberParams{CampaignID: id, UserID: userID})
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "member")
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "not_found", "member not found (the owner can't be removed)")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleGetCanon returns the canon as the caller may see it. GMs and players
// call the same endpoint; row-level security decides what each gets back.
func handleGetCanon(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		var (
			c    canon.Canon
			role string
		)
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			camp, err := q.GetCampaign(r.Context(), id)
			if err != nil {
				return err
			}
			role = camp.Role
			c, err = canonstore.Load(r.Context(), q, id)
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "campaign")
			return
		}
		if c.Entities == nil {
			c.Entities = []canon.Entity{}
		}
		if c.Facts == nil {
			c.Facts = []canon.Fact{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"role": role, "canon": c})
	}
}

func handleListPartyRecaps(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		type recap struct {
			SessionID uuid.UUID `json:"session_id"`
			Number    int32     `json:"number"`
			Body      string    `json:"body"`
		}
		out := []recap{}
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			if _, err := q.GetCampaign(r.Context(), id); err != nil {
				return err
			}
			rows, err := q.ListPartyRecaps(r.Context(), id)
			for _, x := range rows {
				out = append(out, recap{SessionID: x.SessionID, Number: x.SessionNumber, Body: x.Body})
			}
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "campaign")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"recaps": out})
	}
}
