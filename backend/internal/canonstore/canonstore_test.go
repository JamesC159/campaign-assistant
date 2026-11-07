package canonstore_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/canonstore"
	"campaign-assistant/backend/internal/embed"
	"campaign-assistant/backend/internal/eval"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/store/dbq"
	"campaign-assistant/backend/internal/testdb"
)

const campaignDir = "../../testdata/eval-campaign"

// Running the eval campaign through Postgres (load canon → process → apply)
// must give exactly the same result as the in-memory run, and the party's
// database view must match canon.PartyView.
func TestDatabaseRunMatchesInMemoryRun(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	s := store.New(d.App)
	engine := &canon.Engine{Model: canon.HeuristicModel{}, Embedder: embed.Hash{}}

	c, err := canon.LoadCampaign(campaignDir)
	if err != nil {
		t.Fatal(err)
	}
	memFinal, memResults, err := engine.RunCampaign(ctx, c, nil)
	if err != nil {
		t.Fatal(err)
	}

	var gm, player uuid.UUID
	if err := d.Admin.QueryRow(ctx, `INSERT INTO users (email) VALUES ('gm@x.test') RETURNING id`).Scan(&gm); err != nil {
		t.Fatal(err)
	}
	if err := d.Admin.QueryRow(ctx, `INSERT INTO users (email) VALUES ('player@x.test') RETURNING id`).Scan(&player); err != nil {
		t.Fatal(err)
	}

	var campaignID uuid.UUID
	must(t, s.AsUser(ctx, gm, func(q *dbq.Queries) error {
		camp, err := q.CreateCampaign(ctx, c.Name)
		if err != nil {
			return err
		}
		campaignID = camp.ID
		if err := q.AddMember(ctx, dbq.AddMemberParams{CampaignID: camp.ID, UserID: gm, Role: "gm"}); err != nil {
			return err
		}
		if err := q.AddMember(ctx, dbq.AddMemberParams{CampaignID: camp.ID, UserID: player, Role: "player"}); err != nil {
			return err
		}
		return canonstore.SeedParty(ctx, q, camp.ID, c.Party)
	}))

	var dbResults []canon.SessionResult
	for _, sess := range c.Sessions {
		must(t, s.AsUser(ctx, gm, func(q *dbq.Queries) error {
			before, err := canonstore.Load(ctx, q, campaignID)
			if err != nil {
				return err
			}
			p, err := engine.Process(ctx, before, sess)
			if err != nil {
				return err
			}
			row, err := q.CreateGameSession(ctx, dbq.CreateGameSessionParams{CampaignID: campaignID, Number: int32(sess.Number), Notes: sess.Notes}) //nolint:gosec // small
			if err != nil {
				return err
			}
			dbResults = append(dbResults, canon.SessionResult{Before: before, Proposal: p})
			return canonstore.Apply(ctx, q, campaignID, row.ID, p)
		}))
	}

	var dbFinal, partyFinal canon.Canon
	must(t, s.AsUser(ctx, gm, func(q *dbq.Queries) (err error) { dbFinal, err = canonstore.Load(ctx, q, campaignID); return }))
	must(t, s.AsUser(ctx, player, func(q *dbq.Queries) (err error) { partyFinal, err = canonstore.Load(ctx, q, campaignID); return }))

	key, err := eval.LoadAnswerKey(campaignDir)
	if err != nil {
		t.Fatal(err)
	}
	mem, db := eval.Score(key, memFinal, memResults), eval.Score(key, dbFinal, dbResults)
	if f := db.Failures(key.Thresholds["mock"]); len(f) > 0 {
		t.Fatalf("database run fails the eval: %v", f)
	}
	if fmt.Sprint(mem.EntitiesFound, mem.FactsFound, mem.ContradictionsFound, len(mem.FalseAlarms)) !=
		fmt.Sprint(db.EntitiesFound, db.FactsFound, db.ContradictionsFound, len(db.FalseAlarms)) {
		t.Fatalf("database run differs from in-memory run:\nmem %+v\ndb  %+v", mem, db)
	}
	if len(dbFinal.Entities) != len(memFinal.Entities) || len(dbFinal.Facts) != len(memFinal.Facts) {
		t.Fatalf("canon size differs: db %d/%d, mem %d/%d", len(dbFinal.Entities), len(dbFinal.Facts), len(memFinal.Entities), len(memFinal.Facts))
	}

	// What RLS gives a player must be exactly the in-memory party view.
	want := memFinal.PartyView()
	if len(partyFinal.Entities) != len(want.Entities) || len(partyFinal.Facts) != len(want.Facts) {
		t.Fatalf("player DB view %d entities/%d facts, want party view %d/%d",
			len(partyFinal.Entities), len(partyFinal.Facts), len(want.Entities), len(want.Facts))
	}
	for _, e := range partyFinal.Entities {
		if e.Description != "" {
			t.Errorf("player can read GM description of %s", e.CanonicalName())
		}
		for _, n := range e.Names {
			if n.Visibility != canon.VisibilityParty {
				t.Errorf("player can read GM-only name %q", n.Text)
			}
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
