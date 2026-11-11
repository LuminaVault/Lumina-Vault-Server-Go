package skills

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Ports of SkillOutputsControllerTests and JobIntentClassifier/JobOrigin cases
// that need no database.

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestStreakCountsBackFromToday(t *testing.T) {
	today := day("2026-03-10").Add(15 * time.Hour)
	require.Equal(t, 3, streakLength([]time.Time{day("2026-03-10"), day("2026-03-09"), day("2026-03-08"), day("2026-03-06")}, today))
}

func TestStreakStartsYesterdayWhenTodayIsEmpty(t *testing.T) {
	today := day("2026-03-10").Add(time.Hour)
	require.Equal(t, 2, streakLength([]time.Time{day("2026-03-09"), day("2026-03-08")}, today))
}

func TestStreakBrokenByAGap(t *testing.T) {
	require.Equal(t, 0, streakLength([]time.Time{day("2026-03-08")}, day("2026-03-10")))
	require.Equal(t, 0, streakLength(nil, day("2026-03-10")))
}

func TestHeadlineIsFirstNonEmptyLineWithoutHeadingMarks(t *testing.T) {
	require.Equal(t, "Good morning", outputHeadline("\n\n  ## Good morning  \nrest", "fb"))
	require.Equal(t, "fb", outputHeadline("####\n#\n   \n", "fb"))
	require.Equal(t, "fb", outputHeadline("", "fb"))
}

func TestHeadlineCapsAt120Characters(t *testing.T) {
	long := ""
	for range 130 {
		long += "é"
	}
	h := outputHeadline(long, "fb")
	require.Equal(t, 120, len([]rune(h)))
	require.True(t, len(h) > 0 && h[len(h)-3:] == "…")
}

func TestHeadlineDoesNotBreakOnCRLF(t *testing.T) {
	// "\r\n" is one Character in Swift, never equal to "\n".
	require.Equal(t, "a\r\nb", outputHeadline("a\r\nb", "fb"))
}

func TestKindFromName(t *testing.T) {
	for name, want := range map[string]string{
		"contradiction-detector": "contradiction_finding",
		"pattern-detector":       "pattern_finding",
		"health-correlate":       "generic",
		"Correlation Finder":     "correlation_insight",
		"capture-enrich":         "capture_enriched",
		"weekly-memo":            "weekly_memo",
		"daily-brief":            "daily_brief",
		"job-digest":             "daily_brief",
		"anything":               "generic",
	} {
		require.Equal(t, want, outputKind(name), name)
	}
}

func TestFailedRowWithoutMarkdownHeadlinesTheFailure(t *testing.T) {
	msg := "boom"
	o := output(feedRow{name: "x", source: "vault", status: "error", err: &msg})
	require.Equal(t, "x failed", o.Headline)
	require.Equal(t, "boom", o.Body)
	md := "partial"
	o = output(feedRow{name: "x", source: "weird", status: "error", err: &msg, markdown: &md})
	require.Equal(t, "partial", o.Headline)
	require.Equal(t, "builtin", o.Source)
}

func TestParseProposal(t *testing.T) {
	p := parseProposal("```json\n{\"isJob\": true, \"title\": \"Weather\", \"cron\": \"0 8 * * *\"}\n```")
	require.True(t, p.IsJob)
	require.Equal(t, "Weather", *p.Title)
	require.Nil(t, p.Spec)
	for _, bad := range []string{`{"isJob": false, "title": "x"}`, `{}`, `not json`, `{"isJob": "yes"}`, `{"isJob": true, "title": 5}`, "```"} {
		require.False(t, parseProposal(bad).IsJob, bad)
	}
	b, err := json.Marshal(parseProposal(`{"isJob": false, "title": "x"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"isJob":false}`, string(b))
}

func TestDecodeBlocks(t *testing.T) {
	s := func(v string) *string { return &v }
	require.Nil(t, decodeBlocks(nil))
	require.Nil(t, decodeBlocks(s(`null`)))
	require.Nil(t, decodeBlocks(s(`[{"text":"no type"}]`)))
	require.Nil(t, decodeBlocks(s(`[{"type":"lineChart","series":[{"name":"s","points":[{"x":"a"}]}]}]`)))
	empty := decodeBlocks(s(`[]`))
	require.NotNil(t, empty)
	require.Empty(t, *empty)
	got := decodeBlocks(s(`[{"type":"list","items":[],"extra":1},{"type":"lineChart","series":[{"name":"s","points":[{"x":"a","y":2.5},{"x":"b","y":1}]}]}]`))
	b, err := json.Marshal(got)
	require.NoError(t, err)
	require.JSONEq(t, `[{"type":"list","items":[]},{"type":"lineChart","series":[{"name":"s","points":[{"x":"a","y":2.5},{"x":"b","y":1}]}]}]`, string(b))
}

func TestOriginKindUnknownForAnythingElse(t *testing.T) {
	require.Equal(t, "card", originKind("card"))
	require.Equal(t, "conversation", originKind("conversation"))
	require.Equal(t, "unknown", originKind("mystery"))
}
