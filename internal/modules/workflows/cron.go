package workflows

import (
	"lumina/internal/swiftstr"
	"strconv"
	"strings"
	"time"

	"unicode"

	// Workflow schedules name IANA zones. Foundation carries its own database, and
	// the server image may have none.
	_ "time/tzdata"
)

// cronField is CronExpression.FieldSet.
type cronField struct {
	values   map[int]bool
	wildcard bool
}

// cronExpression is Swift's CronExpression (Skills/CronExpression.swift): the parse
// and the match, which WorkflowSchedulePolicy needs. kanban's validCron is parse
// only. The scheduler that runs these stays in Swift.
type cronExpression struct {
	minute, hour, dayOfMonth, month, dayOfWeek cronField
}

// parseCron is CronExpression(raw). false is any ParseError. A field of "/" alone
// makes Swift index an empty array and trap, taking the process down; here it is
// a parse error.
func parseCron(raw string) (cronExpression, bool) {
	tokens := strings.FieldsFunc(swiftstr.TrimWhitespaces(raw), unicode.IsSpace)
	if len(tokens) != 5 {
		return cronExpression{}, false
	}
	bounds := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	fields := [5]cronField{}
	for i, token := range tokens {
		field, ok := parseCronField(token, bounds[i][0], bounds[i][1])
		if !ok {
			return cronExpression{}, false
		}
		fields[i] = field
	}
	return cronExpression{minute: fields[0], hour: fields[1], dayOfMonth: fields[2], month: fields[3], dayOfWeek: fields[4]}, true
}

func parseCronField(raw string, lo, hi int) (cronField, bool) {
	values := map[int]bool{}
	if raw == "*" {
		for v := lo; v <= hi; v++ {
			values[v] = true
		}
		return cronField{values: values, wildcard: true}, true
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
				return cronField{}, false
			}
			step = n
		default:
			return cronField{}, false
		}
		from, to, ok := parseCronRange(parts[0], lo, hi)
		if !ok {
			return cronField{}, false
		}
		for v := from; v <= to; v += step {
			values[v] = true
		}
	}
	return cronField{values: values}, true
}

func parseCronRange(body string, lo, hi int) (from, to int, ok bool) {
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

// matches is CronExpression.matches, with POSIX day-of-month or day-of-week: a
// restricted field matches alone, and two restricted fields match if either does.
func (c cronExpression) matches(t time.Time, zone *time.Location) bool {
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

// scheduleZone is TimeZone(identifier:) ?? GMT.
func scheduleZone(identifier string) *time.Location {
	if identifier == "" || identifier == "Local" {
		return time.UTC
	}
	zone, err := time.LoadLocation(identifier)
	if err != nil {
		return time.UTC
	}
	return zone
}

const (
	scheduleWindowMinutes = 48 * 60
	cronKey               = "cron"
	runAtKey              = "runAt"
	timezoneKey           = "timezone"
)

// validateSchedule is WorkflowSchedulePolicy.validate: a schedule that breaks the
// tier's floor shows two adjacent occurrences within the next 48 hours. A one-shot
// runAt is not recurring, so it needs no check, and its value is not parsed.
func validateSchedule(def WorkflowDefinitionDTO, minimumMinutes int, now time.Time) error {
	if def.Trigger != Schedule {
		return nil
	}
	if minimumMinutes <= 0 {
		return errScheduleTooFrequent(minimumMinutes)
	}
	raw, ok := def.TriggerConfiguration[cronKey]
	if !ok {
		if _, ok := def.TriggerConfiguration[runAtKey]; !ok {
			return errInvalidCron
		}
		return nil
	}
	cron, ok := parseCron(raw)
	if !ok {
		return errInvalidCron
	}
	identifier, ok := def.TriggerConfiguration[timezoneKey]
	if !ok {
		identifier = "UTC"
	}
	zone := scheduleZone(identifier)
	start := now.Truncate(time.Minute)
	floor := time.Duration(minimumMinutes) * time.Minute
	var previous time.Time
	for minute := range scheduleWindowMinutes {
		candidate := start.Add(time.Duration(minute) * time.Minute)
		if !cron.matches(candidate, zone) {
			continue
		}
		if !previous.IsZero() && candidate.Sub(previous) < floor {
			return errScheduleTooFrequent(minimumMinutes)
		}
		previous = candidate
	}
	return nil
}
