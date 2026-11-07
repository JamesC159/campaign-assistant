package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"campaign-assistant/backend/internal/auth"
	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/canonstore"
	"campaign-assistant/backend/internal/notify"
	"campaign-assistant/backend/internal/processing"
	"campaign-assistant/backend/internal/store/dbq"
)

const maxNotesChars = 500_000

type sessionJSON struct {
	ID         uuid.UUID `json:"id"`
	CampaignID uuid.UUID `json:"campaign_id"`
	Number     int32     `json:"number"`
	PlayedOn   *string   `json:"played_on"`
	Status     string    `json:"status"` // queued | processing | review | approved | failed
	Error      *string   `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Notes      *string   `json:"notes,omitempty"` // detail view only
}

func dateString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

func fromSession(s dbq.GameSession, withNotes bool) sessionJSON {
	out := sessionJSON{ID: s.ID, CampaignID: s.CampaignID, Number: s.Number, PlayedOn: dateString(s.PlayedOn),
		Status: s.Status, Error: s.Error, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
	if withNotes {
		out.Notes = &s.Notes
	}
	return out
}

func publish(r *http.Request, q *dbq.Queries, s dbq.GameSession, status string) error {
	return notify.Publish(r.Context(), q, notify.SessionEvent{CampaignID: s.CampaignID, SessionID: s.ID, Status: status})
}

func handleCreateSession(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		campaign, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		var in struct {
			Notes    string  `json:"notes"`
			Number   *int32  `json:"number"`
			PlayedOn *string `json:"played_on"`
		}
		if !decodeJSON(w, r, &in, maxNotesBody) {
			return
		}
		if strings.TrimSpace(in.Notes) == "" || utf8.RuneCountInString(in.Notes) > maxNotesChars {
			writeError(w, http.StatusBadRequest, "invalid_input", fmt.Sprintf("notes must be 1 to %d characters", maxNotesChars))
			return
		}
		var played *time.Time
		if in.PlayedOn != nil {
			t, err := time.Parse(time.DateOnly, *in.PlayedOn)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_input", "played_on must be a date like 2026-11-07")
				return
			}
			played = &t
		}
		if in.Number != nil && (*in.Number < 1 || *in.Number > 10000) {
			writeError(w, http.StatusBadRequest, "invalid_input", "number must be between 1 and 10000")
			return
		}

		var out sessionJSON
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			if err := requireGM(r.Context(), q, campaign); err != nil {
				return err
			}
			number := int32(0)
			if in.Number != nil {
				number = *in.Number
			} else {
				next, err := q.NextSessionNumber(r.Context(), campaign)
				if err != nil {
					return err
				}
				number = next
			}
			row, err := q.CreateGameSession(r.Context(), dbq.CreateGameSessionParams{CampaignID: campaign, Number: number, PlayedOn: played, Notes: in.Notes})
			if err != nil {
				return err
			}
			s := dbq.GameSession{ID: row.ID, CampaignID: row.CampaignID, Number: row.Number, PlayedOn: row.PlayedOn, Status: row.Status,
				Error: row.Error, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
			out = fromSession(s, false)
			if err := processing.Enqueue(r.Context(), q, row.ID, u.ID); err != nil {
				return err
			}
			return publish(r, q, s, row.Status)
		})
		if err != nil {
			writeErr(w, r, d, err, "session")
			return
		}
		writeJSON(w, http.StatusAccepted, out)
	}
}

func handleListSessions(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		campaign, ok := pathUUID(w, r, "campaign")
		if !ok {
			return
		}
		out := []sessionJSON{}
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			if err := requireGM(r.Context(), q, campaign); err != nil {
				return err
			}
			rows, err := q.ListGameSessions(r.Context(), campaign)
			for _, s := range rows {
				out = append(out, fromSession(dbq.GameSession{ID: s.ID, CampaignID: s.CampaignID, Number: s.Number, PlayedOn: s.PlayedOn,
					Status: s.Status, Error: s.Error, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}, false))
			}
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "campaign")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
	}
}

type recapJSON struct {
	Audience  string `json:"audience"`
	Body      string `json:"body"`
	Published bool   `json:"published"`
}

func handleGetSession(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "session")
		if !ok {
			return
		}
		var out struct {
			Session sessionJSON `json:"session"`
			Recaps  []recapJSON `json:"recaps"`
		}
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			s, err := q.GetGameSession(r.Context(), id) // RLS: GMs only
			if err != nil {
				return err
			}
			out.Session = fromSession(s, true)
			recaps, err := q.ListRecaps(r.Context(), id)
			out.Recaps = []recapJSON{}
			for _, rc := range recaps {
				out.Recaps = append(out.Recaps, recapJSON{Audience: rc.Audience, Body: rc.Body, Published: rc.Published})
			}
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "session")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleGetProposal(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "session")
		if !ok {
			return
		}
		var out struct {
			CanonVersion   int32           `json:"canon_version"`
			CurrentVersion int32           `json:"current_canon_version"`
			Stale          bool            `json:"stale"`
			Proposal       json.RawMessage `json:"proposal"`
		}
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			s, err := q.GetGameSession(r.Context(), id)
			if err != nil {
				return err
			}
			p, err := q.GetProposal(r.Context(), id)
			if err != nil {
				return err
			}
			cur, err := q.CanonVersion(r.Context(), s.CampaignID)
			out.CanonVersion, out.CurrentVersion, out.Proposal = p.CanonVersion, cur, p.Body
			out.Stale = p.CanonVersion != cur && s.Status == "review"
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "proposal")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// approval outcomes that are not errors: they still commit their transaction.
var (
	errNotReady     = errors.New("not ready")
	errOutOfOrder   = errors.New("out of order")
	errStaleRequeue = errors.New("stale")
)

// handleApproveSession approves a proposal in full: its entities and facts
// become canon and the player recap is published. Sessions are approved in
// order. A proposal computed against older canon is re-queued instead.
func handleApproveSession(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "session")
		if !ok {
			return
		}
		var (
			out     sessionJSON
			outcome error
		)
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			ctx := r.Context()
			s, err := q.GetGameSession(ctx, id)
			if err != nil {
				return err
			}
			if s.Status != "review" {
				outcome = errNotReady
				return nil
			}
			if n, err := q.CountUnapprovedEarlierSessions(ctx, dbq.CountUnapprovedEarlierSessionsParams{CampaignID: s.CampaignID, Number: s.Number}); err != nil {
				return err
			} else if n > 0 {
				outcome = errOutOfOrder
				return nil
			}
			p, err := q.GetProposal(ctx, id)
			if err != nil {
				return err
			}
			version, err := q.CanonVersion(ctx, s.CampaignID)
			if err != nil {
				return err
			}
			if p.CanonVersion != version {
				outcome = errStaleRequeue
				return requeue(r, q, s, u.ID)
			}

			var proposal canon.Proposal
			if err := json.Unmarshal(p.Body, &proposal); err != nil {
				return fmt.Errorf("decode proposal: %w", err)
			}
			if err := canonstore.Apply(ctx, q, s.CampaignID, s.ID, &proposal); err != nil {
				return err
			}
			if err := q.PublishRecaps(ctx, s.ID); err != nil {
				return err
			}
			if err := q.SetGameSessionStatus(ctx, dbq.SetGameSessionStatusParams{ID: s.ID, Status: "approved"}); err != nil {
				return err
			}
			s.Status, s.Error = "approved", nil
			out = fromSession(s, false)
			if err := publish(r, q, s, "approved"); err != nil {
				return err
			}

			// Later sessions already in review were computed without this canon: redo them.
			stale, err := q.ListStaleReviewSessions(ctx, dbq.ListStaleReviewSessionsParams{CampaignID: s.CampaignID, CanonVersion: version + 1})
			if err != nil {
				return err
			}
			for _, st := range stale {
				later, err := q.GetGameSession(ctx, st.ID)
				if err != nil {
					return err
				}
				if err := requeue(r, q, later, st.CreatedBy); err != nil {
					return err
				}
			}
			return nil
		})
		switch {
		case err != nil:
			writeErr(w, r, d, err, "session")
		case errors.Is(outcome, errNotReady):
			writeError(w, http.StatusConflict, "not_ready", "only sessions waiting for review can be approved")
		case errors.Is(outcome, errOutOfOrder):
			writeError(w, http.StatusConflict, "approve_in_order", "approve earlier sessions first: each session builds on the canon before it")
		case errors.Is(outcome, errStaleRequeue):
			writeError(w, http.StatusConflict, "stale_proposal", "canon changed since this session was processed; it has been re-queued")
		default:
			writeJSON(w, http.StatusOK, out)
		}
	}
}

func requeue(r *http.Request, q *dbq.Queries, s dbq.GameSession, runAs uuid.UUID) error {
	if err := q.SetGameSessionStatus(r.Context(), dbq.SetGameSessionStatusParams{ID: s.ID, Status: "queued"}); err != nil {
		return err
	}
	if err := processing.Enqueue(r.Context(), q, s.ID, runAs); err != nil {
		return err
	}
	return publish(r, q, s, "queued")
}

func handleReprocessSession(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "session")
		if !ok {
			return
		}
		var out sessionJSON
		var notReady bool
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			s, err := q.GetGameSession(r.Context(), id)
			if err != nil {
				return err
			}
			if s.Status != "review" && s.Status != "failed" {
				notReady = true
				return nil
			}
			s.Status, s.Error = "queued", nil
			out = fromSession(s, false)
			return requeue(r, q, s, u.ID)
		})
		switch {
		case err != nil:
			writeErr(w, r, d, err, "session")
		case notReady:
			writeError(w, http.StatusConflict, "not_ready", "only sessions in review or failed can be reprocessed")
		default:
			writeJSON(w, http.StatusAccepted, out)
		}
	}
}

// handleSessionEvents streams status changes as Server-Sent Events:
//
//	event: status
//	data: {"session_id":"…","status":"processing"}
//
// The stream sends the current status first and closes after a terminal
// status (review, approved, failed).
func handleSessionEvents(d Deps) func(http.ResponseWriter, *http.Request, auth.User) {
	return func(w http.ResponseWriter, r *http.Request, u auth.User) {
		id, ok := pathUUID(w, r, "session")
		if !ok {
			return
		}
		var events <-chan notify.SessionEvent
		if d.Events != nil {
			ch, cancel := d.Events.Subscribe(id) // subscribe before reading status, so no change is missed
			defer cancel()
			events = ch
		}
		var current notify.SessionEvent
		err := d.Store.AsUser(r.Context(), u.ID, func(q *dbq.Queries) error {
			s, err := q.GetGameSession(r.Context(), id)
			e := ""
			if s.Error != nil {
				e = *s.Error
			}
			current = notify.SessionEvent{CampaignID: s.CampaignID, SessionID: s.ID, Status: s.Status, Error: e}
			return err
		})
		if err != nil {
			writeErr(w, r, d, err, "session")
			return
		}

		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Time{}) // a stream outlives the server's normal write timeout
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		send := func(ev notify.SessionEvent) bool {
			b, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", b); err != nil {
				return false
			}
			return rc.Flush() == nil
		}
		terminal := func(s string) bool { return s == "review" || s == "approved" || s == "failed" }

		if !send(current) || terminal(current.Status) {
			return
		}
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ping.C:
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			case ev := <-events:
				if !send(ev) || terminal(ev.Status) {
					return
				}
			}
		}
	}
}
