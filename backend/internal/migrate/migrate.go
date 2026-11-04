// Package migrate applies forward-only SQL migrations to PostgreSQL.
//
// Guarantees:
//   - Only one migrator runs at a time (Postgres advisory lock), so parallel
//     deploys are safe.
//   - Each migration runs in its own transaction together with its bookkeeping row.
//   - An applied migration whose file has since changed is an error (checksum check).
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey identifies this app's migration advisory lock. The value is arbitrary
// but must never change, or old and new builds could migrate concurrently.
const lockKey int64 = 7_354_120_986_001

var fileRe = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migration is one SQL file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Status describes one migration's state in a database.
type Status struct {
	Migration
	AppliedAt *time.Time
}

// Load reads and validates migrations from fsys (top level only).
// Versions must be unique and contiguous starting at 1.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var out []Migration
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %q: name must look like 0001_description.sql", e.Name())
		}
		version, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{Version: version, Name: m[2], SQL: string(body), Checksum: hex.EncodeToString(sum[:])})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("migration versions must be contiguous from 0001: expected %04d, found %04d_%s", i+1, m.Version, m.Name)
		}
	}
	return out, nil
}

// Up applies every pending migration and returns the ones it applied.
func Up(ctx context.Context, pool *pgxpool.Pool, migrations []Migration, log *slog.Logger) ([]Migration, error) {
	var applied []Migration
	err := withLock(ctx, pool, func(conn *pgx.Conn) error {
		done, err := appliedChecksums(ctx, conn)
		if err != nil {
			return err
		}
		if err := verify(migrations, done); err != nil {
			return err
		}

		for _, m := range migrations {
			if _, ok := done[m.Version]; ok {
				continue
			}
			start := time.Now()
			if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, m.SQL); err != nil {
					return err
				}
				_, err := tx.Exec(ctx,
					`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
					m.Version, m.Name, m.Checksum)
				return err
			}); err != nil {
				return fmt.Errorf("apply %04d_%s: %w", m.Version, m.Name, err)
			}
			log.Info("applied migration", "version", m.Version, "name", m.Name, "duration", time.Since(start).Round(time.Millisecond))
			applied = append(applied, m)
		}
		return nil
	})
	return applied, err
}

// List reports every known migration and when (if ever) it was applied.
func List(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) ([]Status, error) {
	var out []Status
	err := withLock(ctx, pool, func(conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, `SELECT version, checksum, applied_at FROM schema_migrations`)
		if err != nil {
			return err
		}
		type row struct {
			checksum  string
			appliedAt time.Time
		}
		got := map[int]row{}
		for rows.Next() {
			var v int
			var r row
			if err := rows.Scan(&v, &r.checksum, &r.appliedAt); err != nil {
				return err
			}
			got[v] = r
		}
		if err := rows.Err(); err != nil {
			return err
		}

		sums := make(map[int]string, len(got))
		for v, r := range got {
			sums[v] = r.checksum
		}
		if err := verify(migrations, sums); err != nil {
			return err
		}
		for _, m := range migrations {
			s := Status{Migration: m}
			if r, ok := got[m.Version]; ok {
				t := r.appliedAt
				s.AppliedAt = &t
			}
			out = append(out, s)
		}
		return nil
	})
	return out, err
}

// verify fails if the database has migrations this build doesn't know about,
// or if an applied migration's file was edited.
func verify(migrations []Migration, applied map[int]string) error {
	known := make(map[int]Migration, len(migrations))
	for _, m := range migrations {
		known[m.Version] = m
	}
	var errs []error
	for v, sum := range applied {
		m, ok := known[v]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("database has migration %04d, which this build does not include (running an older build?)", v))
		case m.Checksum != sum:
			errs = append(errs, fmt.Errorf("migration %04d_%s was edited after being applied; add a new migration instead", v, m.Name))
		}
	}
	return errors.Join(errs...)
}

// withLock runs fn on a dedicated connection holding the migration advisory lock,
// after making sure the bookkeeping table exists.
func withLock(ctx context.Context, pool *pgxpool.Pool, fn func(*pgx.Conn) error) error {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer c.Release()
	conn := c.Conn()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Use a fresh context so the lock is released even if ctx was cancelled.
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer     PRIMARY KEY,
			name       text        NOT NULL,
			checksum   text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return fn(conn)
}

func appliedChecksums(ctx context.Context, conn *pgx.Conn) (map[int]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, err
		}
		out[v] = sum
	}
	return out, rows.Err()
}
