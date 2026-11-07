-- name: CreateCampaign :one
INSERT INTO campaigns (owner_id, name) VALUES (app_user_id(), $1)
RETURNING id, owner_id, name, created_at;

-- name: AddMember :exec
INSERT INTO campaign_members (campaign_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT (campaign_id, user_id) DO UPDATE SET role = EXCLUDED.role;

-- name: RemoveMember :execrows
DELETE FROM campaign_members WHERE campaign_id = $1 AND user_id = $2;

-- name: ListMyCampaigns :many
SELECT c.id, c.name, c.owner_id, c.created_at, m.role
FROM campaigns c JOIN campaign_members m ON m.campaign_id = c.id AND m.user_id = app_user_id()
ORDER BY c.created_at DESC;

-- name: GetCampaign :one
SELECT c.id, c.name, c.owner_id, c.created_at, m.role
FROM campaigns c JOIN campaign_members m ON m.campaign_id = c.id AND m.user_id = app_user_id()
WHERE c.id = $1;

-- name: RenameCampaign :execrows
UPDATE campaigns SET name = $2 WHERE id = $1;

-- name: DeleteCampaign :execrows
DELETE FROM campaigns WHERE id = $1;

-- name: ListMembers :many
SELECT m.user_id, u.email, m.role, m.created_at
FROM campaign_members m JOIN users u ON u.id = m.user_id
WHERE m.campaign_id = $1
ORDER BY m.created_at;
