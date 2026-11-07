// Package processing runs the canon pipeline for a game session as a background job.
package processing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/canonstore"
	"campaign-assistant/backend/internal/jobs"
	"campaign-assistant/backend/internal/notify"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/store/dbq"
)

// Kind is the job kind for processing one session.
const Kind = "process_session"

// Payload identifies the session and the GM whose permissions the job runs with.
type Payload struct {
	SessionID uuid.UUID `json:"session_id"`
	UserID    uuid.UUID `json:"user_id"`
}

// Enqueue queues processing for a session inside the caller's transaction.
func Enqueue(ctx context.Context, q *dbq.Queries, sessionID, userID uuid.UUID) error {
	_, err := jobs.Enqueue(ctx, q, Kind, Payload{SessionID: sessionID, UserID: userID}, 3)
	return err
}

// Processor turns session notes into a stored proposal and recaps.
//
// It runs as the GM who submitted the session (store.AsUser), so row-level
// security applies to the worker exactly as it does to the API.
type Processor struct {
	Store  *store.Store
	Engine *canon.Engine
	Log    *slog.Logger
}

// Handle implements jobs.Handler.
func (p *Processor) Handle(ctx context.Context, job jobs.Job) error {
	var pl Payload
	if err := json.Unmarshal(job.Payload, &pl); err != nil {
		return jobs.Permanent(fmt.Errorf("bad payload: %w", err))
	}

	var (
		sess    dbq.GameSession
		before  canon.Canon
		version int32
		skip    bool
	)
	err := p.Store.AsUser(ctx, pl.UserID, func(q *dbq.Queries) error {
		var err error
		if sess, err = q.GetGameSession(ctx, pl.SessionID); err != nil {
			return err
		}
		if sess.Status != "queued" && sess.Status != "processing" {
			skip = true // approved or re-queued elsewhere since; nothing to do
			return nil
		}
		if err = q.SetGameSessionStatus(ctx, dbq.SetGameSessionStatusParams{ID: sess.ID, Status: "processing"}); err != nil {
			return err
		}
		if version, err = q.CanonVersion(ctx, sess.CampaignID); err != nil {
			return err
		}
		if before, err = canonstore.Load(ctx, q, sess.CampaignID); err != nil {
			return err
		}
		return notify.Publish(ctx, q, notify.SessionEvent{CampaignID: sess.CampaignID, SessionID: sess.ID, Status: "processing"})
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return jobs.Permanent(errors.New("session no longer exists or the submitter is no longer its GM"))
	}
	if err != nil || skip {
		return err
	}

	proposal, err := p.Engine.Process(ctx, before, canon.Session{Number: int(sess.Number), Notes: sess.Notes})
	if err != nil {
		return fmt.Errorf("pipeline: %w", err)
	}
	body, err := json.Marshal(proposal)
	if err != nil {
		return jobs.Permanent(err)
	}

	return p.Store.AsUser(ctx, pl.UserID, func(q *dbq.Queries) error {
		if err := q.UpsertProposal(ctx, dbq.UpsertProposalParams{SessionID: sess.ID, CampaignID: sess.CampaignID, Body: body, CanonVersion: version}); err != nil {
			return err
		}
		for audience, text := range map[string]string{"gm": proposal.RecapGM, "party": proposal.RecapParty} {
			if err := q.UpsertRecap(ctx, dbq.UpsertRecapParams{SessionID: sess.ID, CampaignID: sess.CampaignID, SessionNumber: sess.Number, Audience: audience, Body: text}); err != nil {
				return err
			}
		}
		purposes := make([]string, 0, len(proposal.Usage))
		for purpose := range proposal.Usage {
			purposes = append(purposes, purpose)
		}
		sort.Strings(purposes)
		for _, purpose := range purposes {
			u := proposal.Usage[purpose]
			if err := q.InsertUsage(ctx, dbq.InsertUsageParams{CampaignID: sess.CampaignID, SessionID: uuid.NullUUID{UUID: sess.ID, Valid: true},
				Purpose: purpose, Calls: int32(u.Calls), InputTokens: int32(u.InputTokens), OutputTokens: int32(u.OutputTokens)}); err != nil { //nolint:gosec // token counts fit
				return err
			}
		}
		if err := q.SetGameSessionStatus(ctx, dbq.SetGameSessionStatusParams{ID: sess.ID, Status: "review"}); err != nil {
			return err
		}
		return notify.Publish(ctx, q, notify.SessionEvent{CampaignID: sess.CampaignID, SessionID: sess.ID, Status: "review"})
	})
}

// Failed marks the session failed once the job gives up (wire to jobs.Worker.OnFailed).
func (p *Processor) Failed(ctx context.Context, job jobs.Job, cause error) {
	if job.Kind != Kind {
		return
	}
	var pl Payload
	if json.Unmarshal(job.Payload, &pl) != nil {
		return
	}
	msg := "Processing failed: " + cause.Error()
	err := p.Store.AsUser(ctx, pl.UserID, func(q *dbq.Queries) error {
		sess, err := q.GetGameSession(ctx, pl.SessionID)
		if err != nil {
			return err
		}
		if err := q.SetGameSessionStatus(ctx, dbq.SetGameSessionStatusParams{ID: sess.ID, Status: "failed", Error: &msg}); err != nil {
			return err
		}
		return notify.Publish(ctx, q, notify.SessionEvent{CampaignID: sess.CampaignID, SessionID: sess.ID, Status: "failed", Error: msg})
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		p.Log.Error("could not mark session failed", "session", pl.SessionID, "err", err)
	}
}
