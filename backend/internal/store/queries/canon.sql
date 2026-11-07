-- These queries run under row-level security: a GM gets the full canon, a
-- player gets only party-visible rows from the very same statements.

-- name: ListEntities :many
SELECT id, type, first_seen FROM entities WHERE campaign_id = $1 ORDER BY created_at, id;

-- name: ListEntityDescriptions :many
SELECT id, description FROM entity_descriptions WHERE campaign_id = $1;

-- name: ListEntityNames :many
SELECT entity_id, name, position, visibility FROM entity_names
WHERE campaign_id = $1 ORDER BY entity_id, position;

-- name: ListFacts :many
SELECT id, entity_id, session_number, statement, quote, visibility, prompt_version
FROM facts WHERE campaign_id = $1 AND state = 'canon'
ORDER BY session_number, created_at, id;

-- name: InsertEntity :one
INSERT INTO entities (campaign_id, type, description, first_seen) VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: InsertEntityName :exec
INSERT INTO entity_names (entity_id, campaign_id, name, position, visibility)
VALUES ($1, $2, $3, (SELECT COALESCE(MAX(position) + 1, 0) FROM entity_names WHERE entity_id = $1), $4)
ON CONFLICT (entity_id, lower(name)) DO NOTHING;

-- name: PromoteEntityName :exec
-- A name only ever goes from GM-only to party-visible, never back.
UPDATE entity_names SET visibility = 'party'
WHERE entity_id = sqlc.arg(entity_id) AND lower(name) = lower(sqlc.arg(name)::text) AND visibility = 'gm';

-- name: InsertFact :exec
INSERT INTO facts (campaign_id, entity_id, session_id, session_number, statement, quote, visibility, prompt_version)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);
