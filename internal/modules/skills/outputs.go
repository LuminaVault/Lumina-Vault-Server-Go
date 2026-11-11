package skills

import (
	"context"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// SkillOutputsController: GET /v1/skills/outputs, the Today feed. Local skill runs
// with markdown (not source hermes) unioned with collected Hermes job runs, each
// arm limited before the union.

const (
	outputsMaxLimit     = 100
	outputsDefaultLimit = 50
	streakWindowDays    = 60
	activeRunWindowSecs = 3600
	headlineMax         = 120
)

func (m *Module) outputs(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	limit := outputsDefaultLimit
	if n, ok := wire.QueryInt(c, "limit"); ok {
		limit = max(1, min(n, outputsMaxLimit))
	}
	since := queryDate(c, "since")
	before := queryDate(c, "before")
	ctx := c.Request().Context()

	rows, err := m.pool.Query(ctx, `
		SELECT * FROM (
		    (SELECT id, name, source, started_at, status, error, markdown, NULL::text AS vault_path
		       FROM skill_run_log
		      WHERE tenant_id = $1
		        AND source <> 'hermes'
		        AND markdown IS NOT NULL AND markdown <> ''
		        AND ($2::timestamptz IS NULL OR started_at > $2::timestamptz)
		        AND ($3::timestamptz IS NULL OR started_at < $3::timestamptz)
		      ORDER BY started_at DESC
		      LIMIT $4)
		    UNION ALL
		    (SELECT r.id, COALESCE(NULLIF(j.name, ''), r.hermes_job_id) AS name,
		            'hermes' AS source, r.started_at, r.status, r.error,
		            r.output AS markdown, f.path AS vault_path
		       FROM hermes_job_runs r
		       LEFT JOIN hermes_mirrored_jobs j
		              ON j.tenant_id = r.tenant_id AND j.hermes_job_id = r.hermes_job_id
		       LEFT JOIN vault_files f ON f.id = r.vault_file_id
		      WHERE r.tenant_id = $1
		        AND r.status <> 'running'
		        AND (r.output IS NOT NULL OR r.error IS NOT NULL)
		        AND ($2::timestamptz IS NULL OR r.started_at > $2::timestamptz)
		        AND ($3::timestamptz IS NULL OR r.started_at < $3::timestamptz)
		      ORDER BY r.started_at DESC
		      LIMIT $4)
		) feed
		ORDER BY started_at DESC
		LIMIT $4`, tenant, since, before, limit)
	if err != nil {
		return err
	}
	out := []SkillOutputDTO{}
	for rows.Next() {
		var r feedRow
		if err := rows.Scan(&r.id, &r.name, &r.source, &r.startedAt, &r.status, &r.err, &r.markdown, &r.vaultPath); err != nil {
			rows.Close()
			return err
		}
		out = append(out, output(r))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	streak, err := m.streak(ctx, tenant)
	if err != nil {
		return err
	}
	active, err := m.hasActiveRun(ctx, tenant)
	if err != nil {
		return err
	}
	resp := SkillOutputListResponse{Outputs: out, StreakDays: streak, ActiveRun: active}
	// Only a full page can have more behind it.
	if len(out) == limit && len(out) > 0 {
		cursor := isoFractional(out[len(out)-1].CreatedAt.Time)
		resp.NextCursor = &cursor
	}
	return wire.JSON(c, http.StatusOK, resp)
}

type feedRow struct {
	id            uuid.UUID
	name, source  string
	startedAt     time.Time
	status        string
	err, markdown *string
	vaultPath     *string
}

func output(r feedRow) SkillOutputDTO {
	failed := r.status == statusError
	body := ""
	switch {
	case r.markdown != nil && *r.markdown != "":
		body = *r.markdown
	case r.err != nil:
		body = *r.err
	}
	source := r.source
	if source != "builtin" && source != "vault" && source != "hermes" {
		source = "builtin"
	}
	headline := outputHeadline(body, r.name)
	if failed && (r.markdown == nil || *r.markdown == "") {
		headline = r.name + " failed"
	}
	return SkillOutputDTO{
		ID: wire.UUID{UUID: r.id}, SkillName: r.name, Source: source, Kind: outputKind(r.name),
		Headline: headline, Body: body, CreatedAt: wire.Time{Time: r.startedAt}, VaultFilePath: r.vaultPath,
	}
}

// outputHeadline is SkillOutputsController.headline: the first non-empty line
// (Swift splits on the "\n" Character, so a CRLF pair is not a break), heading
// marks stripped, capped at 120 characters.
func outputHeadline(body, fallback string) string {
	var line strings.Builder
	lines := []string{}
	for _, ch := range swiftstr.Characters(body) {
		if ch == "\n" {
			lines = append(lines, line.String())
			line.Reset()
			continue
		}
		line.WriteString(ch)
	}
	lines = append(lines, line.String())
	for _, l := range lines {
		text := swiftstr.TrimWhitespaces(l)
		for strings.HasPrefix(text, "#") {
			text = text[1:]
		}
		text = swiftstr.TrimWhitespaces(text)
		if text == "" {
			continue
		}
		if swiftstr.Count(text) > headlineMax {
			return swiftstr.Prefix(text, headlineMax-1) + "…"
		}
		return text
	}
	return fallback
}

// outputKind is SkillOutputsController.kind(for:).
func outputKind(name string) string {
	lowered := strings.ToLower(name)
	switch {
	case strings.Contains(lowered, "contradiction"):
		return "contradiction_finding"
	case strings.Contains(lowered, "pattern"):
		return "pattern_finding"
	case strings.Contains(lowered, "correlation"):
		return "correlation_insight"
	case strings.Contains(lowered, "capture"):
		return "capture_enriched"
	case strings.Contains(lowered, "weekly") || strings.Contains(lowered, "memo"):
		return "weekly_memo"
	case strings.Contains(lowered, "daily") || strings.Contains(lowered, "brief") || strings.Contains(lowered, "digest"):
		return "daily_brief"
	}
	return "generic"
}

// streak counts consecutive UTC days with an output back from today, or from
// yesterday when today has none yet.
func (m *Module) streak(ctx context.Context, tenant uuid.UUID) (int, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT DISTINCT date_trunc('day', started_at AT TIME ZONE 'UTC') AS day FROM (
		    SELECT started_at FROM skill_run_log
		     WHERE tenant_id = $1 AND source <> 'hermes'
		       AND markdown IS NOT NULL AND markdown <> ''
		       AND started_at > NOW() - ($2 * INTERVAL '1 day')
		    UNION ALL
		    SELECT started_at FROM hermes_job_runs
		     WHERE tenant_id = $1 AND status <> 'running'
		       AND started_at > NOW() - ($2 * INTERVAL '1 day')
		) all_runs
		ORDER BY day DESC`, tenant, streakWindowDays)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var days []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return 0, err
		}
		days = append(days, d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return streakLength(days, m.now()), nil
}

func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// streakLength is SkillOutputsController.streakLength.
func streakLength(days []time.Time, today time.Time) int {
	present := map[time.Time]bool{}
	for _, d := range days {
		present[utcDay(d)] = true
	}
	if len(present) == 0 {
		return 0
	}
	cursor := utcDay(today)
	if !present[cursor] {
		yesterday := cursor.AddDate(0, 0, -1)
		if !present[yesterday] {
			return 0
		}
		cursor = yesterday
	}
	count := 0
	for present[cursor] {
		count++
		cursor = cursor.AddDate(0, 0, -1)
	}
	return count
}

func (m *Module) hasActiveRun(ctx context.Context, tenant uuid.UUID) (bool, error) {
	var active bool
	err := m.pool.QueryRow(ctx, `
		SELECT (
		    EXISTS (SELECT 1 FROM skill_run_log
		             WHERE tenant_id = $1
		               AND status IN ('pending', 'running')
		               AND started_at > NOW() - ($2 * INTERVAL '1 second'))
		    OR
		    EXISTS (SELECT 1 FROM hermes_job_runs
		             WHERE tenant_id = $1
		               AND status = 'running')
		) AS active`, tenant, activeRunWindowSecs).Scan(&active)
	return active, err
}

// queryDate is HermesDates.parse on a non-empty query value: ISO 8601 with or
// without fractional seconds, then the same with "Z" appended (Python's
// offset-less isoformat). Anything else is no bound.
func queryDate(c *echo.Context, name string) *time.Time {
	raw, ok := wire.QueryParam(c, name)
	if !ok || raw == "" {
		return nil
	}
	s := swiftstr.TrimWhitespaces(raw)
	if s == "" {
		return nil
	}
	for _, candidate := range []string{s, s + "Z"} {
		if t, err := time.Parse(time.RFC3339Nano, candidate); err == nil {
			return &t
		}
	}
	return nil
}

// isoFractional is HermesDates.iso: ISO 8601 UTC with milliseconds.
func isoFractional(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
