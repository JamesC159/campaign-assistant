package llm

import (
	"context"
	"errors"
	"testing"
)

type countingModel struct{ calls int }

func (c *countingModel) Complete(_ context.Context, req Request) (Response, error) {
	c.calls++
	return Response{Text: "answer to " + req.Messages[0].Text, Usage: Usage{Calls: 1}}, nil
}

func TestRecordThenReplay(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	req := Request{Purpose: "recap", PromptVersion: 1, Messages: []Message{{Role: RoleUser, Text: "q1"}}}

	real := &countingModel{}
	if _, err := (Recorder{Next: real, Dir: dir}).Complete(ctx, req); err != nil {
		t.Fatal(err)
	}

	replay := Replayer{Dir: dir}
	resp, err := replay.Complete(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "answer to q1" || real.calls != 1 {
		t.Fatalf("replay should return the recorded answer without calling the model: %q, calls=%d", resp.Text, real.calls)
	}

	// Any change to the prompt must miss, not replay a stale answer.
	changed := req
	changed.PromptVersion = 2
	if _, err := replay.Complete(ctx, changed); !errors.Is(err, ErrFixtureMissing) {
		t.Fatalf("want ErrFixtureMissing after prompt change, got %v", err)
	}

	// With a fallback, misses go to it.
	fallback := &countingModel{}
	if _, err := (Replayer{Dir: dir, Fallback: fallback}).Complete(ctx, changed); err != nil || fallback.calls != 1 {
		t.Fatalf("fallback not used: calls=%d err=%v", fallback.calls, err)
	}
}

func TestFixtureKeyIgnoresStructuredInput(t *testing.T) {
	a := Request{Purpose: "x", Messages: []Message{{Role: RoleUser, Text: "same"}}, Input: map[string]int{"a": 1}}
	b := a
	b.Input = "different structured input"
	if FixtureKey(a) != FixtureKey(b) {
		t.Fatal("Input is never sent to the provider, so it must not affect the key")
	}
}
