package dashboard

import "time"

const (
	periodToday     = "today"
	periodYesterday = "yesterday"
	periodWeek      = "week"
	periodMonth     = "month"
)

// window is DashboardPeriodQuery.Window.
type window struct {
	Start, End, PreviousStart, PreviousEnd time.Time
	Trunc                                  string
}

// parsePeriod is DashboardPeriodQuery.parsePeriod: unknown → today.
func parsePeriod(raw string) string {
	switch raw {
	case periodYesterday, periodWeek, periodMonth:
		return raw
	}
	return periodToday
}

// periodWindow is DashboardPeriodQuery.window.
func periodWindow(period string, now time.Time) window {
	today := startOfDayUTC(now)
	switch period {
	case periodYesterday:
		start := today.AddDate(0, 0, -1)
		return window{Start: start, End: today, PreviousStart: start.AddDate(0, 0, -1), PreviousEnd: start, Trunc: "hour"}
	case periodWeek:
		start := today.AddDate(0, 0, -6)
		return window{Start: start, End: now, PreviousStart: start.AddDate(0, 0, -7), PreviousEnd: start, Trunc: "day"}
	case periodMonth:
		start := today.AddDate(0, 0, -29)
		return window{Start: start, End: now, PreviousStart: start.AddDate(0, 0, -30), PreviousEnd: start, Trunc: "day"}
	default:
		return window{Start: today, End: now, PreviousStart: today.AddDate(0, 0, -1), PreviousEnd: today, Trunc: "hour"}
	}
}

// rangeDays is AnalyticsRange.days.
func rangeDays(r string) int {
	switch r {
	case "7d":
		return 7
	case "90d":
		return 90
	default:
		return 30
	}
}

// parseRange is AnalyticsController.range: unknown → 30d.
func parseRange(raw string) string {
	if raw == "7d" || raw == "90d" {
		return raw
	}
	return "30d"
}

// analyticsPeriodStart is AnalyticsController.periodStart.
func analyticsPeriodStart(r string, now time.Time) time.Time {
	return startOfDayUTC(now).AddDate(0, 0, -(rangeDays(r) - 1))
}
