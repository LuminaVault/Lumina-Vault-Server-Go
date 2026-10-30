package kanban

import (
	"strconv"
	"strings"
	"unicode"
)

// validCron reports whether Swift's CronExpression(raw) parses
// (Skills/CronExpression.swift). Only validation is ported: promotion uses
// the parse as a gate, and the scheduler that evaluates it stays in Swift.
func validCron(raw string) bool {
	tokens := strings.FieldsFunc(trimSwiftWhitespace(raw), unicode.IsSpace)
	if len(tokens) != 5 {
		return false
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	for i, tok := range tokens {
		if !validCronField(tok, bounds[i][0], bounds[i][1]) {
			return false
		}
	}
	return true
}

func validCronField(raw string, lo, hi int) bool {
	if raw == "*" {
		return true
	}
	// Swift's split(separator:) drops empty pieces, so "1,,2" and "," parse.
	for _, chunk := range swiftSplit(raw, ',') {
		parts := swiftSplit(chunk, '/')
		switch len(parts) {
		case 1:
		case 2:
			if n, err := swiftInt(parts[1]); err != nil || n < 1 {
				return false
			}
		default:
			// Includes len 0 ("/" alone), where Swift indexes stepParts[0] of
			// an empty array and traps; the request never succeeds either way.
			return false
		}
		body := parts[0]
		switch {
		case body == "*":
		case strings.Contains(body, "-"):
			r := swiftSplit(body, '-')
			if len(r) != 2 {
				return false
			}
			from, err1 := swiftInt(r[0])
			to, err2 := swiftInt(r[1])
			if err1 != nil || err2 != nil || from < lo || from > hi || to < lo || to > hi || from > to {
				return false
			}
		default:
			v, err := swiftInt(body)
			if err != nil || v < lo || v > hi {
				return false
			}
		}
	}
	return true
}

// swiftSplit is String.split(separator:) with omittingEmptySubsequences.
func swiftSplit(s string, sep rune) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == sep })
}

// swiftInt is Int(String): optional sign, ASCII digits, no spaces.
func swiftInt(s string) (int, error) {
	return strconv.Atoi(s)
}

// trimSwiftWhitespace is trimmingCharacters(in: .whitespaces): Unicode Zs
// plus tab, not newlines.
func trimSwiftWhitespace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) })
}

// trimSwiftWhitespaceAndNewlines is trimmingCharacters(in: .whitespacesAndNewlines).
func trimSwiftWhitespaceAndNewlines(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.Is(unicode.Zs, r) || r == ' ' || r == ' '
	})
}
