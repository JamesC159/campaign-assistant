// Package store runs database work in the right security context.
//
// Campaign data is protected by row-level security keyed on app.user_id, so
// all campaign queries go through AsUser. System is for account and job tables
// that have no user yet; under it, every RLS-protected table looks empty.
package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"campaign-assistant/backend/internal/store/dbq"
)

// Store wraps the pool.
type Store struct {
	Pool *pgxpool.Pool
}

// New returns a Store.
func New(pool *pgxpool.Pool) *Store { return &Store{Pool: pool} }

// AsUser runs fn in a transaction as userID: RLS policies see app.user_id and
// filter every campaign table accordingly. fn's error rolls the transaction back.
func (s *Store) AsUser(ctx context.Context, userID uuid.UUID, fn func(q *dbq.Queries) error) error {
	if userID == uuid.Nil {
		return fmt.Errorf("store: AsUser needs a user")
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// is_local = true: the setting ends with the transaction and never leaks to the next pool user.
		if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", userID.String()); err != nil {
			return err
		}
		return fn(dbq.New(tx))
	})
}

// System runs fn in a transaction with no user context.
func (s *Store) System(ctx context.Context, fn func(q *dbq.Queries) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return fn(dbq.New(tx))
	})
}

// Q runs single statements with no user context and no transaction.
func (s *Store) Q() *dbq.Queries { return dbq.New(s.Pool) }

// Notify publishes a Postgres notification inside the current transaction,
// so listeners only hear about committed changes.
func Notify(ctx context.Context, q *dbq.Queries, channel, payload string) error {
	return q.Notify(ctx, dbq.NotifyParams{Channel: channel, Payload: payload})
}

// Listen holds one connection LISTENing on channel and calls fn for each
// notification until ctx ends or the connection fails.
func Listen(ctx context.Context, pool *pgxpool.Pool, channel string, fn func(payload string)) error {
	c, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := c.Hijack() // take it out of the pool: a LISTENing connection must not be reused
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return err
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		fn(n.Payload)
	}
}
