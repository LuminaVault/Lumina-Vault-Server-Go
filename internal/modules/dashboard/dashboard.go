package dashboard

import (
	"context"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/modules/achievements"
	"lumina/internal/modules/dashboard/db"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v5"
)

const (
	graphPreviewLimit    = 30
	activityDefaultLimit = 20
	activityMaxLimit     = 50

	// ModelDisclosurePolicy: managed tenants never see the upstream provider or model.
	genericProviderName = "LuminaVault"
	genericModelID      = "auto"
	genericBrainName    = "LuminaVault Brain · Auto"
)

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// stats is DashboardController.stats.
func (m *Module) stats(c *echo.Context) error {
	ctx, tenant := requestContext(c), auth.UserID(c)
	total, err := m.q.CountMemories(ctx, tenant)
	if err != nil {
		return err
	}
	today, err := m.q.CountMemoriesSince(ctx, db.CountMemoriesSinceParams{TenantID: tenant, Since: timestamptz(startOfDayUTC(time.Now()))})
	if err != nil {
		return err
	}
	compiled, err := orZero(m.q.LatestSpaceCompile(ctx, tenant))
	if err != nil {
		return err
	}
	out := DashboardStatsResponse{MemoriesToday: int(today), MemoriesTotal: int(total)}
	if compiled != nil {
		out.LastCompileAt = &wire.Time{Time: *compiled}
	}
	return wire.JSON(c, http.StatusOK, out)
}

// powerInputs are the counts behind PowerLevel that profile and home share.
type powerInputs struct {
	memories, sessions, jobs, badges, connections, spaces, streak int
}

func (m *Module) loadPowerInputs(ctx context.Context, tenant uuid.UUID) (powerInputs, error) {
	var p powerInputs
	memories, err := m.q.CountMemories(ctx, tenant)
	if err != nil {
		return p, err
	}
	sessions, err := m.q.CountConversations(ctx, tenant)
	if err != nil {
		return p, err
	}
	badges, err := m.q.CountBadges(ctx, tenant)
	if err != nil {
		return p, err
	}
	jobs, err := m.q.CountSkillRuns(ctx, tenant)
	if err != nil {
		return p, err
	}
	connections, err := m.q.CountConnections(ctx, tenant)
	if err != nil {
		return p, err
	}
	spaces, err := m.q.CountActiveSpaces(ctx, tenant)
	if err != nil {
		return p, err
	}
	var streak int
	if err := m.pool.QueryRow(ctx, sqlStreakDays, pgx.NamedArgs{"tenant_id": tenant}).Scan(&streak); err != nil {
		return p, err
	}
	return powerInputs{
		memories: int(memories), sessions: int(sessions), jobs: int(jobs), badges: int(badges),
		connections: int(connections), spaces: int(spaces), streak: streak,
	}, nil
}

func (p powerInputs) xp() int {
	return achievements.PowerLevelXP(achievements.XPInput{
		MemoriesTotal: p.memories, SessionsCount: p.sessions, JobsCount: p.jobs, BadgesEarned: p.badges,
		GraphConnections: p.connections, ActiveSpaces: p.spaces, StreakDays: p.streak,
	})
}

// profile is DashboardController.profile.
func (m *Module) profile(c *echo.Context) error {
	ctx, tenant := requestContext(c), auth.UserID(c)
	p, err := m.loadPowerInputs(ctx, tenant)
	if err != nil {
		return err
	}
	skills, err := m.q.CountEnabledSkills(ctx, tenant)
	if err != nil {
		return err
	}
	xp := p.xp()
	return wire.JSON(c, http.StatusOK, DashboardProfileResponse{
		SkillsCount: int(skills), JobsCount: p.jobs, SessionsCount: p.sessions, BadgesEarned: p.badges,
		PowerLevel: achievements.PowerLevel(xp), PowerXP: xp,
	})
}

// home is DashboardController.home.
//
// Gap: Swift fills cronJobs through CronBridgeService (docker exec into a
// managed tenant's Hermes container, or the BYO Hermes dashboard API), and
// answers [] when the tenant has neither or the call fails. Go always answers
// that disabled path until the Hermes modules move (wave 6).
func (m *Module) home(c *echo.Context) error {
	ctx, tenant := requestContext(c), auth.UserID(c)
	now := time.Now()
	period := parsePeriod(query(c, "period"))

	p, err := m.loadPowerInputs(ctx, tenant)
	if err != nil {
		return err
	}
	xp := p.xp()
	out := HomeSummaryResponse{
		PrimaryProvider: genericProviderName,
		PrimaryModel:    genericBrainName,
		AgentOnline:     true,
		JobsCount:       p.jobs,
		MemoriesTotal:   p.memories,
		SessionsCount:   p.sessions,
		BadgesEarned:    p.badges,
		StreakDays:      p.streak,
		PowerLevel:      achievements.PowerLevel(xp),
		PowerXP:         xp,
		CronJobs:        []CronJobDTO{},
		Period:          period,
	}

	counts := []struct {
		dst *int
		run func(context.Context, uuid.UUID) (int64, error)
	}{
		{&out.SkillsCount, m.q.CountEnabledSkills},
		{&out.RemindersCount, m.q.CountReminders},
		{&out.ProjectsCount, m.q.CountActiveProjects},
		{&out.InsightsCount, m.q.CountActiveInsights},
	}
	for _, cnt := range counts {
		n, err := cnt.run(ctx, tenant)
		if err != nil {
			return err
		}
		*cnt.dst = int(n)
	}
	todos, err := m.q.CountTodos(ctx, tenant)
	if err != nil {
		return err
	}
	out.TodosCount = int(todos)
	today, err := m.q.CountMemoriesSince(ctx, db.CountMemoriesSinceParams{TenantID: tenant, Since: timestamptz(startOfDayUTC(now))})
	if err != nil {
		return err
	}
	out.MemoriesToday = int(today)

	names, err := m.q.EnabledSkillNames(ctx, tenant)
	if err != nil {
		return err
	}
	out.Skills = nonNil(names)

	profile, err := orZero(m.q.DefaultProfile(ctx, tenant))
	if err != nil {
		return err
	}
	if profile != (db.DefaultProfileRow{}) {
		out.ActiveProfileName, out.ActiveProfileSlug = &profile.Label, &profile.Slug
	}

	// Managed tenants see a generic brain label; BYOK tenants their own model,
	// falling back to the label when they saved an empty model.
	pref, err := orZero(m.q.LLMPreference(ctx, tenant))
	if err != nil {
		return err
	}
	if pref.Mode == "byok" {
		out.PrimaryProvider = pref.PrimaryProvider
		if pref.PrimaryModel != "" {
			out.PrimaryModel = pref.PrimaryModel
		}
	}

	if out.ActiveJobs, err = m.activeTasks(ctx, tenant, "", activePreviewLimit); err != nil {
		return err
	}
	if out.ActiveJobsCount, err = m.activeTasksCount(ctx, tenant); err != nil {
		return err
	}
	if out.GraphPreview, err = m.graphPreview(ctx, tenant, now); err != nil {
		return err
	}

	// Swift swallows a failed tools query.
	tools, err := m.q.EnabledPluginSlugs(ctx, tenant)
	if err != nil {
		slog.Warn("dashboard tools preview failed", "error", err)
		tools = nil
	}
	out.Tools, out.ToolsCount = nonNil(tools), len(tools)

	out.PeriodStats, out.PeriodSeries, out.PeriodMix = m.periodCockpit(ctx, tenant, period, periodWindow(period, now))
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) graphPreview(ctx context.Context, tenant uuid.UUID, now time.Time) ([]GraphPreviewNodeDTO, error) {
	rows, err := m.q.GraphPreview(ctx, db.GraphPreviewParams{TenantID: tenant, RowLimit: graphPreviewLimit})
	if err != nil {
		return nil, err
	}
	nodes := make([]GraphPreviewNodeDTO, 0, len(rows))
	for _, r := range rows {
		last := r.LastAccessedAt
		if last == nil {
			last = r.CreatedAt
		}
		nodes = append(nodes, GraphPreviewNodeDTO{
			ID:       wire.UUID{UUID: r.ID},
			Label:    titleFromContent(r.Content),
			X:        swiftDouble(r.GraphX / graphCubeExtent),
			Y:        swiftDouble(r.GraphY / graphCubeExtent),
			Z:        swiftDouble(r.GraphZ / graphCubeExtent),
			Activity: swiftDouble(nodeActivity(r.Score, last, now)),
			Kind:     "memory",
		})
	}
	return nodes, nil
}

type periodCounts struct {
	captures, skillRuns, done, chats, tokens, jobs int
}

func (m *Module) periodCounts(ctx context.Context, tenant uuid.UUID, start, end time.Time) (periodCounts, error) {
	var pc periodCounts
	err := m.pool.QueryRow(ctx, sqlPeriodCounts, pgx.NamedArgs{"tenant_id": tenant, "range_start": start, "range_end": end}).
		Scan(&pc.captures, &pc.skillRuns, &pc.done, &pc.chats, &pc.tokens, &pc.jobs)
	return pc, err
}

func (m *Module) periodSeries(ctx context.Context, tenant uuid.UUID, w window) ([]SeriesPoint, error) {
	rows, err := m.pool.Query(ctx, sqlPeriodSeries, pgx.NamedArgs{
		"tenant_id": tenant, "range_start": w.Start, "range_end": w.End, "trunc": w.Trunc,
	})
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SeriesPoint, error) {
		var at time.Time
		var value int
		err := r.Scan(&at, &value)
		return SeriesPoint{At: wire.Time{Time: at}, Value: value}, err
	})
}

// periodCockpit is DashboardController.periodCockpit: any failure, such as
// tokens overflowing Swift's ::int, logs and answers an empty cockpit.
func (m *Module) periodCockpit(ctx context.Context, tenant uuid.UUID, period string, w window) (PeriodStats, []SeriesPoint, PeriodMix) {
	current, err := m.periodCounts(ctx, tenant, w.Start, w.End)
	var previous periodCounts
	if err == nil {
		previous, err = m.periodCounts(ctx, tenant, w.PreviousStart, w.PreviousEnd)
	}
	var series []SeriesPoint
	if err == nil {
		series, err = m.periodSeries(ctx, tenant, w)
	}
	if err != nil {
		slog.Warn("dashboard period cockpit failed", "error", err, "period", period)
		return PeriodStats{}, []SeriesPoint{}, PeriodMix{}
	}
	return PeriodStats{
		Done: current.done, Captures: current.captures, SkillRuns: current.skillRuns, Tokens: current.tokens,
		PreviousDone: previous.done, PreviousCaptures: previous.captures,
		PreviousSkillRuns: previous.skillRuns, PreviousTokens: previous.tokens,
	}, nonNil(series), PeriodMix{
		Captures: current.captures, Jobs: current.jobs, Skills: current.skillRuns, Chats: current.chats,
	}
}

// activity is DashboardController.activity: the merged recent-activity feed.
func (m *Module) activity(c *echo.Context) error {
	ctx, tenant := requestContext(c), auth.UserID(c)
	limit := activityDefaultLimit
	if n, err := strconv.Atoi(query(c, "limit")); err == nil {
		limit = n
	}
	limit = min(max(limit, 1), activityMaxLimit)

	rows, err := m.pool.Query(ctx, sqlActivityFeed, pgx.NamedArgs{"tenant_id": tenant, "row_limit": limit})
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ActivityFeedItemDTO, error) {
		var (
			id       uuid.UUID
			kind     string
			title    string
			subtitle *string
			at       time.Time
		)
		if err := r.Scan(&id, &kind, &title, &subtitle, &at); err != nil {
			return ActivityFeedItemDTO{}, err
		}
		if kind == "memory" {
			title = titleFromContent(title)
		}
		return ActivityFeedItemDTO{ID: wire.UUID{UUID: id}, Kind: kind, Title: title, Subtitle: subtitle, OccurredAt: wire.Time{Time: at}}, nil
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, ActivityFeedResponse{Items: nonNil(items)})
}
