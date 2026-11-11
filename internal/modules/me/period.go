package me

import (
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/modules/me/db"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// ─── Window (PeriodRecapWindow.swift) ─────────────────────────────────────

type periodDay struct {
	key     string
	weekday int // 1 = Monday … 7 = Sunday
}

type periodWindow struct {
	kind                  string // "year" | "month"
	year                  int
	month                 int // 0 for a year
	timezone              string
	start, end, periodEnd time.Time
	previousStart         time.Time
	previousEnd           time.Time
	days                  []periodDay
	bucketKeys            []string
	isComplete, isFuture  bool
}

// newPeriodWindow is PeriodRecapWindow.init; ok is false where Swift's init? is nil.
func newPeriodWindow(kind string, year, month int, requested string, now time.Time) (periodWindow, bool) {
	if year < 2000 || year > 9999 {
		return periodWindow{}, false
	}
	loc, _ := zone(requested)
	w := periodWindow{kind: kind, year: year, timezone: zoneName(requested)}
	step := func(t time.Time, n int) time.Time {
		if kind == "year" {
			return addMonths(t, 12*n, loc)
		}
		return addMonths(t, n, loc)
	}
	switch kind {
	case "year":
		if month != 0 {
			return periodWindow{}, false
		}
		w.start = time.Date(year, 1, 1, 0, 0, 0, 0, loc)
	case "month":
		if month < 1 || month > 12 {
			return periodWindow{}, false
		}
		w.month = month
		w.start = time.Date(year, time.Month(month), 1, 0, 0, 0, 0, loc)
	default:
		return periodWindow{}, false
	}
	w.periodEnd = step(w.start, 1)
	tomorrow := addDays(startOfDay(now, loc), 1, loc)
	w.isFuture = now.Before(w.start)
	w.isComplete = !now.Before(w.periodEnd)
	w.end = w.start
	if !w.isFuture {
		w.end = w.periodEnd
		if tomorrow.Before(w.end) {
			w.end = tomorrow
		}
	}
	w.previousStart = step(w.start, -1)
	w.previousEnd = step(w.end, -1)
	if w.start.Before(w.previousEnd) {
		w.previousEnd = w.start
	}
	for cursor := w.start; cursor.Before(w.end); cursor = addDays(cursor, 1, loc) {
		w.days = append(w.days, periodDay{key: dayKey(cursor, loc), weekday: (int(cursor.In(loc).Weekday())+6)%7 + 1})
	}
	if kind == "year" {
		seen := map[string]bool{}
		for _, d := range w.days {
			if mk := d.key[:7]; !seen[mk] {
				seen[mk] = true
				w.bucketKeys = append(w.bucketKeys, mk)
			}
		}
	} else {
		for _, d := range w.days {
			w.bucketKeys = append(w.bucketKeys, d.key)
		}
	}
	return w, true
}

func (w periodWindow) periodKey() string {
	if w.month != 0 {
		return fmt.Sprintf("%04d-%02d", w.year, w.month)
	}
	return fmt.Sprintf("%04d", w.year)
}

func (w periodWindow) startKey() string {
	if len(w.days) == 0 {
		return ""
	}
	return w.days[0].key
}

func (w periodWindow) endKey() string {
	if len(w.days) == 0 {
		return ""
	}
	return w.days[len(w.days)-1].key
}

// ─── Controller (PeriodRecapController.swift) ─────────────────────────────

type periodParams struct {
	kind  string
	year  int
	month int
}

// parsePeriodParams is PeriodRecapController.parse.
func parsePeriodParams(period, year, month, timezone string, now time.Time) (periodParams, error) {
	kind := "year"
	if period != "" {
		if period != "year" && period != "month" {
			return periodParams{}, badRequest("period must be year or month")
		}
		kind = period
	}
	loc, _ := zone(timezone)
	local := now.In(loc)
	y := local.Year()
	if year != "" {
		v, err := strconv.Atoi(year)
		if err != nil || v < 2000 || v > 9999 {
			return periodParams{}, badRequest("year must be a four-digit year")
		}
		y = v
	}
	if kind == "year" {
		if month != "" {
			return periodParams{}, badRequest("month is only for period=month")
		}
		return periodParams{kind: "year", year: y}, nil
	}
	if month != "" {
		v, err := strconv.Atoi(month)
		if err != nil || v < 1 || v > 12 {
			return periodParams{}, badRequest("month must be 1-12")
		}
		return periodParams{kind: "month", year: y, month: v}, nil
	}
	return periodParams{kind: "month", year: y, month: int(local.Month())}, nil
}

// periodCacheControl is PeriodRecapController.cacheControl.
func periodCacheControl(r PeriodRecapResponse) string {
	if r.Themes != nil && r.Themes.Status == "pending" {
		return "private, no-cache"
	}
	if r.IsComplete {
		return "private, max-age=3600"
	}
	return "private, max-age=300"
}

func (m *Module) getPeriodRecap(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	now := m.now()
	p, err := parsePeriodParams(wire.Query(c, "period"), wire.Query(c, "year"), wire.Query(c, "month"), u.timezone, now)
	if err != nil {
		return err
	}
	recap, err := m.periodRecap(c, u, p, now)
	if err != nil {
		return err
	}
	c.Response().Header().Set("Cache-Control", periodCacheControl(recap))
	return wire.JSON(c, http.StatusOK, recap)
}

// ─── Service (PeriodRecapService.swift) ───────────────────────────────────

func (m *Module) periodRecap(c *echo.Context, u user, p periodParams, now time.Time) (PeriodRecapResponse, error) {
	w, ok := newPeriodWindow(p.kind, p.year, p.month, u.timezone, now)
	if !ok {
		return PeriodRecapResponse{}, badRequest("invalid recap period")
	}
	in := newPeriodInputs()
	if !w.isFuture {
		var err error
		if in, err = m.periodRead(c, u.id, w); err != nil {
			return PeriodRecapResponse{}, err
		}
	}
	current := in.currentTotals(w)
	var themes *RecapThemesCardDTO
	if periodUnavailableReason(w, u.createdAt, in) == "" {
		card := m.themesCard(c, u.id, w, in, current, now)
		themes = &card
	}
	return buildPeriodRecap(w, u.createdAt, in, themes, now), nil
}

// themesCard never fails: a failing cache read shows the counted fallback.
// The claim is PeriodRecapThemesStore.claim; a won claim hands a job to the
// themes worker.
func (m *Module) themesCard(c *echo.Context, userID uuid.UUID, w periodWindow, in periodInputs, current dayCounts, now time.Time) RecapThemesCardDTO {
	fallback := fallbackSentence(w, in.topTags, current.memories())
	if !m.opts.RecapThemesEnabled {
		return themesPolicyCard(nil, false, false, fallback, now)
	}
	row, err := m.q.RecapThemesLoad(ctx(c), db.RecapThemesLoadParams{UserID: userID, PeriodKey: w.periodKey()})
	var rowp *db.RecapThemesLoadRow
	switch {
	case err == nil:
		rowp = &row
	case errors.Is(err, pgx.ErrNoRows):
	default:
		slog.WarnContext(ctx(c), "recap themes cache failed", "error", err)
		return themesPolicyCard(nil, false, true, fallback, now)
	}
	claimed := false
	if themesNeedGeneration(rowp, w.isComplete, now) {
		won, err := m.q.RecapThemesClaim(ctx(c), db.RecapThemesClaimParams{
			UserID: userID, PeriodKey: w.periodKey(), Now: now, Cutoff: now.Add(-themesInFlight),
		})
		if err != nil {
			slog.WarnContext(ctx(c), "recap themes cache failed", "error", err)
			return themesPolicyCard(nil, false, true, fallback, now)
		}
		claimed = len(won) > 0
		if claimed && m.opts.Themes != nil {
			m.opts.Themes.Schedule(themesJob(userID, w, in, current))
		}
	}
	return themesPolicyCard(rowp, claimed, true, fallback, now)
}

type dayCounts struct{ captures, learned, asked int }

func (d dayCounts) memories() int  { return d.captures + d.learned }
func (d dayCounts) isActive() bool { return d.captures+d.learned+d.asked > 0 }

// periodInputs is PeriodRecapBuilder.Inputs.
type periodInputs struct {
	days          map[string]dayCounts
	previous      dayCounts
	hourCounts    []int
	kinds         []RecapCountDTO
	topTags       []RecapCountDTO
	topSpace      *RecapCountDTO
	conversations int
	skillRuns     int
	topSkill      *RecapCountDTO
	achievements  []RecapAchievementDTO
	firstCapture  *RecapFirstCaptureDTO
}

func newPeriodInputs() periodInputs {
	return periodInputs{
		days: map[string]dayCounts{}, hourCounts: make([]int, 24),
		kinds: []RecapCountDTO{}, topTags: []RecapCountDTO{}, achievements: []RecapAchievementDTO{},
	}
}

func (in periodInputs) currentTotals(w periodWindow) dayCounts {
	var t dayCounts
	for _, d := range w.days {
		c := in.days[d.key]
		t.captures += c.captures
		t.learned += c.learned
		t.asked += c.asked
	}
	return t
}

// periodRead is PeriodRecapService.read.
func (m *Module) periodRead(c *echo.Context, userID uuid.UUID, w periodWindow) (periodInputs, error) {
	in := newPeriodInputs()
	daily, err := m.q.PeriodDailyCounts(ctx(c), db.PeriodDailyCountsParams{
		Tz: w.timezone, TenantID: userID, Start: w.start, EndAt: w.end, PrevStart: w.previousStart, PrevEnd: w.previousEnd,
	})
	if err != nil {
		return in, err
	}
	startKey := w.startKey()
	for _, r := range daily {
		if startKey != "" && r.Day >= startKey {
			in.days[r.Day] = dayCounts{int(r.Captures), int(r.Learned), int(r.Asked)}
		} else {
			in.previous.captures += int(r.Captures)
			in.previous.learned += int(r.Learned)
			in.previous.asked += int(r.Asked)
		}
	}
	hours, err := m.q.PeriodHourCounts(ctx(c), db.PeriodHourCountsParams{Tz: w.timezone, TenantID: userID, Start: w.start, EndAt: w.end})
	if err != nil {
		return in, err
	}
	for _, h := range hours {
		if h.Hour >= 0 && h.Hour < 24 {
			in.hourCounts[h.Hour] = int(h.Count)
		}
	}
	span := db.PeriodKindCountsParams{TenantID: userID, Start: w.start, EndAt: w.end}
	kinds, err := m.q.PeriodKindCounts(ctx(c), span)
	if err != nil {
		return in, err
	}
	for _, k := range kinds {
		in.kinds = append(in.kinds, RecapCountDTO{Label: k.Label, Count: int(k.Count)})
	}
	tags, err := m.q.PeriodTopTags(ctx(c), db.PeriodTopTagsParams(span))
	if err != nil {
		return in, err
	}
	for _, t := range tags {
		in.topTags = append(in.topTags, RecapCountDTO{Label: t.Label, Count: int(t.Count)})
	}
	space, err := m.q.PeriodTopSpace(ctx(c), db.PeriodTopSpaceParams(span))
	switch {
	case err == nil:
		in.topSpace = &RecapCountDTO{Label: space.Label, Count: int(space.Count)}
	case !errors.Is(err, pgx.ErrNoRows):
		return in, err
	}
	convs, err := m.q.PeriodConversationCount(ctx(c), db.PeriodConversationCountParams(span))
	if err != nil {
		return in, err
	}
	in.conversations = int(convs)
	skill, err := m.q.PeriodSkillRuns(ctx(c), db.PeriodSkillRunsParams(span))
	switch {
	case err == nil:
		in.skillRuns = int(skill.Total)
		in.topSkill = &RecapCountDTO{Label: skill.Label, Count: int(skill.Count)}
	case !errors.Is(err, pgx.ErrNoRows):
		return in, err
	}
	unlocks, err := m.q.UnlocksBetweenAsc(ctx(c), db.UnlocksBetweenAscParams(span))
	if err != nil {
		return in, err
	}
	for _, r := range unlocks {
		title := r.AchievementKey
		if s, ok := m.subs[r.AchievementKey]; ok {
			title = s.Label
		}
		in.achievements = append(in.achievements, RecapAchievementDTO{Key: r.AchievementKey, Title: title, UnlockedAt: wt(orZero(r.UnlockedAt))})
	}
	first, err := m.q.PeriodFirstCapture(ctx(c), db.PeriodFirstCaptureParams{Tz: w.timezone, TenantID: userID, Start: w.start, EndAt: w.end})
	switch {
	case err == nil:
		in.firstCapture = &RecapFirstCaptureDTO{MemoryId: wire.UUID{UUID: first.ID}, Date: first.Day, Title: titlePreview(first.Content)}
	case !errors.Is(err, pgx.ErrNoRows):
		return in, err
	}
	return in, nil
}

// ─── Builder (PeriodRecapBuilder.swift) ───────────────────────────────────

const (
	minMonthActivity    = 5
	minMonthActiveDays  = 3
	minYearActivity     = 20
	minYearActiveMonths = 2
	minRhythmEvents     = 10
)

// periodUnavailableReason is PeriodRecapBuilder.unavailableReason ("" = available).
func periodUnavailableReason(w periodWindow, accountCreatedAt *time.Time, in periodInputs) string {
	if w.isFuture {
		return "periodInFuture"
	}
	if accountCreatedAt != nil && !w.periodEnd.After(*accountCreatedAt) {
		return "beforeAccount"
	}
	activity, activeDays := 0, 0
	months := map[string]bool{}
	for _, d := range w.days {
		c := in.days[d.key]
		activity += c.memories() + c.asked
		if c.isActive() {
			activeDays++
			months[d.key[:7]] = true
		}
	}
	if w.kind == "month" {
		if activity < minMonthActivity || activeDays < minMonthActiveDays {
			return "notEnoughActivity"
		}
	} else if activity < minYearActivity || len(months) < minYearActiveMonths {
		return "notEnoughActivity"
	}
	return ""
}

func periodSeries(w periodWindow, days map[string]dayCounts) []RecapBucketDTO {
	by := map[string]int{}
	for _, d := range w.days {
		b := d.key
		if w.kind == "year" {
			b = d.key[:7]
		}
		by[b] += days[d.key].memories()
	}
	out := make([]RecapBucketDTO, 0, len(w.bucketKeys))
	for _, k := range w.bucketKeys {
		out = append(out, RecapBucketDTO{Key: k, Count: by[k]})
	}
	return out
}

// periodPeak: the busiest bucket (the latest wins a tie) and Monday-first
// weekday counts of memories plus messages; the first busiest weekday wins.
func periodPeak(w periodWindow, days map[string]dayCounts, series []RecapBucketDTO) *RecapPeakCardDTO {
	total := 0
	for _, b := range series {
		total += b.Count
	}
	if total <= 0 || len(series) == 0 {
		return nil
	}
	busiest := series[0]
	for _, b := range series[1:] {
		if b.Count >= busiest.Count {
			busiest = b
		}
	}
	weekdays := make([]int, 7)
	for _, d := range w.days {
		c := days[d.key]
		weekdays[d.weekday-1] += c.memories() + c.asked
	}
	best := 0
	for i, n := range weekdays {
		if n > weekdays[best] {
			best = i
		}
	}
	return &RecapPeakCardDTO{Busiest: busiest, WeekdayCounts: weekdays, BusiestWeekday: best + 1}
}

func periodStreak(w periodWindow, days map[string]dayCounts) RecapStreakCardDTO {
	bestLen := 0
	var bestStart, bestEnd *string
	run, active := 0, 0
	var runStart string
	for _, d := range w.days {
		if !days[d.key].isActive() {
			run = 0
			continue
		}
		active++
		if run == 0 {
			runStart = d.key
		}
		run++
		if run >= bestLen {
			bestLen = run
			s, e := runStart, d.key
			bestStart, bestEnd = &s, &e
		}
	}
	return RecapStreakCardDTO{LongestDays: bestLen, LongestStart: bestStart, LongestEnd: bestEnd, ActiveDays: active, TotalDays: len(w.days)}
}

func sumInts(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

// periodPersona is PeriodRecapBuilder.persona: the band with the most
// activity per hour (the first of equal bands wins), balanced below ten events.
func periodPersona(hours []int) string {
	if len(hours) != 24 || sumInts(hours) < minRhythmEvents {
		return "balanced"
	}
	bands := []struct {
		name  string
		hours []int
	}{
		{"earlyBird", []int{5, 6, 7, 8, 9}},
		{"daytime", []int{10, 11, 12, 13, 14, 15, 16}},
		{"evening", []int{17, 18, 19, 20, 21}},
		{"nightOwl", []int{22, 23, 0, 1, 2, 3, 4}},
	}
	best, bestScore := "balanced", -1.0
	for _, b := range bands {
		sum := 0
		for _, h := range b.hours {
			sum += hours[h]
		}
		score := float64(sum) / float64(len(b.hours))
		if score > bestScore {
			best, bestScore = b.name, score
		}
	}
	return best
}

func periodRhythm(hours []int) *RecapRhythmCardDTO {
	if len(hours) != 24 || sumInts(hours) < minRhythmEvents {
		return nil
	}
	peak := 0
	for i, n := range hours {
		if n > hours[peak] {
			peak = i
		}
	}
	return &RecapRhythmCardDTO{HourCounts: append([]int{}, hours...), PeakHour: peak, Persona: RecapPersona(periodPersona(hours))}
}

func periodName(w periodWindow) string {
	if w.month == 0 {
		return strconv.Itoa(w.year)
	}
	return fmt.Sprintf("%s %d", time.Month(w.month).String(), w.year)
}

func periodShareText(w periodWindow, memories, asked, longestStreak int) string {
	parts := []string{plural(memories, "memory", "memories")}
	if asked > 0 {
		parts = append(parts, plural(asked, "question asked", "questions asked"))
	}
	if longestStreak > 1 {
		parts = append(parts, fmt.Sprintf("a %d-day streak", longestStreak))
	}
	soFar := ""
	if !w.isComplete {
		soFar = " so far"
	}
	return fmt.Sprintf("My %s%s on LuminaVault: %s.", periodName(w), soFar, strings.Join(parts, ", "))
}

func fallbackSentence(w periodWindow, topTags []RecapCountDTO, memories int) string {
	span := "A month"
	if w.kind == "year" {
		span = "A year"
	}
	tags := make([]string, 0, 3)
	for _, t := range topTags[:min(3, len(topTags))] {
		tags = append(tags, t.Label)
	}
	switch len(tags) {
	case 0:
		noun := "memories"
		if memories == 1 {
			noun = "memory"
		}
		return fmt.Sprintf("%s of %d %s, all in one place.", span, memories, noun)
	case 1:
		return fmt.Sprintf("%s of %s.", span, tags[0])
	default:
		return fmt.Sprintf("%s of %s and %s.", span, strings.Join(tags[:len(tags)-1], ", "), tags[len(tags)-1])
	}
}

// buildPeriodRecap is PeriodRecapBuilder.build.
func buildPeriodRecap(w periodWindow, accountCreatedAt *time.Time, in periodInputs, themes *RecapThemesCardDTO, now time.Time) PeriodRecapResponse {
	current := in.currentTotals(w)
	streak := periodStreak(w, in.days)
	out := PeriodRecapResponse{
		Period: RecapPeriodKind(w.kind), Year: w.year, Start: w.startKey(), End: w.endKey(), Timezone: w.timezone,
		IsComplete: w.isComplete, Order: []RecapCardKind{},
		ShareText:   periodShareText(w, current.memories(), current.asked, streak.LongestDays),
		GeneratedAt: wt(now),
	}
	if w.month != 0 {
		month := w.month
		out.Month = &month
	}
	if reason := periodUnavailableReason(w, accountCreatedAt, in); reason != "" {
		r := RecapUnavailableReason(reason)
		out.UnavailableReason = &r
		return out
	}
	out.Available = true
	series := periodSeries(w, in.days)
	out.Captures = &RecapCapturesCardDTO{
		Captures: current.captures, Learned: current.learned,
		PreviousCaptures: in.previous.captures, PreviousLearned: in.previous.learned,
		Series: series, ByKind: in.kinds, FirstCapture: in.firstCapture,
	}
	out.Peak = periodPeak(w, in.days, series)
	out.Rhythm = periodRhythm(in.hourCounts)
	if len(in.topTags) > 0 || in.topSpace != nil {
		out.Topics = &RecapTopicsCardDTO{TopTags: in.topTags, TopSpace: in.topSpace}
	}
	if current.asked+in.conversations > 0 {
		out.Chats = &RecapChatsCardDTO{Conversations: in.conversations, MessagesSent: current.asked, PreviousMessagesSent: in.previous.asked}
	}
	if in.skillRuns > 0 {
		out.Skills = &RecapSkillsCardDTO{Runs: in.skillRuns, TopSkill: in.topSkill}
	}
	out.Streak = &streak
	if len(in.achievements) > 0 {
		out.Achievements = &RecapAchievementsCardDTO{Unlocked: len(in.achievements), Items: in.achievements[:min(5, len(in.achievements))]}
	}
	out.Themes = themes
	order := []RecapCardKind{"intro", "captures"}
	for _, o := range []struct {
		kind RecapCardKind
		on   bool
	}{
		{"peak", out.Peak != nil},
		{"rhythm", out.Rhythm != nil},
		{"topics", out.Topics != nil},
		{"chats", out.Chats != nil},
		{"skills", out.Skills != nil},
		{"streak", true},
		{"achievements", out.Achievements != nil},
		{"themes", themes != nil},
	} {
		if o.on {
			order = append(order, o.kind)
		}
	}
	out.Order = append(order, "summary")
	return out
}

// ─── Themes policy (PeriodRecapThemes.swift) ──────────────────────────────

const (
	themesInFlight      = 120 * time.Second
	themesMaxAttempts   = 3
	themesRetryFallback = time.Hour
	themesRefreshAfter  = 24 * time.Hour
	themesAgeInfinity   = time.Duration(1<<63 - 1)
)

func themesInFlightRow(row *db.RecapThemesLoadRow, now time.Time) bool {
	return row != nil && row.RequestedAt != nil && now.Sub(*row.RequestedAt) < themesInFlight
}

func themesNeedGeneration(row *db.RecapThemesLoadRow, isComplete bool, now time.Time) bool {
	if row == nil {
		return true
	}
	if themesInFlightRow(row, now) {
		return false
	}
	age := func(t *time.Time) time.Duration {
		if t == nil {
			return themesAgeInfinity
		}
		return now.Sub(*t)
	}
	switch row.Status {
	case "ready":
		return !isComplete && age(row.GeneratedAt) > themesRefreshAfter
	case "fallback":
		return (row.Attempts < themesMaxAttempts && age(row.GeneratedAt) > themesRetryFallback) || age(row.GeneratedAt) > themesRefreshAfter
	default:
		return row.Attempts < themesMaxAttempts || age(row.RequestedAt) > themesRefreshAfter
	}
}

func themesPolicyCard(row *db.RecapThemesLoadRow, claimed, enabled bool, fallback string, now time.Time) RecapThemesCardDTO {
	card := RecapThemesCardDTO{FallbackSentence: fallback, Themes: []string{}}
	if !enabled {
		card.Status = "disabled"
		return card
	}
	if row != nil && row.Status == "ready" && row.Headline != nil && row.Sentence != nil {
		card.Status = "ready"
		card.Headline, card.Sentence = row.Headline, row.Sentence
		if row.Themes != nil {
			card.Themes = row.Themes
		}
		return card
	}
	if claimed || (row != nil && row.Status == "pending" && themesInFlightRow(row, now)) {
		card.Status = "pending"
		return card
	}
	card.Status = "fallback"
	return card
}

// themesJob is the PeriodRecapThemesJob PeriodRecapService schedules.
func themesJob(userID uuid.UUID, w periodWindow, in periodInputs, current dayCounts) ThemesJob {
	input := ThemesInput{
		PeriodName: periodName(w), Kind: w.kind, Memories: current.memories(), Asked: current.asked,
		TopTags: in.topTags,
	}
	if in.topSpace != nil {
		label := in.topSpace.Label
		input.TopSpace = &label
	}
	if r := periodRhythm(in.hourCounts); r != nil {
		persona := string(r.Persona)
		input.Persona = &persona
	}
	if p := periodPeak(w, in.days, periodSeries(w, in.days)); p != nil {
		busiest := p.Busiest.Key
		input.Busiest = &busiest
	}
	return ThemesJob{UserID: userID, PeriodKey: w.periodKey(), Start: w.start, End: w.end, Input: input}
}
