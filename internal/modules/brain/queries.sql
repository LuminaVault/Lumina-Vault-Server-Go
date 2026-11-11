-- Queries for GET /v1/brain/graph.
--
-- Scope. CandidateVaults only proposes vaults: every id still goes through
-- vaultaccess.ResolveVault, and only the ids it admits become @vault_ids.
--
-- Self-bounding (each returned row's vault is checked against @vault_ids in SQL):
-- Items, MemorySources (the memory and its source note), Spaces, ModelAuthors
-- (through the memory), Entities (the knowledge node and its evidence) and
-- SimilarMemories (both memories).
--
-- Bounded by their callers:
--   - VaultName takes no vault ids; it is called only with a vault id that
--     ResolveVault has just admitted.
--   - Authors takes no vault ids; it is called only with created_by_user_id values
--     of rows Items returned, and returns only id and username.
--   - NoteLinks checks the link row's vault; the note ids at both ends are bounded
--     by @note_ids, which the caller fills only from rows Items returned.

-- name: CandidateVaults :many
SELECT v.id, v.name
FROM vaults v
WHERE v.archived_at IS NULL
  AND (v.personal_owner_user_id = @user_id::uuid
       OR v.id IN (SELECT vm.vault_id FROM vault_memberships vm WHERE vm.user_id = @user_id::uuid))
ORDER BY v.name, v.id;

-- name: VaultName :one
SELECT name FROM vaults WHERE id = $1;

-- Item nodes: text files as notes, and memories that were not rejected.

-- name: Items :many
SELECT id, vault_id, space_id, kind, body, created_at, graph_x, graph_y, graph_z, tags, created_by_user_id
FROM (
  SELECT vf.id, vf.tenant_id AS vault_id, vf.space_id, 'note'::text AS kind, vf.path AS body,
         vf.created_at, vf.updated_at,
         NULL::double precision AS graph_x, NULL::double precision AS graph_y, NULL::double precision AS graph_z,
         NULL::text[] AS tags, vf.created_by_user_id
  FROM vault_files vf
  WHERE vf.tenant_id = ANY(@vault_ids::uuid[])
    AND vf.content_type LIKE 'text/%'
  UNION ALL
  SELECT m.id, m.tenant_id, m.space_id, 'memory'::text, left(m.content, 400),
         m.created_at, m.updated_at, m.graph_x, m.graph_y, m.graph_z, m.tags, m.created_by_user_id
  FROM memories m
  WHERE m.tenant_id = ANY(@vault_ids::uuid[])
    AND m.review_state <> 'rejected'
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

-- The source note is joined so that its vault is checked too: a memory may name a note in a
-- vault the caller cannot read, and that id must not be returned.
-- name: MemorySources :many
SELECT m.id AS memory_id, vf.id AS note_id
FROM memories m
JOIN vault_files vf ON vf.id = m.source_vault_file_id AND vf.tenant_id = ANY(@vault_ids::uuid[])
WHERE m.tenant_id = ANY(@vault_ids::uuid[])
  AND m.id = ANY(@memory_ids::uuid[]);

-- Hub attributes. Each query is bound to the admitted vault ids and to the selected item ids.

-- name: Spaces :many
SELECT s.id, s.tenant_id, s.name
FROM spaces s
WHERE s.tenant_id = ANY(@vault_ids::uuid[])
  AND s.id = ANY(@space_ids::uuid[]);

-- Only id and username leave the users table.
-- name: Authors :many
SELECT u.id, u.username
FROM users u
WHERE u.id = ANY(@user_ids::uuid[]);

-- Scoped through the memory: memory_contributions.tenant_id is the contributing user, not a vault.
-- name: ModelAuthors :many
SELECT DISTINCT mc.memory_id, mc.provider, mc.model::text AS model
FROM memory_contributions mc
JOIN memories m ON m.id = mc.memory_id
WHERE m.tenant_id = ANY(@vault_ids::uuid[])
  AND mc.memory_id = ANY(@memory_ids::uuid[])
  AND mc.actor_kind = 'model'
  AND mc.model IS NOT NULL;

-- name: Entities :many
SELECT kn.id, kn.tenant_id, kn.canonical_key, kn.label, kn.confidence, ke.memory_id
FROM knowledge_nodes kn
JOIN knowledge_evidence ke ON ke.node_id = kn.id
WHERE kn.kind = 'entity'
  AND kn.confidence >= 0.5
  AND kn.tenant_id = ANY(@vault_ids::uuid[])
  AND ke.tenant_id = ANY(@vault_ids::uuid[])
  AND ke.memory_id = ANY(@memory_ids::uuid[]);

-- Top-k nearest neighbours per selected memory. Both sides are bound to the admitted vault ids and
-- to the selected memory ids, and a memory is never its own neighbour. Never reads the all-pairs
-- self-join in the memory module.
-- name: SimilarMemories :many
SELECT m.id AS from_id, n.id AS to_id, n.similarity::float8 AS similarity
FROM memories m
CROSS JOIN LATERAL (
  SELECT c.id, 1 - (c.embedding <=> m.embedding) AS similarity
  FROM memories c
  WHERE c.tenant_id = ANY(@vault_ids::uuid[])
    AND c.id = ANY(@memory_ids::uuid[])
    AND c.id <> m.id
    AND c.embedding IS NOT NULL
  ORDER BY c.embedding <=> m.embedding
  LIMIT @per_node::int
) n
WHERE m.tenant_id = ANY(@vault_ids::uuid[])
  AND m.id = ANY(@memory_ids::uuid[])
  AND m.embedding IS NOT NULL
  AND n.similarity >= @min_similarity::float8;
