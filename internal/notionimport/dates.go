package notionimport

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Date is a parsed Notion date cell.
type Date struct {
	// Time is in UTC. A date without a time is midnight UTC.
	Time    time.Time
	HasTime bool
	// Range is true when the cell was "start → end"; Time is the start.
	Range bool
}

// ISO is the value stored in frontmatter: 2026-11-18, or 2026-11-18T10:00:00Z.
func (d Date) ISO() string {
	if d.HasTime {
		return d.Time.UTC().Format(time.RFC3339)
	}
	return d.Time.Format(time.DateOnly)
}

// zoneSuffix is Notion's "(GMT)", "(UTC)", "(GMT+1)" or "(GMT+5:30)". Any other
// abbreviation is left on the string, which then is no date: guessing a named
// zone's offset would store the wrong moment.
var zoneSuffix = regexp.MustCompile(`\s*\((?:GMT|UTC)\s*(?:([+-])\s*(\d{1,2})(?::?(\d{2}))?)?\)$`)

var (
	dateLayouts     = []string{"January 2, 2006", "Jan 2, 2006", time.DateOnly, "2006/01/02"}
	dateTimeLayouts = []string{
		"January 2, 2006 3:04 PM", "Jan 2, 2006 3:04 PM", "January 2, 2006 15:04", "Jan 2, 2006 15:04",
		"2006-01-02 15:04", "2006-01-02T15:04", "2006/01/02 15:04",
	}
)

// ParseNotionDate reads a date as Notion writes it in a CSV: "November 18,
// 2026", "November 18, 2026 10:00 AM", either with a "(GMT+1)"-style zone, or a
// range "A → B", of which the start is kept.
func ParseNotionDate(s string) (Date, bool) {
	s = strings.TrimSpace(s)
	var out Date
	if start, _, ok := strings.Cut(s, "→"); ok {
		out.Range = true
		s = strings.TrimSpace(start)
	}
	offset := 0
	if m := zoneSuffix.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(s[:len(s)-len(m[0])])
		if m[2] != "" {
			hours, _ := strconv.Atoi(m[2])
			minutes, _ := strconv.Atoi(m[3])
			offset = hours*3600 + minutes*60
			if m[1] == "-" {
				offset = -offset
			}
		}
	}
	if s == "" {
		return Date{}, false
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			out.Time = t
			return out, true
		}
	}
	zone := time.FixedZone("", offset)
	for _, layout := range dateTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, zone); err == nil {
			out.Time, out.HasTime = t.UTC(), true
			return out, true
		}
	}
	return Date{}, false
}
