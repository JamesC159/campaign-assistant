package canon

import (
	"encoding/json"
	"fmt"
	"strings"

	"campaign-assistant/backend/internal/llm"
)

// Prompt versions. Bump one whenever its prompt text or schema changes: the
// version is stored on every fact and is part of the fixture key.
const (
	PromptExtract    = 1
	PromptMatch      = 1
	PromptContradict = 1
	PromptRecapGM    = 1
	PromptRecapParty = 1
)

// Purposes name each model call (metering, fixtures, mock dispatch).
const (
	PurposeExtract    = "extract"
	PurposeMatch      = "match"
	PurposeContradict = "contradict"
	PurposeRecapGM    = "recap_gm"
	PurposeRecapParty = "recap_party"
)

// ---- Extraction ----

// KnownEntity is an existing entity shown to the extractor so it reuses names.
type KnownEntity struct {
	Name    string     `json:"name"`
	Type    EntityType `json:"type"`
	Aliases []string   `json:"aliases,omitempty"`
}

// ExtractInput is the structured input of one extraction call.
type ExtractInput struct {
	Party []string      `json:"party"`
	Known []KnownEntity `json:"known_entities"`
	Chunk Chunk         `json:"chunk"`
}

// ExtractOutput is the model's extraction result (tool arguments).
type ExtractOutput struct {
	Entities []ExtractedEntity `json:"entities"`
	Facts    []ExtractedFact   `json:"facts"`
}

// ExtractedEntity is an entity as the model reported it.
type ExtractedEntity struct {
	Name        string     `json:"name"`
	Type        EntityType `json:"type"`
	Aliases     []string   `json:"aliases,omitempty"`
	Description string     `json:"description,omitempty"`
}

// ExtractedFact is a fact as the model reported it.
type ExtractedFact struct {
	Entity     string     `json:"entity"`
	Statement  string     `json:"statement"`
	Quote      string     `json:"quote"`
	Visibility Visibility `json:"visibility"`
}

const extractSystem = `You maintain the canon of a tabletop role-playing campaign. You read a game master's session notes and record the entities and facts they establish.

Rules:
- Record only what the notes state. Never invent, infer motives, or fill gaps.
- Every fact needs a "quote": a short span copied EXACTLY, character for character, from the notes that supports it (one sentence or less). Facts without an exact quote are discarded.
- One fact per distinct claim. Keep statements short, standalone and in past or present tense ("Oswin Pell is the harbormaster of Saltmere.").
- Entity types: npc (non-player character), pc (player character, only the listed party), place, faction, item, thread (an open quest, mystery or plot hook).
- When the notes refer to an entity from the known list, use its exact known name in "entity". Put other names used for it in "aliases".
- Text inside <gm_only> tags is the game master's private knowledge. Facts quoted from it MUST have visibility "gm". Everything else the party saw or heard at the table is "party".
- If GM-only text reveals that two names are the same being (a disguise, a secret identity), record that as a gm fact; do NOT add the secret name as a party-facing alias.`

var extractTool = &llm.Tool{
	Name:        "record_canon",
	Description: "Record the entities and facts established by these session notes.",
	Schema: json.RawMessage(`{
  "type": "object",
  "required": ["entities", "facts"],
  "properties": {
    "entities": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["name", "type"],
        "properties": {
          "name": {"type": "string", "description": "Most complete proper name used in the notes"},
          "type": {"type": "string", "enum": ["npc", "pc", "place", "faction", "item", "thread"]},
          "aliases": {"type": "array", "items": {"type": "string"}},
          "description": {"type": "string", "description": "One sentence on who or what this is"}
        }
      }
    },
    "facts": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["entity", "statement", "quote", "visibility"],
        "properties": {
          "entity": {"type": "string", "description": "Name of the entity this fact is about"},
          "statement": {"type": "string"},
          "quote": {"type": "string", "description": "Exact supporting text copied from the notes"},
          "visibility": {"type": "string", "enum": ["party", "gm"]}
        }
      }
    }
  }
}`),
}

func extractRequest(in ExtractInput) llm.Request {
	var b strings.Builder
	fmt.Fprintf(&b, "Party (player characters): %s\n\n", strings.Join(in.Party, ", "))
	if len(in.Known) > 0 {
		known, _ := json.Marshal(in.Known)
		fmt.Fprintf(&b, "Known entities:\n%s\n\n", known)
	}
	b.WriteString("<notes>\n")
	for _, p := range in.Chunk.Paragraphs {
		if p.GMOnly {
			fmt.Fprintf(&b, "<gm_only>%s</gm_only>\n\n", p.Text)
		} else {
			fmt.Fprintf(&b, "%s\n\n", p.Text)
		}
	}
	b.WriteString("</notes>")
	return llm.Request{
		Purpose: PurposeExtract, PromptVersion: PromptExtract, Tier: llm.TierStrong,
		System: extractSystem, Messages: []llm.Message{{Role: llm.RoleUser, Text: b.String()}},
		Tool: extractTool, MaxTokens: 8000, Temperature: zero(), Input: in,
	}
}

// ---- Entity matching tiebreak ----

// MatchOption is an existing entity that might be the same as the candidate.
type MatchOption struct {
	ID          string     `json:"id"`
	Type        EntityType `json:"type"`
	Names       []string   `json:"names"`
	Description string     `json:"description,omitempty"`
}

// MatchInput asks whether a newly extracted entity is an existing one.
type MatchInput struct {
	Candidate ExtractedEntity `json:"candidate"`
	Options   []MatchOption   `json:"options"`
}

// MatchOutput is the model's decision.
type MatchOutput struct {
	MatchID    string `json:"match_id"` // "" = new entity
	Confidence string `json:"confidence"`
	Reason     string `json:"reason"`
}

const matchSystem = `You deduplicate entities in a tabletop campaign wiki. Decide whether the candidate from the latest session notes is the same entity as one of the existing options. Prefer "no match" unless the evidence is clear: similar names can belong to different people or places.`

var matchTool = &llm.Tool{
	Name:        "decide_match",
	Description: "Say which existing entity the candidate is, or none.",
	Schema: json.RawMessage(`{
  "type": "object",
  "required": ["match_id", "confidence", "reason"],
  "properties": {
    "match_id": {"type": "string", "description": "id of the matching option, or empty string if it is a new entity"},
    "confidence": {"type": "string", "enum": ["high", "medium", "low"]},
    "reason": {"type": "string"}
  }
}`),
}

func matchRequest(in MatchInput) llm.Request {
	body, _ := json.MarshalIndent(in, "", "  ")
	return llm.Request{
		Purpose: PurposeMatch, PromptVersion: PromptMatch, Tier: llm.TierFast,
		System: matchSystem, Messages: []llm.Message{{Role: llm.RoleUser, Text: string(body)}},
		Tool: matchTool, MaxTokens: 500, Temperature: zero(), Input: in,
	}
}

// ---- Contradiction check ----

// FactRef is an established fact shown to the contradiction checker.
type FactRef struct {
	ID        string `json:"id"`
	Session   int    `json:"session"`
	Statement string `json:"statement"`
}

// NewFactRef is a proposed fact, referenced by index.
type NewFactRef struct {
	Index     int    `json:"index"`
	Statement string `json:"statement"`
}

// ContradictInput holds one entity's established and proposed facts.
type ContradictInput struct {
	Entity   string       `json:"entity"`
	Existing []FactRef    `json:"established"`
	New      []NewFactRef `json:"proposed"`
}

// ContradictOutput lists conflicts found.
type ContradictOutput struct {
	Contradictions []FoundContradiction `json:"contradictions"`
}

// FoundContradiction pairs a proposed fact with the established fact it conflicts with.
type FoundContradiction struct {
	NewFact        int    `json:"new_fact"`
	ExistingFactID string `json:"existing_fact_id"`
	Explanation    string `json:"explanation"`
}

const contradictSystem = `You check continuity in a tabletop campaign. Compare the proposed facts from the latest session with the established facts about the same entity. Report only genuine conflicts, where both cannot be true at once (different names, ages, alive vs dead, locations at the same time). Change over time is not a conflict if the facts allow it (a character can be injured, then healed). Report nothing if there are no conflicts.`

var contradictTool = &llm.Tool{
	Name:        "report_contradictions",
	Description: "Report proposed facts that contradict established ones.",
	Schema: json.RawMessage(`{
  "type": "object",
  "required": ["contradictions"],
  "properties": {
    "contradictions": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["new_fact", "existing_fact_id", "explanation"],
        "properties": {
          "new_fact": {"type": "integer", "description": "index of the proposed fact"},
          "existing_fact_id": {"type": "string"},
          "explanation": {"type": "string", "description": "One sentence a GM can act on"}
        }
      }
    }
  }
}`),
}

func contradictRequest(in ContradictInput) llm.Request {
	body, _ := json.MarshalIndent(in, "", "  ")
	return llm.Request{
		Purpose: PurposeContradict, PromptVersion: PromptContradict, Tier: llm.TierStrong,
		System: contradictSystem, Messages: []llm.Message{{Role: llm.RoleUser, Text: string(body)}},
		Tool: contradictTool, MaxTokens: 2000, Temperature: zero(), Input: in,
	}
}

// ---- Recaps ----

// RecapFact is a fact as given to the recap writer.
type RecapFact struct {
	Entity    string `json:"entity"`
	Statement string `json:"statement"`
}

// RecapInput is the material for one recap. The party recap gets only
// party-visible facts and never the raw notes, so it cannot see secrets.
type RecapInput struct {
	Session  int         `json:"session"`
	Audience Visibility  `json:"audience"`
	Facts    []RecapFact `json:"facts"`
	Notes    string      `json:"notes,omitempty"` // GM recap only
	// Retry is set when a previous party recap used a name not in the facts.
	Retry bool `json:"retry,omitempty"`
}

const recapGMSystem = `You write a concise recap of a tabletop session for the game master: what happened, what changed, open threads, and the secrets in play. Plain prose and a few bullets, under 250 words. Use only the provided notes.`

const recapPartySystem = `You write a "previously on..." recap of a tabletop session for the players. Use ONLY the facts provided; do not add names, motives, or events that are not in them. Second person plural ("you"), evocative but brief: one or two short paragraphs, under 180 words.`

func recapRequest(in RecapInput) llm.Request {
	var b strings.Builder
	fmt.Fprintf(&b, "Session %d.\n\n", in.Session)
	if in.Audience == VisibilityGM && in.Notes != "" {
		fmt.Fprintf(&b, "<notes>\n%s\n</notes>\n\n", in.Notes)
	}
	b.WriteString("Facts established this session:\n")
	for _, f := range in.Facts {
		fmt.Fprintf(&b, "- %s: %s\n", f.Entity, f.Statement)
	}
	if in.Retry {
		b.WriteString("\nYour previous recap mentioned a name that is not in these facts. Mention only names that appear above.\n")
	}

	req := llm.Request{
		Tier: llm.TierFast, Messages: []llm.Message{{Role: llm.RoleUser, Text: b.String()}},
		MaxTokens: 700, Input: in,
	}
	if in.Audience == VisibilityGM {
		req.Purpose, req.PromptVersion, req.System = PurposeRecapGM, PromptRecapGM, recapGMSystem
	} else {
		req.Purpose, req.PromptVersion, req.System = PurposeRecapParty, PromptRecapParty, recapPartySystem
	}
	return req
}

func zero() *float32 { z := float32(0); return &z }
