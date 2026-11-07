// Command api serves the HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"campaign-assistant/backend/internal/app"
	"campaign-assistant/backend/internal/auth"
	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/httpapi"
	"campaign-assistant/backend/internal/logging"
	"campaign-assistant/backend/internal/notify"
	"campaign-assistant/backend/internal/providers"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log := logging.New(cfg.Env, cfg.LogLevel, os.Stderr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := app.OpenDB(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer pool.Close()

	sender, mailbox, err := providers.Mail(ctx, cfg, log)
	if err != nil {
		return err
	}
	st := store.New(pool)
	broker := &notify.Broker{Pool: pool, Log: log}
	go broker.Run(ctx)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewHandler(httpapi.Deps{
			Log:      log,
			DB:       pool,
			Env:      cfg.Env,
			Provider: cfg.Provider,
			Version:  version.Version,
			Store:    st,
			Auth:     &auth.Service{Store: st, Mail: sender, Log: log, Params: auth.DefaultParams, AppURL: cfg.AppURL},
			Events:   broker,
			Mailbox:  mailbox,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Server-wide; the SSE endpoint clears its own deadline.
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.HTTPAddr, "env", cfg.Env, "provider", cfg.Provider, "version", version.Version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down", "timeout", cfg.ShutdownTimeout)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	log.Info("stopped")
	return nil
}
