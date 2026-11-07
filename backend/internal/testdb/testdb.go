// Package testdb gives each test its own freshly migrated database.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"campaign-assistant/backend/internal/db"
	"campaign-assistant/backend/internal/migrate"
	"campaign-assistant/backend/migrations"
)

// AppRole is the role the API and worker run as.
const AppRole = "campaign_app"

// DB is a migrated, throwaway database.
type DB struct {
	URL   string        // connection URL for this database (login user)
	Admin *pgxpool.Pool // login user, no SET ROLE: bypasses RLS, for fixtures and assertions
	App   *pgxpool.Pool // SET ROLE campaign_app: RLS applies, like the real API
}

// New creates and migrates a database, dropped when the test ends. It skips
// the test when TEST_DATABASE_URL is unset (`make test` sets it).
func New(t testing.TB) *DB {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test (run `make test`)")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	d := &DB{URL: u.String()}

	if d.Admin, err = db.Open(ctx, d.URL, ""); err != nil {
		t.Fatal(err)
	}
	ms, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Up(ctx, d.Admin, ms, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if d.App, err = db.Open(ctx, d.URL, AppRole); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.App.Close(); d.Admin.Close() })
	return d
}
