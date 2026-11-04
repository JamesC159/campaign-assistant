package canon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// Campaign is a campaign on disk: campaign.json plus sessions/NN.md.
type Campaign struct {
	Name     string        `json:"name"`
	Party    []PartyMember `json:"party"`
	Sessions []Session     `json:"-"`
}

var sessionFileRe = regexp.MustCompile(`^(\d+)\.md$`)

// LoadCampaign reads dir/campaign.json and every dir/sessions/<number>.md.
func LoadCampaign(dir string) (Campaign, error) {
	var c Campaign
	b, err := os.ReadFile(filepath.Join(dir, "campaign.json")) //nolint:gosec // dir is a trusted CLI argument
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse campaign.json: %w", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil {
		return c, err
	}
	for _, e := range entries {
		m := sessionFileRe.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		notes, err := os.ReadFile(filepath.Join(dir, "sessions", e.Name())) //nolint:gosec // dir is a trusted CLI argument
		if err != nil {
			return c, err
		}
		c.Sessions = append(c.Sessions, Session{Number: n, Notes: string(notes)})
	}
	sort.Slice(c.Sessions, func(i, j int) bool { return c.Sessions[i].Number < c.Sessions[j].Number })
	for i := 1; i < len(c.Sessions); i++ {
		if c.Sessions[i].Number == c.Sessions[i-1].Number {
			return c, fmt.Errorf("duplicate session number %d", c.Sessions[i].Number)
		}
	}
	if len(c.Sessions) == 0 {
		return c, fmt.Errorf("no sessions/<number>.md files in %s", dir)
	}
	return c, nil
}

// SessionResult is one processed session: the canon it was checked against and
// what the pipeline proposed.
type SessionResult struct {
	Before   Canon     `json:"-"`
	Proposal *Proposal `json:"proposal"`
}

// RunCampaign processes every session in order, approving each proposal in
// full before the next session (the CLI / eval path). onSession, if not nil,
// is called after each session.
func (e *Engine) RunCampaign(ctx context.Context, c Campaign, onSession func(SessionResult)) (Canon, []SessionResult, error) {
	current := SeedParty(c.Party)
	var results []SessionResult
	for _, s := range c.Sessions {
		p, err := e.Process(ctx, current, s)
		if err != nil {
			return current, results, fmt.Errorf("session %d: %w", s.Number, err)
		}
		r := SessionResult{Before: current, Proposal: p}
		results = append(results, r)
		if onSession != nil {
			onSession(r)
		}
		current = Apply(current, p)
	}
	return current, results, nil
}
