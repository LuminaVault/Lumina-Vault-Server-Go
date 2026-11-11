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
