package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(envFrom(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Env != EnvDev || cfg.Provider != ProviderMock {
		t.Errorf("got env=%q provider=%q, want dev/mock", cfg.Env, cfg.Provider)
	}
	if cfg.HTTPAddr != "127.0.0.1:8090" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(envFrom(map[string]string{
		"APP_ENV":              "prod",
		"PROVIDER":             "aws",
		"BEDROCK_MODEL_STRONG": "strong-id",
		"BEDROCK_MODEL_FAST":   "fast-id",
		"MAIL_FROM":            "gm@example.com",
		"APP_URL":              "https://app.example.com",
		"WORKER_CONCURRENCY":   "8",
		"DATABASE_URL":         "postgres://x",
		"LOG_LEVEL":            "debug",
		"SHUTDOWN_TIMEOUT":     "3s",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.WorkerConcurrency != 8 || cfg.MailFrom != "gm@example.com" || cfg.DBAppRole != "campaign_app" {
		t.Errorf("worker/mail/db settings not loaded: %+v", cfg)
	}
	if cfg.BedrockStrongModel != "strong-id" || cfg.BedrockFastModel != "fast-id" || cfg.BedrockEmbedModel == "" {
		t.Errorf("bedrock models not loaded: %+v", cfg)
	}
	if cfg.Env != EnvProd || cfg.Provider != ProviderAWS || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("overrides not applied: %+v", cfg)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	tests := map[string]struct {
		env  map[string]string
		want string
	}{
		"bad env":          {map[string]string{"APP_ENV": "staging"}, "APP_ENV"},
		"bad provider":     {map[string]string{"PROVIDER": "gcp"}, "PROVIDER"},
		"mock in prod":     {map[string]string{"APP_ENV": "prod", "DATABASE_URL": "postgres://x"}, "not allowed"},
		"prod needs db":    {map[string]string{"APP_ENV": "prod", "PROVIDER": "aws", "BEDROCK_MODEL_STRONG": "s", "BEDROCK_MODEL_FAST": "f", "MAIL_FROM": "a@b.c", "APP_URL": "https://x.test"}, "DATABASE_URL"},
		"prod needs https": {map[string]string{"APP_ENV": "prod", "APP_URL": "http://x.test"}, "https"},
		"bad app url":      {map[string]string{"APP_URL": "localhost:5173"}, "APP_URL"},
		"bad concurrency":  {map[string]string{"WORKER_CONCURRENCY": "0"}, "WORKER_CONCURRENCY"},
		"aws needs from":   {map[string]string{"PROVIDER": "aws", "BEDROCK_MODEL_STRONG": "s", "BEDROCK_MODEL_FAST": "f"}, "MAIL_FROM"},
		"aws needs models": {map[string]string{"PROVIDER": "aws"}, "BEDROCK_MODEL_STRONG is required"},
		"bad log level":    {map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		"bad shutdown":     {map[string]string{"SHUTDOWN_TIMEOUT": "soon"}, "SHUTDOWN_TIMEOUT"},
		"negative timeout": {map[string]string{"SHUTDOWN_TIMEOUT": "-1s"}, "SHUTDOWN_TIMEOUT"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(envFrom(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := Load(envFrom(map[string]string{"APP_ENV": "x", "PROVIDER": "y"}))
	if err == nil || !strings.Contains(err.Error(), "APP_ENV") || !strings.Contains(err.Error(), "PROVIDER") {
		t.Fatalf("expected both errors, got %v", err)
	}
}
