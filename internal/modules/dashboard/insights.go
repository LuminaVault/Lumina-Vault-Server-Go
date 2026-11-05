package dashboard

import (
	"errors"
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/internal/modules/dashboard/db"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

const (
	insightsMaxLimit     = 100
	insightsDefaultLimit = 50
)

func isInsightSection(s string) bool {
	switch s {
	case "this_week", "this_month", "patterns", "contradictions", "connections":
		return true
	}
	return false
}

// insightRow is the column set both insight queries select.
type insightRow struct {
	ID                                uuid.UUID
	Section, Headline, Summary        string
	Sources                           []uuid.UUID
	PeriodStart, PeriodEnd, Dismissed *time.Time
	CreatedAt                         time.Time
}

// toDTO is Insight.toDTO: an unknown section reads as patterns.
func (r insightRow) toDTO() InsightDTO {
	section := r.Section
	if !isInsightSection(section) {
		section = "patterns"
	}
	sources := make([]wire.UUID, len(r.Sources))
	for i, id := range r.Sources {
		sources[i] = wire.UUID{UUID: id}
	}
	dto := InsightDTO{
		ID: wire.UUID{UUID: r.ID}, Headline: r.Headline, Summary: r.Summary, Section: section,
		CreatedAt: wire.Time{Time: r.CreatedAt}, SourceMemoryIDs: sources, Dismissed: r.Dismissed != nil,
	}
	if r.PeriodStart != nil {
		dto.PeriodStart = &wire.Time{Time: *r.PeriodStart}
	}
	if r.PeriodEnd != nil {
		dto.PeriodEnd = &wire.Time{Time: *r.PeriodEnd}
	}
	return dto
}

// listInsights is InsightsController.list.
func (m *Module) listInsights(c *echo.Context) error {
	var section *string
	if raw := wire.Query(c, "section"); isInsightSection(raw) {
		section = &raw
	}
	rows, err := m.q.ListInsights(requestContext(c), db.ListInsightsParams{
		TenantID: auth.UserID(c),
		Section:  section,
		RowLimit: int32(clampedLimit(wire.Query(c, "limit"), insightsDefaultLimit, insightsMaxLimit)), //nolint:gosec // clamped to 1..100
	})
	if err != nil {
		return err
	}
	out := make([]InsightDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, insightRow{
			ID: r.ID, Section: r.Section, Headline: r.Headline, Summary: r.Summary, Sources: r.SourceMemoryIds,
			PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, Dismissed: r.DismissedAt, CreatedAt: r.CreatedAt,
		}.toDTO())
	}
	return wire.JSON(c, http.StatusOK, InsightListResponse{Insights: out})
}

// latestSynthesis is InsightsController.latestSynthesis. Dismissal does not
// matter: the row is a snapshot, not a nag.
func (m *Module) latestSynthesis(c *echo.Context) error {
	r, err := m.q.LatestSynthesis(requestContext(c), auth.UserID(c))
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("no synthesis available yet")
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, insightRow{
		ID: r.ID, Section: r.Section, Headline: r.Headline, Summary: r.Summary, Sources: r.SourceMemoryIds,
		PeriodStart: r.PeriodStart, PeriodEnd: r.PeriodEnd, Dismissed: r.DismissedAt, CreatedAt: r.CreatedAt,
	}.toDTO())
}

// dismissInsight is InsightsController.dismiss. Dismissing twice keeps the first
// dismissed_at.
func (m *Module) dismissInsight(c *echo.Context) error {
	ctx, tenant := requestContext(c), auth.UserID(c)
	raw := c.Param("id")
	id, err := ids.Parse(raw)
	if err != nil {
		return wire.NewError(http.StatusBadRequest, "invalid insight id")
	}
	dismissed, err := m.q.InsightDismissedAt(ctx, db.InsightDismissedAtParams{TenantID: tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("insight not found")
	}
	if err != nil {
		return err
	}
	if dismissed == nil {
		err := m.q.DismissInsight(ctx, db.DismissInsightParams{DismissedAt: timestamptz(time.Now()), TenantID: tenant, ID: id})
		if err != nil {
			return err
		}
	}
	return c.NoContent(http.StatusNoContent)
}
