package canon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"campaign-assistant/backend/internal/embed"
	"campaign-assistant/backend/internal/llm"
)

// scripted answers extraction calls with fixed outputs (in order) and defers
// everything else to the heuristic model. It records party recap inputs.
type scripted struct {
	mu          sync.Mutex
	extracts    []ExtractOutput
	partyRecaps []string // returned in order for recap_party calls; heuristic when exhausted
	recapInputs []RecapInput
}

func (s *scripted) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch in := req.Input.(type) {
	case ExtractInput:
		if len(s.extracts) == 0 {
			return llm.Response{}, fmt.Errorf("unexpected extract call")
		}
		out := s.extracts[0]
		s.extracts = s.extracts[1:]
		b, _ := json.Marshal(out)
		return llm.Response{ToolInput: b, Usage: llm.Usage{Calls: 1}}, nil
	case RecapInput:
		if in.Audience == VisibilityParty {
			s.recapInputs = append(s.recapInputs, in)
			if len(s.partyRecaps) > 0 {
				text := s.partyRecaps[0]
				s.partyRecaps = s.partyRecaps[1:]
				return llm.Response{Text: text, Usage: llm.Usage{Calls: 1}}, nil
			}
		}
	}
	return HeuristicModel{}.Complete(ctx, req)
}

func newEngine(m llm.Model) *Engine { return &Engine{Model: m, Embedder: embed.Hash{}} }

var testParty = []PartyMember{{Name: "Iska Vale", Aliases: []string{"Iska"}}}

// A model that (against instructions) attaches a disguise alias to the real
// identity must not reveal the link to players.
func TestHiddenIdentityNeverReachesParty(t *testing.T) {
	ctx := context.Background()
	model := &scripted{extracts: []ExtractOutput{
		{ // session 1: party only sees a hooded stranger; GM text says who it is
			Entities: []ExtractedEntity{{Name: "Lady Maren Ashcombe", Type: TypeNPC, Aliases: []string{"Hooded Stranger"}}},
			Facts: []ExtractedFact{
				{Entity: "Lady Maren Ashcombe", Statement: "A hooded stranger watched the party.", Quote: "A hooded stranger watched the party from the cliffs.", Visibility: VisibilityParty},
				{Entity: "Lady Maren Ashcombe", Statement: "The stranger is Maren in disguise.", Quote: "The hooded stranger is Lady Maren Ashcombe in disguise.", Visibility: VisibilityGM},
			},
		},
		{ // session 2: party meets Lady Maren openly
			Entities: []ExtractedEntity{{Name: "Lady Maren Ashcombe", Type: TypeNPC}},
			Facts: []ExtractedFact{
				{Entity: "Lady Maren Ashcombe", Statement: "Lady Maren Ashcombe hired the party.", Quote: "Lady Maren Ashcombe hired the party.", Visibility: VisibilityParty},
			},
		},
	}}
	e := newEngine(model)
	final, results, err := e.RunCampaign(ctx, Campaign{Party: testParty, Sessions: []Session{
		{Number: 1, Notes: "A hooded stranger watched the party from the cliffs.\n\n## GM only\n\nThe hooded stranger is Lady Maren Ashcombe in disguise."},
		{Number: 2, Notes: "Lady Maren Ashcombe hired the party."},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, pe := range final.PartyView().Entities {
		var names []string
		for _, n := range pe.Names {
			names = append(names, n.Text)
		}
		joined := strings.ToLower(strings.Join(names, " | "))
		if strings.Contains(joined, "hooded") && strings.Contains(joined, "maren") {
			t.Fatalf("party view links the disguise to the identity: %s", joined)
		}
		for _, f := range final.PartyView().FactsFor(pe.ID) {
			text := strings.ToLower(f.Statement + " " + f.Quote)
			if strings.Contains(joined, "hooded") && strings.Contains(text, "maren") ||
				strings.Contains(joined, "maren") && strings.Contains(text, "hooded") {
				t.Fatalf("party fact on %q links the identities: %q", joined, text)
			}
		}
	}

	var flagged bool
	for _, r := range results[1].Proposal.Reviews {
		flagged = flagged || r.Kind == "possible_secret_identity"
	}
	if !flagged {
		t.Errorf("GM should be asked to confirm the identity link, got reviews %+v", results[1].Proposal.Reviews)
	}
}

func TestQuotesAreVerifiedAndGMTextForcesGMVisibility(t *testing.T) {
	model := &scripted{extracts: []ExtractOutput{{
		Entities: []ExtractedEntity{{Name: "Oswin Pell", Type: TypeNPC}},
		Facts: []ExtractedFact{
			{Entity: "Oswin Pell", Statement: "Oswin is the harbormaster.", Quote: "Oswin Pell is the harbormaster.", Visibility: VisibilityParty},
			{Entity: "Oswin Pell", Statement: "Oswin smuggles.", Quote: "Oswin Pell smuggles for the cult.", Visibility: VisibilityParty}, // model got visibility wrong
			{Entity: "Oswin Pell", Statement: "Oswin has a dog.", Quote: "Oswin owns a dog named Biscuit.", Visibility: VisibilityParty},  // invented quote
			{Entity: "Nobody Known", Statement: "x", Quote: "Oswin Pell is the harbormaster.", Visibility: VisibilityParty},               // unknown entity
		},
	}}}
	p, err := newEngine(model).Process(context.Background(), SeedParty(testParty), Session{Number: 1,
		Notes: "Oswin Pell is the harbormaster.\n\nGM: Oswin Pell smuggles for the cult."})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Facts) != 2 {
		t.Fatalf("want 2 facts kept, got %d: %+v", len(p.Facts), p.Facts)
	}
	if p.Facts[1].Visibility != VisibilityGM || p.Stats.ForcedGM != 1 {
		t.Errorf("fact quoted from GM text must be GM-only: %+v (stats %+v)", p.Facts[1], p.Stats)
	}
	if p.Stats.DroppedBadQuote != 1 || p.Stats.DroppedNoEntity != 1 {
		t.Errorf("stats = %+v", p.Stats)
	}
}

func TestSecretNamesAreNeverShownToPlayers(t *testing.T) {
	model := &scripted{
		extracts: []ExtractOutput{{
			Entities: []ExtractedEntity{{Name: "Order of the Pale Tide", Type: TypeFaction}, {Name: "Greywatch Lighthouse", Type: TypePlace}},
			Facts: []ExtractedFact{
				// Party-visible quote, but the model's paraphrase names the secret cult.
				{Entity: "Greywatch Lighthouse", Statement: "The Order of the Pale Tide lights fires at Greywatch Lighthouse.", Quote: "Strange lights burn at Greywatch Lighthouse.", Visibility: VisibilityParty},
				{Entity: "Order of the Pale Tide", Statement: "The Order hides in the caves.", Quote: "The Order of the Pale Tide hides in the caves.", Visibility: VisibilityGM},
			},
		}},
		// The recap model "leaks" twice, which must trigger the plain fallback.
		partyRecaps: []string{"The Order of the Pale Tide is up to something.", "Beware the Pale Tide."},
	}
	p, err := newEngine(model).Process(context.Background(), SeedParty(testParty), Session{Number: 1,
		Notes: "Strange lights burn at Greywatch Lighthouse.\n\nSecret: The Order of the Pale Tide hides in the caves."})
	if err != nil {
		t.Fatal(err)
	}

	if got := p.Facts[0].Statement; strings.Contains(got, "Pale Tide") || p.Stats.StatementsReplaced != 1 {
		t.Errorf("leaking statement should be replaced by its quote, got %q (stats %+v)", got, p.Stats)
	}
	if !p.RecapPartyFallback || strings.Contains(p.RecapParty, "Pale Tide") {
		t.Errorf("leaking recap must fall back to the plain list, got %q", p.RecapParty)
	}
	for _, in := range model.recapInputs {
		b, _ := json.Marshal(in)
		if strings.Contains(string(b), "Pale Tide") {
			t.Fatalf("party recap prompt must never contain secrets: %s", b)
		}
	}
}

func TestApplyPromotesNamesAndNeverDemotes(t *testing.T) {
	c := Canon{Entities: []Entity{{ID: "e1", Type: TypeNPC, Names: []Name{{Text: "Maren", Visibility: VisibilityGM}, {Text: "Lady M", Visibility: VisibilityParty}}}}}
	p := &Proposal{Session: 2, Entities: []ProposedEntity{
		{Ref: "e1", Names: []Name{{Text: "Maren", Visibility: VisibilityParty}, {Text: "Lady M", Visibility: VisibilityGM}, {Text: "The Patron", Visibility: VisibilityGM}}},
		{Ref: "new:1", New: true, Type: TypePlace, Names: []Name{{Text: "Saltmere", Visibility: VisibilityParty}}},
	}, Facts: []ProposedFact{{EntityRef: "new:1", Statement: "A town.", Quote: "A town.", Visibility: VisibilityParty}}}

	next := Apply(c, p)
	got := next.Entities[0].Names
	want := []Name{{"Maren", VisibilityParty}, {"Lady M", VisibilityParty}, {"The Patron", VisibilityGM}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if c.Entities[0].Names[0].Visibility != VisibilityGM {
		t.Error("Apply must not modify its input")
	}
	if len(next.Entities) != 2 || next.Facts[0].EntityID != next.Entities[1].ID || next.Entities[1].FirstSeen != 2 {
		t.Errorf("new entity/fact not applied correctly: %+v", next)
	}
}

func TestCallToolRetriesInvalidJSONOnce(t *testing.T) {
	calls := 0
	m := modelFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		calls++
		if calls == 1 {
			return llm.Response{ToolInput: json.RawMessage(`{"match_id": 42}`)}, nil
		}
		if last := req.Messages[len(req.Messages)-1].Text; !strings.Contains(last, "invalid") {
			t.Errorf("retry should explain the error, got %q", last)
		}
		return llm.Response{ToolInput: json.RawMessage(`{"match_id":"e1","confidence":"high","reason":"same"}`)}, nil
	})
	out, err := callTool[MatchOutput](context.Background(), m, matchRequest(MatchInput{}))
	if err != nil || out.MatchID != "e1" || calls != 2 {
		t.Fatalf("out=%+v err=%v calls=%d", out, err, calls)
	}
}

type modelFunc func(context.Context, llm.Request) (llm.Response, error)

func (f modelFunc) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	return f(ctx, req)
}

func TestPartyViewHidesGMData(t *testing.T) {
	c := Canon{
		Entities: []Entity{
			{ID: "e1", Names: []Name{{"Maren", VisibilityParty}, {"Cult Leader", VisibilityGM}}, Description: "secretly evil"},
			{ID: "e2", Names: []Name{{"Pale Tide", VisibilityGM}}},
		},
		Facts: []Fact{
			{ID: "f1", EntityID: "e1", Visibility: VisibilityParty, Statement: "ok"},
			{ID: "f2", EntityID: "e1", Visibility: VisibilityGM, Statement: "secret"},
			{ID: "f3", EntityID: "e2", Visibility: VisibilityParty, Statement: "about a hidden entity"},
		},
	}
	pv := c.PartyView()
	if len(pv.Entities) != 1 || len(pv.Entities[0].Names) != 1 || pv.Entities[0].Description != "" {
		t.Fatalf("party entities = %+v", pv.Entities)
	}
	if len(pv.Facts) != 1 || pv.Facts[0].ID != "f1" {
		t.Fatalf("party facts = %+v", pv.Facts)
	}
}
