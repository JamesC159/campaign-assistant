package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoadSortsAndChecksums(t *testing.T) {
	fsys := fstest.MapFS{
		"0002_second.sql": {Data: []byte("SELECT 2;")},
		"0001_first.sql":  {Data: []byte("SELECT 1;")},
		"README.md":       {Data: []byte("ignored")},
	}
	ms, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Name != "first" || ms[1].Name != "second" {
		t.Fatalf("unexpected order: %+v", ms)
	}
	if ms[0].Checksum == "" || ms[0].Checksum == ms[1].Checksum {
		t.Fatalf("checksums not computed correctly")
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	tests := map[string]struct {
		fsys fstest.MapFS
		want string
	}{
		"bad name": {fstest.MapFS{"1_first.sql": {Data: []byte("x")}}, "name must look like"},
		"gap":      {fstest.MapFS{"0001_a.sql": {Data: []byte("x")}, "0003_c.sql": {Data: []byte("x")}}, "contiguous"},
		"dup":      {fstest.MapFS{"0001_a.sql": {Data: []byte("x")}, "0001_b.sql": {Data: []byte("x")}}, "contiguous"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(tc.fsys)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	ms := []Migration{{Version: 1, Name: "a", Checksum: "abc"}}
	if err := verify(ms, map[int]string{1: "abc"}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if err := verify(ms, map[int]string{1: "changed"}); err == nil || !strings.Contains(err.Error(), "edited") {
		t.Fatalf("expected edited error, got %v", err)
	}
	if err := verify(ms, map[int]string{1: "abc", 2: "zzz"}); err == nil || !strings.Contains(err.Error(), "does not include") {
		t.Fatalf("expected unknown-migration error, got %v", err)
	}
}

// tempDatabase creates an empty database for one test and drops it afterwards.
// It needs TEST_DATABASE_URL pointing at a server where that user may CREATE DATABASE;
// `make test` sets it to the docker compose database.
func tempDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database test (run `make test`)")
	}
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "migrate_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return pool
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestUpIsIdempotentAndConcurrencySafe(t *testing.T) {
	pool := tempDatabase(t)
	ctx := context.Background()
	ms, err := Load(fstest.MapFS{
		"0001_create.sql": {Data: []byte("CREATE TABLE widgets (id int PRIMARY KEY);")},
		"0002_seed.sql":   {Data: []byte("INSERT INTO widgets VALUES (1);")},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Several migrators at once must apply each migration exactly once.
	var wg sync.WaitGroup
	total := make([]int, 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			applied, err := Up(ctx, pool, ms, quietLog())
			total[i], errs[i] = len(applied), err
		}()
	}
	wg.Wait()

	sum := 0
	for i := range 4 {
		if errs[i] != nil {
			t.Fatalf("migrator %d: %v", i, errs[i])
		}
		sum += total[i]
	}
	if sum != 2 {
		t.Fatalf("expected 2 migrations applied in total, got %d", sum)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM widgets").Scan(&count); err != nil || count != 1 {
		t.Fatalf("seed applied %d times (err %v), want 1", count, err)
	}

	status, err := List(ctx, pool, ms)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range status {
		if s.AppliedAt == nil {
			t.Errorf("migration %d not marked applied", s.Version)
		}
	}
}

func TestUpRollsBackFailedMigration(t *testing.T) {
	pool := tempDatabase(t)
	ctx := context.Background()
	ms, err := Load(fstest.MapFS{
		"0001_ok.sql":  {Data: []byte("CREATE TABLE a (id int);")},
		"0002_bad.sql": {Data: []byte("CREATE TABLE b (id int); SELECT * FROM missing_table;")},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Up(ctx, pool, ms, quietLog()); err == nil || !strings.Contains(err.Error(), "0002_bad") {
		t.Fatalf("expected failure in 0002_bad, got %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('b') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("table b should have been rolled back (exists=%v, err=%v)", exists, err)
	}
	status, err := List(ctx, pool, ms)
	if err != nil {
		t.Fatal(err)
	}
	if status[0].AppliedAt == nil || status[1].AppliedAt != nil {
		t.Fatalf("want 0001 applied and 0002 pending, got %+v", status)
	}
}

func TestUpRejectsEditedMigration(t *testing.T) {
	pool := tempDatabase(t)
	ctx := context.Background()
	original, _ := Load(fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id int);")}})
	if _, err := Up(ctx, pool, original, quietLog()); err != nil {
		t.Fatal(err)
	}
	edited, _ := Load(fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id bigint);")}})
	if _, err := Up(ctx, pool, edited, quietLog()); err == nil || !strings.Contains(err.Error(), "edited") {
		t.Fatalf("expected edited-migration error, got %v", err)
	}
}
