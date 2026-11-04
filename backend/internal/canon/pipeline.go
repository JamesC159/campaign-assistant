package canon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"campaign-assistant/backend/internal/embed"
	"campaign-assistant/backend/internal/llm"
)

// Options tunes the pipeline. Zero values fall back to DefaultOptions.
type Options struct {
	ChunkChars  int // max characters of notes per extraction call
	Concurrency int // parallel model calls per step
	// Entity matching: an embedding score at or above AutoMatch links without asking
	// the model; scores between Candidate and AutoMatch go to a model tiebreak.
	AutoMatchScore float32
	CandidateScore float32
	MaxCandidates  int
	// MaxEstablishedFacts caps how many existing facts one contradiction check sees
	// (the most similar to the new facts are kept).
	MaxEstablishedFacts int
}

// DefaultOptions are tuned for production embeddings.
func DefaultOptions() Options {
	return Options{ChunkChars: 12000, Concurrency: 4, AutoMatchScore: 0.92, CandidateScore: 0.45, MaxCandidates: 3, MaxEstablishedFacts: 40}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.ChunkChars <= 0 {
		o.ChunkChars = d.ChunkChars
	}
	if o.Concurrency <= 0 {
		o.Concurrency = d.Concurrency
	}
	if o.AutoMatchScore <= 0 {
		o.AutoMatchScore = d.AutoMatchScore
	}
	if o.CandidateScore <= 0 {
		o.CandidateScore = d.CandidateScore
	}
	if o.MaxCandidates <= 0 {
		o.MaxCandidates = d.MaxCandidates
	}
	if o.MaxEstablishedFacts <= 0 {
		o.MaxEstablishedFacts = d.MaxEstablishedFacts
	}
	return o
}

// Engine runs the session pipeline. It holds no campaign state; all state is
// passed in (Canon) and returned (Proposal), so it is safe for concurrent use.
type Engine struct {
	Model    llm.Model
	Embedder embed.Embedder
	Options  Options
}

// Session is one game session's notes.
type Session struct {
	Number int
	Notes  string
}

// Proposal is everything the pipeline suggests for one session. Nothing in it
// is canon until a GM approves it (Apply approves all of it at once).
type Proposal struct {
	Session        int              `json:"session"`
	Entities       []ProposedEntity `json:"entities"`
	Facts          []ProposedFact   `json:"facts"`
	Contradictions []Contradiction  `json:"contradictions"`
	Reviews        []ReviewItem     `json:"reviews"`
	RecapGM        string           `json:"recap_gm"`
	RecapParty     string           `json:"recap_party"`
	// RecapPartyFallback is true when the model's player recap failed the secret
	// check twice and was replaced by a plain list of party-visible facts.
	RecapPartyFallback bool                 `json:"recap_party_fallback"`
	Stats              Stats                `json:"stats"`
	Usage              map[string]llm.Usage `json:"usage"`
}

// ProposedEntity is a new entity, or new names for an existing one.
type ProposedEntity struct {
	// Ref is the existing entity ID, or "new:N" for a new entity.
	Ref         string     `json:"ref"`
	New         bool       `json:"new"`
	Type        EntityType `json:"type"`
	Names       []Name     `json:"names"` // every name seen this session, with visibility
	Description string     `json:"description,omitempty"`
	Match       *MatchInfo `json:"match,omitempty"` // how an existing entity was identified
}

// MatchInfo explains how an extracted entity was linked to an existing one.
type MatchInfo struct {
	Method        string  `json:"method"` // exact | embedding | model
	Score         float32 `json:"score,omitempty"`
	ExtractedName string  `json:"extracted_name"`
	Confidence    string  `json:"confidence,omitempty"`
}

// ProposedFact is a fact awaiting approval.
type ProposedFact struct {
	EntityRef  string     `json:"entity_ref"`
	Statement  string     `json:"statement"`
	Quote      string     `json:"quote"`
	Visibility Visibility `json:"visibility"`
	Paragraph  int        `json:"paragraph"`
}

// Contradiction says a proposed fact conflicts with an established one.
type Contradiction struct {
	FactIndex      int    `json:"fact_index"` // into Proposal.Facts
	ExistingFactID string `json:"existing_fact_id"`
	Explanation    string `json:"explanation"`
}

// ReviewItem is something the GM should look at before approving.
type ReviewItem struct {
	Kind      string `json:"kind"` // uncertain_match | unnamed_for_party
	Message   string `json:"message"`
	EntityRef string `json:"entity_ref,omitempty"`
}

// Stats counts what happened in each step (useful in evals and the UI).
type Stats struct {
	Chunks             int `json:"chunks"`
	ExtractedEntities  int `json:"extracted_entities"`
	ExtractedFacts     int `json:"extracted_facts"`
	DroppedBadQuote    int `json:"dropped_bad_quote"`
	DroppedNoEntity    int `json:"dropped_unknown_entity"`
	DroppedDuplicate   int `json:"dropped_duplicate"`
	ForcedGM           int `json:"forced_gm"`           // model said party, but the quote is GM-only text
	HiddenUnnamed      int `json:"hidden_unnamed"`      // party fact about an entity players can't name
	HiddenIdentity     int `json:"hidden_identity"`     // party fact links an entity to a name players can't connect
	StatementsReplaced int `json:"statements_replaced"` // statement leaked a secret term; replaced by quote
}

// Process runs the full pipeline for one session against the current canon.
func (e *Engine) Process(ctx context.Context, current Canon, s Session) (*Proposal, error) {
	if strings.TrimSpace(s.Notes) == "" {
		return nil, errors.New("session notes are empty")
	}
	opts := e.Options.withDefaults()
	m := &meter{next: e.Model, usage: map[string]llm.Usage{}}
	p := &Proposal{Session: s.Number}

	paras := ParseNotes(s.Notes)
	chunks := ChunkParagraphs(paras, opts.ChunkChars)
	p.Stats.Chunks = len(chunks)

	extracted, err := e.extract(ctx, m, opts, current, chunks, &p.Stats)
	if err != nil {
		return nil, err
	}
	resolved, err := e.resolve(ctx, m, opts, current, extracted, p)
	if err != nil {
		return nil, err
	}
	applyVisibility(current, resolved, PartyText(paras), p)
	if err := e.checkContradictions(ctx, m, opts, current, p); err != nil {
		return nil, err
	}
	if err := e.writeRecaps(ctx, m, current, s, p); err != nil {
		return nil, err
	}
	p.Usage = m.snapshot()
	return p, nil
}

// ---- Step 1: extraction ----

// sessionEntity is an extracted entity merged across chunks, with its facts.
type sessionEntity struct {
	ExtractedEntity
	names []string
	facts []ProposedFact
}

func (e *Engine) extract(ctx context.Context, m llm.Model, opts Options, current Canon, chunks []Chunk, st *Stats) ([]*sessionEntity, error) {
	in := ExtractInput{Party: partyNames(current), Known: knownEntities(current)}
	outs := make([]ExtractOutput, len(chunks))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)
	for i, c := range chunks {
		g.Go(func() error {
			ci := in
			ci.Chunk = c
			out, err := callTool[ExtractOutput](gctx, m, extractRequest(ci))
			if err != nil {
				return fmt.Errorf("extract chunk %d: %w", i, err)
			}
			outs[i] = out
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Merge per-chunk results in chunk order so output is deterministic.
	var ents []*sessionEntity
	byName := map[string]*sessionEntity{}
	lookup := func(name string) *sessionEntity { return byName[NormalizeName(name)] }
	register := func(se *sessionEntity, name string) {
		key := NormalizeName(name)
		if key == "" {
			return
		}
		if _, taken := byName[key]; !taken {
			byName[key] = se
			se.names = append(se.names, strings.TrimSpace(name))
		}
	}
	seenQuote := map[string]bool{}

	for ci, out := range outs {
		st.ExtractedEntities += len(out.Entities)
		st.ExtractedFacts += len(out.Facts)
		for _, x := range out.Entities {
			if strings.TrimSpace(x.Name) == "" {
				continue
			}
			if !x.Type.Valid() {
				x.Type = TypeNPC
			}
			se := lookup(x.Name)
			for _, a := range x.Aliases {
				if se == nil {
					se = lookup(a)
				}
			}
			if se == nil {
				se = &sessionEntity{ExtractedEntity: x}
				ents = append(ents, se)
			}
			if se.Description == "" {
				se.Description = x.Description
			}
			register(se, x.Name)
			for _, a := range x.Aliases {
				register(se, a)
			}
		}
		// Facts may reference known canon entities that were not re-listed in this chunk.
		for _, f := range out.Facts {
			se := lookup(f.Entity)
			if se == nil {
				if k, ok := findKnown(current, f.Entity); ok {
					se = &sessionEntity{ExtractedEntity: ExtractedEntity{Name: k.CanonicalName(), Type: k.Type}}
					ents = append(ents, se)
					register(se, k.CanonicalName())
					register(se, f.Entity)
				}
			}
			if se == nil {
				st.DroppedNoEntity++
				continue
			}
			para, ok := locateQuote(chunks[ci], f.Quote)
			if !ok {
				st.DroppedBadQuote++
				continue
			}
			vis := f.Visibility
			if vis != VisibilityGM && vis != VisibilityParty {
				vis = VisibilityGM // unknown visibility: fail closed
			}
			if para.GMOnly && vis == VisibilityParty {
				vis = VisibilityGM
				st.ForcedGM++
			}
			dedupe := NormalizeName(se.Name) + "\x00" + normalizeText(f.Statement)
			if seenQuote[dedupe] {
				st.DroppedDuplicate++
				continue
			}
			seenQuote[dedupe] = true
			statement := strings.TrimSpace(f.Statement)
			if statement == "" {
				statement = strings.TrimSpace(f.Quote)
			}
			se.facts = append(se.facts, ProposedFact{Statement: statement, Quote: strings.TrimSpace(f.Quote), Visibility: vis, Paragraph: para.Index})
		}
	}
	return ents, nil
}

// ---- Step 2: entity resolution ----

type resolved struct {
	se    *sessionEntity
	ref   string // existing ID or new:N
	new   bool
	match *MatchInfo
}

// resolve links each extracted entity to an existing one or marks it new, then
// merges extracted entities that resolved to the same existing entity.
func (e *Engine) resolve(ctx context.Context, m llm.Model, opts Options, current Canon, ents []*sessionEntity, p *Proposal) ([]resolved, error) {
	out := make([]resolved, len(ents))
	var pending []int // indexes needing embedding / model matching

	for i, se := range ents {
		if id, ok := exactMatch(current, se.names); ok {
			out[i] = resolved{se: se, ref: id, match: &MatchInfo{Method: "exact", ExtractedName: se.Name}}
			continue
		}
		pending = append(pending, i)
	}

	if len(pending) > 0 && len(current.Entities) > 0 {
		texts := make([]string, 0, len(pending)+len(current.Entities))
		for _, i := range pending {
			texts = append(texts, entityText(ents[i].names, ents[i].Type, ents[i].Description))
		}
		for _, ce := range current.Entities {
			texts = append(texts, entityText(nameTexts(ce.Names), ce.Type, ce.Description))
		}
		vecs, err := e.Embedder.Embed(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("embed entities: %w", err)
		}
		canonVecs := vecs[len(pending):]

		for k, i := range pending {
			se := ents[i]
			type cand struct {
				idx   int
				score float32
			}
			var cands []cand
			for j := range current.Entities {
				if s := embed.Cosine(vecs[k], canonVecs[j]); s >= opts.CandidateScore {
					cands = append(cands, cand{j, s})
				}
			}
			sort.SliceStable(cands, func(a, b int) bool { return cands[a].score > cands[b].score })
			if len(cands) > opts.MaxCandidates {
				cands = cands[:opts.MaxCandidates]
			}
			if len(cands) == 0 {
				continue
			}
			if top := cands[0]; top.score >= opts.AutoMatchScore {
				out[i] = resolved{se: se, ref: current.Entities[top.idx].ID, match: &MatchInfo{Method: "embedding", Score: top.score, ExtractedName: se.Name}}
				continue
			}

			mi := MatchInput{Candidate: se.ExtractedEntity}
			mi.Candidate.Aliases = se.names[1:]
			for _, c := range cands {
				ce := current.Entities[c.idx]
				mi.Options = append(mi.Options, MatchOption{ID: ce.ID, Type: ce.Type, Names: nameTexts(ce.Names), Description: ce.Description})
			}
			dec, err := callTool[MatchOutput](ctx, m, matchRequest(mi))
			if err != nil {
				return nil, fmt.Errorf("match %q: %w", se.Name, err)
			}
			if dec.MatchID == "" || !slices.ContainsFunc(mi.Options, func(o MatchOption) bool { return o.ID == dec.MatchID }) {
				continue // new entity (or an invalid ID, treated as no match)
			}
			out[i] = resolved{se: se, ref: dec.MatchID, match: &MatchInfo{Method: "model", Score: cands[0].score, ExtractedName: se.Name, Confidence: dec.Confidence}}
			if dec.Confidence != "high" {
				p.Reviews = append(p.Reviews, ReviewItem{Kind: "uncertain_match", EntityRef: dec.MatchID,
					Message: fmt.Sprintf("%q was linked to an existing entity with %s confidence: %s", se.Name, dec.Confidence, dec.Reason)})
			}
		}
	}

	n := 0
	for i, se := range ents {
		if out[i].ref == "" {
			n++
			out[i] = resolved{se: se, ref: fmt.Sprintf("new:%d", n), new: true}
		}
	}

	// Two extracted entities can resolve to the same existing entity (e.g. "Oswin
	// Pell" in one chunk, "the harbormaster" in another): merge them.
	var merged []resolved
	at := map[string]int{}
	for _, r := range out {
		if j, ok := at[r.ref]; ok {
			dst := merged[j].se
			for _, name := range r.se.names {
				if !slices.ContainsFunc(dst.names, func(x string) bool { return NormalizeName(x) == NormalizeName(name) }) {
					dst.names = append(dst.names, name)
				}
			}
			dst.facts = append(dst.facts, r.se.facts...)
			continue
		}
		at[r.ref] = len(merged)
		merged = append(merged, r)
	}
	for _, r := range merged {
		pe := ProposedEntity{Ref: r.ref, New: r.new, Type: r.se.Type, Match: r.match}
		if r.new {
			pe.Description = r.se.Description
		}
		p.Entities = append(p.Entities, pe)
	}
	return merged, nil
}

// ---- Step 3: visibility ----

// applyVisibility decides which names players know and makes every
// party-visible fact safe to show them.
//
// A name is party-visible when it appears in party-visible text AND is linked to
// a name the party already uses for that entity (one contains the other, as in
// "Maren" / "Lady Maren Ashcombe"). A second, unrelated name the party also
// knows ("a hooded stranger") stays GM-only: the party knows both names but not
// that they are the same being. Party facts that mention such an unlinked name
// are kept GM-only and flagged for review.
//
// Finally, a party fact's statement (model prose) must not contain a secret
// term; if it does, it is replaced by the fact's quote, which is party text.
func applyVisibility(current Canon, rs []resolved, partyText string, p *Proposal) {
	byRef := map[string]*ProposedEntity{}
	for i := range p.Entities {
		byRef[p.Entities[i].Ref] = &p.Entities[i]
	}

	unlinked := map[string][]string{} // ref -> names players know but can't connect to this entity
	for _, r := range rs {
		pe := byRef[r.ref]
		ce, existing := current.Entity(r.ref)

		var linked []string // party names already accepted for this entity
		if existing {
			for _, n := range ce.Names {
				if n.Visibility == VisibilityParty {
					linked = append(linked, n.Text)
				}
			}
		}
		// This session's names, then the entity's existing names (so an alias the
		// party has now heard can be promoted), deduplicated.
		var uniq []string
		seen := map[string]bool{}
		all := r.se.names
		if existing {
			all = append(slices.Clone(all), nameTexts(ce.Names)...)
		}
		for _, raw := range all {
			if key := NormalizeName(raw); !seen[key] {
				seen[key] = true
				uniq = append(uniq, raw)
			}
		}
		var candidates []string // names seen in party text this session, not yet linked
		for _, raw := range uniq {
			alreadyParty := slices.ContainsFunc(linked, func(l string) bool { return NormalizeName(l) == NormalizeName(raw) })
			if !alreadyParty && containsTerm(partyText, raw) {
				candidates = append(candidates, raw)
			}
		}
		if len(linked) == 0 && len(candidates) > 0 {
			linked = append(linked, candidates[0]) // first name the party learns anchors the entity
			candidates = candidates[1:]
		}
		for changed := true; changed; { // link names that are parts of each other, transitively
			changed = false
			for i := 0; i < len(candidates); i++ {
				if containsAnyName(linked, candidates[i]) {
					linked = append(linked, candidates[i])
					candidates = slices.Delete(candidates, i, i+1)
					changed = true
					i--
				}
			}
		}
		unlinked[r.ref] = candidates

		isParty := map[string]bool{}
		for _, l := range linked {
			isParty[NormalizeName(l)] = true
		}
		for _, raw := range uniq {
			v := VisibilityGM
			if isParty[NormalizeName(raw)] {
				v = VisibilityParty
			}
			pe.Names = append(pe.Names, Name{Text: raw, Visibility: v})
		}
		if len(candidates) > 0 {
			p.Reviews = append(p.Reviews, ReviewItem{Kind: "possible_secret_identity", EntityRef: r.ref,
				Message: fmt.Sprintf("Players know the name(s) %s, but nothing at the table links them to %q, so they stay GM-only. Link them if the party has learned the connection.", quoteList(candidates), r.se.Name)})
		}
	}

	secrets := secretTerms(current, p)
	partyNamed := func(ref string) bool {
		for _, n := range byRef[ref].Names {
			if n.Visibility == VisibilityParty {
				return true
			}
		}
		ce, ok := current.Entity(ref)
		return ok && ce.PartyName() != ""
	}

	warned := map[string]bool{}
	for _, r := range rs {
		for _, f := range r.se.facts {
			f.EntityRef = r.ref
			if f.Visibility == VisibilityParty {
				switch {
				case !partyNamed(r.ref):
					f.Visibility = VisibilityGM
					p.Stats.HiddenUnnamed++
					if !warned[r.ref] {
						warned[r.ref] = true
						p.Reviews = append(p.Reviews, ReviewItem{Kind: "unnamed_for_party", EntityRef: r.ref,
							Message: fmt.Sprintf("Players saw things about %q but don't know any name for it, so those facts were kept GM-only.", r.se.Name)})
					}
				case mentionsAny(f.Statement+"\n"+f.Quote, unlinked[r.ref]):
					f.Visibility = VisibilityGM
					p.Stats.HiddenIdentity++
				case revealsSecret(f.Statement, secrets, f.Quote):
					f.Statement = f.Quote // the quote is party text, so it holds no secret terms
					p.Stats.StatementsReplaced++
				}
			}
			p.Facts = append(p.Facts, f)
		}
	}
}

// containsAnyName reports whether name and any of names are parts of each other.
func containsAnyName(names []string, name string) bool {
	for _, n := range names {
		if containsTerm(n, name) || containsTerm(name, n) {
			return true
		}
	}
	return false
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = fmt.Sprintf("%q", x)
	}
	return strings.Join(q, ", ")
}

// ---- Step 4: contradictions ----

func (e *Engine) checkContradictions(ctx context.Context, m llm.Model, opts Options, current Canon, p *Proposal) error {
	type job struct {
		entity   Entity
		existing []Fact
		newIdx   []int
	}
	var jobs []job
	byEntity := map[string][]int{}
	var order []string
	for i, f := range p.Facts {
		if strings.HasPrefix(f.EntityRef, "new:") {
			continue
		}
		if _, ok := byEntity[f.EntityRef]; !ok {
			order = append(order, f.EntityRef)
		}
		byEntity[f.EntityRef] = append(byEntity[f.EntityRef], i)
	}
	for _, id := range order {
		ent, _ := current.Entity(id)
		existing := current.FactsFor(id)
		if len(existing) == 0 {
			continue
		}
		jobs = append(jobs, job{entity: ent, existing: existing, newIdx: byEntity[id]})
	}

	results := make([][]Contradiction, len(jobs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(opts.Concurrency)
	for ji, j := range jobs {
		g.Go(func() error {
			established, err := e.mostRelevant(gctx, j.existing, p, j.newIdx, opts.MaxEstablishedFacts)
			if err != nil {
				return err
			}
			in := ContradictInput{Entity: j.entity.CanonicalName()}
			for _, f := range established {
				in.Existing = append(in.Existing, FactRef{ID: f.ID, Session: f.Session, Statement: f.Statement})
			}
			for local, idx := range j.newIdx {
				in.New = append(in.New, NewFactRef{Index: local, Statement: p.Facts[idx].Statement})
			}
			out, err := callTool[ContradictOutput](gctx, m, contradictRequest(in))
			if err != nil {
				return fmt.Errorf("contradiction check for %q: %w", in.Entity, err)
			}
			valid := map[string]bool{}
			for _, f := range established {
				valid[f.ID] = true
			}
			for _, c := range out.Contradictions {
				if c.NewFact < 0 || c.NewFact >= len(j.newIdx) || !valid[c.ExistingFactID] {
					continue // ignore references the model made up
				}
				results[ji] = append(results[ji], Contradiction{FactIndex: j.newIdx[c.NewFact], ExistingFactID: c.ExistingFactID, Explanation: c.Explanation})
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for _, r := range results {
		p.Contradictions = append(p.Contradictions, r...)
	}
	return nil
}

// mostRelevant keeps the established facts most similar to the new ones when
// there are more than limit.
func (e *Engine) mostRelevant(ctx context.Context, existing []Fact, p *Proposal, newIdx []int, limit int) ([]Fact, error) {
	if len(existing) <= limit {
		return existing, nil
	}
	texts := make([]string, 0, len(existing)+len(newIdx))
	for _, f := range existing {
		texts = append(texts, f.Statement)
	}
	for _, i := range newIdx {
		texts = append(texts, p.Facts[i].Statement)
	}
	vecs, err := e.Embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embed facts: %w", err)
	}
	type scored struct {
		f     Fact
		score float32
	}
	ranked := make([]scored, len(existing))
	for i, f := range existing {
		var best float32
		for _, nv := range vecs[len(existing):] {
			best = max(best, embed.Cosine(vecs[i], nv))
		}
		ranked[i] = scored{f, best}
	}
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].score > ranked[b].score })
	out := make([]Fact, limit)
	for i := range out {
		out[i] = ranked[i].f
	}
	return out, nil
}

// ---- Step 5: recaps ----

func (e *Engine) writeRecaps(ctx context.Context, m llm.Model, current Canon, s Session, p *Proposal) error {
	names := map[string]string{} // ref -> GM name
	party := map[string]string{} // ref -> party name
	for _, pe := range p.Entities {
		for _, n := range pe.Names {
			if names[pe.Ref] == "" {
				names[pe.Ref] = n.Text
			}
			if party[pe.Ref] == "" && n.Visibility == VisibilityParty {
				party[pe.Ref] = n.Text
			}
		}
		if ce, ok := current.Entity(pe.Ref); ok {
			names[pe.Ref] = ce.CanonicalName()
			if pn := ce.PartyName(); pn != "" {
				party[pe.Ref] = pn
			}
		}
	}

	var gmFacts, partyFacts []RecapFact
	for _, f := range p.Facts {
		gmFacts = append(gmFacts, RecapFact{Entity: names[f.EntityRef], Statement: f.Statement})
		if f.Visibility == VisibilityParty {
			partyFacts = append(partyFacts, RecapFact{Entity: party[f.EntityRef], Statement: f.Statement})
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		resp, err := m.Complete(gctx, recapRequest(RecapInput{Session: s.Number, Audience: VisibilityGM, Facts: gmFacts, Notes: s.Notes}))
		if err != nil {
			return fmt.Errorf("gm recap: %w", err)
		}
		p.RecapGM = strings.TrimSpace(resp.Text)
		return nil
	})
	g.Go(func() error {
		recap, fallback, err := partyRecap(gctx, m, s.Number, partyFacts, secretTerms(current, p))
		if err != nil {
			return fmt.Errorf("party recap: %w", err)
		}
		p.RecapParty, p.RecapPartyFallback = recap, fallback
		return nil
	})
	return g.Wait()
}

// partyRecap writes the player recap from party-visible facts only, then checks
// it for secret terms. On a leak it retries once, then falls back to a plain list.
func partyRecap(ctx context.Context, m llm.Model, session int, facts []RecapFact, secrets []string) (string, bool, error) {
	if len(facts) == 0 {
		return "Nothing new was recorded for the party this session.", true, nil
	}
	var given strings.Builder // everything the recap writer was shown is fair game
	for _, f := range facts {
		given.WriteString(f.Entity + ". " + f.Statement + "\n")
	}
	in := RecapInput{Session: session, Audience: VisibilityParty, Facts: facts}
	for attempt := 0; attempt < 2; attempt++ {
		in.Retry = attempt > 0
		resp, err := m.Complete(ctx, recapRequest(in))
		if err != nil {
			return "", false, err
		}
		text := strings.TrimSpace(resp.Text)
		if text != "" && !revealsSecret(text, secrets, given.String()) {
			return text, false, nil
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Previously, in session %d:\n", session)
	for _, f := range facts {
		fmt.Fprintf(&b, "- %s\n", f.Statement)
	}
	return strings.TrimSpace(b.String()), true, nil
}

// secretTerms lists every name not known to the party after this proposal.
func secretTerms(current Canon, p *Proposal) []string {
	vis := map[string]Visibility{}
	note := func(n Name) {
		key := NormalizeName(n.Text)
		if vis[key] != VisibilityParty {
			vis[key] = n.Visibility
		}
	}
	for _, e := range current.Entities {
		for _, n := range e.Names {
			note(n)
		}
	}
	for _, pe := range p.Entities {
		for _, n := range pe.Names {
			note(n)
		}
	}
	var partyNames []string
	for term, v := range vis {
		if v == VisibilityParty {
			partyNames = append(partyNames, term)
		}
	}
	var out []string
	for term, v := range vis {
		// "Maren" is no secret once the party knows "Lady Maren Ashcombe".
		if v == VisibilityGM && !slices.ContainsFunc(partyNames, func(pn string) bool { return containsTerm(pn, term) }) {
			out = append(out, term)
		}
	}
	sort.Strings(out)
	return out
}

// ---- helpers ----

// revealsSecret reports whether text reveals a secret name, in full or in part:
// "Beware the Pale Tide" reveals "Order of the Pale Tide". It checks every
// multi-word piece of each secret name, and each distinctive single word when
// written capitalized (a proper-noun use, so "the tide was high" is fine).
// Pieces that also appear in allowed, text the reader may already see, don't count.
func revealsSecret(text string, secrets []string, allowed string) bool {
	for _, secret := range secrets {
		words := strings.Fields(secret)
		for i := range words {
			for j := i + 2; j <= len(words); j++ {
				if allStopwords(words[i:j]) {
					continue
				}
				phrase := strings.Join(words[i:j], " ")
				if containsTerm(text, phrase) && !containsTerm(allowed, phrase) {
					return true
				}
			}
			w := words[i]
			if len([]rune(w)) >= 4 && !stopwords[w] && !titles[w] && hasCapitalizedWord(text, w) && !containsTerm(allowed, w) {
				return true
			}
		}
		if len(words) == 1 && containsTerm(text, secret) && !containsTerm(allowed, secret) {
			return true
		}
	}
	return false
}

func allStopwords(ws []string) bool {
	for _, w := range ws {
		if !stopwords[w] {
			return false
		}
	}
	return true
}

// hasCapitalizedWord reports whether text contains word (case-insensitive) with an uppercase first letter.
func hasCapitalizedWord(text, word string) bool {
	for _, loc := range wordRe.FindAllStringIndex(text, -1) {
		w := strings.TrimSuffix(strings.TrimSuffix(text[loc[0]:loc[1]], "'s"), "’s")
		if strings.EqualFold(w, word) {
			if r, _ := utf8.DecodeRuneInString(w); unicode.IsUpper(r) {
				return true
			}
		}
	}
	return false
}

func mentionsAny(text string, terms []string) bool {
	for _, t := range terms {
		if containsTerm(text, t) {
			return true
		}
	}
	return false
}

func exactMatch(c Canon, names []string) (string, bool) {
	for _, n := range names {
		for _, e := range c.Entities {
			if e.HasName(n) {
				return e.ID, true
			}
		}
	}
	return "", false
}

func findKnown(c Canon, name string) (Entity, bool) {
	for _, e := range c.Entities {
		if e.HasName(name) {
			return e, true
		}
	}
	return Entity{}, false
}

func partyNames(c Canon) []string {
	var out []string
	for _, e := range c.Entities {
		if e.Type == TypePC {
			out = append(out, e.CanonicalName())
		}
	}
	return out
}

func knownEntities(c Canon) []KnownEntity {
	out := make([]KnownEntity, 0, len(c.Entities))
	for _, e := range c.Entities {
		names := nameTexts(e.Names)
		out = append(out, KnownEntity{Name: names[0], Type: e.Type, Aliases: names[1:]})
	}
	return out
}

func nameTexts(ns []Name) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Text
	}
	return out
}

func entityText(names []string, t EntityType, desc string) string {
	s := strings.Join(names, " / ") + " (" + string(t) + ")"
	if desc != "" {
		s += ": " + desc
	}
	return s
}

// callTool runs a forced-tool request and decodes the arguments into T. If the
// arguments don't parse, it asks the model once more with the error.
func callTool[T any](ctx context.Context, m llm.Model, req llm.Request) (T, error) {
	var zeroT T
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := m.Complete(ctx, req)
		if err != nil {
			return zeroT, err
		}
		var out T
		if err := json.Unmarshal(resp.ToolInput, &out); err == nil {
			return out, nil
		} else {
			lastErr = err
		}
		msgs := slices.Clone(req.Messages)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, Text: string(resp.ToolInput)},
			llm.Message{Role: llm.RoleUser, Text: fmt.Sprintf("Those tool arguments were invalid (%v). Call %s again with arguments matching the schema.", lastErr, req.Tool.Name)})
		req.Messages = msgs
	}
	return zeroT, fmt.Errorf("invalid %s output after retry: %w", req.Purpose, lastErr)
}

// meter wraps a Model and totals usage per purpose.
type meter struct {
	next  llm.Model
	mu    sync.Mutex
	usage map[string]llm.Usage
}

func (m *meter) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	resp, err := m.next.Complete(ctx, req)
	m.mu.Lock()
	m.usage[req.Purpose] = m.usage[req.Purpose].Add(resp.Usage)
	m.mu.Unlock()
	return resp, err
}

func (m *meter) snapshot() map[string]llm.Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]llm.Usage, len(m.usage))
	for k, v := range m.usage {
		out[k] = v
	}
	return out
}
