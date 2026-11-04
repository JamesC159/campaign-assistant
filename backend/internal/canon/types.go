// Package canon is the campaign-continuity engine: it turns session notes into
// proposed canon (entities and cited facts), checks them against what is already
// established, and writes GM and player recaps.
//
// # Visibility model (the "secrets never leak" guarantee)
//
// Notes are split into party-visible text (what happened at the table) and
// GM-only text (marked with a "GM only" heading or a "GM:"/"Secret:" prefix).
// A term (name or alias) is known to the party only if it appears in
// party-visible text. Everything shown to players is either verbatim
// party-visible text or model prose that has been checked against the
// secret terms. See visibility.go.
package canon

import (
	"slices"
	"strings"
)

// EntityType classifies an entity.
type EntityType string

const (
	TypeNPC     EntityType = "npc"
	TypePC      EntityType = "pc"
	TypePlace   EntityType = "place"
	TypeFaction EntityType = "faction"
	TypeItem    EntityType = "item"
	TypeThread  EntityType = "thread" // plot thread / quest / mystery
)

// EntityTypes lists every valid type.
var EntityTypes = []EntityType{TypeNPC, TypePC, TypePlace, TypeFaction, TypeItem, TypeThread}

// Valid reports whether t is a known type.
func (t EntityType) Valid() bool { return slices.Contains(EntityTypes, t) }

// Visibility says who may see a fact, name or alias.
type Visibility string

const (
	VisibilityGM    Visibility = "gm"
	VisibilityParty Visibility = "party"
)

// Name is an entity name or alias with its own visibility: players may know
// "the Hooded Stranger" without knowing it is "Lady Maren Ashcombe".
type Name struct {
	Text       string     `json:"text"`
	Visibility Visibility `json:"visibility"`
}

// Entity is a person, place, faction, item or plot thread in the campaign.
type Entity struct {
	ID   string     `json:"id"`
	Type EntityType `json:"type"`
	// Names[0] is the canonical (GM-facing) name; the rest are aliases.
	Names []Name `json:"names"`
	// Description is GM-facing context from extraction. It may contain secrets,
	// so it is never shown to players.
	Description string `json:"description,omitempty"`
	FirstSeen   int    `json:"first_seen"`
}

// CanonicalName is the GM-facing name.
func (e Entity) CanonicalName() string {
	if len(e.Names) == 0 {
		return ""
	}
	return e.Names[0].Text
}

// PartyName is the first name players know, or "" if they know none.
func (e Entity) PartyName() string {
	for _, n := range e.Names {
		if n.Visibility == VisibilityParty {
			return n.Text
		}
	}
	return ""
}

// HasName reports whether text matches any of the entity's names (normalized).
func (e Entity) HasName(text string) bool {
	key := NormalizeName(text)
	for _, n := range e.Names {
		if NormalizeName(n.Text) == key {
			return true
		}
	}
	return false
}

// Fact is one established piece of canon with its citation.
type Fact struct {
	ID            string     `json:"id"`
	EntityID      string     `json:"entity_id"`
	Statement     string     `json:"statement"`
	Quote         string     `json:"quote"` // verbatim from the session notes
	Session       int        `json:"session"`
	Visibility    Visibility `json:"visibility"`
	PromptVersion int        `json:"prompt_version"`
}

// Canon is the approved state of a campaign.
type Canon struct {
	Entities []Entity `json:"entities"`
	Facts    []Fact   `json:"facts"`
}

// Entity returns the entity with id, if any.
func (c Canon) Entity(id string) (Entity, bool) {
	for _, e := range c.Entities {
		if e.ID == id {
			return e, true
		}
	}
	return Entity{}, false
}

// FactsFor returns the facts about one entity, oldest first.
func (c Canon) FactsFor(entityID string) []Fact {
	var out []Fact
	for _, f := range c.Facts {
		if f.EntityID == entityID {
			out = append(out, f)
		}
	}
	return out
}

// PartyView is the canon a player may see: party-visible facts, entities known
// to the party by at least one name, and only party-visible names. GM
// descriptions are removed. Facts about entities the party can't name are dropped.
func (c Canon) PartyView() Canon {
	var out Canon
	visible := map[string]bool{}
	for _, e := range c.Entities {
		var names []Name
		for _, n := range e.Names {
			if n.Visibility == VisibilityParty {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			continue
		}
		visible[e.ID] = true
		out.Entities = append(out.Entities, Entity{ID: e.ID, Type: e.Type, Names: names, FirstSeen: e.FirstSeen})
	}
	for _, f := range c.Facts {
		if f.Visibility == VisibilityParty && visible[f.EntityID] {
			out.Facts = append(out.Facts, f)
		}
	}
	return out
}

var nameArticles = []string{"the ", "a ", "an "}

// NormalizeName folds case, punctuation and leading articles so "The Drowned
// Lantern" and "drowned lantern" compare equal.
func NormalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		switch r {
		case '’', '‘', '`':
			return '\''
		case '.', ',', '"', '“', '”', '!', '?', ':', ';':
			return -1
		}
		return r
	}, s)
	for _, a := range nameArticles {
		s = strings.TrimPrefix(s, a)
	}
	return strings.Join(strings.Fields(s), " ")
}
