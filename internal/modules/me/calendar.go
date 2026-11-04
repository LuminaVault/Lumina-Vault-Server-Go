package me

import (
	"fmt"
	"lumina/internal/swiftstr"
	"strings"
	"time"
	"unicode"
)

// zone is `TimeZone(identifier: name) ?? UTC`. ok reports whether name was valid.
func zone(name string) (loc *time.Location, ok bool) {
	if name == "" || name == "Local" {
		return time.UTC, false
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC, false
	}
	return loc, true
}

// zoneName is the identifier Swift passes on to SQL: the stored one when it is
// valid, "UTC" otherwise.
func zoneName(name string) string {
	if _, ok := zone(name); ok {
		return name
	}
	return "UTC"
}

// startOfDay is Calendar.startOfDay(for:) in loc.
func startOfDay(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// addDays is calendar.date(byAdding: .day, value: n, to: midnight).
func addDays(midnight time.Time, n int, loc *time.Location) time.Time {
	l := midnight.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day()+n, l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), loc)
}

// addMonths is calendar.date(byAdding: .month, value: n, to:): the day is
// clamped to the end of a shorter month (31 March − 1 month = 28 February).
func addMonths(t time.Time, n int, loc *time.Location) time.Time {
	l := t.In(loc)
	first := time.Date(l.Year(), l.Month()+time.Month(n), 1, 0, 0, 0, 0, loc)
	last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, loc).Day()
	return time.Date(first.Year(), first.Month(), min(l.Day(), last), l.Hour(), l.Minute(), l.Second(), l.Nanosecond(), loc)
}

// dayKey is `yyyy-MM-dd` of t in loc.
func dayKey(t time.Time, loc *time.Location) string {
	l := t.In(loc)
	return fmt.Sprintf("%04d-%02d-%02d", l.Year(), int(l.Month()), l.Day())
}

// localDay is ProactivePushBudget.localDay: today in timezone, UTC when unknown.
func localDay(now time.Time, timezone string) string {
	loc, _ := zone(timezone)
	return dayKey(now, loc)
}

// isCFWhitespace is CharacterSet.whitespaces: Zs, tab and U+200B (see swiftstr).
func isCFWhitespace(r rune) bool {
	return r == '\t' || r == 0x200B || unicode.Is(unicode.Zs, r)
}

// isSwiftNewline is Character.isNewline.
func isSwiftNewline(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// titlePreview is MeTodayService.titlePreview: the first non-empty line,
// trimmed of spaces, at most 80 characters (77 + "...").
func titlePreview(content string) string {
	first := content
	for _, part := range strings.FieldsFunc(content, isSwiftNewline) {
		first = part
		break
	}
	trimmed := strings.TrimFunc(first, isCFWhitespace)
	if swiftstr.Count(trimmed) <= 80 {
		return trimmed
	}
	return swiftstr.Prefix(trimmed, 77) + "..."
}

// relatedSnippet is MemoryController.relatedSnippet: whitespace runs folded
// to one space, at most 160 characters (159 + "…").
func relatedSnippet(content string) string {
	flat := strings.Join(strings.FieldsFunc(content, unicode.IsSpace), " ")
	if swiftstr.Count(flat) <= 160 {
		return flat
	}
	return swiftstr.Prefix(flat, 159) + "…"
}

// plural is the "\(n) \(n == 1 ? one : many)" helper several Swift services use.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
