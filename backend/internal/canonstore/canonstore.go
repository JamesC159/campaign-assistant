// Package canonstore moves canon between Postgres and the canon engine's
// in-memory types. It runs inside a store.AsUser transaction, so what Load
// returns depends on who is asking: a GM gets the full canon, a player only
// what row-level security lets them see.
package canonstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/store/dbq"
)

// Load reads the approved canon of a campaign as visible to the current user.
func Load(ctx context.Context, q *dbq.Queries, campaignID uuid.UUID) (canon.Canon, error) {
	ents, err := q.ListEntities(ctx, campaignID)
	if err != nil {
		return canon.Canon{}, fmt.Errorf("list entities: %w", err)
	}
	names, err := q.ListEntityNames(ctx, campaignID)
	if err != nil {
		return canon.Canon{}, fmt.Errorf("list names: %w", err)
	}
	descs, err := q.ListEntityDescriptions(ctx, campaignID) // empty for players
	if err != nil {
		return canon.Canon{}, fmt.Errorf("list descriptions: %w", err)
	}
	facts, err := q.ListFacts(ctx, campaignID)
	if err != nil {
		return canon.Canon{}, fmt.Errorf("list facts: %w", err)
	}

	namesBy := map[uuid.UUID][]canon.Name{}
	for _, n := range names { // ordered by entity, position
		namesBy[n.EntityID] = append(namesBy[n.EntityID], canon.Name{Text: n.Name, Visibility: canon.Visibility(n.Visibility)})
	}
	descBy := map[uuid.UUID]string{}
	for _, d := range descs {
		descBy[d.ID] = d.Description
	}

	var c canon.Canon
	for _, e := range ents {
		if len(namesBy[e.ID]) == 0 {
			continue // RLS hid every name; the entity is not part of this user's view
		}
		c.Entities = append(c.Entities, canon.Entity{
			ID: e.ID.String(), Type: canon.EntityType(e.Type), Names: namesBy[e.ID],
			Description: descBy[e.ID], FirstSeen: int(e.FirstSeen),
		})
	}
	for _, f := range facts {
		c.Facts = append(c.Facts, canon.Fact{
			ID: f.ID.String(), EntityID: f.EntityID.String(), Statement: f.Statement, Quote: f.Quote,
			Session: int(f.SessionNumber), Visibility: canon.Visibility(f.Visibility), PromptVersion: int(f.PromptVersion),
		})
	}
	return c, nil
}

// SeedParty creates player-character entities, known to everyone.
func SeedParty(ctx context.Context, q *dbq.Queries, campaignID uuid.UUID, party []canon.PartyMember) error {
	for _, pm := range party {
		if strings.TrimSpace(pm.Name) == "" {
			continue
		}
		id, err := q.InsertEntity(ctx, dbq.InsertEntityParams{CampaignID: campaignID, Type: string(canon.TypePC)})
		if err != nil {
			return fmt.Errorf("insert pc %q: %w", pm.Name, err)
		}
		for _, n := range append([]string{pm.Name}, pm.Aliases...) {
			if err := q.InsertEntityName(ctx, dbq.InsertEntityNameParams{EntityID: id, CampaignID: campaignID, Name: strings.TrimSpace(n), Visibility: string(canon.VisibilityParty)}); err != nil {
				return fmt.Errorf("insert pc name %q: %w", n, err)
			}
		}
	}
	return nil
}

// Apply writes an approved proposal into canon (the database twin of canon.Apply).
// Names are only ever added or promoted from GM-only to party, never demoted.
func Apply(ctx context.Context, q *dbq.Queries, campaignID, sessionID uuid.UUID, p *canon.Proposal) error {
	refs := map[string]uuid.UUID{}
	for _, pe := range p.Entities {
		var id uuid.UUID
		if pe.New {
			var err error
			id, err = q.InsertEntity(ctx, dbq.InsertEntityParams{
				CampaignID: campaignID, Type: string(pe.Type), Description: pe.Description, FirstSeen: int32(p.Session), //nolint:gosec // session numbers are small
			})
			if err != nil {
				return fmt.Errorf("insert entity %s: %w", pe.Ref, err)
			}
		} else {
			parsed, err := uuid.Parse(pe.Ref)
			if err != nil {
				return fmt.Errorf("proposal references invalid entity %q", pe.Ref)
			}
			id = parsed
		}
		refs[pe.Ref] = id

		for _, n := range pe.Names {
			text := strings.TrimSpace(n.Text)
			if text == "" {
				continue
			}
			if err := q.InsertEntityName(ctx, dbq.InsertEntityNameParams{EntityID: id, CampaignID: campaignID, Name: text, Visibility: string(n.Visibility)}); err != nil {
				return fmt.Errorf("insert name %q: %w", text, err)
			}
			if n.Visibility == canon.VisibilityParty {
				if err := q.PromoteEntityName(ctx, dbq.PromoteEntityNameParams{EntityID: id, Name: text}); err != nil {
					return fmt.Errorf("promote name %q: %w", text, err)
				}
			}
		}
	}

	for _, f := range p.Facts {
		id, ok := refs[f.EntityRef]
		if !ok {
			continue
		}
		if err := q.InsertFact(ctx, dbq.InsertFactParams{
			CampaignID: campaignID, EntityID: id, SessionID: sessionID, SessionNumber: int32(p.Session), //nolint:gosec // small
			Statement: f.Statement, Quote: f.Quote, Visibility: string(f.Visibility), PromptVersion: canon.PromptExtract,
		}); err != nil {
			return fmt.Errorf("insert fact: %w", err)
		}
	}
	return nil
}
