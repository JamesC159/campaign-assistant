package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"campaign-assistant/backend/internal/jobs"
	"campaign-assistant/backend/internal/store"
	"campaign-assistant/backend/internal/store/dbq"
	"campaign-assistant/backend/internal/testdb"
)

func setup(t *testing.T, handlers map[string]jobs.Handler) (*store.Store, *jobs.Worker, *[]string) {
	d := testdb.New(t)
	s := store.New(d.App)
	var mu sync.Mutex
	var failed []string
	w := &jobs.Worker{Store: s, Handlers: handlers, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnFailed: func(_ context.Context, j jobs.Job, err error) {
			mu.Lock()
			failed = append(failed, j.Kind+": "+err.Error())
			mu.Unlock()
		}}
	return s, w, &failed
}

func enqueue(t *testing.T, s *store.Store, kind string, max int) int64 {
	t.Helper()
	var id int64
	err := s.System(context.Background(), func(q *dbq.Queries) (err error) {
		id, err = jobs.Enqueue(context.Background(), q, kind, map[string]string{"k": "v"}, max)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func state(t *testing.T, s *store.Store, id int64) dbq.GetJobRow {
	t.Helper()
	row, err := s.Q().GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestJobSucceeds(t *testing.T) {
	var got []byte
	s, w, _ := setup(t, map[string]jobs.Handler{"ok": func(_ context.Context, j jobs.Job) error { got = j.Payload; return nil }})
	id := enqueue(t, s, "ok", 3)

	ran, err := w.RunOne(context.Background())
	if !ran || err != nil {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if string(got) != `{"k": "v"}` && string(got) != `{"k":"v"}` {
		t.Errorf("payload = %s", got)
	}
	if row := state(t, s, id); row.DoneAt == nil || row.Attempts != 1 {
		t.Errorf("job not completed: %+v", row)
	}
	if ran, _ := w.RunOne(context.Background()); ran {
		t.Error("a completed job must not run again")
	}
}

func TestJobRetriesWithBackoffThenFails(t *testing.T) {
	s, w, failed := setup(t, map[string]jobs.Handler{"flaky": func(context.Context, jobs.Job) error { return errors.New("bedrock throttled") }})
	id := enqueue(t, s, "flaky", 2)

	if ran, _ := w.RunOne(context.Background()); !ran {
		t.Fatal("first attempt should run")
	}
	row := state(t, s, id)
	if row.DoneAt != nil || row.FailedAt != nil || row.LastError == nil || *row.LastError != "bedrock throttled" {
		t.Fatalf("after one failure the job should wait for retry: %+v", row)
	}
	if ran, _ := w.RunOne(context.Background()); ran {
		t.Fatal("retry must wait for its backoff delay")
	}

	// Skip the backoff and run the final attempt.
	if _, err := s.Pool.Exec(context.Background(), "UPDATE jobs SET run_at = now() WHERE id = $1", id); err != nil {
		t.Fatal(err)
	}
	if ran, _ := w.RunOne(context.Background()); !ran {
		t.Fatal("second attempt should run")
	}
	if row := state(t, s, id); row.FailedAt == nil || row.Attempts != 2 {
		t.Fatalf("job should have failed for good: %+v", row)
	}
	if len(*failed) != 1 {
		t.Errorf("OnFailed should run once, got %v", *failed)
	}
}

func TestPermanentErrorsAndPanicsAndUnknownKinds(t *testing.T) {
	s, w, failed := setup(t, map[string]jobs.Handler{
		"bad":   func(context.Context, jobs.Job) error { return jobs.Permanent(errors.New("session deleted")) },
		"panic": func(context.Context, jobs.Job) error { panic("boom") },
	})
	bad := enqueue(t, s, "bad", 5)
	panicky := enqueue(t, s, "panic", 1)
	unknown := enqueue(t, s, "nobody-handles-this", 5)
	for range 3 {
		if _, err := w.RunOne(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{bad, panicky, unknown} {
		if row := state(t, s, id); row.FailedAt == nil || row.Attempts != 1 {
			t.Errorf("job %d should fail after one attempt: %+v", id, row)
		}
	}
	if len(*failed) != 3 {
		t.Errorf("OnFailed calls = %v", *failed)
	}
}

func TestConcurrentWorkersNeverDoubleRun(t *testing.T) {
	var runs atomic.Int64
	seen := sync.Map{}
	handler := func(_ context.Context, j jobs.Job) error {
		if _, dup := seen.LoadOrStore(j.ID, true); dup {
			t.Errorf("job %d ran twice", j.ID)
		}
		runs.Add(1)
		time.Sleep(5 * time.Millisecond)
		return nil
	}
	s, _, _ := setup(t, map[string]jobs.Handler{"work": handler})
	for range 40 {
		enqueue(t, s, "work", 1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 3 { // three independent workers, as if three processes
		w := &jobs.Worker{Store: s, Handlers: map[string]jobs.Handler{"work": handler}, Concurrency: 4,
			Poll: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.Run(ctx) }()
	}
	deadline := time.Now().Add(20 * time.Second)
	for runs.Load() < 40 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if runs.Load() != 40 {
		t.Fatalf("ran %d of 40 jobs", runs.Load())
	}
}

func TestEnqueueRollsBackWithTransaction(t *testing.T) {
	s, w, _ := setup(t, map[string]jobs.Handler{"x": func(context.Context, jobs.Job) error { return nil }})
	boom := errors.New("caller failed")
	err := s.System(context.Background(), func(q *dbq.Queries) error {
		if _, err := jobs.Enqueue(context.Background(), q, "x", nil, 1); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if ran, _ := w.RunOne(context.Background()); ran {
		t.Fatal("a job enqueued in a rolled-back transaction must not exist")
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if b1, b3 := jobs.Backoff(1), jobs.Backoff(3); b1 < 10*time.Second || b3 < 40*time.Second {
		t.Errorf("backoff too small: %v %v", b1, b3)
	}
	if b := jobs.Backoff(50); b > 15*time.Minute+15*time.Minute/4 {
		t.Errorf("backoff not capped: %v", b)
	}
}
