-- llm-router: RouterController.swift's dashboard and scope checks, and
-- ParallelExecutionStore.swift's reads, deletes and synthesis presets.

-- name: UserTier :one
SELECT tier, tier_override, privacy_no_cn_origin FROM users WHERE id = $1;

-- name: Dashboard :one
SELECT COUNT(*)::bigint AS requests,
       (COUNT(*) FILTER (WHERE status = 'ok'))::bigint AS successful,
       COALESCE(SUM(fallback_count), 0)::bigint AS fallback_count,
       COALESCE(SUM(tokens_in), 0)::bigint AS tokens_in,
       COALESCE(SUM(tokens_out), 0)::bigint AS tokens_out,
       COALESCE(SUM(estimated_cost_usd_micros), 0)::bigint AS cost,
       COALESCE(AVG(latency_ms), 0)::bigint AS latency
FROM router_executions
WHERE tenant_id = $1 AND occurred_at >= date_trunc('month', NOW());

-- name: SpaceExists :one
SELECT EXISTS (SELECT 1 FROM spaces WHERE tenant_id = $1 AND id = $2);

-- name: JobExists :one
SELECT EXISTS (SELECT 1 FROM skills_state WHERE tenant_id = $1 AND name = $2);

-- name: WorkflowExists :one
SELECT EXISTS (SELECT 1 FROM workflows WHERE tenant_id = $1 AND id = $2);

-- name: ListExecutions :many
SELECT id, COALESCE(parallel_strategy, 'consensus')::text AS parallel_strategy, status,
       COALESCE(prompt, '')::text AS prompt, participant_count,
       estimated_cost_usd_micros, latency_ms, occurred_at
FROM router_executions
WHERE tenant_id = $1 AND parallel_strategy IS NOT NULL
ORDER BY occurred_at DESC LIMIT 50;

-- name: GetExecution :one
SELECT id, COALESCE(parallel_strategy, 'consensus')::text AS parallel_strategy, status,
       COALESCE(prompt, '')::text AS prompt, participant_count,
       estimated_cost_usd_micros, latency_ms, occurred_at, synthesized_answer
FROM router_executions WHERE id = $1 AND tenant_id = $2;

-- name: ListOutputs :many
SELECT id, participant_id, role, provider, model, stage, round, content, status,
       tokens_in, tokens_out, estimated_cost_usd_micros, latency_ms
FROM router_outputs WHERE execution_id = $1
ORDER BY round, occurred_at;

-- name: DeleteExecution :execrows
DELETE FROM router_executions WHERE id = $1 AND tenant_id = $2;

-- name: ListPresets :many
SELECT id, name, prompt, created_at, updated_at
FROM router_synthesis_presets WHERE tenant_id = $1
ORDER BY updated_at DESC;

-- name: GetPreset :one
SELECT id, name, prompt, created_at, updated_at
FROM router_synthesis_presets WHERE tenant_id = $1 AND id = $2;

-- name: CreatePreset :one
INSERT INTO router_synthesis_presets (id, tenant_id, name, prompt)
VALUES ($1, $2, $3, $4)
RETURNING id, name, prompt, created_at, updated_at;

-- name: UpdatePreset :one
UPDATE router_synthesis_presets SET name = $3, prompt = $4, updated_at = NOW()
WHERE tenant_id = $1 AND id = $2
RETURNING id, name, prompt, created_at, updated_at;

-- name: DeletePreset :execrows
DELETE FROM router_synthesis_presets WHERE tenant_id = $1 AND id = $2;
