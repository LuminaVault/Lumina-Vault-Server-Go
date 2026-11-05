package memorystore

import (
	"lumina/internal/swiftstr"
	"math"
	"strings"
	"time"
)

const titleMarkers = "#>-* \t"

// isCharacterNewline is Character.isNewline.
func isCharacterNewline(r rune) bool {
	switch r {
	case '\n', '\r', 0x0B, 0x0C, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// GraphTitle is MemoryGraphService.titleFromContent: the first non-empty line with
// leading markdown markers dropped, capped at 60 characters.
func GraphTitle(content string) string {
	raw := content
	if lines := strings.FieldsFunc(content, isCharacterNewline); len(lines) > 0 {
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
	first := swiftstr.TrimWhitespaces(rest)
	if first == "" {
		first = swiftstr.TrimWhitespaces(raw)
	}
	if swiftstr.Count(first) <= 60 {
		return first
	}
	return swiftstr.Prefix(first, 60) + "…"
}

// GraphActivity is MemoryGraphService.activity: recency over a 90-day window blended
// with a log-normalized score. The explicit float64 conversions stop the compiler
// fusing the multiply-add on arm64, which would change the result.
func GraphActivity(score float64, lastAccessed *time.Time, now time.Time) float64 {
	scoreNorm := math.Min(1, math.Max(0, math.Log1p(math.Max(0, score))/math.Log1p(100)))
	recency := 0.0
	if lastAccessed != nil {
		ageDays := now.Sub(*lastAccessed).Seconds() / 86400
		recency = math.Min(1, math.Max(0, 1-ageDays/90))
	}
	return math.Min(1, math.Max(0, float64(0.6*recency)+float64(0.4*scoreNorm)))
}
