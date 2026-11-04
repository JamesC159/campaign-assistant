// Command eval runs the canon pipeline over the eval campaign and scores it
// against the answer key. It exits non-zero when any threshold fails.
//
//	eval                                  # mock provider, mock thresholds
//	PROVIDER=aws eval                     # real Bedrock, production thresholds
//	eval -replay testdata/fixtures        # recorded Bedrock answers, offline
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/eval"
	"campaign-assistant/backend/internal/providers"
)

func main() {
	ok, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		os.Exit(2)
	}
	if !ok {
		os.Exit(1)
	}
}

func run() (bool, error) {
	campaignDir := flag.String("campaign", "testdata/eval-campaign", "campaign folder with answer-key.json")
	thresholdSet := flag.String("thresholds", "", "threshold set from answer-key.json (default: the provider name)")
	record := flag.String("record", "", "record model responses as fixtures into this folder")
	replay := flag.String("replay", "", "replay model responses from this fixtures folder")
	jsonOut := flag.String("json", "", "also write the full report as JSON to this file")
	flag.Parse()

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return false, fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c, err := canon.LoadCampaign(*campaignDir)
	if err != nil {
		return false, err
	}
	key, err := eval.LoadAnswerKey(*campaignDir)
	if err != nil {
		return false, err
	}
	set := *thresholdSet
	if set == "" {
		set = string(cfg.Provider)
	}
	thresholds, ok := key.Thresholds[set]
	if !ok {
		return false, fmt.Errorf("answer key has no %q thresholds", set)
	}

	ai, err := providers.New(ctx, cfg)
	if err != nil {
		return false, err
	}
	engine := &canon.Engine{Model: providers.WithFixtures(ai.Model, *record, *replay), Embedder: ai.Embedder}

	final, results, err := engine.RunCampaign(ctx, c, nil)
	if err != nil {
		return false, err
	}
	report := eval.Score(key, final, results)

	fmt.Printf("Eval: %s, provider %s, thresholds %q\n\n", c.Name, cfg.Provider, set)
	report.Print(os.Stdout, thresholds)

	if *jsonOut != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil { //nolint:gosec // report, not secret
			return false, err
		}
	}

	failures := report.Failures(thresholds)
	if len(failures) > 0 {
		fmt.Println("\nFAILED:")
		for _, f := range failures {
			fmt.Println("  -", f)
		}
		return false, nil
	}
	fmt.Println("\nPASSED")
	return true, nil
}
