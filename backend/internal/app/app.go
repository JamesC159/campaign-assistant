// Package app holds startup code shared by the api and worker binaries.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"campaign-assistant/backend/internal/config"
	"campaign-assistant/backend/internal/db"
)

// OpenDB opens the pool as the restricted app role and refuses unsafe setups:
// an effective role that bypasses RLS (always), or a superuser login in
// production (it could RESET ROLE). An unreachable database is only a warning,
// so the process can start and report it through health checks.
func OpenDB(ctx context.Context, cfg config.Config, log *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBAppRole)
	if err != nil {
		return nil, err
	}
	info, err := db.CheckRole(ctx, pool)
	if err != nil {
		log.Warn("database not reachable at startup; role checks deferred", "err", err)
		return pool, nil
	}
	if info.BypassesRLS {
		pool.Close()
		return nil, fmt.Errorf("database role %q bypasses row-level security; set DB_APP_ROLE (default campaign_app) or use a non-superuser login", info.CurrentUser)
	}
	if info.LoginIsSuperuser {
		if cfg.Env == config.EnvProd {
			pool.Close()
			return nil, errors.New("DATABASE_URL logs in as a superuser; in production use a non-superuser login that is a member of " + cfg.DBAppRole)
		}
		log.Debug("dev: superuser login, running as restricted role", "login", info.SessionUser, "role", info.CurrentUser)
	}
	return pool, nil
}
