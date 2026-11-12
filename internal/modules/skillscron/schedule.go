package skillscron

import (
	"fmt"
	"lumina/internal/swiftstr"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// The patterns are CronBridgeService's NSRegularExpression ones, compiled with
// ICU's classes spelled out: ICU's \d is any decimal digit (\p{Nd}) and its \s
// is [\t\n\f\r\p{Z}], where RE2's are ASCII only. Int() then accepts only ASCII
// digits, so a match on other digits fails the same way it does in Swift.
const (
	icuDigit = `\p{Nd}`
	icuSpace = `[\t\n\f\r\p{Z}]`
)

var (
	everyMinutesRe = regexp.MustCompile(`(?i)every` + icuSpace + `+(` + icuDigit + `{1,3})` + icuSpace + `*min`)
	everyHoursRe   = regexp.MustCompile(`(?i)every` + icuSpace + `+(` + icuDigit + `{1,3})` + icuSpace + `*hour`)
	// `range(of:options: .regularExpression)`: no case-insensitive option.
	everyHourRe = regexp.MustCompile(`every` + icuSpace + `+hour`)
	meridiemRe  = regexp.MustCompile(`(?i)(` + icuDigit + `{1,2})(?::(` + icuDigit + `{2}))?` + icuSpace + `*(am|pm)`)
	atHourRe    = regexp.MustCompile(`(?i)at` + icuSpace + `+(` + icuDigit + `{1,2})(?::(` + icuDigit + `{2}))?`)
)

// firstMatch is CronBridgeService.firstMatch: the capture groups of the first
// match (group 0 omitted), an unmatched group as "".
func firstMatch(re *regexp.Regexp, text string) []string {
	m := re.FindStringSubmatch(text)
	if m == nil {
		return nil
	}
	return m[1:]
}

// swiftInt is Int(String): ASCII digits with an optional sign, nothing else.
func swiftInt(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Schedule is parseSchedule's result: a 5-field crontab and its English reading.
type Schedule struct {
	Cron  string
	Human string
}

type dayOfWeek struct {
	needle string
	dow    int
	label  string
}

var dowNames = []dayOfWeek{
	{"monday", 1, "Monday"},
	{"tuesday", 2, "Tuesday"},
	{"wednesday", 3, "Wednesday"},
	{"thursday", 4, "Thursday"},
	{"friday", 5, "Friday"},
	{"saturday", 6, "Saturday"},
	{"sunday", 0, "Sunday"},
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ParseSchedule is CronBridgeService.parseSchedule: common English schedule
// phrasings to a crontab, or false when no recurring schedule is recognizable
// (the caller falls back to the LLM).
func ParseSchedule(text string) (Schedule, bool) {
	lower := swiftstr.Lowercased(text)

	// "every N minutes" / "every N hours"
	if m := firstMatch(everyMinutesRe, lower); m != nil {
		if n, ok := swiftInt(m[0]); ok && n > 0 {
			return Schedule{fmt.Sprintf("*/%d * * * *", n), fmt.Sprintf("Every %d minute%s", n, plural(n))}, true
		}
	}
	if m := firstMatch(everyHoursRe, lower); m != nil {
		if n, ok := swiftInt(m[0]); ok && n > 0 {
			return Schedule{fmt.Sprintf("0 */%d * * *", n), fmt.Sprintf("Every %d hour%s", n, plural(n))}, true
		}
	}
	if strings.Contains(lower, "hourly") || everyHourRe.MatchString(lower) {
		return Schedule{"0 * * * *", "Every hour"}, true
	}

	hour, minute, ok := ParseTimeOfDay(lower)
	if !ok {
		hour, minute = 9, 0 // sensible default
	}
	hhmm := fmt.Sprintf("%d %d", minute, hour)
	humanTime := formatHuman(hour, minute)

	if strings.Contains(lower, "weekday") {
		return Schedule{hhmm + " * * 1-5", "Every weekday at " + humanTime}, true
	}
	if strings.Contains(lower, "weekend") {
		return Schedule{hhmm + " * * 0,6", "Every weekend at " + humanTime}, true
	}
	for _, d := range dowNames {
		if strings.Contains(lower, d.needle) {
			return Schedule{fmt.Sprintf("%s * * %d", hhmm, d.dow), "Every " + d.label + " at " + humanTime}, true
		}
	}
	if strings.Contains(lower, "weekly") || strings.Contains(lower, "each week") || strings.Contains(lower, "every week") {
		return Schedule{hhmm + " * * 1", "Every Monday at " + humanTime}, true
	}
	for _, needle := range []string{"daily", "every day", "each day", "every morning", "every evening", "every night", "each morning"} {
		if strings.Contains(lower, needle) {
			return Schedule{hhmm + " * * *", "Every day at " + humanTime}, true
		}
	}

	// A bare time with no frequency → assume daily; otherwise unrecognized.
	if ok {
		return Schedule{hhmm + " * * *", "Every day at " + humanTime}, true
	}
	return Schedule{}, false
}

// ParseTimeOfDay is CronBridgeService.parseTimeOfDay: an hour and minute from
// "9am", "9:30 pm", "at 17:00", "at 8". The minute is not range-checked, as in
// Swift.
func ParseTimeOfDay(lower string) (hour, minute int, ok bool) {
	// With meridiem (am/pm). groups: [hour, minute?, am|pm]
	if m := firstMatch(meridiemRe, lower); m != nil {
		if h, ok := swiftInt(m[0]); ok && h >= 1 && h <= 12 {
			mins, _ := swiftInt(m[1])
			mer := m[2]
			if mer == "pm" && h < 12 {
				h += 12
			}
			if mer == "am" && h == 12 {
				h = 0
			}
			return h % 24, mins, true
		}
	}
	// 24h "at HH:MM" or "at HH". groups: [hour, minute?]
	if m := firstMatch(atHourRe, lower); m != nil {
		if h, ok := swiftInt(m[0]); ok && h >= 0 && h <= 23 {
			mins, _ := swiftInt(m[1])
			return h, mins, true
		}
	}
	return 0, 0, false
}

// formatHuman is CronBridgeService.formatHuman.
func formatHuman(hour, minute int) string {
	mer := "PM"
	if hour < 12 {
		mer = "AM"
	}
	h12 := hour % 12
	if h12 == 0 {
		h12 = 12
	}
	if minute == 0 {
		return fmt.Sprintf("%d:00 %s", h12, mer)
	}
	return fmt.Sprintf("%d:%02d %s", h12, minute, mer)
}

var nameStopWords = map[string]bool{
	"every": true, "each": true, "daily": true, "weekly": true, "hourly": true, "weekday": true, "weekdays": true,
	"weekend": true, "weekends": true, "morning": true, "evening": true, "night": true, "at": true, "am": true, "pm": true,
	"monday": true, "tuesday": true, "wednesday": true, "thursday": true, "friday": true, "saturday": true, "sunday": true,
}

// isAlphanumeric is CharacterSet.alphanumerics: the General Categories L*, M* and N*.
func isAlphanumeric(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r)
}

// DeriveName is CronBridgeService.deriveName: a short job name from the request
// text. Words split on non-alphanumerics; stop words and any word holding a
// decimal digit are dropped; the first six are kept and the first Character is
// uppercased with the full mapping ("ß" becomes "SS"), as String.uppercased() does.
func DeriveName(text string) string {
	words := strings.FieldsFunc(swiftstr.Lowercased(text), func(r rune) bool { return !isAlphanumeric(r) })
	picked := make([]string, 0, 6)
	for _, w := range words {
		if len(picked) == 6 {
			break
		}
		if nameStopWords[w] || strings.IndexFunc(w, func(r rune) bool { return unicode.Is(unicode.Nd, r) }) >= 0 {
			continue
		}
		picked = append(picked, w)
	}
	title := strings.Join(picked, " ")
	if title == "" {
		title = "Scheduled job"
	}
	n := swiftstr.CharacterLen(title)
	// A Caser is stateful, so each call makes its own.
	return cases.Upper(language.Und).String(title[:n]) + title[n:]
}

// deliverTarget is preview's deliver keyword detection.
func deliverTarget(text string) string {
	lower := swiftstr.Lowercased(text)
	switch {
	case strings.Contains(lower, "telegram"):
		return "telegram"
	case strings.Contains(lower, "discord"):
		return "discord"
	case strings.Contains(lower, "slack"):
		return "slack"
	case strings.Contains(lower, "signal"):
		return "signal"
	case strings.Contains(lower, "email") || strings.Contains(lower, "e-mail"):
		return "email"
	default:
		return "origin"
	}
}
