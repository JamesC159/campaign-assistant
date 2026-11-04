// Command canon runs the canon pipeline over a campaign folder, approving every
// proposal, and writes the results.
//
//	canon -campaign testdata/eval-campaign -out ../data/canon-run
//
// The folder holds campaign.json and sessions/<number>.md. Output: canon.json
// (final GM canon), party-view.json, and per session proposal.json,
// recap-gm.md and recap-party.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/providers"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "canon:", err)
		os.Exit(1)
	}
}

func run() error {
	campaignDir := flag.String("campaign", "testdata/eval-campaign", "campaign folder (campaign.json + sessions/)")
	outDir := flag.String("out", "", "write results here (default: print a summary only)")
	record := flag.String("record", "", "record model responses as fixtures into this folder")
	replay := flag.String("replay", "", "replay model responses from this fixtures folder")
	flag.Parse()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := canon.LoadCampaign(*campaignDir)
	if err != nil {
		return err
	}
	ai, err := providers.New(ctx, cfg)
	if err != nil {
		return err
	}
	engine := &canon.Engine{Model: providers.WithFixtures(ai.Model, *record, *replay), Embedder: ai.Embedder}

	fmt.Printf("Campaign %q: %d sessions (provider %s)\n", c.Name, len(c.Sessions), cfg.Provider)
	final, results, err := engine.RunCampaign(ctx, c, func(r canon.SessionResult) {
		p := r.Proposal
		newCount := 0
		for _, e := range p.Entities {
			if e.New {
				newCount++
			}
		}
		fmt.Printf("  session %d: %d facts, %d new entities, %d contradictions, %d review items\n",
			p.Session, len(p.Facts), newCount, len(p.Contradictions), len(p.Reviews))
	})
	if err != nil {
		return err
	}
	party := final.PartyView()
	fmt.Printf("Canon: %d entities, %d facts (party view: %d entities, %d facts)\n",
		len(final.Entities), len(final.Facts), len(party.Entities), len(party.Facts))

	if *outDir == "" {
		return nil
	}
	if err := writeJSON(filepath.Join(*outDir, "canon.json"), final); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outDir, "party-view.json"), party); err != nil {
		return err
	}
	for _, r := range results {
		dir := filepath.Join(*outDir, fmt.Sprintf("session-%02d", r.Proposal.Session))
		if err := writeJSON(filepath.Join(dir, "proposal.json"), r.Proposal); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, "recap-gm.md"), r.Proposal.RecapGM); err != nil {
			return err
		}
		if err := writeFile(filepath.Join(dir, "recap-party.md"), r.Proposal.RecapParty); err != nil {
			return err
		}
	}
	fmt.Println("Wrote", *outDir)
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, string(b))
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // local output folder
		return err
	}
	return os.WriteFile(path, []byte(content+"\n"), 0o644) //nolint:gosec // local output, not secret
}
