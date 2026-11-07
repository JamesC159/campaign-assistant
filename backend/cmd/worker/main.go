// Command worker runs background jobs (session processing). Run as many
// copies as you like; the Postgres queue hands each job to exactly one.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"campaign-assistant/backend/internal/app"
	"campaign-assistant/backend/internal/canon"
	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/jobs"
	"campaign-assistant/backend/internal/logging"
	"campaign-assistant/backend/internal/processing"
	"campaign-assistant/backend/internal/providers"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := logging.New(cfg.Env, cfg.LogLevel, os.Stderr).With("component", "worker")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := app.OpenDB(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	ai, err := providers.New(ctx, cfg)
	if err != nil {
		return err
	}
	st := store.New(pool)
	proc := &processing.Processor{Store: st, Engine: &canon.Engine{Model: ai.Model, Embedder: ai.Embedder}, Log: log}

	log.Info("worker starting", "provider", cfg.Provider, "version", version.Version)
	w := &jobs.Worker{
		Store:       st,
		Log:         log,
		Concurrency: cfg.WorkerConcurrency,
		Handlers:    map[string]jobs.Handler{processing.Kind: proc.Handle},
		OnFailed:    proc.Failed,
	}
	return w.Run(ctx)
}
