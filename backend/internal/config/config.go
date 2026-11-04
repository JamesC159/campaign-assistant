// Package config loads application settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Env is the deployment environment.
type Env string

const (
	EnvDev  Env = "dev"
	EnvProd Env = "prod"
)

// Provider selects real AWS clients or local mocks for every external dependency
// (Bedrock, Transcribe, S3, SES).
type Provider string

const (
	ProviderMock Provider = "mock"
	ProviderAWS  Provider = "aws"
)

// Config holds every runtime setting. Build it with Load.
type Config struct {
	Env             Env
	LogLevel        slog.Level
	HTTPAddr        string
	DatabaseURL     string
	Provider        Provider
	AWSRegion       string
	ShutdownTimeout time.Duration
}

// defaultDatabaseURL matches docker-compose.yml. Dev-only: prod must set DATABASE_URL (enforced in Load).
const defaultDatabaseURL = //nolint:gosec // local docker compose credentials, not a secret
"postgres://campaign:campaign@127.0.0.1:5433/campaign?sslmode=disable"

// Load reads configuration using getenv (pass os.Getenv in production code).
// Unset values fall back to local-dev defaults. Every invalid value is reported, not just the first.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, fallback string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return fallback
	}

	var errs []error
	cfg := Config{
		Env:             Env(get("APP_ENV", string(EnvDev))),
		HTTPAddr:        get("HTTP_ADDR", "127.0.0.1:8090"),
		DatabaseURL:     get("DATABASE_URL", defaultDatabaseURL),
		Provider:        Provider(get("PROVIDER", string(ProviderMock))),
		AWSRegion:       get("AWS_REGION", "us-east-1"),
		ShutdownTimeout: 15 * time.Second,
	}

	switch cfg.Env {
	case EnvDev, EnvProd:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be %q or %q, got %q", EnvDev, EnvProd, cfg.Env))
	}

	switch cfg.Provider {
	case ProviderMock, ProviderAWS:
	default:
		errs = append(errs, fmt.Errorf("PROVIDER must be %q or %q, got %q", ProviderMock, ProviderAWS, cfg.Provider))
	}

	if cfg.Env == EnvProd && cfg.Provider == ProviderMock {
		errs = append(errs, errors.New("PROVIDER=mock is not allowed when APP_ENV=prod"))
	}
	if cfg.Env == EnvProd && getenv("DATABASE_URL") == "" {
		errs = append(errs, errors.New("DATABASE_URL is required when APP_ENV=prod"))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	if d := getenv("SHUTDOWN_TIMEOUT"); d != "" {
		parsed, err := time.ParseDuration(d)
		if err != nil || parsed <= 0 {
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT must be a positive duration like 15s, got %q", d))
		} else {
			cfg.ShutdownTimeout = parsed
		}
	}

	return cfg, errors.Join(errs...)
}
