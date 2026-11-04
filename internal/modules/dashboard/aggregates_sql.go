package dashboard

// The multi-table aggregates, close to verbatim from the Swift controllers. They
// are raw SQL with pgx.NamedArgs because sqlc's analyzer rejects their nested
// scopes. Every Date bind is cast to timestamptz, as PostgresNIO types it: with a
// bare `usage_meter.day >= $1` Postgres would infer date, and `day < now` would
// then drop today.

const (
	sqlStreakDays = `WITH activity_days AS (
    SELECT DISTINCT (created_at AT TIME ZONE 'UTC')::date AS day
    FROM memories
    WHERE tenant_id = @tenant_id
    UNION
    SELECT DISTINCT (created_at AT TIME ZONE 'UTC')::date AS day
    FROM conversations
    WHERE tenant_id = @tenant_id
),
ranked AS (
    SELECT day,
           day - (row_number() OVER (ORDER BY day))::int AS island_key
    FROM activity_days
),
latest AS (
    SELECT island_key
    FROM ranked
    ORDER BY day DESC
    LIMIT 1
)
SELECT COUNT(*)::int AS count
FROM ranked
WHERE island_key = (SELECT island_key FROM latest)`

	sqlActivityFeed = `SELECT merged.id::uuid AS id, merged.kind::text AS kind, merged.title::text AS title,
       merged.subtitle::text AS subtitle, merged.occurred_at::timestamptz AS occurred_at FROM (
    (SELECT conversations.id, 'conversation' AS kind,
            COALESCE(NULLIF(title, ''), 'Conversation') AS title,
            NULL AS subtitle, created_at AS occurred_at
     FROM conversations
     WHERE tenant_id = @tenant_id AND created_at IS NOT NULL
     ORDER BY created_at DESC LIMIT @row_limit)
    UNION ALL
    (SELECT memories.id, 'memory' AS kind,
            LEFT(content, 120) AS title, NULL AS subtitle,
            created_at AS occurred_at
     FROM memories
     WHERE tenant_id = @tenant_id AND created_at IS NOT NULL
     ORDER BY created_at DESC LIMIT @row_limit)
    UNION ALL
    (SELECT achievement_progress.id, 'achievement' AS kind,
            achievement_key AS title, NULL AS subtitle,
            unlocked_at AS occurred_at
     FROM achievement_progress
     WHERE tenant_id = @tenant_id AND unlocked_at IS NOT NULL
     ORDER BY unlocked_at DESC LIMIT @row_limit)
    UNION ALL
    (SELECT skill_run_log.id, 'skillRun' AS kind,
            name AS title, status AS subtitle,
            started_at AS occurred_at
     FROM skill_run_log
     WHERE tenant_id = @tenant_id
     ORDER BY started_at DESC LIMIT @row_limit)
) merged
ORDER BY merged.occurred_at DESC
LIMIT @row_limit`

	sqlPeriodCounts = `SELECT
    COALESCE((SELECT COUNT(*)::int FROM memories
              WHERE tenant_id = @tenant_id
                AND created_at >= @range_start::timestamptz
                AND created_at < @range_end::timestamptz), 0)::int AS captures,
    COALESCE((SELECT COUNT(*)::int FROM skill_run_log
              WHERE tenant_id = @tenant_id
                AND started_at >= @range_start::timestamptz
                AND started_at < @range_end::timestamptz), 0)::int AS skill_runs,
    COALESCE((SELECT COUNT(*)::int FROM skill_run_log
              WHERE tenant_id = @tenant_id
                AND started_at >= @range_start::timestamptz
                AND started_at < @range_end::timestamptz
                AND LOWER(COALESCE(status, '')) IN ('ok', 'completed', 'succeeded', 'success')), 0)::int AS done,
    COALESCE((SELECT COUNT(*)::int FROM conversations
              WHERE tenant_id = @tenant_id
                AND created_at >= @range_start::timestamptz
                AND created_at < @range_end::timestamptz), 0)::int AS chats,
    COALESCE((SELECT (SUM(COALESCE(mtok_in, 0) + COALESCE(mtok_out, 0)))::int FROM usage_meter
              WHERE tenant_id = @tenant_id
                AND day >= @range_start::timestamptz
                AND day < @range_end::timestamptz), 0)::int AS tokens,
    COALESCE((SELECT COUNT(*)::int FROM skill_run_log
              WHERE tenant_id = @tenant_id
                AND started_at >= @range_start::timestamptz
                AND started_at < @range_end::timestamptz
                AND LOWER(COALESCE(status, '')) IN ('running', 'queued', 'ok', 'completed')), 0)::int AS jobs`

	sqlPeriodSeries = `SELECT merged.bucket::timestamptz AS bucket, SUM(merged.value)::int AS value FROM (
    SELECT date_trunc(@trunc::text, created_at) AS bucket, COUNT(*)::int AS value
    FROM memories
    WHERE tenant_id = @tenant_id
      AND created_at >= @range_start::timestamptz
      AND created_at < @range_end::timestamptz
    GROUP BY 1
    UNION ALL
    SELECT date_trunc(@trunc::text, started_at) AS bucket, COUNT(*)::int AS value
    FROM skill_run_log
    WHERE tenant_id = @tenant_id
      AND started_at >= @range_start::timestamptz
      AND started_at < @range_end::timestamptz
    GROUP BY 1
    UNION ALL
    SELECT date_trunc(@trunc::text, created_at) AS bucket, COUNT(*)::int AS value
    FROM conversations
    WHERE tenant_id = @tenant_id
      AND created_at >= @range_start::timestamptz
      AND created_at < @range_end::timestamptz
    GROUP BY 1
) merged
WHERE merged.bucket IS NOT NULL
GROUP BY merged.bucket
ORDER BY merged.bucket`

	sqlRetrievalWindow = `SELECT COUNT(*)::int AS events,
       (COUNT(*) FILTER (WHERE zero_hit = false))::int AS hits,
       AVG(top_distance)::float8 AS mean_top
FROM retrieval_telemetry_events
WHERE tenant_id = @tenant_id
  AND created_at >= @range_start::timestamptz AND created_at < @range_end::timestamptz`

	sqlUsageTotals = `SELECT COALESCE((SELECT SUM(mtok_in) FROM usage_meter
                 WHERE tenant_id = @tenant_id AND day >= @since::timestamptz), 0)::bigint AS tin,
       COALESCE((SELECT SUM(mtok_out) FROM usage_meter
                 WHERE tenant_id = @tenant_id AND day >= @since::timestamptz), 0)::bigint AS tout,
       COALESCE((SELECT COUNT(*) FROM conversations
                 WHERE tenant_id = @tenant_id AND created_at >= @since::timestamptz), 0)::bigint AS sessions,
       COALESCE((SELECT SUM(estimated_cost_usd_micros) FROM router_executions
                 WHERE actor_user_id = @tenant_id AND occurred_at >= @since::timestamptz), 0)::bigint AS cost`

	sqlVoiceUsage = `SELECT COALESCE(metadata->>'channel', 'unknown')::text                  AS channel,
       COUNT(*)::bigint                                                 AS calls,
       (COUNT(*) FILTER (WHERE metadata->>'outcome' <> 'ok'))::bigint   AS failed,
       COALESCE(SUM(amount), 0)::bigint                                 AS duration_ms,
       COALESCE(SUM((metadata->>'usdMicros')::bigint), 0)::bigint       AS real_micros,
       COALESCE(SUM((metadata->>'imputedUsdMicros')::bigint), 0)::bigint AS imputed_micros
FROM usage_events
WHERE tenant_id = @tenant_id
  AND metric = 'voice_transcribe'
  AND occurred_at >= @since::timestamptz
GROUP BY 1
ORDER BY duration_ms DESC`

	sqlAnalyticsSummary = `SELECT COALESCE((SELECT COUNT(*) FROM conversations WHERE tenant_id = @user_id
                 AND created_at >= @since::timestamptz), 0)::bigint AS personal_sessions,
       COALESCE((SELECT COUNT(*) FROM analytics_events WHERE vault_id = @vault_id
                 AND event_name = 'conversation_created' AND occurred_at >= @since::timestamptz), 0)::bigint AS team_sessions,
       COALESCE((SELECT COUNT(*) FROM router_executions WHERE vault_id = @vault_id
                 AND occurred_at >= @since::timestamptz), 0)::bigint AS requests,
       COALESCE((SELECT SUM(tokens_in) FROM router_executions WHERE vault_id = @vault_id
                 AND occurred_at >= @since::timestamptz), 0)::bigint AS tin,
       COALESCE((SELECT SUM(tokens_out) FROM router_executions WHERE vault_id = @vault_id
                 AND occurred_at >= @since::timestamptz), 0)::bigint AS tout,
       COALESCE((SELECT COUNT(*) FROM memories WHERE tenant_id = @vault_id
                 AND created_at >= @since::timestamptz AND review_state != 'rejected'), 0)::bigint AS captures,
       COALESCE((SELECT COUNT(*) FROM analytics_events WHERE vault_id = @vault_id
                 AND event_name = 'memory_retrieved' AND occurred_at >= @since::timestamptz), 0)::bigint AS retrievals,
       COALESCE((SELECT SUM(estimated_cost_usd_micros) FROM router_executions
                 WHERE vault_id = @vault_id AND occurred_at >= @since::timestamptz), 0)::bigint AS cost`

	sqlAnalyticsDaily = `WITH days AS (
    SELECT generate_series(date_trunc('day', @since::timestamptz),
                           date_trunc('day', NOW()), interval '1 day') AS day
), memory_day AS (
    SELECT date_trunc('day', created_at) AS day, COUNT(*) AS captures
    FROM memories WHERE tenant_id = @vault_id AND created_at >= @since::timestamptz
      AND review_state != 'rejected' GROUP BY 1
), retrieval_day AS (
    SELECT date_trunc('day', occurred_at) AS day, COUNT(*) AS retrievals
    FROM analytics_events WHERE vault_id = @vault_id
      AND event_name = 'memory_retrieved' AND occurred_at >= @since::timestamptz GROUP BY 1
), router_day AS (
    SELECT date_trunc('day', occurred_at) AS day, COUNT(*) AS requests,
           COALESCE(SUM(tokens_in + tokens_out), 0)::bigint AS tokens,
           COALESCE(SUM(estimated_cost_usd_micros), 0)::bigint AS cost
    FROM router_executions WHERE vault_id = @vault_id AND occurred_at >= @since::timestamptz
    GROUP BY 1
), session_day AS (
    SELECT date_trunc('day', created_at) AS day, COUNT(*) AS sessions
    FROM conversations WHERE @personal::bool AND tenant_id = @user_id
      AND created_at >= @since::timestamptz
    GROUP BY 1
    UNION ALL
    SELECT date_trunc('day', occurred_at) AS day, COUNT(*) AS sessions
    FROM analytics_events WHERE NOT @personal::bool AND vault_id = @vault_id
      AND event_name = 'conversation_created' AND occurred_at >= @since::timestamptz
    GROUP BY 1
)
SELECT d.day::timestamptz AS day, COALESCE(s.sessions, 0)::bigint AS sessions,
       COALESCE(r.requests, 0)::bigint AS requests, COALESCE(r.tokens, 0)::bigint AS tokens,
       COALESCE(m.captures, 0)::bigint AS captures, COALESCE(q.retrievals, 0)::bigint AS retrievals,
       COALESCE(r.cost, 0)::bigint AS cost
FROM days d LEFT JOIN memory_day m ON m.day = d.day
LEFT JOIN retrieval_day q ON q.day = d.day
LEFT JOIN router_day r ON r.day = d.day LEFT JOIN session_day s ON s.day = d.day
ORDER BY d.day LIMIT @row_limit`

	sqlMemoryHealth = `SELECT COUNT(*) AS total,
       COUNT(*) FILTER (WHERE COALESCE(last_reviewed_at, last_accessed_at, created_at) < @now::timestamptz - interval '30 days') AS stale,
       COUNT(*) FILTER (WHERE query_hit_count = 0) AS never_retrieved,
       COUNT(*) FILTER (WHERE COALESCE(cardinality(tags), 0) = 0 AND space_id IS NULL AND source_vault_file_id IS NULL) AS unorganized,
       COUNT(*) FILTER (WHERE review_state = 'pending') AS pending,
       COALESCE(AVG(exp(-ln(2.0) * GREATEST(0, EXTRACT(EPOCH FROM (@now::timestamptz - COALESCE(last_reviewed_at, last_accessed_at, created_at))) / 86400.0) / 30.0)), 0)::float8 AS freshness,
       COALESCE(AVG(LEAST(1.0, ln(1.0 + access_count + query_hit_count) / ln(6.0))), 0)::float8 AS engagement,
       COALESCE(AVG((CASE WHEN COALESCE(cardinality(tags), 0) > 0 THEN 0.5 ELSE 0 END) +
                    (CASE WHEN space_id IS NOT NULL OR source_vault_file_id IS NOT NULL THEN 0.5 ELSE 0 END)), 0)::float8 AS organization,
       COALESCE(AVG(CASE WHEN review_state != 'pending' AND COALESCE(last_reviewed_at, last_accessed_at, created_at) >= @now::timestamptz - interval '30 days' THEN 1.0 ELSE 0 END), 0)::float8 AS review
FROM memories WHERE tenant_id = @vault_id AND review_state != 'rejected'`

	sqlModelReliability = `SELECT COUNT(*) AS requests, COUNT(*) FILTER (WHERE status != 'ok') AS failures,
       COALESCE(AVG(latency_ms), 0)::bigint AS latency
FROM router_executions WHERE vault_id = @vault_id AND occurred_at >= @since::timestamptz`

	sqlModelEffectiveness = `WITH feedback AS (
    SELECT CASE WHEN @hide::bool
                THEN @generic_provider::text
                ELSE COALESCE(dimensions->>'provider', 'unknown') END AS provider,
           CASE WHEN @hide::bool
                THEN @generic_model::text
                ELSE COALESCE(dimensions->>'model', 'unknown') END AS model,
           COUNT(*) FILTER (WHERE event_name = 'model_feedback_positive') AS positive_feedback,
           COUNT(*) FILTER (WHERE event_name = 'model_feedback_negative') AS negative_feedback
    FROM analytics_events
    WHERE vault_id = @vault_id
      AND occurred_at >= @since::timestamptz
      AND event_name IN ('model_feedback_positive', 'model_feedback_negative')
    GROUP BY 1, 2
)
SELECT (CASE WHEN @hide::bool
            THEN @generic_provider::text
            ELSE COALESCE(r.selected_provider, 'unknown') END)::text AS provider,
       (CASE WHEN @hide::bool
            THEN @generic_model::text
            ELSE COALESCE(r.selected_model, 'unknown') END)::text AS model,
       COUNT(*) AS requests,
       COUNT(*) FILTER (WHERE r.status = 'ok') AS successes,
       COUNT(*) FILTER (WHERE r.fallback_count > 0) AS fallbacks,
       COALESCE(AVG(r.latency_ms), 0)::bigint AS latency,
       COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY r.latency_ms), 0)::bigint AS p95,
       COALESCE(SUM(r.tokens_in + r.tokens_out), 0)::bigint AS tokens,
       COALESCE(SUM(r.estimated_cost_usd_micros), 0)::bigint AS cost,
       COALESCE(f.positive_feedback, 0)::bigint AS positive_feedback,
       COALESCE(f.negative_feedback, 0)::bigint AS negative_feedback
FROM router_executions r
LEFT JOIN feedback f
  ON f.provider = CASE WHEN @hide::bool
                       THEN @generic_provider::text
                       ELSE COALESCE(r.selected_provider, 'unknown') END
 AND f.model = CASE WHEN @hide::bool
                    THEN @generic_model::text
                    ELSE COALESCE(r.selected_model, 'unknown') END
WHERE r.vault_id = @vault_id
  AND r.occurred_at >= @since::timestamptz
GROUP BY 1, 2, f.positive_feedback, f.negative_feedback
ORDER BY requests DESC`

	sqlTeamMembers = `SELECT u.id, u.username::text AS name,
       COALESCE(m.captures, 0)::bigint AS captures,
       COALESCE(q.retrievals, 0)::bigint AS retrievals,
       COALESCE(r.requests, 0)::bigint AS requests,
       COALESCE(r.tokens, 0)::bigint AS tokens,
       COALESCE(r.cost, 0)::bigint AS cost
FROM vault_memberships vm
JOIN users u ON u.id = vm.user_id
LEFT JOIN (
    SELECT created_by_user_id AS actor, COUNT(*) AS captures
    FROM memories WHERE tenant_id = @vault_id AND created_at >= @since::timestamptz
    GROUP BY created_by_user_id
) m ON m.actor = u.id
LEFT JOIN (
    SELECT actor_user_id AS actor, COUNT(*) AS retrievals
    FROM analytics_events WHERE vault_id = @vault_id
      AND event_name = 'memory_retrieved' AND occurred_at >= @since::timestamptz
    GROUP BY actor_user_id
) q ON q.actor = u.id
LEFT JOIN (
    SELECT actor_user_id AS actor, COUNT(*) AS requests,
           COALESCE(SUM(tokens_in + tokens_out), 0)::bigint AS tokens,
           COALESCE(SUM(estimated_cost_usd_micros), 0)::bigint AS cost
    FROM router_executions WHERE vault_id = @vault_id
      AND occurred_at >= @since::timestamptz GROUP BY actor_user_id
) r ON r.actor = u.id
WHERE vm.vault_id = @vault_id
  AND EXISTS (SELECT 1 FROM team_memberships tm WHERE tm.team_id = @team_id
              AND tm.user_id = u.id)
ORDER BY m.captures + r.requests DESC, u.username`
)
