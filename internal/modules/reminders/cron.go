package reminders

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
)

// errBadCron is any CronExpression.ParseError; the route only reports that
// the expression is invalid.
var errBadCron = errors.New("invalid cron expression")

// ParseCron is the validating half of Swift's CronExpression.init
// (Sources/App/Skills/CronExpression.swift): five whitespace-separated fields,
// each `*`, a literal, a comma list, a range, or a stepped range, within
// minute 0-59, hour 0-23, dom 1-31, mon 1-12, dow 0-6. Only acceptance
// matters here: ReminderScheduler (Swift) evaluates the schedule.
//
// Swift's split(separator:) drops empty pieces, so "1,,2", "*/" and "/5" are
// accepted exactly as Swift accepts them. A field of a bare "/" crashes the
// Swift process (index out of range); Go rejects it instead.
func ParseCron(raw string) error {
	tokens := strings.FieldsFunc(raw, unicode.IsSpace)
	if len(tokens) != 5 {
		return errBadCron
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i, tok := range tokens {
		if !validField(tok, bounds[i][0], bounds[i][1]) {
			return errBadCron
		}
	}
	return nil
}

func validField(raw string, lo, hi int) bool {
	if raw == "*" {
		return true
	}
	for _, chunk := range splitNonEmpty(raw, ",") {
		stepParts := splitNonEmpty(chunk, "/")
		if len(stepParts) == 0 || len(stepParts) > 2 {
			return false
		}
		if len(stepParts) == 2 {
			step, err := strconv.Atoi(stepParts[1])
			if err != nil || step < 1 {
				return false
			}
		}
		body := stepParts[0]
		switch {
		case body == "*":
		case strings.Contains(body, "-"):
			parts := splitNonEmpty(body, "-")
			if len(parts) != 2 {
				return false
			}
			from, err1 := strconv.Atoi(parts[0])
			to, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil {
				return false
			}
			if from < lo || from > hi || to < lo || to > hi || from > to {
				return false
			}
		default:
			v, err := strconv.Atoi(body)
			if err != nil || v < lo || v > hi {
				return false
			}
		}
	}
	return true
}

// splitNonEmpty is Swift's String.split(separator:) with its default
// omittingEmptySubsequences: true.
func splitNonEmpty(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
