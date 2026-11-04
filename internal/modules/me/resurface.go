package me

import (
	"fmt"
	"lumina/internal/modules/me/db"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const (
	resurfaceDefaultLimit   = 3
	resurfaceMaxLimit       = 10
	recentlyReviewedWindow  = 24 * time.Hour
	reviewOverdueAge        = 30 * 24 * time.Hour // MemoryHealthFilter.reviewOverdueAge
	onThisDayMinimumMemoAge = 27 * 24 * time.Hour
)

func (m *Module) getResurface(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	limit, err := parseResurfaceLimit(c.QueryParam("limit"))
	if err != nil {
		return err
	}
	resp, err := m.resurface(c, u, limit, m.now())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, resp)
}

// parseResurfaceLimit is MeResurfaceController.parseLimit: 1…10, default 3.
func parseResurfaceLimit(raw string) (int, error) {
	if raw == "" {
		return resurfaceDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > resurfaceMaxLimit {
		return 0, badRequest(fmt.Sprintf("limit must be between 1 and %d", resurfaceMaxLimit))
	}
	return n, nil
}

type resurfaceRow struct {
	id        uuid.UUID
	content   string
	kind      *string
	createdAt *time.Time
	spaceID   *uuid.UUID
	localDay  string
}

func resurfaceItem(r resurfaceRow, reason, why string) ResurfaceItemDTO {
	return ResurfaceItemDTO{
		MemoryId: wire.UUID{UUID: r.id}, Snippet: relatedSnippet(r.content), Kind: r.kind,
		CreatedAt: wt(orZero(r.createdAt)), SpaceId: wup(r.spaceID), Reason: ResurfaceReason(reason), Why: why,
	}
}

// resurface is MeResurfaceService.build: due, then on this day, then fading,
// deduped by memory, at most limit.
func (m *Module) resurface(c *echo.Context, u user, limit int, now time.Time) (ResurfaceResponse, error) {
	loc, _ := zone(u.timezone)
	tz := zoneName(u.timezone)
	picked := make([]ResurfaceItemDTO, 0, limit)
	seen := map[uuid.UUID]bool{}
	take := func(items []ResurfaceItemDTO) {
		for _, it := range items {
			if len(picked) < limit && !seen[it.MemoryId.UUID] {
				picked = append(picked, it)
				seen[it.MemoryId.UUID] = true
			}
		}
	}
	reviewedBefore := now.Add(-recentlyReviewedWindow)

	due, err := m.q.ResurfaceDue(ctx(c), db.ResurfaceDueParams{TenantID: u.id, ReviewedBefore: reviewedBefore, Now: now, Lim: int32(limit)}) //nolint:gosec // 1...10
	if err != nil {
		return ResurfaceResponse{}, err
	}
	items := make([]ResurfaceItemDTO, 0, len(due))
	for _, r := range due {
		items = append(items, resurfaceItem(resurfaceRow{r.ID, r.Content, r.Kind, r.CreatedAt, r.SpaceID, ""}, "due", "You asked to see this again"))
	}
	take(items)
	if len(picked) >= limit {
		return ResurfaceResponse{Items: picked}, nil
	}

	ann := anniversariesFor(now, loc)
	today := dayKey(now, loc)
	twoYearsAgo, _ := strconv.Atoi(today[:4])
	otd, err := m.q.ResurfaceOnThisDay(ctx(c), db.ResurfaceOnThisDayParams{
		Tz: tz, TenantID: u.id, ReviewedBefore: reviewedBefore, Now: now,
		CreatedBefore: now.Add(-onThisDayMinimumMemoAge),
		MonthAgo:      ann.month, HalfYearAgo: ann.halfYear, YearAgo: ann.year,
		MonthDay: today[5:], TwoYearsAgo: int32(twoYearsAgo - 2), //nolint:gosec // a year
	})
	if err != nil {
		return ResurfaceResponse{}, err
	}
	items = items[:0]
	for _, r := range otd {
		why, ok := onThisDayWhy(r.LocalDay, today, ann)
		if !ok {
			continue
		}
		items = append(items, resurfaceItem(resurfaceRow{r.ID, r.Content, r.Kind, r.CreatedAt, r.SpaceID, r.LocalDay}, "onThisDay", why))
	}
	take(items)
	if len(picked) >= limit {
		return ResurfaceResponse{Items: picked}, nil
	}

	fading, err := m.q.ResurfaceFading(ctx(c), db.ResurfaceFadingParams{
		Tz: tz, TenantID: u.id, ReviewedBefore: reviewedBefore, Now: now,
		Cutoff: now.Add(-reviewOverdueAge), Lim: int32(limit), //nolint:gosec // 1...10
	})
	if err != nil {
		return ResurfaceResponse{}, err
	}
	items = items[:0]
	for _, r := range fading {
		items = append(items, resurfaceItem(resurfaceRow{r.ID, r.Content, r.Kind, r.CreatedAt, r.SpaceID, r.LocalDay}, "fading", fadingWhy(r.Kind, r.LocalDay)))
	}
	take(items)
	return ResurfaceResponse{Items: picked}, nil
}

type anniversaries struct{ month, halfYear, year string }

// anniversariesFor is MeResurfaceService.anniversaries: today minus one, six
// and twelve months on the user's calendar, clamped to the month's end.
func anniversariesFor(now time.Time, loc *time.Location) anniversaries {
	today := startOfDay(now, loc)
	back := func(n int) string { return dayKey(addMonths(today, -n, loc), loc) }
	return anniversaries{month: back(1), halfYear: back(6), year: back(12)}
}

func onThisDayWhy(day, today string, a anniversaries) (string, bool) {
	switch day {
	case a.year:
		return "Saved a year ago today", true
	case a.halfYear:
		return "Saved 6 months ago today", true
	case a.month:
		return "Saved a month ago today", true
	}
	if len(day) < 5 || len(today) < 5 {
		return "", false
	}
	year, err1 := strconv.Atoi(day[:4])
	thisYear, err2 := strconv.Atoi(today[:4])
	if err1 != nil || err2 != nil || day[5:] != today[5:] || thisYear-year < 2 {
		return "", false
	}
	return fmt.Sprintf("Saved %d years ago today", thisYear-year), true
}

func fadingWhy(kind *string, day string) string {
	noun := "A decision"
	if kind != nil && *kind == "preference" {
		noun = "A preference"
	}
	if since, ok := monthYear(day); ok {
		return noun + " you haven't revisited since " + since
	}
	return noun + " you haven't revisited in a while"
}

// monthYear is MeResurfaceService.monthYear: `2026-03-14` → `March 2026`.
func monthYear(day string) (string, bool) {
	parts := strings.Split(day, "-")
	if len(parts) != 3 {
		return "", false
	}
	year, err1 := strconv.Atoi(parts[0])
	month, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || month < 1 || month > 12 {
		return "", false
	}
	return fmt.Sprintf("%s %d", time.Month(month).String(), year), true
}
