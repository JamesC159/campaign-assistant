package canon

import (
	"fmt"
	"strings"
)

// PartyMember is a player character, known to everyone from the start.
type PartyMember struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases,omitempty"`
}

// SeedParty returns a canon containing only the player characters.
func SeedParty(party []PartyMember) Canon {
	var c Canon
	for i, pm := range party {
		names := []Name{{Text: pm.Name, Visibility: VisibilityParty}}
		for _, a := range pm.Aliases {
			names = append(names, Name{Text: a, Visibility: VisibilityParty})
		}
		c.Entities = append(c.Entities, Entity{ID: fmt.Sprintf("e%d", i+1), Type: TypePC, Names: names})
	}
	return c
}

// Apply approves an entire proposal and returns the new canon; current is not
// modified. (The review UI will approve item by item; this is the
// "approve all" path used by the CLI and evals.)
func Apply(current Canon, p *Proposal) Canon {
	next := Canon{
		Entities: make([]Entity, len(current.Entities)),
		Facts:    append([]Fact(nil), current.Facts...),
	}
	for i, e := range current.Entities {
		e.Names = append([]Name(nil), e.Names...)
		next.Entities[i] = e
	}

	index := map[string]int{} // entity ID -> position in next.Entities
	for i, e := range next.Entities {
		index[e.ID] = i
	}
	refToID := map[string]string{}

	for _, pe := range p.Entities {
		if pe.New {
			id := fmt.Sprintf("e%d", len(next.Entities)+1)
			refToID[pe.Ref] = id
			index[id] = len(next.Entities)
			next.Entities = append(next.Entities, Entity{ID: id, Type: pe.Type, Names: dedupeNames(pe.Names), Description: pe.Description, FirstSeen: p.Session})
			continue
		}
		refToID[pe.Ref] = pe.Ref
		i, ok := index[pe.Ref]
		if !ok {
			continue // proposal for an entity that no longer exists
		}
		next.Entities[i].Names = mergeNames(next.Entities[i].Names, pe.Names)
	}

	for _, f := range p.Facts {
		id, ok := refToID[f.EntityRef]
		if !ok {
			continue
		}
		next.Facts = append(next.Facts, Fact{
			ID:            fmt.Sprintf("f%d", len(next.Facts)+1),
			EntityID:      id,
			Statement:     f.Statement,
			Quote:         f.Quote,
			Session:       p.Session,
			Visibility:    f.Visibility,
			PromptVersion: PromptExtract,
		})
	}
	return next
}

// mergeNames adds new names and upgrades GM-only names the party has now learned.
// A name never goes from party back to GM.
func mergeNames(have, add []Name) []Name {
	for _, n := range add {
		found := false
		for i := range have {
			if NormalizeName(have[i].Text) == NormalizeName(n.Text) {
				found = true
				if n.Visibility == VisibilityParty {
					have[i].Visibility = VisibilityParty
				}
				break
			}
		}
		if !found && strings.TrimSpace(n.Text) != "" {
			have = append(have, n)
		}
	}
	return have
}

func dedupeNames(ns []Name) []Name { return mergeNames(nil, ns) }
