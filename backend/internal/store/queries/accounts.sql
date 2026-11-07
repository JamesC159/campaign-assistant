-- name: CreateUser :one
INSERT INTO users (email, password_hash) VALUES ($1, $2)
RETURNING id, email, created_at;

-- name: GetUserByEmail :one
SELECT id, email, password_hash, created_at FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT id, email, created_at FROM users WHERE id = $1;

-- name: CreateAuthSession :exec
INSERT INTO auth_sessions (user_id, token_hash, expires_at) VALUES ($1, $2, $3);

-- name: GetAuthSession :one
SELECT s.id, s.user_id, s.expires_at, s.last_seen_at, u.email
FROM auth_sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: TouchAuthSession :exec
UPDATE auth_sessions SET last_seen_at = now() WHERE id = $1;

-- name: DeleteAuthSession :exec
DELETE FROM auth_sessions WHERE token_hash = $1;

-- name: DeleteExpiredAuthSessions :execrows
DELETE FROM auth_sessions WHERE expires_at <= now();

-- name: CreateMagicLink :exec
INSERT INTO magic_links (user_id, token_hash, expires_at) VALUES ($1, $2, $3);

-- name: UseMagicLink :one
-- Single use: the UPDATE succeeds at most once per token.
UPDATE magic_links SET used_at = now()
WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
RETURNING user_id;
