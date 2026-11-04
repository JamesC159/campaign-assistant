package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Fixtures let tests and evals replay real model responses without AWS.
//
//	Recorder{Next: bedrock, Dir: "testdata/fixtures"}  // run once with real credentials
//	Replayer{Dir: "testdata/fixtures"}                 // replay offline, deterministically
//
// A fixture is keyed by everything that affects the model's answer (purpose,
// prompt version, tier, system prompt, messages, tool), so changing a prompt
// invalidates its fixtures instead of silently replaying stale answers.

// ErrFixtureMissing is returned by a Replayer with no Fallback when no fixture matches.
var ErrFixtureMissing = errors.New("llm: no fixture recorded for this request")

type fixture struct {
	Key           string   `json:"key"`
	Purpose       string   `json:"purpose"`
	PromptVersion int      `json:"prompt_version"`
	Response      Response `json:"response"`
}

// FixtureKey identifies a request's content.
func FixtureKey(req Request) string {
	keyed := struct {
		Purpose       string    `json:"p"`
		PromptVersion int       `json:"v"`
		Tier          Tier      `json:"t"`
		System        string    `json:"s"`
		Messages      []Message `json:"m"`
		Tool          *Tool     `json:"tool,omitempty"`
		MaxTokens     int       `json:"max"`
		Temperature   *float32  `json:"temp,omitempty"`
	}{req.Purpose, req.PromptVersion, req.Tier, req.System, req.Messages, req.Tool, req.MaxTokens, req.Temperature}
	b, _ := json.Marshal(keyed) // only plain data; cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func fixturePath(dir string, req Request, key string) string {
	purpose := req.Purpose
	if purpose == "" {
		purpose = "unnamed"
	}
	return filepath.Join(dir, purpose, key+".json")
}

// Recorder passes requests to Next and saves each successful response.
type Recorder struct {
	Next Model
	Dir  string
}

// Complete implements Model.
func (r Recorder) Complete(ctx context.Context, req Request) (Response, error) {
	resp, err := r.Next.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	key := FixtureKey(req)
	path := fixturePath(r.Dir, req, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // fixtures are not secret
		return resp, fmt.Errorf("record fixture: %w", err)
	}
	b, err := json.MarshalIndent(fixture{Key: key, Purpose: req.Purpose, PromptVersion: req.PromptVersion, Response: resp}, "", "  ")
	if err != nil {
		return resp, fmt.Errorf("record fixture: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil { //nolint:gosec // fixtures are not secret
		return resp, fmt.Errorf("record fixture: %w", err)
	}
	return resp, nil
}

// Replayer answers from recorded fixtures. On a miss it uses Fallback, or
// returns ErrFixtureMissing when Fallback is nil.
type Replayer struct {
	Dir      string
	Fallback Model
}

// Complete implements Model.
func (r Replayer) Complete(ctx context.Context, req Request) (Response, error) {
	key := FixtureKey(req)
	b, err := os.ReadFile(fixturePath(r.Dir, req, key))
	switch {
	case errors.Is(err, os.ErrNotExist):
		if r.Fallback != nil {
			return r.Fallback.Complete(ctx, req)
		}
		return Response{}, fmt.Errorf("%w (purpose %q, key %s)", ErrFixtureMissing, req.Purpose, key)
	case err != nil:
		return Response{}, fmt.Errorf("read fixture: %w", err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		return Response{}, fmt.Errorf("parse fixture %s: %w", key, err)
	}
	return f.Response, nil
}
