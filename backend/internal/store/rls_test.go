package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"campaign-assistant/backend/internal/db"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/store/dbq"
	"campaign-assistant/backend/internal/testdb"
)

// fixture is a campaign with secrets, built as the admin role (bypassing RLS).
type fixture struct {
	gm, player, outsider uuid.UUID
	campaign, session    uuid.UUID
	known, hidden        uuid.UUID // entities: known to the party / GM-only
}

func seed(t *testing.T, d *testdb.DB) fixture {
	t.Helper()
	ctx := context.Background()
	var f fixture
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := d.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	scan := func(dst *uuid.UUID, sql string, args ...any) {
		t.Helper()
		if err := d.Admin.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	scan(&f.gm, `INSERT INTO users (email) VALUES ('gm@example.com') RETURNING id`)
	scan(&f.player, `INSERT INTO users (email) VALUES ('player@example.com') RETURNING id`)
	scan(&f.outsider, `INSERT INTO users (email) VALUES ('outsider@example.com') RETURNING id`)
	scan(&f.campaign, `INSERT INTO campaigns (owner_id, name) VALUES ($1, 'Lantern Coast') RETURNING id`, f.gm)
	exec(`INSERT INTO campaign_members (campaign_id, user_id, role) VALUES ($1, $2, 'gm'), ($1, $3, 'player')`, f.campaign, f.gm, f.player)
	scan(&f.session, `INSERT INTO game_sessions (campaign_id, number, notes, status, created_by)
		VALUES ($1, 1, 'GM: Maren leads the Pale Tide.', 'approved', $2) RETURNING id`, f.campaign, f.gm)
	exec(`INSERT INTO proposals (session_id, campaign_id, body, canon_version) VALUES ($1, $2, '{"secret":"Pale Tide"}', 0)`, f.session, f.campaign)
	exec(`INSERT INTO recaps (session_id, campaign_id, session_number, audience, body, published) VALUES
		($1, $2, 1, 'gm', 'Maren leads the Pale Tide.', true), ($1, $2, 1, 'party', 'You met Lady Maren.', true)`, f.session, f.campaign)

	scan(&f.known, `INSERT INTO entities (campaign_id, type, description) VALUES ($1, 'npc', 'secretly leads the cult') RETURNING id`, f.campaign)
	scan(&f.hidden, `INSERT INTO entities (campaign_id, type) VALUES ($1, 'faction') RETURNING id`, f.campaign)
	exec(`INSERT INTO entity_names (entity_id, campaign_id, name, position, visibility) VALUES
		($1, $3, 'Lady Maren', 0, 'party'), ($1, $3, 'Cult Leader', 1, 'gm'), ($2, $3, 'Pale Tide', 0, 'gm')`, f.known, f.hidden, f.campaign)
	exec(`INSERT INTO facts (campaign_id, entity_id, session_id, session_number, statement, quote, visibility, state, prompt_version) VALUES
		($1, $2, $4, 1, 'Lady Maren hired the party.', 'q', 'party', 'canon', 1),
		($1, $2, $4, 1, 'Maren leads the Pale Tide.', 'q', 'gm', 'canon', 1),
		($1, $2, $4, 1, 'Maren was retconned.', 'q', 'party', 'retconned', 1),
		($1, $3, $4, 1, 'The Pale Tide meets at night.', 'q', 'party', 'canon', 1)`, f.campaign, f.known, f.hidden, f.session)
	return f
}

// view is everything one user can read about the campaign.
type view struct {
	entities, names, facts, descriptions, sessions, recaps int
	proposal                                               bool
	text                                                   string // all readable text, for secret scanning
}

func read(t *testing.T, s *store.Store, user uuid.UUID, f fixture) view {
	t.Helper()
	var v view
	err := s.AsUser(context.Background(), user, func(q *dbq.Queries) error {
		ctx := context.Background()
		ents, err := q.ListEntities(ctx, f.campaign)
		if err != nil {
			return err
		}
		names, err := q.ListEntityNames(ctx, f.campaign)
		if err != nil {
			return err
		}
		facts, err := q.ListFacts(ctx, f.campaign)
		if err != nil {
			return err
		}
		descs, err := q.ListEntityDescriptions(ctx, f.campaign)
		if err != nil {
			return err
		}
		sessions, err := q.ListGameSessions(ctx, f.campaign)
		if err != nil {
			return err
		}
		recaps, err := q.ListRecaps(ctx, f.session)
		if err != nil {
			return err
		}
		_, perr := q.GetProposal(ctx, f.session)
		v.proposal = perr == nil

		v.entities, v.names, v.facts, v.descriptions, v.sessions, v.recaps = len(ents), len(names), len(facts), len(descs), len(sessions), len(recaps)
		var b strings.Builder
		for _, n := range names {
			b.WriteString(n.Name + " ")
		}
		for _, x := range facts {
			b.WriteString(x.Statement + " ")
		}
		for _, d := range descs {
			b.WriteString(d.Description + " ")
		}
		for _, r := range recaps {
			b.WriteString(r.Body + " ")
		}
		v.text = b.String()
		return nil
	})
	if err != nil {
		t.Fatalf("read as %s: %v", user, err)
	}
	return v
}

func TestRLSPlayerSeesOnlyPartyCanon(t *testing.T) {
	d := testdb.New(t)
	f := seed(t, d)
	s := store.New(d.App)

	gm := read(t, s, f.gm, f)
	if gm.entities != 2 || gm.names != 3 || gm.facts != 3 || gm.descriptions != 2 || gm.sessions != 1 || gm.recaps != 2 || !gm.proposal {
		t.Errorf("GM should see everything (retconned facts excluded): %+v", gm)
	}

	p := read(t, s, f.player, f)
	if p.entities != 1 || p.names != 1 || p.facts != 1 || p.descriptions != 0 || p.sessions != 0 || p.recaps != 1 || p.proposal {
		t.Errorf("player view wrong: %+v", p)
	}
	for _, secret := range []string{"Pale Tide", "Cult Leader", "secretly", "retconned"} {
		if strings.Contains(p.text, secret) {
			t.Errorf("player can read secret %q in: %s", secret, p.text)
		}
	}

	o := read(t, s, f.outsider, f)
	if o != (view{}) {
		t.Errorf("outsider must see nothing: %+v", o)
	}
}

func TestRLSRejectsPlayerWritesAndSecretColumns(t *testing.T) {
	d := testdb.New(t)
	f := seed(t, d)
	s := store.New(d.App)
	ctx := context.Background()

	err := s.AsUser(ctx, f.player, func(q *dbq.Queries) error {
		return q.InsertFact(ctx, dbq.InsertFactParams{CampaignID: f.campaign, EntityID: f.known, SessionID: f.session,
			SessionNumber: 1, Statement: "forged", Quote: "q", Visibility: "party", PromptVersion: 1})
	})
	if !isRLSViolation(err) {
		t.Errorf("player insert must violate RLS, got %v", err)
	}

	err = s.AsUser(ctx, f.player, func(q *dbq.Queries) error {
		return q.AddMember(ctx, dbq.AddMemberParams{CampaignID: f.campaign, UserID: f.outsider, Role: "gm"})
	})
	if !isRLSViolation(err) {
		t.Errorf("player must not add members, got %v", err)
	}

	// The description column itself is revoked from the app role; even a GM reads it via the gated view.
	err = s.AsUser(ctx, f.gm, func(*dbq.Queries) error {
		_, err := d.App.Exec(ctx, "SELECT description FROM entities")
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("reading entities.description directly must be denied, got %v", err)
	}
}

func TestRLSWithoutUserContextSeesNothing(t *testing.T) {
	d := testdb.New(t)
	f := seed(t, d)
	var n int
	err := store.New(d.App).System(context.Background(), func(q *dbq.Queries) error {
		facts, err := q.ListFacts(context.Background(), f.campaign)
		n = len(facts)
		return err
	})
	if err != nil || n != 0 {
		t.Fatalf("no user context must read zero facts, got %d (%v)", n, err)
	}

	// The setting is transaction-local: it must not leak to the next use of a pooled connection.
	_ = store.New(d.App).AsUser(context.Background(), f.gm, func(*dbq.Queries) error { return nil })
	var leaked string
	if err := d.App.QueryRow(context.Background(), "SELECT COALESCE(current_setting('app.user_id', true), '')").Scan(&leaked); err != nil || leaked != "" {
		t.Fatalf("app.user_id leaked outside its transaction: %q (%v)", leaked, err)
	}
}

func TestAppPoolRunsAsRestrictedRole(t *testing.T) {
	d := testdb.New(t)
	info, err := db.CheckRole(context.Background(), d.App)
	if err != nil {
		t.Fatal(err)
	}
	if info.CurrentUser != testdb.AppRole || info.BypassesRLS {
		t.Fatalf("app pool must run as %s without BYPASSRLS: %+v", testdb.AppRole, info)
	}
}

func isRLSViolation(err error) bool {
	var pgErr *pgconn.PgError
	// 42501: insufficient_privilege, raised for "new row violates row-level security policy".
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}
