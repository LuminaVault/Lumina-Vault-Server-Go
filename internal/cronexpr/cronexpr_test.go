package cronexpr

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// CronExpressionTests: pure-function ports. Every case pins an explicit instant
// and zone, as the Swift suite does.

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func at(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, loc)
}

func mustParse(t *testing.T, raw string) Expression {
	t.Helper()
	e, ok := Parse(raw)
	require.True(t, ok, raw)
	return e
}

func values(f field) []int {
	out := []int{}
	for v := range 60 {
		if f.values[v] {
			out = append(out, v)
		}
	}
	return out
}

// CronExpressionTests: "parses 5 fields".
func TestParsesFiveFields(t *testing.T) {
	e := mustParse(t, "0 7 * * *")
	require.Equal(t, []int{0}, values(e.minute))
	require.Equal(t, []int{7}, values(e.hour))
	require.True(t, e.dayOfMonth.wildcard)
	require.True(t, e.month.wildcard)
	require.True(t, e.dayOfWeek.wildcard)
}

// CronExpressionTests: "rejects wrong field count".
func TestRejectsWrongFieldCount(t *testing.T) {
	_, ok := Parse("0 7 * *")
	require.False(t, ok)
	_, ok = Parse("")
	require.False(t, ok)
}

// CronExpressionTests: "daily 7am Lisbon matches 0700 local".
func TestDaily7amLisbonMatches0700Local(t *testing.T) {
	lisbon := zone(t, "Europe/Lisbon")
	e := mustParse(t, "0 7 * * *")
	require.True(t, e.Matches(at(2026, 5, 11, 7, 0, lisbon), lisbon))
	require.False(t, e.Matches(at(2026, 5, 11, 6, 59, lisbon), lisbon))
}

// CronExpressionTests: "tz aware 0700 lisbon does not match 0700 utc when offset
// differs". May 2026: Lisbon is UTC+1, so 07:00 there is 06:00 UTC.
func TestTZAware0700LisbonDoesNotMatchUTC(t *testing.T) {
	lisbon := zone(t, "Europe/Lisbon")
	e := mustParse(t, "0 7 * * *")
	instant := at(2026, 5, 11, 7, 0, lisbon)
	require.True(t, e.Matches(instant, lisbon))
	require.False(t, e.Matches(instant, time.UTC))
}

// CronExpressionTests: "step expression every 15 minutes".
func TestStepExpressionEvery15Minutes(t *testing.T) {
	e := mustParse(t, "*/15 * * * *")
	for _, m := range []int{0, 15, 30, 45} {
		require.True(t, e.Matches(at(2026, 5, 11, 7, m, time.UTC), time.UTC), m)
	}
	for _, m := range []int{1, 14, 16, 29} {
		require.False(t, e.Matches(at(2026, 5, 11, 7, m, time.UTC), time.UTC), m)
	}
}

// CronExpressionTests: "range expression office hours".
func TestRangeExpressionOfficeHours(t *testing.T) {
	e := mustParse(t, "0 9-17 * * *")
	for _, h := range []int{9, 12, 17} {
		require.True(t, e.Matches(at(2026, 5, 11, h, 0, time.UTC), time.UTC), h)
	}
	for _, h := range []int{8, 18} {
		require.False(t, e.Matches(at(2026, 5, 11, h, 0, time.UTC), time.UTC), h)
	}
}

// CronExpressionTests: "dow only weekly memo monday 9am". 2026-05-11 is a Monday.
func TestDOWOnlyWeeklyMemoMonday9am(t *testing.T) {
	e := mustParse(t, "0 9 * * 1")
	require.True(t, e.Matches(at(2026, 5, 11, 9, 0, time.UTC), time.UTC))
	require.False(t, e.Matches(at(2026, 5, 12, 9, 0, time.UTC), time.UTC))
}

// CronExpressionTests: "dom or dow semantics". POSIX: with both restricted, either
// matching fires, so "0 0 1 * 1" runs every 1st and every Monday.
func TestDOMOrDOWSemantics(t *testing.T) {
	e := mustParse(t, "0 0 1 * 1")
	require.True(t, e.Matches(at(2026, 6, 1, 0, 0, time.UTC), time.UTC))   // Monday the 1st
	require.True(t, e.Matches(at(2026, 7, 1, 0, 0, time.UTC), time.UTC))   // Wednesday the 1st
	require.True(t, e.Matches(at(2026, 5, 11, 0, 0, time.UTC), time.UTC))  // a Monday
	require.False(t, e.Matches(at(2026, 5, 12, 0, 0, time.UTC), time.UTC)) // neither
}

// CronExpressionTests: "comma list of minutes".
func TestCommaListOfMinutes(t *testing.T) {
	e := mustParse(t, "0,30 * * * *")
	require.True(t, e.Matches(at(2026, 5, 11, 8, 0, time.UTC), time.UTC))
	require.True(t, e.Matches(at(2026, 5, 11, 8, 30, time.UTC), time.UTC))
	require.False(t, e.Matches(at(2026, 5, 11, 8, 15, time.UTC), time.UTC))
}

// CronExpressionTests: "out of range integer rejected".
func TestOutOfRangeIntegerRejected(t *testing.T) {
	for _, raw := range []string{
		"60 7 * * *",
		"0 24 * * *",
		"0 7 0 * *", // day-of-month starts at 1
		"0 7 * * 7", // day-of-week ends at 6
	} {
		_, ok := Parse(raw)
		require.False(t, ok, raw)
	}
}
