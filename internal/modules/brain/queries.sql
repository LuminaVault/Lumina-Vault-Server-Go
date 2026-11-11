-- Vault scope for GET /v1/brain/graph. These are only candidates: every id
-- still goes through vaultaccess.ResolveVault before it is read.

-- name: CandidateVaults :many
SELECT v.id, v.name
FROM vaults v
WHERE v.archived_at IS NULL
  AND (v.personal_owner_user_id = @user_id::uuid
       OR v.id IN (SELECT vm.vault_id FROM vault_memberships vm WHERE vm.user_id = @user_id::uuid))
ORDER BY v.name, v.id;

-- name: VaultName :one
SELECT name FROM vaults WHERE id = $1;

-- Item nodes. Every query below is restricted to vault ids that scopes() already
-- admitted through vaultaccess; none reads outside them.

-- name: Items :many
SELECT id, vault_id, space_id, kind, body, created_at, graph_x, graph_y, graph_z
FROM (
  SELECT vf.id, vf.tenant_id AS vault_id, vf.space_id, 'note'::text AS kind, vf.path AS body,
         vf.created_at, vf.updated_at,
         NULL::double precision AS graph_x, NULL::double precision AS graph_y, NULL::double precision AS graph_z
  FROM vault_files vf
  WHERE vf.tenant_id = ANY(@vault_ids::uuid[])
  UNION ALL
  SELECT m.id, m.tenant_id, m.space_id, 'memory'::text, left(m.content, 400),
         m.created_at, m.updated_at, m.graph_x, m.graph_y, m.graph_z
  FROM memories m
  WHERE m.tenant_id = ANY(@vault_ids::uuid[])
) items
ORDER BY coalesce(updated_at, created_at) DESC NULLS LAST, id
LIMIT @row_limit::int;

-- name: NoteLinks :many
SELECT l.source_vault_file_id AS from_id, l.target_vault_file_id AS to_id
FROM vault_links l
WHERE l.resolved
  AND l.tenant_id = ANY(@vault_ids::uuid[])
  AND l.source_vault_file_id = ANY(@note_ids::uuid[])
  AND l.target_vault_file_id = ANY(@note_ids::uuid[]);

-- name: MemorySources :many
SELECT m.id AS memory_id, m.source_vault_file_id AS note_id
FROM memories m
WHERE m.tenant_id = ANY(@vault_ids::uuid[])
  AND m.id = ANY(@memory_ids::uuid[])
  AND m.source_vault_file_id IS NOT NULL;
