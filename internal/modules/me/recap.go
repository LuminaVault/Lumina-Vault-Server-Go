package me

import (
	"errors"
	"fmt"
	"lumina/internal/ids"
	"lumina/internal/modules/me/db"
	"lumina/internal/wire"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

const maxHighlights = 5

// recapWindow is MeRecapService.Window.
type recapWindow struct {
	timezone                    string
	previousDays, currentDays   []string
	previousStart, currentStart time.Time
	end                         time.Time
}

func newRecapWindow(days int, requested string, now time.Time) recapWindow {
	loc, _ := zone(requested)
	w := recapWindow{timezone: zoneName(requested)}
	today := startOfDay(now, loc)
	day := func(offset int) time.Time { return addDays(today, offset, loc) }
	for i := range days {
		w.currentDays = append(w.currentDays, dayKey(day(i-days+1), loc))
		w.previousDays = append(w.previousDays, dayKey(day(i-2*days+1), loc))
	}
	w.previousStart = day(-2*days + 1)
	w.currentStart = day(-days + 1)
	w.end = day(1)
	return w
}

func (m *Module) getRecap(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	rng := c.QueryParam("range")
	if rng == "" {
		rng = "7d"
	}
	days := map[string]int{"7d": 7, "30d": 30}[rng]
	if days == 0 {
		return badRequest("range must be 7d or 30d")
	}
	resp, err := m.recap(c, u, rng, days, m.now())
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, resp)
}

func (m *Module) recap(c *echo.Context, u user, rng string, ndays int, now time.Time) (MeRecapResponse, error) {
	w := newRecapWindow(ndays, u.timezone, now)
	rows, err := m.q.RecapDailyCounts(ctx(c), db.RecapDailyCountsParams{Tz: w.timezone, TenantID: u.id, Start: w.previousStart, EndAt: w.end})
	if err != nil {
		return MeRecapResponse{}, err
	}
	counts := map[string]RecapDayDTO{}
	for _, r := range rows {
		counts[r.Day] = RecapDayDTO{Date: r.Day, Captures: int(r.Captures), Learned: int(r.Learned), Asked: int(r.Asked), Reviewed: int(r.Reviewed)}
	}
	days := func(keys []string) []RecapDayDTO {
		out := make([]RecapDayDTO, 0, len(keys))
		for _, k := range keys {
			if d, ok := counts[k]; ok {
				out = append(out, d)
			} else {
				out = append(out, RecapDayDTO{Date: k})
			}
		}
		return out
	}
	daily, previousDaily := days(w.currentDays), days(w.previousDays)

	streak, err := m.q.StreakDays(ctx(c), db.StreakDaysParams{Tz: w.timezone, TenantID: u.id, AsOfDay: w.currentDays[len(w.currentDays)-1]})
	if err != nil {
		return MeRecapResponse{}, err
	}
	prevStreak, err := m.q.StreakDays(ctx(c), db.StreakDaysParams{Tz: w.timezone, TenantID: u.id, AsOfDay: w.previousDays[len(w.previousDays)-1]})
	if err != nil {
		return MeRecapResponse{}, err
	}
	kinds, err := m.recapKinds(c, u.id, w)
	if err != nil {
		return MeRecapResponse{}, err
	}
	highlights, err := m.recapHighlights(c, u.id, w)
	if err != nil {
		return MeRecapResponse{}, err
	}
	totals := recapTotals(daily, int(streak))
	return MeRecapResponse{
		Range: MeRecapResponseRange(rng), Start: w.currentDays[0], End: w.currentDays[len(w.currentDays)-1],
		Timezone: w.timezone, Totals: totals, PreviousTotals: recapTotals(previousDaily, int(prevStreak)),
		Daily: daily, Kinds: kinds, Highlights: highlights, ShareText: recapShareText(ndays, totals),
	}, nil
}

func recapTotals(days []RecapDayDTO, streak int) RecapTotalsDTO {
	t := RecapTotalsDTO{StreakDays: streak}
	for _, d := range days {
		t.Captures += d.Captures
		t.Learned += d.Learned
		t.Asked += d.Asked
		t.Reviewed += d.Reviewed
		if d.Captures+d.Learned+d.Asked+d.Reviewed > 0 {
			t.ActiveDays++
		}
	}
	return t
}

func (m *Module) recapKinds(c *echo.Context, tenant uuid.UUID, w recapWindow) (RecapKindsDTO, error) {
	rows, err := m.q.RecapKindCounts(ctx(c), db.RecapKindCountsParams{TenantID: tenant, Start: w.currentStart, EndAt: w.end})
	if err != nil {
		return RecapKindsDTO{}, err
	}
	by := map[string]int{}
	for _, r := range rows {
		by[r.Kind] += int(r.Count)
	}
	return RecapKindsDTO{Preference: by["preference"], Decision: by["decision"], Fact: by["fact"], Note: by["note"]}, nil
}

// recapHighlights is MeRecapService.highlights.
func (m *Module) recapHighlights(c *echo.Context, tenant uuid.UUID, w recapWindow) ([]RecapHighlightDTO, error) {
	span := db.RecapHighlightMemoriesParams{TenantID: tenant, Start: w.currentStart, EndAt: w.end}
	kindRows, err := m.q.RecapHighlightMemories(ctx(c), span)
	if err != nil {
		return nil, err
	}
	unlocks, err := m.q.UnlocksBetweenDesc(ctx(c), db.UnlocksBetweenDescParams(span))
	if err != nil {
		return nil, err
	}
	revisited, err := m.q.RecapMostRevisited(ctx(c), db.RecapMostRevisitedParams(span))
	hasRevisited := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	others := make([]RecapHighlightDTO, 0, len(kindRows)+len(unlocks))
	for _, r := range kindRows {
		kind := r.Kind
		if kind != "decision" && kind != "preference" {
			kind = "unknown"
		}
		others = append(others, RecapHighlightDTO{Kind: RecapHighlightKind(kind), RefId: ids.Upper(r.ID), Title: titlePreview(r.Content), Date: wt(orZero(r.At))})
	}
	for _, r := range unlocks {
		title := r.AchievementKey
		if s, ok := m.subs[r.AchievementKey]; ok {
			title = s.Label
		}
		others = append(others, RecapHighlightDTO{Kind: "achievement", RefId: r.AchievementKey, Title: title, Date: wt(orZero(r.UnlockedAt))})
	}
	sortByDateDesc(others)

	picked := make([]RecapHighlightDTO, 0, maxHighlights)
	if hasRevisited {
		picked = append(picked, RecapHighlightDTO{Kind: "mostRevisited", RefId: ids.Upper(revisited.ID), Title: titlePreview(revisited.Content), Date: wt(revisited.At)})
	}
	picked = append(picked, others[:min(len(others), maxHighlights-len(picked))]...)
	sortByDateDesc(picked)
	return picked, nil
}

// sortByDateDesc is Swift's `sort { $0.date > $1.date }` (stable in practice).
func sortByDateDesc(h []RecapHighlightDTO) {
	sort.SliceStable(h, func(i, j int) bool { return h[i].Date.After(h[j].Date.Time) })
}

// recapShareText is MeRecapService.shareText: numbers only.
func recapShareText(days int, t RecapTotalsDTO) string {
	parts := []string{plural(t.Captures, "capture", "captures")}
	if t.Learned > 0 {
		parts = append(parts, plural(t.Learned, "thing learned", "things learned"))
	}
	if t.Asked > 0 {
		parts = append(parts, plural(t.Asked, "question asked", "questions asked"))
	}
	if t.StreakDays > 0 {
		parts = append(parts, fmt.Sprintf("a %d-day streak", t.StreakDays))
	}
	return fmt.Sprintf("My last %d days on LuminaVault: %s.", days, strings.Join(parts, ", "))
}
