package canon

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"campaign-assistant/backend/internal/llm"
)

// HeuristicModel is the offline stand-in for Bedrock (PROVIDER=mock). It reads
// each request's structured Input and answers with simple rules: capitalized
// phrases become entities, sentences become facts, and a few patterns (alive
// vs dead, ages, relatives' names) become contradictions.
//
// It exists so the whole pipeline runs end to end with no AWS account. Its
// output is deliberately plain; quality is measured with the real model.
type HeuristicModel struct{}

const heuristicModelName = "heuristic-mock"

// Complete implements llm.Model.
func (HeuristicModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	var (
		tool any
		text string
	)
	switch in := req.Input.(type) {
	case ExtractInput:
		tool = heuristicExtract(in)
	case MatchInput:
		tool = heuristicMatch(in)
	case ContradictInput:
		tool = heuristicContradict(in)
	case RecapInput:
		text = heuristicRecap(in)
	default:
		return llm.Response{}, fmt.Errorf("heuristic model: unsupported input %T for purpose %q", req.Input, req.Purpose)
	}

	resp := llm.Response{Model: heuristicModelName, StopReason: "end_turn", Text: text}
	if tool != nil {
		b, err := json.Marshal(tool)
		if err != nil {
			return llm.Response{}, err
		}
		resp.ToolInput, resp.StopReason = b, "tool_use"
	}
	promptChars := len(req.System)
	for _, m := range req.Messages {
		promptChars += len(m.Text)
	}
	resp.Usage = llm.Usage{Calls: 1, InputTokens: promptChars / 4, OutputTokens: (len(resp.ToolInput) + len(text)) / 4}
	return resp, nil
}

// ---- extraction ----

var (
	// Words that are capitalized for grammar, not because they name something.
	stopwords = toSet("a an the and but or so if as at by for from in into of off on onto out over to up with " +
		"i we you he she it they me us him her them my our your his its their this that these those there here " +
		"after afterwards then when while later meanwhile before during finally next first also once soon now " +
		"session gm secret notes party everyone nobody someone something nothing one two three four five " +
		"yes no not what who why where how which monday tuesday wednesday thursday friday saturday sunday " +
		"inside outside above below behind beneath near across along around through without within despite " +
		"although though because since until unless perhaps suddenly eventually instead still again even only just")
	titles      = toSet("lord lady duke duchess baron baroness count countess captain brother sister father mother sir dame king queen prince princess master mistress harbormaster old elder magister")
	placeWords  = toSet("inn tavern lighthouse manor temple keep tower harbor harbour docks forest wood woods mountains mountain river bay coast isle island town village city road market hall abbey crypt vault cave caves ruins port shrine")
	factionWord = toSet("guild order brotherhood sisterhood cult company circle council league crew gang syndicate")
	itemWords   = toSet("key sword blade amulet ring crown tome book map orb staff dagger shield idol relic chalice")
	placeSuffix = []string{"wood", "mere", "watch", "port", "ford", "haven", "holm", "gate", "fall", "falls"}

	sentenceEnd = regexp.MustCompile(`([.!?])\s+`)
	wordRe      = regexp.MustCompile(`[\p{L}][\p{L}'’\-]*`)
)

type span struct {
	text     string
	nextWord string // lowercase word right after the span, for type cues ("" after a possessive)
	atStart  bool   // span begins the sentence
}

func heuristicExtract(in ExtractInput) ExtractOutput {
	known := map[string]KnownEntity{} // normalized name/alias -> entity
	for _, k := range in.Known {
		known[NormalizeName(k.Name)] = k
		for _, a := range k.Aliases {
			known[NormalizeName(a)] = k
		}
	}
	party := map[string]bool{}
	for _, p := range in.Party {
		party[NormalizeName(p)] = true
	}

	type ent struct{ ExtractedEntity }
	var ents []*ent
	byKey := map[string]*ent{}
	add := func(name string, t EntityType, aliases []string, desc string) *ent {
		keys := append([]string{NormalizeName(name)}, normAll(aliases)...)
		for _, k := range keys {
			if e := byKey[k]; e != nil {
				return e
			}
		}
		e := &ent{ExtractedEntity: ExtractedEntity{Name: name, Type: t, Aliases: aliases, Description: desc}}
		ents = append(ents, e)
		for _, k := range keys {
			byKey[k] = e
		}
		return e
	}
	resolveName := func(s span) (*ent, bool) {
		name, aliases := splitTitle(s.text)
		for _, k := range append([]string{NormalizeName(s.text), NormalizeName(name)}, normAll(aliases)...) {
			if e := byKey[k]; e != nil {
				return e, true
			}
			if k2, ok := known[k]; ok {
				return add(k2.Name, k2.Type, nil, ""), true
			}
		}
		return nil, false
	}

	var facts []ExtractedFact
	for _, p := range in.Chunk.Paragraphs {
		vis := VisibilityParty
		if p.GMOnly {
			vis = VisibilityGM
		}
		for _, sentence := range splitSentences(p.Text) {
			spans := findSpans(sentence)
			// Longest spans first, so "Lady Maren Ashcombe" claims "Maren" before a bare "Maren" appears.
			var mentioned []*ent
			for _, s := range longestFirst(spans) {
				e, ok := resolveName(s)
				if !ok {
					if s.atStart && !strings.Contains(s.text, " ") && looksLikeCommonWord(s.text) {
						continue // "Rumors spread..." — a capitalized sentence opener, not a name
					}
					bare, _ := splitTitle(s.text)
					var aliases []string
					if bare != s.text {
						aliases = append(aliases, bare)
					}
					t := classify(s, party)
					if t == TypePC || t == TypeNPC {
						if f := firstWord(bare); f != bare {
							aliases = append(aliases, f)
						}
					}
					e = add(s.text, t, aliases, sentence)
				}
				if !containsPtr(mentioned, e) {
					mentioned = append(mentioned, e)
				}
			}
			if len(sentence) > 400 {
				continue
			}
			for _, e := range mentioned {
				facts = append(facts, ExtractedFact{Entity: e.Name, Statement: sentence, Quote: sentence, Visibility: vis})
			}
		}
	}

	out := ExtractOutput{Facts: facts}
	for _, e := range ents {
		out.Entities = append(out.Entities, e.ExtractedEntity)
	}
	return out
}

func splitSentences(text string) []string {
	text = strings.Join(strings.Fields(text), " ")
	var out []string
	last := 0
	for _, m := range sentenceEnd.FindAllStringSubmatchIndex(text, -1) {
		end := m[3] // include the punctuation
		if next, _ := utf8.DecodeRuneInString(text[m[1]:]); m[1] < len(text) && !unicode.IsUpper(next) && next != '"' {
			continue // "e.g. something" — not a sentence break
		}
		if s := strings.TrimSpace(text[last:end]); s != "" {
			out = append(out, s)
		}
		last = m[1]
	}
	if s := strings.TrimSpace(text[last:]); s != "" {
		out = append(out, s)
	}
	return out
}

// findSpans returns runs of capitalized words ("Oswin Pell", "Order of the Pale Tide").
func findSpans(sentence string) []span {
	locs := wordRe.FindAllStringIndex(sentence, -1)
	words := make([]string, len(locs))
	for i, l := range locs {
		words[i] = strings.TrimSuffix(strings.TrimSuffix(sentence[l[0]:l[1]], "'s"), "’s")
	}
	isCap := func(i int) bool {
		r, _ := utf8.DecodeRuneInString(words[i])
		return unicode.IsUpper(r) && !stopwords[strings.ToLower(words[i])]
	}
	adjacent := func(i int) bool { // only whitespace between word i-1 and word i
		return strings.TrimSpace(sentence[locs[i-1][1]:locs[i][0]]) == ""
	}

	var out []span
	for i := 0; i < len(words); i++ {
		if !isCap(i) {
			continue
		}
		j := i + 1
		for j < len(words) && adjacent(j) {
			if isCap(j) {
				j++
				continue
			}
			// "Order of the Pale Tide": connectors followed by a capitalized word.
			lw := strings.ToLower(words[j])
			if (lw == "of" || lw == "the") && j+1 < len(words) && adjacent(j+1) && (isCap(j+1) || (strings.ToLower(words[j+1]) == "the" && j+2 < len(words) && isCap(j+2))) {
				j++
				continue
			}
			break
		}
		// Drop trailing connectors.
		for j > i+1 && (strings.EqualFold(words[j-1], "of") || strings.EqualFold(words[j-1], "the")) {
			j--
		}
		raw := sentence[locs[i][0]:locs[j-1][1]]
		s := span{text: strings.TrimSuffix(strings.TrimSuffix(raw, "'s"), "’s"), atStart: i == 0}
		if possessive := s.text != raw; j < len(words) && !possessive && adjacent(j) {
			s.nextWord = strings.ToLower(words[j])
		}
		out = append(out, s)
		i = j - 1
	}
	return out
}

func classify(s span, party map[string]bool) EntityType {
	name, _ := splitTitle(s.text)
	if party[NormalizeName(s.text)] || party[NormalizeName(name)] {
		return TypePC
	}
	words := strings.Fields(strings.ToLower(s.text))
	last := words[len(words)-1]
	switch {
	case factionWord[words[0]] || factionWord[last] || factionWord[s.nextWord]:
		return TypeFaction
	case itemWords[last] || itemWords[s.nextWord]:
		return TypeItem
	case placeWords[last] || placeWords[s.nextWord] || hasPlaceSuffix(last):
		return TypePlace
	}
	return TypeNPC
}

func looksLikeCommonWord(w string) bool {
	lw := strings.ToLower(w)
	for _, suf := range []string{"ing", "ed", "ly", "s"} {
		if strings.HasSuffix(lw, suf) {
			return true
		}
	}
	return false
}

func hasPlaceSuffix(w string) bool {
	for _, suf := range placeSuffix {
		if strings.HasSuffix(w, suf) && len(w) > len(suf)+2 {
			return true
		}
	}
	return false
}

// splitTitle turns "Lady Maren Ashcombe" into ("Maren Ashcombe", ["Maren Ashcombe"]) when it has a title.
func splitTitle(name string) (string, []string) {
	fields := strings.Fields(name)
	if len(fields) > 1 && titles[strings.ToLower(fields[0])] {
		bare := strings.Join(fields[1:], " ")
		return bare, []string{bare}
	}
	return name, nil
}

func firstWord(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return s
}

func longestFirst(spans []span) []span {
	out := append([]span(nil), spans...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j].text) > len(out[j-1].text); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---- matching, contradictions, recaps ----

func heuristicMatch(in MatchInput) MatchOutput {
	cand := significantWords(append([]string{in.Candidate.Name}, in.Candidate.Aliases...))
	for _, o := range in.Options {
		if !compatibleTypes(in.Candidate.Type, o.Type) {
			continue // "Ashcombe Manor" is not "Lady Maren Ashcombe"
		}
		for w := range significantWords(o.Names) {
			if cand[w] {
				return MatchOutput{MatchID: o.ID, Confidence: "medium", Reason: fmt.Sprintf("shares the name %q", w)}
			}
		}
	}
	return MatchOutput{Confidence: "high", Reason: "no shared names"}
}

// compatibleTypes treats people (npc/pc) as one family, since extraction often
// can't tell; everything else must match exactly.
func compatibleTypes(a, b EntityType) bool {
	person := func(t EntityType) bool { return t == TypeNPC || t == TypePC }
	return a == b || (person(a) && person(b))
}

func significantWords(names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		for _, w := range strings.Fields(NormalizeName(n)) {
			if len(w) >= 4 && !titles[w] && !stopwords[w] {
				out[w] = true
			}
		}
	}
	return out
}

var (
	aliveRe    = regexp.MustCompile(`(?i)\b(alive|alive and well)\b`)
	deadRe     = regexp.MustCompile(`(?i)\b(killed|died|dead|slain|murdered)\b`)
	ageRe      = regexp.MustCompile(`\b(\d{1,3}) years old\b`)
	relativeRe = regexp.MustCompile(`(?i:\b(daughter|son|wife|husband|brother|sister)\b),?\s+(?:(?i:named|called)\s+)?([A-Z][a-z]+)`)
)

func heuristicContradict(in ContradictInput) ContradictOutput {
	var out ContradictOutput
	for _, nf := range in.New {
		for _, ef := range in.Existing {
			if why := conflict(nf.Statement, ef.Statement); why != "" {
				out.Contradictions = append(out.Contradictions, FoundContradiction{NewFact: nf.Index, ExistingFactID: ef.ID,
					Explanation: fmt.Sprintf("%s (established in session %d).", why, ef.Session)})
			}
		}
	}
	return out
}

func conflict(newer, older string) string {
	switch {
	case aliveRe.MatchString(newer) && deadRe.MatchString(older):
		return "Described as alive, but earlier notes say they died"
	case deadRe.MatchString(newer) && aliveRe.MatchString(older):
		return "Described as dead, but earlier notes say they were alive"
	}
	if a, b := ageRe.FindStringSubmatch(newer), ageRe.FindStringSubmatch(older); a != nil && b != nil && a[1] != b[1] {
		return fmt.Sprintf("Age given as %s, but earlier as %s", a[1], b[1])
	}
	for _, a := range relativeRe.FindAllStringSubmatch(newer, -1) {
		for _, b := range relativeRe.FindAllStringSubmatch(older, -1) {
			if strings.EqualFold(a[1], b[1]) && a[2] != b[2] {
				return fmt.Sprintf("Their %s is named %s here, but %s earlier", strings.ToLower(a[1]), a[2], b[2])
			}
		}
	}
	return ""
}

func heuristicRecap(in RecapInput) string {
	var b strings.Builder
	if in.Audience == VisibilityGM {
		fmt.Fprintf(&b, "Session %d (GM recap)\n", in.Session)
	} else {
		fmt.Fprintf(&b, "Previously, in session %d...\n", in.Session)
	}
	seen := map[string]bool{}
	n := 0
	for _, f := range in.Facts {
		if seen[f.Statement] || n == 12 {
			continue
		}
		seen[f.Statement] = true
		n++
		fmt.Fprintf(&b, "- %s\n", f.Statement)
	}
	return strings.TrimSpace(b.String())
}

// ---- small helpers ----

func toSet(words string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(words) {
		out[w] = true
	}
	return out
}

func normAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = NormalizeName(x)
	}
	return out
}

func containsPtr[T any](xs []*T, x *T) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
