// Command migrate applies or reports database schema migrations.
//
//	migrate up       apply all pending migrations
//	migrate status   list migrations and whether each is applied
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/db"
	"campaign-assistant/backend/internal/logging"
	"campaign-assistant/backend/internal/migrate"
	"campaign-assistant/backend/migrations"
)

const usage = "usage: migrate <up|status>"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%s", usage)
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := logging.New(cfg.Env, cfg.LogLevel, os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ms, err := migrate.Load(migrations.FS)
	if err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL, "")
	if err != nil {
		return err
	}
	defer pool.Close()

	switch args[0] {
	case "up":
		applied, err := migrate.Up(ctx, pool, ms, log)
		if err != nil {
			return err
		}
		log.Info("migrations up to date", "applied", len(applied), "total", len(ms))
		return nil
	case "status":
		rows, err := migrate.List(ctx, pool, ms)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "VERSION\tNAME\tAPPLIED")
		for _, r := range rows {
			applied := "pending"
			if r.AppliedAt != nil {
				applied = r.AppliedAt.Local().Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(tw, "%04d\t%s\t%s\n", r.Version, r.Name, applied)
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown command %q; %s", args[0], usage)
	}
}
