// Package jobs is a small durable job queue on Postgres.
//
// Jobs are rows; workers claim them with FOR UPDATE SKIP LOCKED, so any number
// of workers can run without double-processing. Enqueue happens inside the
// caller's transaction, so a job exists if and only if the work that created
// it committed. A claimed job holds a lease; if its worker dies, the lease
// expires and another worker retries it.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/store/dbq"
)

// Channel is the Postgres NOTIFY channel that wakes idle workers.
const Channel = "jobs"

// Job is a claimed unit of work.
type Job struct {
	ID          int64
	Kind        string
	Payload     json.RawMessage
	Attempt     int // 1-based
	MaxAttempts int
}

// Handler does the work. Returning an error retries the job with backoff,
// unless it is Permanent or the job is out of attempts.
type Handler func(ctx context.Context, job Job) error

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent marks an error as not worth retrying (bad input, deleted data).
func Permanent(err error) error { return permanentError{err} }

// IsPermanent reports whether err was marked Permanent.
func IsPermanent(err error) bool { return errors.As(err, &permanentError{}) }

// Enqueue adds a job inside the caller's transaction and wakes workers on commit.
func Enqueue(ctx context.Context, q *dbq.Queries, kind string, payload any, maxAttempts int) (int64, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	id, err := q.EnqueueJob(ctx, dbq.EnqueueJobParams{Kind: kind, Payload: b, MaxAttempts: int32(maxAttempts)}) //nolint:gosec // small
	if err != nil {
		return 0, fmt.Errorf("enqueue %s: %w", kind, err)
	}
	return id, store.Notify(ctx, q, Channel, kind)
}

// Worker claims and runs jobs.
type Worker struct {
	Store       *store.Store
	Pool        *pgxpool.Pool // for LISTEN; usually Store.Pool
	Handlers    map[string]Handler
	Log         *slog.Logger
	Concurrency int           // default 2
	Lease       time.Duration // how long a claimed job is reserved; also the handler timeout. Default 10m
	Poll        time.Duration // fallback poll when no notification arrives. Default 5s
	// OnFailed runs after a job fails for good (permanent error, out of
	// attempts, or abandoned by a dead worker). Use it to surface the failure.
	OnFailed func(ctx context.Context, job Job, err error)

	id   string
	wake chan struct{}
}

// Run processes jobs until ctx is cancelled, then waits for in-flight jobs.
func (w *Worker) Run(ctx context.Context) error {
	w.setDefaults()
	w.Log.Info("worker started", "id", w.id, "concurrency", w.Concurrency, "kinds", len(w.Handlers))

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.listen(ctx) }()
	go func() { defer wg.Done(); w.reap(ctx) }()
	for range w.Concurrency {
		wg.Add(1)
		go func() { defer wg.Done(); w.loop(ctx) }()
	}
	wg.Wait()
	w.Log.Info("worker stopped", "id", w.id)
	return nil
}

func (w *Worker) setDefaults() {
	if w.Concurrency <= 0 {
		w.Concurrency = 2
	}
	if w.Lease <= 0 {
		w.Lease = 10 * time.Minute
	}
	if w.Poll <= 0 {
		w.Poll = 5 * time.Second
	}
	if w.Pool == nil {
		w.Pool = w.Store.Pool
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	host, _ := os.Hostname()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	w.id = fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))
	w.wake = make(chan struct{}, w.Concurrency)
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		ran, err := w.RunOne(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("claim failed", "err", err)
		}
		if ran {
			continue // there may be more work; check again immediately
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-time.After(w.Poll):
		}
	}
}

// RunOne claims and runs a single job, reporting whether one was available.
// Exported so tests can drive the queue step by step.
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	if w.id == "" {
		w.setDefaults()
	}
	row, err := w.Store.Q().ClaimJob(ctx, dbq.ClaimJobParams{Worker: w.id, LeaseSeconds: w.Lease.Seconds()})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	job := Job{ID: row.ID, Kind: row.Kind, Payload: row.Payload, Attempt: int(row.Attempts), MaxAttempts: int(row.MaxAttempts)}
	log := w.Log.With("job", job.ID, "kind", job.Kind, "attempt", job.Attempt)

	// Jobs finish even during shutdown (bounded by the lease), so a deploy doesn't waste work.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.Lease-5*time.Second)
	defer cancel()

	start := time.Now()
	herr := w.run(runCtx, job)
	bookCtx := context.WithoutCancel(ctx)
	switch {
	case herr == nil:
		log.Info("job done", "duration", time.Since(start).Round(time.Millisecond))
		return true, w.Store.Q().CompleteJob(bookCtx, dbq.CompleteJobParams{ID: job.ID, Worker: w.id})
	case IsPermanent(herr) || job.Attempt >= job.MaxAttempts:
		log.Error("job failed", "err", herr)
		if err := w.Store.Q().FailJob(bookCtx, dbq.FailJobParams{ID: job.ID, Worker: w.id, LastError: ptr(herr.Error())}); err != nil {
			return true, err
		}
		if w.OnFailed != nil {
			w.OnFailed(bookCtx, job, herr)
		}
		return true, nil
	default:
		delay := Backoff(job.Attempt)
		log.Warn("job will retry", "err", herr, "in", delay.Round(time.Second))
		return true, w.Store.Q().RetryJob(bookCtx, dbq.RetryJobParams{ID: job.ID, Worker: w.id, LastError: ptr(herr.Error()), DelaySeconds: delay.Seconds()})
	}
}

func (w *Worker) run(ctx context.Context, job Job) (err error) {
	h, ok := w.Handlers[job.Kind]
	if !ok {
		return Permanent(fmt.Errorf("no handler for job kind %q", job.Kind))
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()
	return h(ctx, job)
}

// listen wakes workers on NOTIFY so new jobs start immediately instead of on the next poll.
func (w *Worker) listen(ctx context.Context) {
	for ctx.Err() == nil {
		err := store.Listen(ctx, w.Pool, Channel, func(string) {
			select {
			case w.wake <- struct{}{}:
			default: // all workers already awake
			}
		})
		if ctx.Err() == nil {
			w.Log.Warn("job listener disconnected; polling until it reconnects", "err", err)
			sleep(ctx, 2*time.Second)
		}
	}
}

// reap fails jobs whose worker died on their final attempt.
func (w *Worker) reap(ctx context.Context) {
	for {
		rows, err := w.Store.Q().FailAbandonedJobs(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("reap abandoned jobs", "err", err)
		}
		for _, r := range rows {
			w.Log.Error("job abandoned by a dead worker on its final attempt", "job", r.ID, "kind", r.Kind)
			if w.OnFailed != nil {
				w.OnFailed(ctx, Job{ID: r.ID, Kind: r.Kind, Payload: r.Payload}, errors.New("worker stopped responding"))
			}
		}
		if !sleep(ctx, time.Minute) {
			return
		}
	}
}

// Backoff is the delay before retry attempt+1: exponential from 10s, capped at 15m, with jitter.
func Backoff(attempt int) time.Duration {
	base := 10 * time.Second * time.Duration(math.Pow(2, float64(attempt-1)))
	if base > 15*time.Minute || base <= 0 {
		base = 15 * time.Minute
	}
	jitter, _ := rand.Int(rand.Reader, big.NewInt(int64(base/4)+1))
	return base + time.Duration(jitter.Int64())
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func ptr[T any](v T) *T { return &v }
