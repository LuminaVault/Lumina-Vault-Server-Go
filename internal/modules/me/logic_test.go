package me

import (
	"errors"
	"fmt"
	"lumina/internal/modules/me/db"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func iso(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return v
}

func window(t *testing.T, kind string, year, month int, tz, now string) periodWindow {
	t.Helper()
	w, ok := newPeriodWindow(kind, year, month, tz, iso(t, now))
	require.True(t, ok)
	return w
}

const octNow = "2026-10-16T12:00:00Z"

func requireBadRequest(t *testing.T, err error) {
	t.Helper()
	var we *wire.Error
	require.True(t, errors.As(err, &we), "%v", err)
	require.Equal(t, http.StatusBadRequest, we.Status)
}

// ─── PeriodRecapLogicTests: window ─────────────────────────────────────────

func TestRunningYearIsClampedToTodayAndComparedWithTheSameStretch(t *testing.T) {
	w := window(t, "year", 2026, 0, "UTC", octNow)
	require.True(t, w.start.Equal(iso(t, "2026-01-01T00:00:00Z")))
	require.True(t, w.end.Equal(iso(t, "2026-10-17T00:00:00Z")))
	require.True(t, w.previousStart.Equal(iso(t, "2025-01-01T00:00:00Z")))
	require.True(t, w.previousEnd.Equal(iso(t, "2025-10-17T00:00:00Z")))
	require.False(t, w.isComplete)
	require.False(t, w.isFuture)
	require.Len(t, w.days, 289)
	require.Equal(t, "2026-01-01", w.startKey())
	require.Equal(t, "2026-10-16", w.endKey())
	want := make([]string, 0, 10)
	for m := 1; m <= 10; m++ {
		want = append(want, fmt.Sprintf("2026-%02d", m))
	}
	require.Equal(t, want, w.bucketKeys)
	require.Equal(t, "2026", w.periodKey())
}

func TestFinishedMonthCoversEveryDayAndComparesWithThePreviousMonth(t *testing.T) {
	w := window(t, "month", 2026, 2, "UTC", octNow)
	require.True(t, w.isComplete)
	require.Len(t, w.days, 28)
	require.Equal(t, "2026-02-01", w.bucketKeys[0])
	require.Equal(t, "2026-02-28", w.bucketKeys[len(w.bucketKeys)-1])
	require.True(t, w.previousStart.Equal(iso(t, "2026-01-01T00:00:00Z")))
	require.True(t, w.previousEnd.Equal(iso(t, "2026-02-01T00:00:00Z")))
	require.Equal(t, "2026-02", w.periodKey())
}

func TestDaysAreLocalAndSurviveDST(t *testing.T) {
	// Lisbon leaves summer time on 25 October 2026.
	w := window(t, "month", 2026, 10, "Europe/Lisbon", "2026-11-02T12:00:00Z")
	require.Len(t, w.days, 31)
	require.True(t, w.start.Equal(iso(t, "2026-09-30T23:00:00Z")))
	require.True(t, w.end.Equal(iso(t, "2026-11-01T00:00:00Z")))
	keys := map[string]bool{}
	for _, d := range w.days {
		keys[d.key] = true
	}
	require.Len(t, keys, 31)
}

func TestWeekdaysAreISOMondayFirst(t *testing.T) {
	w := window(t, "month", 2026, 6, "UTC", octNow)
	// 1 June 2026 is a Monday, 7 June a Sunday.
	require.Equal(t, 1, w.days[0].weekday)
	require.Equal(t, 7, w.days[6].weekday)
}

func TestUnknownTimezoneFallsBackAndBadPeriodsAreRefused(t *testing.T) {
	w := window(t, "year", 2026, 0, "Mars/Olympus", octNow)
	require.Equal(t, "UTC", w.timezone)
	now := time.Now()
	for _, c := range []struct {
		kind        string
		year, month int
	}{{"month", 2026, 13}, {"month", 2026, 0}, {"year", 2026, 1}, {"year", 1999, 0}} {
		_, ok := newPeriodWindow(c.kind, c.year, c.month, "UTC", now)
		require.False(t, ok, "%+v", c)
	}
}

func TestFuturePeriodHasNoDays(t *testing.T) {
	w := window(t, "year", 2027, 0, "UTC", octNow)
	require.True(t, w.isFuture)
	require.Empty(t, w.days)
	require.True(t, w.start.Equal(w.end))
}

// ─── PeriodRecapLogicTests: builder ────────────────────────────────────────

// activeMonth has two runs: 2–4 and 10–12 (three days each; the later wins).
func activeMonth(t *testing.T) (periodWindow, periodInputs) {
	t.Helper()
	w := window(t, "month", 2026, 3, "UTC", octNow)
	in := newPeriodInputs()
	for _, day := range []int{2, 3, 4, 10, 11, 12} {
		in.days[fmt.Sprintf("2026-03-%02d", day)] = dayCounts{captures: 2, learned: 1, asked: 1}
	}
	in.days["2026-03-20"] = dayCounts{captures: 9}
	return w, in
}

func TestSeriesFillsEmptyBucketsAndFindsTheBusiest(t *testing.T) {
	w, in := activeMonth(t)
	series := periodSeries(w, in.days)
	require.Len(t, series, 31)
	require.Equal(t, RecapBucketDTO{Key: "2026-03-01", Count: 0}, series[0])
	require.Equal(t, RecapBucketDTO{Key: "2026-03-02", Count: 3}, series[1])
	peak := periodPeak(w, in.days, series)
	require.NotNil(t, peak)
	require.Equal(t, RecapBucketDTO{Key: "2026-03-20", Count: 9}, peak.Busiest)
	// 20 March 2026 is a Friday.
	require.Equal(t, 5, peak.BusiestWeekday)
	require.Equal(t, 6*4+9, sumInts(peak.WeekdayCounts))
}

func TestYearSeriesBucketsByMonth(t *testing.T) {
	w := window(t, "year", 2026, 0, "UTC", "2026-03-10T12:00:00Z")
	series := periodSeries(w, map[string]dayCounts{
		"2026-01-05": {captures: 1},
		"2026-01-20": {learned: 2},
		"2026-03-01": {captures: 4, asked: 5},
	})
	require.Equal(t, []RecapBucketDTO{{Key: "2026-01", Count: 3}, {Key: "2026-02", Count: 0}, {Key: "2026-03", Count: 4}}, series)
}

func TestLongestStreakIsInsideThePeriodAndTheLatestTieWins(t *testing.T) {
	w, in := activeMonth(t)
	s := periodStreak(w, in.days)
	require.Equal(t, 3, s.LongestDays)
	require.Equal(t, "2026-03-10", *s.LongestStart)
	require.Equal(t, "2026-03-12", *s.LongestEnd)
	require.Equal(t, 7, s.ActiveDays)
	require.Equal(t, 31, s.TotalDays)
}

func TestNoActivityMeansNoStreak(t *testing.T) {
	w := window(t, "month", 2026, 3, "UTC", octNow)
	require.Equal(t, RecapStreakCardDTO{TotalDays: 31}, periodStreak(w, map[string]dayCounts{}))
}

func TestPersonaIsTheBusiestBandPerHour(t *testing.T) {
	hours := make([]int, 24)
	hours[23], hours[1], hours[12] = 6, 4, 9
	require.Equal(t, "nightOwl", periodPersona(hours))
	hours[12] = 30
	require.Equal(t, "daytime", periodPersona(hours))
	few := make([]int, 24)
	few[6] = 9
	require.Equal(t, "balanced", periodPersona(few))
	require.Nil(t, periodRhythm(few))
	few[7] = 1
	rhythm := periodRhythm(few)
	require.NotNil(t, rhythm)
	require.Equal(t, RecapPersona("earlyBird"), rhythm.Persona)
	require.Equal(t, 6, rhythm.PeakHour)
}

func TestEligibilityExplainsWhyThereIsNoRecap(t *testing.T) {
	empty := newPeriodInputs()
	require.Equal(t, "periodInFuture", periodUnavailableReason(window(t, "year", 2027, 0, "UTC", octNow), nil, empty))
	created := iso(t, "2026-01-03T00:00:00Z")
	require.Equal(t, "beforeAccount", periodUnavailableReason(window(t, "year", 2024, 0, "UTC", octNow), &created, empty))
	month, in := activeMonth(t)
	require.Empty(t, periodUnavailableReason(month, nil, in))
	thin := newPeriodInputs()
	thin.days["2026-03-02"] = dayCounts{captures: 9}
	thin.days["2026-03-03"] = dayCounts{captures: 9}
	require.Equal(t, "notEnoughActivity", periodUnavailableReason(month, nil, thin))
	// A year needs activity in two months.
	year := window(t, "year", 2026, 0, "UTC", octNow)
	one := newPeriodInputs()
	one.days["2026-03-02"] = dayCounts{captures: 30}
	require.Equal(t, "notEnoughActivity", periodUnavailableReason(year, nil, one))
	one.days["2026-04-02"] = dayCounts{captures: 1}
	require.Empty(t, periodUnavailableReason(year, nil, one))
}

func TestDeckOnlyHoldsCardsWithSomethingToSay(t *testing.T) {
	w, in := activeMonth(t)
	themes := &RecapThemesCardDTO{Status: "pending", FallbackSentence: "x"}
	recap := buildPeriodRecap(w, nil, in, themes, time.Now())
	require.True(t, recap.Available)
	require.Equal(t, []RecapCardKind{"intro", "captures", "peak", "chats", "streak", "themes", "summary"}, recap.Order)
	require.Equal(t, 21, recap.Captures.Captures)
	require.Equal(t, 6, recap.Captures.Learned)
	require.Equal(t, 6, recap.Chats.MessagesSent)
	require.Nil(t, recap.Rhythm)
	require.Nil(t, recap.Topics)

	in.topTags = []RecapCountDTO{{Label: "swift", Count: 3}}
	in.skillRuns = 2
	in.achievements = []RecapAchievementDTO{{Key: "k", Title: "K"}}
	in.hourCounts[9] = 12
	full := buildPeriodRecap(w, nil, in, nil, time.Now())
	require.Equal(t, []RecapCardKind{"intro", "captures", "peak", "rhythm", "topics", "chats", "skills", "streak", "achievements", "summary"}, full.Order)
}

func TestUnavailableRecapCarriesNoCards(t *testing.T) {
	w := window(t, "month", 2026, 3, "UTC", octNow)
	recap := buildPeriodRecap(w, nil, newPeriodInputs(), nil, time.Now())
	require.False(t, recap.Available)
	require.Equal(t, RecapUnavailableReason("notEnoughActivity"), *recap.UnavailableReason)
	require.Empty(t, recap.Order)
	require.NotNil(t, recap.Order, "encodes as [], not null")
	require.Nil(t, recap.Captures)
}

func TestPeriodShareTextIsNumbersOnlyAndSaysSoFarWhileRunning(t *testing.T) {
	require.Equal(t, "My 2026 so far on LuminaVault: 160 memories, 1 question asked, a 12-day streak.",
		periodShareText(window(t, "year", 2026, 0, "UTC", octNow), 160, 1, 12))
	require.Equal(t, "My March 2026 on LuminaVault: 1 memory.",
		periodShareText(window(t, "month", 2026, 3, "UTC", octNow), 1, 0, 1))
}

func TestFallbackSentenceIsBuiltFromTagsOrCounts(t *testing.T) {
	year := window(t, "year", 2026, 0, "UTC", octNow)
	tags := []RecapCountDTO{{Label: "swift", Count: 1}, {Label: "running", Count: 1}, {Label: "coffee", Count: 1}, {Label: "extra", Count: 1}}
	require.Equal(t, "A year of swift, running and coffee.", fallbackSentence(year, tags, 9))
	require.Equal(t, "A year of swift.", fallbackSentence(year, tags[:1], 9))
	require.Equal(t, "A month of 9 memories, all in one place.", fallbackSentence(window(t, "month", 2026, 3, "UTC", octNow), nil, 9))
}

// ─── PeriodRecapLogicTests: themes policy ──────────────────────────────────

func themesRow(status string, attempts int32, requested, generated *time.Time) *db.RecapThemesLoadRow {
	row := &db.RecapThemesLoadRow{Status: status, Attempts: attempts, RequestedAt: requested, GeneratedAt: generated}
	if status == "ready" {
		h, s := "H", "S"
		row.Headline, row.Sentence = &h, &s
	}
	return row
}

func TestThemesGeneratedOnceRetriedWithLimitsRefreshedDaily(t *testing.T) {
	now := iso(t, octNow)
	ago := func(seconds int) *time.Time {
		v := now.Add(-time.Duration(seconds) * time.Second)
		return &v
	}
	require.True(t, themesNeedGeneration(nil, true, now))
	// In flight.
	require.False(t, themesNeedGeneration(themesRow("pending", 1, ago(30), nil), true, now))
	// Pending but abandoned.
	require.True(t, themesNeedGeneration(themesRow("pending", 1, ago(600), nil), true, now))
	require.False(t, themesNeedGeneration(themesRow("pending", 3, ago(600), nil), true, now))
	// Ready: never again for a finished period, daily for a running one.
	fresh, stale := themesRow("ready", 1, nil, ago(3600)), themesRow("ready", 1, nil, ago(90000))
	require.False(t, themesNeedGeneration(stale, true, now))
	require.False(t, themesNeedGeneration(fresh, false, now))
	require.True(t, themesNeedGeneration(stale, false, now))
	// Fallback: hourly while attempts remain, then daily.
	require.False(t, themesNeedGeneration(themesRow("fallback", 1, nil, ago(60)), true, now))
	require.True(t, themesNeedGeneration(themesRow("fallback", 1, nil, ago(4000)), true, now))
	require.False(t, themesNeedGeneration(themesRow("fallback", 3, nil, ago(4000)), true, now))
}

func TestThemesCardAlwaysHasSomethingToShow(t *testing.T) {
	now := time.Now()
	require.Equal(t, RecapThemesStatus("disabled"), themesPolicyCard(nil, false, false, "f", now).Status)
	require.Equal(t, RecapThemesStatus("pending"), themesPolicyCard(nil, true, true, "f", now).Status)
	require.Equal(t, RecapThemesStatus("fallback"), themesPolicyCard(nil, false, true, "f", now).Status)
	ready := themesPolicyCard(themesRow("ready", 1, nil, nil), true, true, "f", now)
	require.Equal(t, RecapThemesStatus("ready"), ready.Status)
	require.Equal(t, "H", *ready.Headline)
	require.Equal(t, "f", ready.FallbackSentence)
	recent := now.Add(-5 * time.Second)
	require.Equal(t, RecapThemesStatus("pending"), themesPolicyCard(themesRow("pending", 1, &recent, nil), false, true, "f", now).Status)
}

// ─── PeriodRecapLogicTests: controller ─────────────────────────────────────

func TestPeriodParamsDefaultToTheCurrentLocalYearAndMonth(t *testing.T) {
	// 31 Dec 23:30 in Los Angeles is already 1 Jan in UTC.
	now := iso(t, "2027-01-01T07:30:00Z")
	const tz = "America/Los_Angeles"
	p, err := parsePeriodParams("", "", "", tz, now)
	require.NoError(t, err)
	require.Equal(t, periodParams{kind: "year", year: 2026}, p)
	p, err = parsePeriodParams("month", "", "", tz, now)
	require.NoError(t, err)
	require.Equal(t, periodParams{kind: "month", year: 2026, month: 12}, p)
	p, err = parsePeriodParams("month", "2025", "3", tz, now)
	require.NoError(t, err)
	require.Equal(t, periodParams{kind: "month", year: 2025, month: 3}, p)
}

func TestBadPeriodParamsAreRefused(t *testing.T) {
	now := time.Now()
	for _, c := range [][3]string{{"week", "", ""}, {"year", "26", ""}, {"year", "", "3"}, {"month", "", "0"}} {
		_, err := parsePeriodParams(c[0], c[1], c[2], "UTC", now)
		requireBadRequest(t, err)
	}
}

func TestPendingThemesCardIsNeverCached(t *testing.T) {
	recap := func(complete bool, status RecapThemesStatus) PeriodRecapResponse {
		r := PeriodRecapResponse{IsComplete: complete}
		if status != "" {
			r.Themes = &RecapThemesCardDTO{Status: status}
		}
		return r
	}
	require.Equal(t, "private, no-cache", periodCacheControl(recap(true, "pending")))
	require.Equal(t, "private, max-age=3600", periodCacheControl(recap(true, "ready")))
	require.Equal(t, "private, max-age=300", periodCacheControl(recap(false, "")))
}

// ─── MeLearnedTests / MeResurfaceTests / MeRecapControllerTests: pure parts ─

func TestParseSinceAcceptsOffsetsAndFractionalSeconds(t *testing.T) {
	now := iso(t, "2026-10-18T18:00:00Z")
	got, err := parseSince("", now)
	require.NoError(t, err)
	require.True(t, got.Equal(now.Add(-7*24*time.Hour)))
	got, err = parseSince("2026-10-12T00:00:00Z", now)
	require.NoError(t, err)
	require.True(t, got.Equal(iso(t, "2026-10-12T00:00:00Z")))
	got, err = parseSince("2026-10-12T01:00:00 01:00", now)
	require.NoError(t, err)
	require.True(t, got.Equal(iso(t, "2026-10-12T00:00:00Z")))
	got, err = parseSince("2026-10-12T00:00:00.500Z", now)
	require.NoError(t, err)
	require.Equal(t, 500*time.Millisecond, time.Duration(got.Nanosecond()))
	_, err = parseSince("2026-10-19T00:00:00Z", now)
	requireBadRequest(t, err)
}

func TestLimitParsing(t *testing.T) {
	n, err := parseResurfaceLimit("")
	require.NoError(t, err)
	require.Equal(t, 3, n)
	n, err = parseResurfaceLimit("10")
	require.NoError(t, err)
	require.Equal(t, 10, n)
	for _, bad := range []string{"abc", "0"} {
		_, err = parseResurfaceLimit(bad)
		requireBadRequest(t, err)
	}
}

func TestRecapShareTextPluralisesAndSkipsZeroParts(t *testing.T) {
	require.Equal(t, "My last 7 days on LuminaVault: 12 captures, 9 things learned, 31 questions asked, a 5-day streak.",
		recapShareText(7, RecapTotalsDTO{Captures: 12, Learned: 9, Asked: 31, StreakDays: 5}))
	require.Equal(t, "My last 30 days on LuminaVault: 0 captures, 2 questions asked.",
		recapShareText(30, RecapTotalsDTO{Asked: 2}))
}

// ─── Go-only ───────────────────────────────────────────────────────────────

func TestSnippetIsOneLineOfAtMost160Characters(t *testing.T) {
	s := relatedSnippet("First line\nsecond " + strings.Repeat("word ", 80))
	require.LessOrEqual(t, len([]rune(s)), 160)
	require.NotContains(t, s, "\n")
	require.True(t, strings.HasPrefix(s, "First line second"))
}

func TestTitlePreview(t *testing.T) {
	require.Equal(t, "We ship on Tuesdays.", titlePreview("We ship on Tuesdays.\nBecause QA is Monday."))
	require.Equal(t, "Leading blank lines", titlePreview("\n\n   Leading blank lines   \nthen more"))
	long := titlePreview(strings.Repeat("x", 100))
	require.Len(t, long, 80)
	require.True(t, strings.HasSuffix(long, "..."))
}

func TestISO8601Duration(t *testing.T) {
	require.Equal(t, "PT7H30M", iso8601Duration(450))
	require.Equal(t, "PT8H", iso8601Duration(480))
	require.Equal(t, "PT45M", iso8601Duration(45))
}

func TestAddMonthsClampsToTheMonthEnd(t *testing.T) {
	loc := time.UTC
	require.Equal(t, "2026-02-28", dayKey(addMonths(iso(t, "2026-03-31T00:00:00Z"), -1, loc), loc))
	require.Equal(t, "2024-02-29", dayKey(addMonths(iso(t, "2024-03-31T00:00:00Z"), -1, loc), loc))
}

func TestDecodeCardsIsStrictButTheKindIsNot(t *testing.T) {
	cards := decodeCards(`[{"id":"a","kind":"horoscope","title":"t","body":"b","refs":[{"kind":"x","id":"1"}]}]`)
	require.Len(t, cards, 1)
	require.Equal(t, BriefCardKind("unknown"), cards[0].Kind)
	require.Equal(t, BriefCardRefKind("unknown"), cards[0].Refs[0].Kind)
	for _, bad := range []string{`{"not":"an array"}`, `null`, `[{"id":"a","kind":"weather","title":"t","body":"b"}]`, `[1]`} {
		require.Empty(t, decodeCards(bad), bad)
		require.NotNil(t, decodeCards(bad), bad)
	}
}

func TestParseBriefDate(t *testing.T) {
	now := iso(t, "2027-01-01T07:30:00Z")
	got, err := parseBriefDate("", "America/Los_Angeles", now)
	require.NoError(t, err)
	require.Equal(t, "2026-12-31", got)
	got, err = parseBriefDate("2026-10-01", "UTC", now)
	require.NoError(t, err)
	require.Equal(t, "2026-10-01", got)
	for _, bad := range []string{"2026-13-01", "yesterday", "2026-02-30", "2026-1-01", "2026/10/01"} {
		_, err = parseBriefDate(bad, "UTC", now)
		requireBadRequest(t, err)
	}
}

func TestTodayCacheTTLRunsFromGeneratedAt(t *testing.T) {
	c := newTodayCache(todayTTL)
	now := iso(t, octNow)
	tenant := [16]byte{1}
	c.put(tenant, todayEntry{etag: "e", generatedAt: now})
	_, ok := c.get(tenant, now.Add(todayTTL-time.Second))
	require.True(t, ok)
	_, ok = c.get(tenant, now.Add(todayTTL))
	require.False(t, ok)
	_, ok = c.get(tenant, now)
	require.False(t, ok, "the expired entry is gone")
}
