package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Swift AnalyticsScoringTests.
func TestWeightedHealthScore(t *testing.T) {
	require.Equal(t, 73, weightedHealthScore(80, 60, 50, 100))
	require.Equal(t, 100, weightedHealthScore(200, 200, 200, 200))
	require.Equal(t, 0, weightedHealthScore(-100, -100, -100, -100))
}

// Swift VoiceUsageSummaryTests.
func TestAudioMinutes(t *testing.T) {
	require.InDelta(t, 1.0, audioMinutes(60000), 0)
	require.InDelta(t, 1.5, audioMinutes(90000), 0)
	require.InDelta(t, 0.0, audioMinutes(0), 0)
	require.InDelta(t, 0.2, audioMinutes(9000), 0, "9 s = 0.15 min rounds to a visible fraction")
}

func TestCentsFromMicros(t *testing.T) {
	require.Equal(t, 1, centsFromMicros(10000))
	require.Equal(t, 100, centsFromMicros(1_000_000))
	require.Equal(t, 0, centsFromMicros(6000), "sub-cent spend truncates")
	require.Equal(t, 0, centsFromMicros(9999))
}

// Swift DashboardPeriodQueryTests.
func TestPeriodWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	today := periodWindow(periodToday, now)
	require.Equal(t, "hour", today.Trunc)
	require.Equal(t, startOfDayUTC(now), today.Start)
	require.Equal(t, now, today.End)
	require.Equal(t, today.Start, today.PreviousEnd)

	week := periodWindow(periodWeek, now)
	require.Equal(t, startOfDayUTC(now).AddDate(0, 0, -6), week.Start)
	require.Equal(t, "day", week.Trunc)
	require.Equal(t, week.Start.AddDate(0, 0, -7), week.PreviousStart)

	yesterday := periodWindow(periodYesterday, now)
	require.Equal(t, startOfDayUTC(now), yesterday.End)
	require.Equal(t, "hour", yesterday.Trunc)

	month := periodWindow(periodMonth, now)
	require.Equal(t, startOfDayUTC(now).AddDate(0, 0, -29), month.Start)
	require.Equal(t, month.Start.AddDate(0, 0, -30), month.PreviousStart)
}

func TestParsePeriod(t *testing.T) {
	require.Equal(t, periodToday, parsePeriod(""))
	require.Equal(t, periodToday, parsePeriod("nope"))
	require.Equal(t, periodYesterday, parsePeriod("yesterday"))
}

func TestAnalyticsPeriodStart(t *testing.T) {
	now := time.Date(2026, 3, 2, 15, 4, 5, 0, time.UTC)
	require.Equal(t, time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC), analyticsPeriodStart("7d", now))
	require.Equal(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), analyticsPeriodStart("30d", now))
	require.Equal(t, "30d", parseRange("nope"))
	require.Equal(t, scopeActive, parseScope("nope"))
}

func TestTitleFromContent(t *testing.T) {
	for in, want := range map[string]string{
		"# KB Healthcheck Report\nbody": "KB Healthcheck Report",
		"  > - * Quoted  ":              "Quoted",
		"\n\r\nAfter blanks\nx":         "After blanks",
		"###":                           "###",
		"":                              "",
		"\n\n":                          "\n\n",
		"#́ combining":                  "#́ combining",
		strings.Repeat("a", 60):         strings.Repeat("a", 60),
		strings.Repeat("a", 61):         strings.Repeat("a", 60) + "…",
		strings.Repeat("é", 61):        strings.Repeat("é", 60) + "…",
	} {
		require.Equal(t, want, titleFromContent(in), "%q", in)
	}
}

func TestNodeActivity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	future, old := now.Add(time.Hour), now.Add(-100*24*time.Hour)
	require.InDelta(t, 1.0, nodeActivity(100, &future, now), 1e-12)
	require.InDelta(t, 0.4, nodeActivity(100, &old, now), 1e-12)
	require.InDelta(t, 0.0, nodeActivity(0, nil, now), 0)
	require.InDelta(t, 0.0, nodeActivity(-5, nil, now), 0)
	halfway := now.Add(-45 * 24 * time.Hour)
	require.InDelta(t, 0.3, nodeActivity(0, &halfway, now), 1e-12)
}

func TestIdentifierValidation(t *testing.T) {
	require.True(t, validRecommendationID("memory-organization_1"))
	require.True(t, validRecommendationID("résumé-日本-٣"))
	require.True(t, validRecommendationID("é"), "the combining mark belongs to the letter's Character")
	require.False(t, validRecommendationID(""))
	require.False(t, validRecommendationID("bad id"))
	require.False(t, validRecommendationID("-́"), "a marked dash is not the Character -")
	require.True(t, validRecommendationID(strings.Repeat("r", 96)))
	require.False(t, validRecommendationID(strings.Repeat("r", 97)))
	require.True(t, validIdempotencyKey(strings.Repeat("k", 128)))
	require.False(t, validIdempotencyKey(strings.Repeat("k", 129)))
	require.False(t, validIdempotencyKey("a.b"))
	require.True(t, validModelIdentity("anthropic/claude-3.5:beta_x"))
	require.False(t, validModelIdentity("prompt content is forbidden"))
}

func TestSwiftDouble(t *testing.T) {
	for v, want := range map[float64]string{
		0: "0", 1: "1", 0.5: "0.5", -1: "-1", 0.1: "0.1", 0.0001: "0.0001",
		0.00001: "1e-05", 1.5e-05: "1.5e-05", 2.0 / 3: "0.6666666666666666", 12345: "12345",
		1e16: "1e+16",
	} {
		b, err := json.Marshal(swiftDouble(v))
		require.NoError(t, err)
		require.Equal(t, want, string(b))
	}
}
