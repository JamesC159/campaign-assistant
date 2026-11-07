// Package db opens the shared PostgreSQL connection pool.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates a connection pool. Connections are made lazily, so this succeeds
// even if the database is down; health checks report reachability.
//
// If role is set, every connection runs SET ROLE to it, so row-level security
// applies even when DATABASE_URL logs in as a superuser (local dev). In
// production DATABASE_URL should log in as a non-superuser member of role;
// see CheckRole.
func Open(ctx context.Context, url, role string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	if role != "" {
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
			return err
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// RoleInfo describes who the pool's connections run as.
type RoleInfo struct {
	SessionUser      string // the login role
	CurrentUser      string // the effective role after SET ROLE
	LoginIsSuperuser bool
	BypassesRLS      bool // the effective role ignores row-level security
}

// CheckRole reports the pool's roles. Callers refuse to start in production
// when the login is a superuser: it could RESET ROLE and bypass RLS.
func CheckRole(ctx context.Context, pool *pgxpool.Pool) (RoleInfo, error) {
	var r RoleInfo
	err := pool.QueryRow(ctx, `
		SELECT session_user, current_user,
		       (SELECT rolsuper FROM pg_roles WHERE rolname = session_user),
		       (SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user)`).
		Scan(&r.SessionUser, &r.CurrentUser, &r.LoginIsSuperuser, &r.BypassesRLS)
	return r, err
}
