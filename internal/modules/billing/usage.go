package billing

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// UsageDailyPoint is LuminaVaultShared.UsageDailyPointDTO.
type UsageDailyPoint struct {
	Day           wire.Time `json:"day"`
	TokensIn      int64     `json:"tokensIn"`
	TokensOut     int64     `json:"tokensOut"`
	TTSCharacters int64     `json:"ttsCharacters"`
	CompileRuns   int64     `json:"compileRuns"`
	CompileFiles  int64     `json:"compileFiles"`
}

// MeUsageResponse is LuminaVaultShared.MeUsageResponse.
type MeUsageResponse struct {
	Tier          string            `json:"tier"`
	PeriodStart   wire.Time         `json:"periodStart"`
	PeriodEnd     wire.Time         `json:"periodEnd"`
	GeneratedAt   wire.Time         `json:"generatedAt"`
	StorageBytes  int64             `json:"storageBytes"`
	TokensIn      int64             `json:"tokensIn"`
	TokensOut     int64             `json:"tokensOut"`
	TokensTotal   int64             `json:"tokensTotal"`
	TTSCharacters int64             `json:"ttsCharacters"`
	CompileRuns   int64             `json:"compileRuns"`
	CompileFiles  int64             `json:"compileFiles"`
	Daily         []UsageDailyPoint `json:"daily"`
}

var usageTiers = map[string]bool{"free": true, "trial": true, "pro": true, "ultimate": true, "lapsed": true, "archived": true}

// currentUTCMonth is UsageMetricsService.currentUTCMonth(containing:).
func currentUTCMonth(now time.Time) (start, end time.Time) {
	u := now.UTC()
	start = time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

const dailyUsageSQL = `
WITH meter AS (
    SELECT day::timestamptz AS day,
           SUM(mtok_in) AS tokens_in,
           SUM(mtok_out) AS tokens_out,
           SUM(chars_out) AS tts_characters,
           0::bigint AS compile_runs,
           0::bigint AS compile_files
    FROM usage_meter
    WHERE tenant_id = $1 AND day >= $2 AND day < $3
    GROUP BY day
    UNION ALL
    SELECT date_trunc('day', occurred_at) AS day,
           0::bigint AS tokens_in,
           0::bigint AS tokens_out,
           0::bigint AS tts_characters,
           SUM(amount) FILTER (WHERE metric = 'memory_compile_run') AS compile_runs,
           SUM(amount) FILTER (WHERE metric = 'memory_compile_file') AS compile_files
    FROM usage_events
    WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3
    GROUP BY date_trunc('day', occurred_at)
)
SELECT day,
       COALESCE(SUM(tokens_in), 0)::bigint AS tokens_in,
       COALESCE(SUM(tokens_out), 0)::bigint AS tokens_out,
       COALESCE(SUM(tts_characters), 0)::bigint AS tts_characters,
       COALESCE(SUM(compile_runs), 0)::bigint AS compile_runs,
       COALESCE(SUM(compile_files), 0)::bigint AS compile_files
FROM meter
GROUP BY day
ORDER BY day ASC`

// CurrentMonthUsage is UsageMetricsService.currentMonthUsage(for:now:).
func (m *Module) CurrentMonthUsage(ctx context.Context, tenant uuid.UUID, now time.Time) (*MeUsageResponse, error) {
	var tier string
	if err := m.pool.QueryRow(ctx, `SELECT tier FROM users WHERE id = $1`, tenant).Scan(&tier); err != nil {
		return nil, err
	}
	if !usageTiers[tier] {
		tier = "trial" // UserTier(rawValue:) ?? .trial
	}
	start, end := currentUTCMonth(now)
	out := &MeUsageResponse{
		Tier: tier, PeriodStart: wire.Time{Time: start}, PeriodEnd: wire.Time{Time: end},
		GeneratedAt: wire.Time{Time: now}, Daily: []UsageDailyPoint{},
	}
	if err := m.pool.QueryRow(ctx, `SELECT COALESCE(SUM(size_bytes), 0)::bigint FROM vault_files WHERE tenant_id = $1`,
		tenant).Scan(&out.StorageBytes); err != nil {
		return nil, err
	}
	if err := m.pool.QueryRow(ctx, `SELECT
            COALESCE(SUM(mtok_in), 0)::bigint, COALESCE(SUM(mtok_out), 0)::bigint, COALESCE(SUM(chars_out), 0)::bigint
        FROM usage_meter WHERE tenant_id = $1 AND day >= $2 AND day < $3`, tenant, start, end).
		Scan(&out.TokensIn, &out.TokensOut, &out.TTSCharacters); err != nil {
		return nil, err
	}
	out.TokensTotal = out.TokensIn + out.TokensOut
	if err := m.pool.QueryRow(ctx, `SELECT
            COALESCE(SUM(amount) FILTER (WHERE metric = 'memory_compile_run'), 0)::bigint,
            COALESCE(SUM(amount) FILTER (WHERE metric = 'memory_compile_file'), 0)::bigint
        FROM usage_events WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3`, tenant, start, end).
		Scan(&out.CompileRuns, &out.CompileFiles); err != nil {
		return nil, err
	}
	rows, err := m.pool.Query(ctx, dailyUsageSQL, tenant, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p UsageDailyPoint
		var day time.Time
		if err := rows.Scan(&day, &p.TokensIn, &p.TokensOut, &p.TTSCharacters, &p.CompileRuns, &p.CompileFiles); err != nil {
			return nil, err
		}
		p.Day = wire.Time{Time: day}
		out.Daily = append(out.Daily, p)
	}
	return out, rows.Err()
}

func (m *Module) meUsage(c *echo.Context) error {
	out, err := m.CurrentMonthUsage(c.Request().Context(), auth.UserID(c), m.opt.Now())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}
