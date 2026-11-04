package dashboard

import (
	"lumina/internal/swiftstr"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// graphCubeExtent is GraphLayoutService.cubeExtent.
const graphCubeExtent = 60.0

const titleMarkers = "#>-* \t"

// isSwiftNewline is Character.isNewline.
func isSwiftNewline(r rune) bool {
	switch r {
	case '\n', '\r', 0x0B, 0x0C, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// isSwiftWhitespace is Foundation's CharacterSet.whitespaces as CoreFoundation
// implements it on Linux: Zs, tab and U+200B (see internal/swiftstr).
func isSwiftWhitespace(r rune) bool {
	return r == '\t' || r == 0x200B || unicode.Is(unicode.Zs, r)
}

// titleFromContent is MemoryGraphService.titleFromContent: the first non-empty
// line with leading markdown markers dropped, capped at 60 characters.
func titleFromContent(content string) string {
	raw := content
	if lines := strings.FieldsFunc(content, isSwiftNewline); len(lines) > 0 {
		raw = lines[0]
	}
	rest := raw
	for rest != "" {
		// Swift drops whole Characters, so a marker carrying a combining mark stays.
		c := swiftstr.Prefix(rest, 1)
		if len(c) != 1 || !strings.Contains(titleMarkers, c) {
			break
		}
		rest = rest[len(c):]
	}
	first := strings.TrimFunc(rest, isSwiftWhitespace)
	if first == "" {
		first = strings.TrimFunc(raw, isSwiftWhitespace)
	}
	if swiftstr.Count(first) <= 60 {
		return first
	}
	return swiftstr.Prefix(first, 60) + "…"
}

// nodeActivity is MemoryGraphService.activity: recency over a 90-day window
// blended with a log-normalized score. The explicit float64 conversions stop
// the compiler fusing the multiply-add on arm64, which would change the result.
func nodeActivity(score float64, lastAccessed *time.Time, now time.Time) float64 {
	scoreNorm := math.Min(1, math.Max(0, math.Log1p(math.Max(0, score))/math.Log1p(100)))
	recency := 0.0
	if lastAccessed != nil {
		ageDays := now.Sub(*lastAccessed).Seconds() / 86400
		recency = math.Min(1, math.Max(0, 1-ageDays/90))
	}
	return math.Min(1, math.Max(0, float64(0.6*recency)+float64(0.4*scoreNorm)))
}

// isSwiftLetterOrNumber is Character.isLetter || Character.isNumber on a
// Character's first scalar: Alphabetic, or any numeric type (N*; numeric
// ideographs are already letters).
func isSwiftLetterOrNumber(r rune) bool {
	return unicode.IsLetter(r) || unicode.Is(unicode.Other_Alphabetic, r) || unicode.IsNumber(r)
}

// allowedID applies Swift's `value.count <= maxLen && value.allSatisfy { … }`
// over Characters: letters, numbers, and the exact ASCII marks in extra.
func allowedID(value string, maxLen int, extra string) bool {
	if value == "" || swiftstr.Count(value) > maxLen {
		return false
	}
	for rest := value; rest != ""; {
		c := swiftstr.Prefix(rest, 1)
		rest = rest[len(c):]
		first, _ := utf8.DecodeRuneInString(c)
		if isSwiftLetterOrNumber(first) || (len(c) == 1 && strings.Contains(extra, c)) {
			continue
		}
		return false
	}
	return true
}

func validRecommendationID(v string) bool { return allowedID(v, 96, "-_") }
func validIdempotencyKey(v string) bool   { return allowedID(v, 128, "-_") }
func validModelIdentity(v string) bool    { return allowedID(v, 128, "-_./:") }

// weightedHealthScore is AnalyticsController.weightedHealthScore. float64()
// keeps each product separately rounded, as Swift does (no FMA fusion on arm64).
func weightedHealthScore(freshness, engagement, organization, review int) int {
	score := float64(float64(freshness)*0.35) + float64(float64(engagement)*0.25) +
		float64(float64(organization)*0.20) + float64(float64(review)*0.20)
	return min(100, max(0, int(math.Round(score))))
}

// audioMinutes is AnalyticsController.minutes: one decimal, half away from zero.
func audioMinutes(ms int64) float64 {
	return math.Round(float64(ms)/60000*10) / 10
}

// centsFromMicros is AnalyticsController.cents: truncates to whole cents.
func centsFromMicros(micros int64) int { return int(micros / 10000) }

// startOfDayUTC is Calendar.startOfDay in GMT.
func startOfDayUTC(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}
