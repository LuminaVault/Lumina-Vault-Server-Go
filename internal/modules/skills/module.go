// Package skills serves /v1/skills and /v1/jobs: the Go port of Swift's
// SkillsController (list, patch, runs), SkillOutputsController (the Today feed)
// and JobsController (chat→job detection and creation).
package skills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/cronexpr"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/ratelimit"
	"lumina/internal/skills"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	runsMaxLimit      = 100
	runsDefaultLimit  = 50
	sparklineDays     = 14
	hermesSkillPrefix = "hermes-"
	excerptLength     = 200
)

// Options are the module's dependencies.
type Options struct {
	Identify echo.MiddlewareFunc
	Limits   *ratelimit.Store
	Catalog  *skills.Catalog
	Paths    vaultpaths.Paths
	// Mirror is whether Swift would have a HermesMirrorService (a secret box and a
	// Hermes endpoint resolver). With it, GET /v1/skills appends mirrored skills.
	Mirror bool
	// Classifier backs POST /v1/jobs/detect.
	Transport    llm.ChatTransport
	DefaultModel string
	// The /v1/jobs entitlement: EntitlementMiddleware(requires: .chat) with BYO keys.
	EnforcementEnabled bool
	HasCredential      entitlement.HasUsableCredential
	Log                *slog.Logger
}

// Module is the route group.
type Module struct {
	pool *pgxpool.Pool
	opt  Options
	now  func() time.Time
}

// New builds the module.
func New(pool *pgxpool.Pool, opt Options) *Module {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Module{pool: pool, opt: opt, now: time.Now}
}

// Register mounts the routes. Swift has three groups on two prefixes: /v1/skills
// under skillRunByUser, /v1/skills/outputs under skillOutputsByUser, and /v1/jobs
// under jobsByUser plus the chat entitlement.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	skillsChain := []echo.MiddlewareFunc{
		m.opt.Identify,
		ratelimit.Middleware(m.opt.Limits, ratelimit.SkillRunByUser, ratelimit.UserOrIPKey),
	}
	r.GET("/v1/skills", m.list, skillsChain...)
	r.PATCH("/v1/skills/:name", m.patch, skillsChain...)
	r.GET("/v1/skills/:name/runs", m.runs, skillsChain...)

	r.GET("/v1/skills/outputs", m.outputs, m.opt.Identify,
		ratelimit.Middleware(m.opt.Limits, ratelimit.SkillOutputsByUser, ratelimit.UserOrIPKey))

	jobsChain := []echo.MiddlewareFunc{
		m.opt.Identify,
		ratelimit.Middleware(m.opt.Limits, ratelimit.JobsByUser, ratelimit.UserOrIPKey),
		entitlement.Middleware(m.pool, entitlement.Chat, m.opt.EnforcementEnabled, m.opt.HasCredential),
	}
	r.POST("/v1/jobs/detect", m.detect, jobsChain...)
	r.POST("/v1/jobs", m.createJob, jobsChain...)
}

func requireIdentity(c *echo.Context) (uuid.UUID, error) {
	id, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, auth.ErrUnauthorized
	}
	return id, nil
}

// nameParam is ctx.parameters.get("name"): the raw path segment, which
// Hummingbird does not percent-decode.
func nameParam(c *echo.Context, fromEnd int) string {
	segments := wire.PathSegments(c.Request())
	return segments[len(segments)-1-fromEnd]
}

// tier is `user.tier.isEmpty ? "trial" : user.tier`.
func (m *Module) tier(ctx context.Context, tenant uuid.UUID) (string, error) {
	var tier string
	if err := m.pool.QueryRow(ctx, `SELECT tier FROM users WHERE id = $1`, tenant).Scan(&tier); err != nil {
		return "", err
	}
	if tier == "" {
		return "trial", nil
	}
	return tier, nil
}

// state is a skills_state row, the fields the DTO reads.
type state struct {
	source, name     string
	enabled          bool
	scheduleOverride *string
	lastRunAt        *time.Time
	lastStatus       *string
	lastError        *string
	apnsCategory     *string
	originKind       *string
	originID         *uuid.UUID
}

const stateColumns = `source, name, enabled, schedule_override, last_run_at, last_status, last_error, apns_category, origin_kind, origin_id`

func scanState(row pgx.Row) (state, error) {
	var s state
	err := row.Scan(&s.source, &s.name, &s.enabled, &s.scheduleOverride, &s.lastRunAt, &s.lastStatus, &s.lastError,
		&s.apnsCategory, &s.originKind, &s.originID)
	return s, err
}

// GET /v1/skills

func (m *Module) list(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	manifests := m.opt.Catalog.Manifests(tenant)
	rows, err := m.pool.Query(ctx, `SELECT `+stateColumns+` FROM skills_state WHERE tenant_id = $1`, tenant)
	if err != nil {
		return err
	}
	states := map[string]state{}
	for rows.Next() {
		s, err := scanState(rows)
		if err != nil {
			rows.Close()
			return err
		}
		states[s.source+":"+s.name] = s
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	counts, err := m.dailyRunCounts(ctx, tenant, nil)
	if err != nil {
		return err
	}
	tier, err := m.tier(ctx, tenant)
	if err != nil {
		return err
	}
	out := make([]SkillDTO, 0, len(manifests))
	for _, mf := range manifests {
		var st *state
		if s, ok := states[string(mf.Source)+":"+mf.Name]; ok {
			st = &s
		}
		out = append(out, buildDTO(mf, st, counts[mf.Name], tier))
	}
	if m.opt.Mirror {
		mirrored, err := m.mirroredSkills(ctx, tenant)
		if err != nil {
			return err
		}
		out = append(out, mirrored...)
	}
	return wire.JSON(c, http.StatusOK, SkillListResponse{Skills: out})
}

// mirroredSkills is HermesMirrorService.mirroredSkills mapped through hermesSkillDTO.
func (m *Module) mirroredSkills(ctx context.Context, tenant uuid.UUID) ([]SkillDTO, error) {
	rows, err := m.pool.Query(ctx, `SELECT name, description, enabled FROM hermes_mirrored_skills
		WHERE tenant_id = $1 ORDER BY name ASC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillDTO
	for rows.Next() {
		var name, description string
		var enabled bool
		if err := rows.Scan(&name, &description, &enabled); err != nil {
			return nil, err
		}
		out = append(out, SkillDTO{
			ID: hermesSkillPrefix + name, Source: "hermes", Name: name, Title: name,
			DescriptionText: description, Capability: string(skills.CapabilityMedium), Enabled: enabled,
			BodyExcerpt: swiftstr.Prefix(description, excerptLength),
		})
	}
	return out, rows.Err()
}

func buildDTO(mf skills.Manifest, st *state, dailyRunCount int, tier string) SkillDTO {
	d := SkillDTO{
		ID: string(mf.Source) + ":" + mf.Name, Source: string(mf.Source), Name: mf.Name, Title: mf.Name,
		DescriptionText: mf.Description, Capability: string(mf.Capability), Schedule: mf.Schedule,
		Enabled: true, DailyRunCount: dailyRunCount, BodyExcerpt: swiftstr.Prefix(mf.Body, excerptLength),
	}
	if mf.DailyRunCap != nil {
		d.DailyRunCap = mf.DailyRunCap.Value(tier)
	}
	if st == nil {
		return d
	}
	d.ScheduleOverride = st.scheduleOverride
	d.Enabled = st.enabled
	if st.lastRunAt != nil {
		d.LastRunAt = &wire.Time{Time: *st.lastRunAt}
	}
	if st.lastStatus != nil && validRunStatus(*st.lastStatus) {
		d.LastStatus = st.lastStatus
	}
	d.LastError = st.lastError
	if st.apnsCategory != nil && apnsCategories[*st.apnsCategory] {
		d.APNSCategory = st.apnsCategory
	}
	if st.originKind != nil && st.originID != nil {
		d.Origin = &JobOriginDTO{Kind: originKind(*st.originKind), ID: wire.UUID{UUID: *st.originID}}
	}
	return d
}

// dailyRunCounts counts today's skill_run_log rows per name (any source), in
// the session time zone's day as date_trunc('day', NOW()) has it.
func (m *Module) dailyRunCounts(ctx context.Context, tenant uuid.UUID, names []string) (map[string]int, error) {
	query := `SELECT name, COUNT(*)::int FROM skill_run_log
		WHERE tenant_id = $1 AND started_at >= date_trunc('day', NOW()) GROUP BY name`
	args := []any{tenant}
	if len(names) > 0 {
		query = `SELECT name, COUNT(*)::int FROM skill_run_log
		WHERE tenant_id = $1 AND started_at >= date_trunc('day', NOW()) AND name = ANY($2) GROUP BY name`
		args = append(args, names)
	}
	rows, err := m.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// PATCH /v1/skills/{name}

func (m *Module) patch(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	name := nameParam(c, 0)
	body, err := wire.Bind[skillPatchRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	mf, ok := m.opt.Catalog.Manifest(name, tenant)
	if !ok {
		return wire.NewError(http.StatusNotFound, "no skill named "+name)
	}
	if body.ScheduleOverride != nil && *body.ScheduleOverride != "" {
		if _, ok := cronexpr.Parse(*body.ScheduleOverride); !ok {
			return wire.NewError(http.StatusBadRequest, "invalid cron expression")
		}
	}

	st, err := scanState(m.pool.QueryRow(ctx, `SELECT `+stateColumns+` FROM skills_state
		WHERE tenant_id = $1 AND source = $2 AND name = $3 LIMIT 1`, tenant, string(mf.Source), mf.Name))
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !exists {
		st = state{source: string(mf.Source), name: mf.Name, enabled: true}
	}

	// Fluent's save: an existing row updates only the fields assigned, keyed by
	// its @ID alone (tenant_id), so every skills_state row of the tenant takes
	// them. Swift's weakness, kept until the archive (D-security-after-archive).
	var sets []string
	var args []any
	set := func(column string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)+1))
	}
	if body.Enabled != nil {
		st.enabled = *body.Enabled
		set("enabled", st.enabled)
	}
	if body.ScheduleOverride != nil {
		st.scheduleOverride = nil
		if *body.ScheduleOverride != "" {
			st.scheduleOverride = body.ScheduleOverride
		}
		set("schedule_override", st.scheduleOverride)
	}
	if body.APNSCategory != nil {
		v := string(*body.APNSCategory)
		st.apnsCategory = &v
		set("apns_category", v)
	}
	switch {
	case !exists:
		if _, err := m.pool.Exec(ctx, `INSERT INTO skills_state (tenant_id, source, name, enabled, schedule_override,
			run_at, last_run_at, last_status, last_error, apns_category)
			VALUES ($1, $2, $3, $4, $5, NULL, NULL, NULL, NULL, $6)`,
			tenant, st.source, st.name, st.enabled, st.scheduleOverride, st.apnsCategory); err != nil {
			return err
		}
	case len(sets) > 0:
		query := "UPDATE skills_state SET " + strings.Join(sets, ", ") + " WHERE tenant_id = $1"
		if _, err := m.pool.Exec(ctx, query, append([]any{tenant}, args...)...); err != nil {
			return err
		}
	}

	counts, err := m.dailyRunCounts(ctx, tenant, []string{mf.Name})
	if err != nil {
		return err
	}
	tier, err := m.tier(ctx, tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, buildDTO(mf, &st, counts[mf.Name], tier))
}

// GET /v1/skills/{name}/runs

func (m *Module) runs(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	name := nameParam(c, 1)
	limit := runsDefaultLimit
	if n, ok := wire.QueryInt(c, "limit"); ok {
		limit = max(1, min(n, runsMaxLimit))
	}
	ctx := c.Request().Context()
	rows, err := m.pool.Query(ctx, `SELECT id, started_at, ended_at, status, error, model_used, mtok_in, mtok_out,
		markdown, blocks::text FROM skill_run_log WHERE tenant_id = $1 AND name = $2
		ORDER BY started_at DESC LIMIT $3`, tenant, name, limit)
	if err != nil {
		return err
	}
	out := []SkillRunDTO{}
	for rows.Next() {
		var (
			id                      uuid.UUID
			started                 time.Time
			ended                   *time.Time
			status                  string
			errText, model, md, raw *string
			in, outTok              int
		)
		if err := rows.Scan(&id, &started, &ended, &status, &errText, &model, &in, &outTok, &md, &raw); err != nil {
			rows.Close()
			return err
		}
		if !validRunStatus(status) {
			status = statusError
		}
		d := SkillRunDTO{
			ID: wire.UUID{UUID: id}, StartedAt: wire.Time{Time: started}, Status: status, Error: errText,
			ModelUsed: model, MtokIn: in, MtokOut: outTok, Markdown: md, Blocks: decodeBlocks(raw),
		}
		if ended != nil {
			d.EndedAt = &wire.Time{Time: *ended}
		}
		out = append(out, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	buckets, err := m.pool.Query(ctx, `SELECT date_trunc('day', started_at), COUNT(*)::int FROM skill_run_log
		WHERE tenant_id = $1 AND name = $2 AND started_at >= NOW() - INTERVAL '14 days' GROUP BY 1`, tenant, name)
	if err != nil {
		return err
	}
	byDay := map[int64]int{}
	for buckets.Next() {
		var day time.Time
		var n int
		if err := buckets.Scan(&day, &n); err != nil {
			buckets.Close()
			return err
		}
		byDay[day.Unix()] = n
	}
	buckets.Close()
	if err := buckets.Err(); err != nil {
		return err
	}
	now := m.now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	sparkline := make([]SkillSparklinePoint, 0, sparklineDays)
	for offset := sparklineDays - 1; offset >= 0; offset-- {
		day := today.AddDate(0, 0, -offset)
		sparkline = append(sparkline, SkillSparklinePoint{Day: wire.Time{Time: day}, Count: byDay[day.Unix()]})
	}
	return wire.JSON(c, http.StatusOK, SkillRunsResponse{Runs: out, Sparkline: sparkline})
}

// decodeBlocks is `try? JSONDecoder().decode([LuminaBlock].self, ...)`: nil
// when the column is null or anything fails to decode.
func decodeBlocks(raw *string) *[]luminaBlock {
	if raw == nil {
		return nil
	}
	var env struct {
		Blocks []luminaBlock `json:"blocks"`
	}
	if err := wire.DecodeInto([]byte(`{"blocks":`+*raw+`}`), &env); err != nil {
		return nil
	}
	if env.Blocks == nil {
		return nil
	}
	return &env.Blocks
}
