-- MemoryCompileController.pending: vault rows the compile has not reached.

-- name: CountPendingFiles :one
SELECT count(*) FROM vault_files WHERE tenant_id = $1 AND processed_at IS NULL;
