-- Dashboard (Swift: Sources/App/Dashboard/*). The multi-table aggregates live in
-- aggregates.go as raw SQL: sqlc's analyzer rejects their nested scopes.

-- name: CountMemories :one
SELECT COUNT(*) FROM memories WHERE tenant_id = $1;

-- name: CountMemoriesSince :one
SELECT COUNT(*) FROM memories WHERE tenant_id = @tenant_id AND created_at >= @since::timestamptz;

-- name: LatestSpaceCompile :one
SELECT last_compiled_at FROM spaces
WHERE tenant_id = $1 AND last_compiled_at IS NOT NULL
ORDER BY last_compiled_at DESC LIMIT 1;

-- name: CountConversations :one
SELECT COUNT(*) FROM conversations WHERE tenant_id = $1;

-- name: CountEnabledSkills :one
SELECT COUNT(*) FROM skills_state WHERE tenant_id = $1 AND enabled = true;

-- name: EnabledSkillNames :many
SELECT name FROM skills_state WHERE tenant_id = $1 AND enabled = true ORDER BY name ASC LIMIT 8;

-- name: CountBadges :one
SELECT COUNT(*) FROM achievement_progress WHERE tenant_id = $1 AND unlocked_at IS NOT NULL;

-- name: CountSkillRuns :one
SELECT COUNT(*)::int AS count FROM skill_run_log WHERE tenant_id = $1;

-- name: CountConnections :one
SELECT COUNT(*)::int AS count FROM memories WHERE tenant_id = $1 AND source_vault_file_id IS NOT NULL;

-- name: CountActiveSpaces :one
SELECT COUNT(DISTINCT space_id)::int AS count FROM memories WHERE tenant_id = $1 AND space_id IS NOT NULL;

-- name: CountTodos :one
SELECT COUNT(*)::int AS count FROM vault_files WHERE tenant_id = $1 AND metadata->>'isTodo' = 'true';

-- name: CountReminders :one
SELECT COUNT(*) FROM reminders WHERE tenant_id = $1;

-- name: CountActiveProjects :one
SELECT COUNT(*) FROM projects WHERE tenant_id = $1 AND archived = false;

-- name: CountActiveInsights :one
SELECT COUNT(*) FROM insights WHERE tenant_id = $1 AND dismissed_at IS NULL;

-- name: DefaultProfile :one
SELECT label, slug FROM user_hermes_profiles WHERE tenant_id = $1 AND is_default = true LIMIT 1;

-- name: LLMPreference :one
SELECT mode, primary_provider, primary_model FROM user_llm_preferences WHERE tenant_id = $1 LIMIT 1;

-- name: EnabledPluginSlugs :many
SELECT plugin_slug FROM plugin_installs
WHERE tenant_id = $1 AND status = 'enabled'
ORDER BY plugin_slug ASC LIMIT 8;

-- name: GraphPreview :many
SELECT id, content, score, last_accessed_at, created_at,
       graph_x::float8 AS graph_x, graph_y::float8 AS graph_y, graph_z::float8 AS graph_z
FROM memories
WHERE tenant_id = @tenant_id
  AND graph_x IS NOT NULL AND graph_y IS NOT NULL AND graph_z IS NOT NULL
ORDER BY last_accessed_at DESC NULLS LAST, score DESC, created_at DESC
LIMIT @row_limit;

-- name: ActiveWorkflowRuns :many
-- ActiveTasksQuery.workflowTasks.
SELECT r.id,
       COALESCE(w.name, 'Workflow')::text AS workflow_name,
       r.status,
       r.started_at,
       r.created_at,
       r.error_message
FROM workflow_runs r
LEFT JOIN workflows w ON w.id = r.workflow_id
WHERE r.tenant_id = @tenant_id
  AND r.status IN ('queued', 'running', 'waitingForApproval')
ORDER BY
  CASE r.status
    WHEN 'running' THEN 0
    WHEN 'queued' THEN 1
    WHEN 'waitingForApproval' THEN 2
    ELSE 3
  END,
  COALESCE(r.started_at, r.created_at) DESC NULLS LAST
LIMIT @row_limit;

-- name: CountActiveWorkflowRuns :one
SELECT COUNT(*)::int AS count FROM workflow_runs
WHERE tenant_id = $1 AND status IN ('queued', 'running', 'waitingForApproval');

-- name: RunningGatewayApplyJobs :many
SELECT id, error_message, created_at FROM hermes_gateway_apply_jobs
WHERE tenant_id = @tenant_id AND state = 'running'
ORDER BY created_at DESC
LIMIT @row_limit;

-- name: CountRunningGatewayApplyJobs :one
SELECT COUNT(*) FROM hermes_gateway_apply_jobs WHERE tenant_id = $1 AND state = 'running';

-- name: ListInsights :many
SELECT id, section, headline, summary, source_memory_ids, period_start, period_end, dismissed_at, created_at
FROM insights
WHERE tenant_id = @tenant_id
  AND dismissed_at IS NULL
  AND (sqlc.narg('section')::text IS NULL OR section = sqlc.narg('section')::text)
ORDER BY created_at DESC
LIMIT @row_limit;

-- name: LatestSynthesis :one
SELECT id, section, headline, summary, source_memory_ids, period_start, period_end, dismissed_at, created_at
FROM insights
WHERE tenant_id = $1 AND (section = 'this_week' OR section = 'this_month')
ORDER BY created_at DESC
LIMIT 1;

-- name: InsightDismissedAt :one
SELECT dismissed_at FROM insights WHERE tenant_id = $1 AND id = $2;

-- name: DismissInsight :exec
UPDATE insights SET dismissed_at = @dismissed_at::timestamptz WHERE tenant_id = @tenant_id AND id = @id;

-- name: LatestLeakCount :one
SELECT zero_hit_count FROM retrieval_leak_reports WHERE tenant_id = $1 ORDER BY period_end DESC LIMIT 1;

-- name: EmbeddingTokens :one
SELECT tokens_used FROM embedding_usage WHERE tenant_id = $1 AND year_month = $2 LIMIT 1;

-- name: HiddenRecommendations :many
SELECT recommendation_id FROM analytics_recommendation_states
WHERE vault_id = @vault_id AND user_id = @user_id
  AND (dismissed_at IS NOT NULL OR snoozed_until > @now::timestamptz);

-- name: InsertAnalyticsEvent :exec
INSERT INTO analytics_events
    (vault_id, actor_user_id, event_name, source, dimensions, idempotency_key)
VALUES (@vault_id, @actor_user_id, @event_name, @source,
        @dimensions::jsonb, sqlc.narg('idempotency_key')::text)
ON CONFLICT (actor_user_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING;

-- name: BumpAnalyticsRollup :exec
INSERT INTO analytics_daily_rollups (day, vault_id, actor_user_id, metric, value)
VALUES (CURRENT_DATE, @vault_id, @actor_user_id, @metric, 1)
ON CONFLICT (day, vault_id, actor_user_id, metric, dimension_key) DO UPDATE SET
    value = analytics_daily_rollups.value + 1, updated_at = NOW();

-- name: UpsertRecommendationState :exec
INSERT INTO analytics_recommendation_states
    (vault_id, user_id, recommendation_id, dismissed_at, snoozed_until)
VALUES (@vault_id, @user_id, @recommendation_id,
        sqlc.narg('dismissed_at')::timestamptz, sqlc.narg('snoozed_until')::timestamptz)
ON CONFLICT (vault_id, user_id, recommendation_id) DO UPDATE SET
    dismissed_at = EXCLUDED.dismissed_at,
    snoozed_until = EXCLUDED.snoozed_until,
    updated_at = NOW();
