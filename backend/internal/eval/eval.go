// Package eval scores a pipeline run against a hand-written answer key.
package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/llm"
)

// AnswerKey is answer-key.json.
type AnswerKey struct {
	Entities       []ExpectedEntity       `json:"entities"`
	Facts          []ExpectedFact         `json:"facts"`
	Contradictions []PlantedContradiction `json:"contradictions"`
	Secrets        Secrets                `json:"secrets"`
	Thresholds     map[string]Thresholds  `json:"thresholds"`
}

// ExpectedEntity is an entity the pipeline should find.
type ExpectedEntity struct {
	Name    string           `json:"name"`
	Aliases []string         `json:"aliases"`
	Type    canon.EntityType `json:"type"`
	// PartyFrom is the session in which players first learn the entity (0 = from
	// the start); null means players never learn it, so it must stay hidden.
	PartyFrom *int `json:"party_from"`
}

func (e ExpectedEntity) names() []string { return append([]string{e.Name}, e.Aliases...) }

// ExpectedFact is a fact the pipeline should record.
type ExpectedFact struct {
	Entity     string           `json:"entity"`
	Session    int              `json:"session"`
	Contains   []string         `json:"contains"`
	Visibility canon.Visibility `json:"visibility"`
}

// PlantedContradiction is a deliberate continuity error in the notes.
type PlantedContradiction struct {
	Session          int    `json:"session"`
	Entity           string `json:"entity"`
	NewContains      string `json:"new_contains"`
	ExistingContains string `json:"existing_contains"`
}

// Secrets must never reach anything a player can see.
type Secrets struct {
	Markers    []string         `json:"markers"`
	Identities []IdentitySecret `json:"identities"`
}

// IdentitySecret: an entity known by Names must not be linked, in the party
// view, to the Forbidden terms (e.g. a disguise).
type IdentitySecret struct {
	Names     []string `json:"names"`
	Forbidden []string `json:"forbidden"`
}

// Thresholds are pass/fail limits for one provider.
type Thresholds struct {
	EntityRecall          float64 `json:"entity_recall"`
	FactRecall            float64 `json:"fact_recall"`
	ContradictionRecall   float64 `json:"contradiction_recall"`
	MaxFalseAlarmsPerSess float64 `json:"max_false_alarms_per_session"`
	MaxWrongMerges        int     `json:"max_wrong_merges"`
	MaxSecretLeaks        int     `json:"max_secret_leaks"`
}

// LoadAnswerKey reads dir/answer-key.json.
func LoadAnswerKey(dir string) (AnswerKey, error) {
	var k AnswerKey
	b, err := os.ReadFile(filepath.Join(dir, "answer-key.json")) //nolint:gosec // dir is a trusted CLI argument
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil {
		return k, fmt.Errorf("parse answer-key.json: %w", err)
	}
	return k, nil
}

// Report is the scorecard for one run.
type Report struct {
	Sessions int `json:"sessions"`

	EntitiesExpected int      `json:"entities_expected"`
	EntitiesFound    int      `json:"entities_found"`
	MissingEntities  []string `json:"missing_entities,omitempty"`
	SplitEntities    []string `json:"split_entities,omitempty"` // one expected entity spread over several canon entities
	WrongMerges      []string `json:"wrong_merges,omitempty"`   // several expected entities collapsed into one

	FactsExpected    int      `json:"facts_expected"`
	FactsFound       int      `json:"facts_found"`
	MissingFacts     []string `json:"missing_facts,omitempty"`
	VisibilityErrors []string `json:"visibility_errors,omitempty"`

	ContradictionsPlanted int      `json:"contradictions_planted"`
	ContradictionsFound   int      `json:"contradictions_found"`
	MissedContradictions  []string `json:"missed_contradictions,omitempty"`
	FalseAlarms           []string `json:"false_alarms,omitempty"`

	SecretLeaks []string `json:"secret_leaks,omitempty"`

	Usage      map[string]llm.Usage `json:"usage"`
	TotalUsage llm.Usage            `json:"total_usage"`
}

// Rates used for thresholds.
func (r Report) EntityRecall() float64 { return ratio(r.EntitiesFound, r.EntitiesExpected) }
func (r Report) FactRecall() float64   { return ratio(r.FactsFound, r.FactsExpected) }
func (r Report) ContradictionRecall() float64 {
	return ratio(r.ContradictionsFound, r.ContradictionsPlanted)
}
func (r Report) FalseAlarmsPerSession() float64 {
	if r.Sessions == 0 {
		return 0
	}
	return float64(len(r.FalseAlarms)) / float64(r.Sessions)
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}

// Score compares a run with the answer key.
func Score(key AnswerKey, final canon.Canon, results []canon.SessionResult) Report {
	r := Report{Sessions: len(results), Usage: map[string]llm.Usage{}}

	// --- entities ---
	matches := map[string][]canon.Entity{} // expected name -> matching canon entities
	owner := map[string][]string{}         // canon entity ID -> expected names it matched
	for _, exp := range key.Entities {
		for _, ce := range final.Entities {
			if anyNameMatches(ce, exp.names()) {
				matches[exp.Name] = append(matches[exp.Name], ce)
				owner[ce.ID] = append(owner[ce.ID], exp.Name)
			}
		}
	}
	r.EntitiesExpected = len(key.Entities)
	for _, exp := range key.Entities {
		n := len(matches[exp.Name])
		if n == 0 {
			r.MissingEntities = append(r.MissingEntities, exp.Name)
			continue
		}
		r.EntitiesFound++
		if n > 1 {
			r.SplitEntities = append(r.SplitEntities, fmt.Sprintf("%s (%d entities)", exp.Name, n))
		}
	}
	for id, names := range owner {
		if len(names) > 1 {
			sort.Strings(names)
			r.WrongMerges = append(r.WrongMerges, fmt.Sprintf("%s merged into %s", strings.Join(names, " + "), id))
		}
	}
	sort.Strings(r.WrongMerges)

	// --- facts ---
	r.FactsExpected = len(key.Facts)
	for _, ef := range key.Facts {
		label := fmt.Sprintf("s%d %s: %s", ef.Session, ef.Entity, strings.Join(ef.Contains, ", "))
		found, visOK := false, false
		for _, ce := range matches[ef.Entity] {
			for _, f := range final.FactsFor(ce.ID) {
				if f.Session == ef.Session && containsAll(f.Statement+" "+f.Quote, ef.Contains) {
					found = true
					if f.Visibility == ef.Visibility {
						visOK = true
					}
				}
			}
		}
		switch {
		case !found:
			r.MissingFacts = append(r.MissingFacts, label)
		case !visOK:
			r.FactsFound++
			r.VisibilityErrors = append(r.VisibilityErrors, fmt.Sprintf("%s: expected %s", label, ef.Visibility))
		default:
			r.FactsFound++
		}
	}

	// --- contradictions ---
	r.ContradictionsPlanted = len(key.Contradictions)
	used := map[string]bool{} // "session/index" of reported contradictions that matched a planted one
	for _, pc := range key.Contradictions {
		hit := false
		for _, res := range results {
			if res.Proposal.Session != pc.Session {
				continue
			}
			for ci, c := range res.Proposal.Contradictions {
				nf := res.Proposal.Facts[c.FactIndex]
				old, ok := findFact(res.Before, c.ExistingFactID)
				ent, _ := res.Before.Entity(nf.EntityRef)
				if ok && anyNameMatches(ent, []string{pc.Entity}) &&
					containsAll(nf.Statement+" "+nf.Quote, []string{pc.NewContains}) &&
					containsAll(old.Statement+" "+old.Quote, []string{pc.ExistingContains}) {
					hit = true
					used[fmt.Sprintf("%d/%d", res.Proposal.Session, ci)] = true
				}
			}
		}
		if hit {
			r.ContradictionsFound++
		} else {
			r.MissedContradictions = append(r.MissedContradictions, fmt.Sprintf("s%d %s: %q vs %q", pc.Session, pc.Entity, pc.NewContains, pc.ExistingContains))
		}
	}
	for _, res := range results {
		for ci, c := range res.Proposal.Contradictions {
			if !used[fmt.Sprintf("%d/%d", res.Proposal.Session, ci)] {
				r.FalseAlarms = append(r.FalseAlarms, fmt.Sprintf("s%d: %s", res.Proposal.Session, c.Explanation))
			}
		}
	}

	// --- secrets ---
	r.SecretLeaks = findLeaks(key, final, results)

	// --- usage ---
	for _, res := range results {
		for purpose, u := range res.Proposal.Usage {
			r.Usage[purpose] = r.Usage[purpose].Add(u)
			r.TotalUsage = r.TotalUsage.Add(u)
		}
	}
	return r
}

// findLeaks inspects everything a player can see: party recaps and the party view of the canon.
func findLeaks(key AnswerKey, final canon.Canon, results []canon.SessionResult) []string {
	var leaks []string
	check := func(where, text string) {
		for _, m := range key.Secrets.Markers {
			if containsAll(text, []string{m}) {
				leaks = append(leaks, fmt.Sprintf("%s contains %q", where, m))
			}
		}
	}
	for _, res := range results {
		check(fmt.Sprintf("s%d party recap", res.Proposal.Session), res.Proposal.RecapParty)
	}

	party := final.PartyView()
	for _, e := range party.Entities {
		names := make([]string, len(e.Names))
		for i, n := range e.Names {
			names[i] = n.Text
		}
		check("party entity names "+strings.Join(names, "/"), strings.Join(names, " "))
	}
	for _, f := range party.Facts {
		check("party fact "+f.ID, f.Statement+" "+f.Quote)
	}

	// Gated entities must not appear in the party view at all.
	for _, exp := range key.Entities {
		if exp.PartyFrom != nil {
			continue
		}
		for _, e := range party.Entities {
			if anyNameMatches(e, exp.names()) {
				leaks = append(leaks, fmt.Sprintf("hidden entity %q is visible to the party", exp.Name))
			}
		}
	}

	for _, id := range key.Secrets.Identities {
		for _, e := range party.Entities {
			if !anyNamePartMatches(e, id.Names) {
				continue
			}
			text := ""
			for _, n := range e.Names {
				text += n.Text + " "
			}
			for _, f := range party.FactsFor(e.ID) {
				text += f.Statement + " " + f.Quote + " "
			}
			for _, bad := range id.Forbidden {
				if containsAll(text, []string{bad}) {
					leaks = append(leaks, fmt.Sprintf("party view links %v to %q", id.Names, bad))
				}
			}
		}
	}
	return leaks
}

// Failures lists every threshold the report misses.
func (r Report) Failures(t Thresholds) []string {
	var out []string
	if r.EntityRecall() < t.EntityRecall {
		out = append(out, fmt.Sprintf("entity recall %.0f%% < %.0f%%", 100*r.EntityRecall(), 100*t.EntityRecall))
	}
	if r.FactRecall() < t.FactRecall {
		out = append(out, fmt.Sprintf("fact recall %.0f%% < %.0f%%", 100*r.FactRecall(), 100*t.FactRecall))
	}
	if r.ContradictionRecall() < t.ContradictionRecall {
		out = append(out, fmt.Sprintf("contradiction recall %.0f%% < %.0f%%", 100*r.ContradictionRecall(), 100*t.ContradictionRecall))
	}
	if r.FalseAlarmsPerSession() > t.MaxFalseAlarmsPerSess {
		out = append(out, fmt.Sprintf("false alarms %.1f/session > %.1f", r.FalseAlarmsPerSession(), t.MaxFalseAlarmsPerSess))
	}
	if len(r.WrongMerges) > t.MaxWrongMerges {
		out = append(out, fmt.Sprintf("wrong merges %d > %d", len(r.WrongMerges), t.MaxWrongMerges))
	}
	if len(r.SecretLeaks) > t.MaxSecretLeaks {
		out = append(out, fmt.Sprintf("SECRET LEAKS %d > %d", len(r.SecretLeaks), t.MaxSecretLeaks))
	}
	if len(r.VisibilityErrors) > 0 {
		// A GM fact shown as party is a leak in waiting; a party fact hidden is only lost value.
		for _, v := range r.VisibilityErrors {
			if strings.HasSuffix(v, "expected gm") {
				out = append(out, "GM-only fact marked party: "+v)
			}
		}
	}
	return out
}

// Print writes a human-readable scorecard.
func (r Report) Print(w io.Writer, t Thresholds) {
	row := func(label string, value string, ok bool) {
		mark := "PASS"
		if !ok {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  %-28s %-22s %s\n", label, value, mark)
	}
	fmt.Fprintf(w, "Scorecard (%d sessions)\n", r.Sessions)
	row("Entity recall", fmt.Sprintf("%d/%d (%.0f%%)", r.EntitiesFound, r.EntitiesExpected, 100*r.EntityRecall()), r.EntityRecall() >= t.EntityRecall)
	row("Fact recall", fmt.Sprintf("%d/%d (%.0f%%)", r.FactsFound, r.FactsExpected, 100*r.FactRecall()), r.FactRecall() >= t.FactRecall)
	row("Contradiction recall", fmt.Sprintf("%d/%d", r.ContradictionsFound, r.ContradictionsPlanted), r.ContradictionRecall() >= t.ContradictionRecall)
	row("False alarms", fmt.Sprintf("%d (%.1f/session)", len(r.FalseAlarms), r.FalseAlarmsPerSession()), r.FalseAlarmsPerSession() <= t.MaxFalseAlarmsPerSess)
	row("Wrong merges", fmt.Sprint(len(r.WrongMerges)), len(r.WrongMerges) <= t.MaxWrongMerges)
	row("Secret leaks", fmt.Sprint(len(r.SecretLeaks)), len(r.SecretLeaks) <= t.MaxSecretLeaks)
	fmt.Fprintf(w, "  %-28s %d calls, %d in / %d out tokens\n", "Model usage", r.TotalUsage.Calls, r.TotalUsage.InputTokens, r.TotalUsage.OutputTokens)

	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s:\n", title)
		for _, it := range items {
			fmt.Fprintf(w, "  - %s\n", it)
		}
	}
	section("SECRET LEAKS", r.SecretLeaks)
	section("Wrong merges", r.WrongMerges)
	section("Missing entities", r.MissingEntities)
	section("Split entities", r.SplitEntities)
	section("Missing facts", r.MissingFacts)
	section("Visibility errors", r.VisibilityErrors)
	section("Missed contradictions", r.MissedContradictions)
	section("False alarms", r.FalseAlarms)
}

// ---- matching helpers ----

func anyNameMatches(e canon.Entity, names []string) bool {
	for _, n := range names {
		if e.HasName(n) {
			return true
		}
	}
	return false
}

// anyNamePartMatches is looser: "Maren" matches an entity named "Lady Maren Ashcombe".
func anyNamePartMatches(e canon.Entity, parts []string) bool {
	for _, n := range e.Names {
		if containsAll(n.Text, parts) {
			return true
		}
	}
	return false
}

func containsAll(text string, needles []string) bool {
	t := strings.ToLower(text)
	for _, n := range needles {
		if !strings.Contains(t, strings.ToLower(n)) {
			return false
		}
	}
	return true
}

func findFact(c canon.Canon, id string) (canon.Fact, bool) {
	for _, f := range c.Facts {
		if f.ID == id {
			return f, true
		}
	}
	return canon.Fact{}, false
}
