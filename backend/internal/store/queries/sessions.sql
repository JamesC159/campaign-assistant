-- name: NextSessionNumber :one
SELECT (COALESCE(MAX(number), 0) + 1)::int AS next FROM game_sessions WHERE campaign_id = $1;

-- name: CreateGameSession :one
INSERT INTO game_sessions (campaign_id, number, played_on, notes, created_by)
VALUES ($1, $2, $3, $4, app_user_id())
RETURNING id, campaign_id, number, played_on, status, error, created_at, updated_at;

-- name: GetGameSession :one
SELECT id, campaign_id, number, played_on, notes, status, error, created_by, created_at, updated_at
FROM game_sessions WHERE id = $1;

-- name: ListGameSessions :many
SELECT id, campaign_id, number, played_on, status, error, created_at, updated_at
FROM game_sessions WHERE campaign_id = $1 ORDER BY number;

-- name: SetGameSessionStatus :exec
UPDATE game_sessions SET status = $2, error = $3, updated_at = now() WHERE id = $1;

-- name: CountUnapprovedEarlierSessions :one
-- Sessions build on earlier canon, so they are approved in order.
SELECT count(*)::int FROM game_sessions
WHERE campaign_id = $1 AND number < $2 AND status <> 'approved';

-- name: UpsertProposal :exec
INSERT INTO proposals (session_id, campaign_id, body, canon_version) VALUES ($1, $2, $3, $4)
ON CONFLICT (session_id) DO UPDATE SET body = EXCLUDED.body, canon_version = EXCLUDED.canon_version, created_at = now();

-- name: GetProposal :one
SELECT body, canon_version FROM proposals WHERE session_id = $1;

-- name: CanonVersion :one
SELECT count(*)::int FROM game_sessions WHERE campaign_id = $1 AND status = 'approved';

-- name: ListStaleReviewSessions :many
-- Sessions awaiting review whose proposal predates the current canon.
SELECT s.id, s.created_by FROM game_sessions s JOIN proposals p ON p.session_id = s.id
WHERE s.campaign_id = $1 AND s.status = 'review' AND p.canon_version <> $2;

-- name: UpsertRecap :exec
INSERT INTO recaps (session_id, campaign_id, session_number, audience, body) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (session_id, audience) DO UPDATE SET body = EXCLUDED.body, published = false, created_at = now();

-- name: ListRecaps :many
SELECT audience, body, published FROM recaps WHERE session_id = $1 ORDER BY audience;

-- name: ListPartyRecaps :many
-- No join to game_sessions: players can't read it (it holds raw notes).
SELECT session_id, session_number, body FROM recaps
WHERE campaign_id = $1 AND audience = 'party' AND published
ORDER BY session_number;

-- name: PublishRecaps :exec
UPDATE recaps SET published = true WHERE session_id = $1;

-- name: InsertUsage :exec
INSERT INTO usage_events (campaign_id, user_id, session_id, purpose, calls, input_tokens, output_tokens)
VALUES ($1, app_user_id(), $2, $3, $4, $5, $6);
