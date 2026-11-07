-- name: EnqueueJob :one
INSERT INTO jobs (kind, payload, max_attempts) VALUES ($1, $2, $3) RETURNING id;

-- name: ClaimJob :one
-- SKIP LOCKED lets many workers claim concurrently without blocking or double-claiming.
UPDATE jobs SET
  attempts = attempts + 1,
  locked_by = sqlc.arg(worker)::text,
  locked_until = now() + make_interval(secs => sqlc.arg(lease_seconds)::float8)
WHERE id = (
  SELECT j.id FROM jobs j
  WHERE j.done_at IS NULL AND j.failed_at IS NULL AND j.run_at <= now()
    AND j.attempts < j.max_attempts
    AND (j.locked_until IS NULL OR j.locked_until < now())
  ORDER BY j.run_at, j.id
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
RETURNING id, kind, payload, attempts, max_attempts;

-- name: CompleteJob :exec
UPDATE jobs SET done_at = now(), locked_until = NULL, last_error = NULL
WHERE id = sqlc.arg(id) AND locked_by = sqlc.arg(worker)::text;

-- name: RetryJob :exec
UPDATE jobs SET
  run_at = now() + make_interval(secs => sqlc.arg(delay_seconds)::float8),
  locked_until = NULL, last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND locked_by = sqlc.arg(worker)::text;

-- name: FailJob :exec
UPDATE jobs SET failed_at = now(), locked_until = NULL, last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND locked_by = sqlc.arg(worker)::text;

-- name: FailAbandonedJobs :many
-- A worker died while holding a job on its last attempt: give up on it.
UPDATE jobs SET failed_at = now(), locked_until = NULL,
  last_error = COALESCE(last_error || '; ', '') || 'lease expired on final attempt'
WHERE done_at IS NULL AND failed_at IS NULL AND attempts >= max_attempts AND locked_until < now()
RETURNING id, kind, payload;

-- name: GetJob :one
SELECT id, kind, payload, attempts, max_attempts, done_at, failed_at, last_error FROM jobs WHERE id = $1;

-- name: Notify :exec
SELECT pg_notify(sqlc.arg(channel)::text, sqlc.arg(payload)::text);
