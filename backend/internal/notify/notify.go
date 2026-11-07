// Package notify fans Postgres notifications about game sessions out to
// in-process subscribers (one SSE stream per browser tab). A single LISTEN
// connection serves every subscriber, so open tabs don't each hold a
// database connection.
package notify

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/store/dbq"
)

// Channel carries SessionEvent JSON.
const Channel = "session_events"

// SessionEvent says a session changed status.
type SessionEvent struct {
	CampaignID uuid.UUID `json:"campaign_id"`
	SessionID  uuid.UUID `json:"session_id"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
}

// Publish sends ev when the surrounding transaction commits.
func Publish(ctx context.Context, q *dbq.Queries, ev SessionEvent) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return store.Notify(ctx, q, Channel, string(b))
}

// Broker delivers session events to subscribers.
type Broker struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger

	mu   sync.Mutex
	next int
	subs map[int]subscriber
}

type subscriber struct {
	session uuid.UUID
	ch      chan SessionEvent
}

// Subscribe returns events for one session. Call cancel when done.
// Slow subscribers miss events rather than block others; each event carries
// the full current status, so a missed one is harmless.
func (b *Broker) Subscribe(session uuid.UUID) (<-chan SessionEvent, func()) {
	ch := make(chan SessionEvent, 8)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[int]subscriber{}
	}
	id := b.next
	b.next++
	b.subs[id] = subscriber{session: session, ch: ch}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

func (b *Broker) deliver(payload string) {
	var ev SessionEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		b.Log.Warn("ignoring malformed session event", "err", err)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		if s.session == ev.SessionID {
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// Run listens until ctx ends, reconnecting after connection failures.
func (b *Broker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := store.Listen(ctx, b.Pool, Channel, b.deliver)
		if ctx.Err() != nil {
			return
		}
		b.Log.Warn("session event listener disconnected; reconnecting", "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}
