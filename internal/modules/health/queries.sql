-- Health (Swift: Sources/App/Health/HealthIngestController.swift, model HealthEvent).

-- name: InsertHealthEvent :exec
INSERT INTO health_events (id, tenant_id, event_type, value_numeric, value_text, unit, recorded_at, source, metadata, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListHealthEvents :many
SELECT id, event_type, value_numeric, value_text, unit, recorded_at, source
FROM health_events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND recorded_at >= sqlc.arg(from_at) AND recorded_at < sqlc.arg(to_at)
  AND (sqlc.narg(event_type)::text IS NULL OR event_type = sqlc.narg(event_type)::text)
ORDER BY recorded_at DESC
LIMIT sqlc.arg(row_limit)::bigint OFFSET sqlc.arg(row_offset)::bigint;

-- name: DailySum :many
-- Swift's two statements differ only in SUM/AVG.
SELECT (date_trunc('day', recorded_at AT TIME ZONE 'UTC'))::timestamp AS day,
       SUM(value_numeric)::float8 AS value,
       COUNT(*) AS sample_count
FROM health_events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND event_type = sqlc.arg(event_type)
  AND recorded_at >= sqlc.arg(from_at)
  AND recorded_at < sqlc.arg(to_at)
  AND value_numeric IS NOT NULL
GROUP BY day
ORDER BY day ASC;

-- name: DailyAvg :many
SELECT (date_trunc('day', recorded_at AT TIME ZONE 'UTC'))::timestamp AS day,
       AVG(value_numeric)::float8 AS value,
       COUNT(*) AS sample_count
FROM health_events
WHERE tenant_id = sqlc.arg(tenant_id)
  AND event_type = sqlc.arg(event_type)
  AND recorded_at >= sqlc.arg(from_at)
  AND recorded_at < sqlc.arg(to_at)
  AND value_numeric IS NOT NULL
GROUP BY day
ORDER BY day ASC;
