// Package cronexpr is Swift's CronExpression (Sources/App/Skills/CronExpression.swift):
// the parse and the per-minute match. Jobs that fire on a cron schedule use it;
// internal/modules/workflows keeps its own unexported copy of the same code.
package cronexpr

import (
	"lumina/internal/swiftstr"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// field is CronExpression.FieldSet.
type field struct {
	values   map[int]bool
	wildcard bool
}

// Expression is a parsed five-field cron expression.
type Expression struct {
	minute, hour, dayOfMonth, month, dayOfWeek field
}

// Parse is CronExpression(raw). false is any ParseError. A chunk of "/" alone makes
// Swift index an empty array and trap; here it is a parse error.
func Parse(raw string) (Expression, bool) {
	tokens := strings.FieldsFunc(swiftstr.TrimWhitespaces(raw), unicode.IsSpace)
	if len(tokens) != 5 {
		return Expression{}, false
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	fields := [5]field{}
	for i, token := range tokens {
		f, ok := parseField(token, bounds[i][0], bounds[i][1])
		if !ok {
			return Expression{}, false
		}
		fields[i] = f
	}
	return Expression{minute: fields[0], hour: fields[1], dayOfMonth: fields[2], month: fields[3], dayOfWeek: fields[4]}, true
}

func parseField(raw string, lo, hi int) (field, bool) {
	values := map[int]bool{}
	if raw == "*" {
		for v := lo; v <= hi; v++ {
			values[v] = true
		}
		return field{values: values, wildcard: true}, true
	}
	// split(separator:) drops empty pieces, so "1,,2" and "," parse.
	for _, chunk := range splitOmittingEmpty(raw, ',') {
		parts := splitOmittingEmpty(chunk, '/')
		step := 1
		switch len(parts) {
		case 1:
		case 2:
			n, err := strconv.Atoi(parts[1])
			if err != nil || n < 1 {
				return field{}, false
			}
			step = n
		default:
			return field{}, false
		}
		from, to, ok := parseRange(parts[0], lo, hi)
		if !ok {
			return field{}, false
		}
		for v := from; v <= to; v += step {
			values[v] = true
		}
	}
	return field{values: values}, true
}

func parseRange(body string, lo, hi int) (from, to int, ok bool) {
	switch {
	case body == "*":
		return lo, hi, true
	case strings.Contains(body, "-"):
		ends := splitOmittingEmpty(body, '-')
		if len(ends) != 2 {
			return 0, 0, false
		}
		first, err1 := strconv.Atoi(ends[0])
		last, err2 := strconv.Atoi(ends[1])
		if err1 != nil || err2 != nil || first < lo || first > hi || last < lo || last > hi || first > last {
			return 0, 0, false
		}
		return first, last, true
	default:
		v, err := strconv.Atoi(body)
		if err != nil || v < lo || v > hi {
			return 0, 0, false
		}
		return v, v, true
	}
}

func splitOmittingEmpty(s string, sep rune) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == sep })
}

// Matches is CronExpression.matches: t's minute in zone matches, with POSIX
// day-of-month or day-of-week (a restricted field matches alone, and two restricted
// fields match if either does).
func (c Expression) Matches(t time.Time, zone *time.Location) bool {
	local := t.In(zone)
	if !c.minute.values[local.Minute()] || !c.hour.values[local.Hour()] || !c.month.values[int(local.Month())] {
		return false
	}
	day, weekday := c.dayOfMonth.values[local.Day()], c.dayOfWeek.values[int(local.Weekday())]
	switch {
	case c.dayOfMonth.wildcard && c.dayOfWeek.wildcard:
		return true
	case c.dayOfMonth.wildcard:
		return weekday
	case c.dayOfWeek.wildcard:
		return day
	default:
		return day || weekday
	}
}
