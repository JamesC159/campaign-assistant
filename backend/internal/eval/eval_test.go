package eval

import (
	"context"
	"testing"

	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/embed"
)

const campaignDir = "../../testdata/eval-campaign"

// The full eval campaign must pass the mock thresholds on every `go test`, so a
// pipeline change that breaks extraction, matching or secret handling fails CI.
func TestEvalCampaignPassesInMockMode(t *testing.T) {
	c, err := canon.LoadCampaign(campaignDir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := LoadAnswerKey(campaignDir)
	if err != nil {
		t.Fatal(err)
	}
	e := &canon.Engine{Model: canon.HeuristicModel{}, Embedder: embed.Hash{}}
	final, results, err := e.RunCampaign(context.Background(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	report := Score(key, final, results)
	if failures := report.Failures(key.Thresholds["mock"]); len(failures) > 0 {
		t.Fatalf("eval failed: %v\nreport: %+v", failures, report)
	}
	if len(report.SecretLeaks) != 0 {
		t.Fatalf("secret leaks: %v", report.SecretLeaks)
	}
}

// The scorer itself must catch a leak, or a passing eval means nothing.
func TestScoreDetectsLeaks(t *testing.T) {
	key := AnswerKey{Secrets: Secrets{Markers: []string{"Pale Tide"}}, Entities: []ExpectedEntity{{Name: "Order of the Pale Tide"}}}
	final := canon.Canon{
		Entities: []canon.Entity{{ID: "e1", Names: []canon.Name{{Text: "Order of the Pale Tide", Visibility: canon.VisibilityParty}}}},
	}
	results := []canon.SessionResult{{Proposal: &canon.Proposal{Session: 1, RecapParty: "Beware the Pale Tide."}}}

	leaks := Score(key, final, results).SecretLeaks
	if len(leaks) != 3 { // recap marker, entity-name marker, hidden entity visible
		t.Fatalf("want 3 leaks, got %d: %v", len(leaks), leaks)
	}
}
