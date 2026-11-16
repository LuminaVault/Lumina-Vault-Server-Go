package vaultprops

import (
	"encoding/json"
	"strings"
	"time"
)

// Type is a property's declared type in the tenant registry (spec D3).
type Type string

const (
	Text        Type = "text"
	Number      Type = "number"
	Select      Type = "select"
	MultiSelect Type = "multiselect"
	Date        Type = "date"
	DateTime    Type = "datetime"
	Checkbox    Type = "checkbox"
	URL         Type = "url"
)

// Valid reports whether t is one of the eight types.
func (t Type) Valid() bool {
	switch t {
	case Text, Number, Select, MultiSelect, Date, DateTime, Checkbox, URL:
		return true
	}
	return false
}

// Reserved keys have fixed types; they also feed the sidecar (D1).
var Reserved = map[string]Type{
	"title": Text,
	"tags":  MultiSelect,
	"due":   Date,
	"done":  Checkbox,
}

// Infer picks a type for a property seen for the first time (D3).
func Infer(name string, v any) Type {
	if t, ok := Reserved[strings.ToLower(name)]; ok {
		if t == Date {
			if s, ok := v.(string); ok && !isDate(s) && isDateTime(s) {
				return DateTime
			}
		}
		return t
	}
	switch x := v.(type) {
	case bool:
		return Checkbox
	case []any:
		return MultiSelect
	case string:
		switch {
		case isDate(x):
			return Date
		case isDateTime(x):
			return DateTime
		case isURL(x):
			return URL
		}
		return Text
	}
	if _, ok := asFloat(v); ok {
		return Number
	}
	return Text
}

// Fits reports whether v is a valid value of type t. A value that doesn't
// fit is kept as written, shown raw and left out of typed filters (D3);
// a PATCH with one is a 422. nil (an empty property) fits every type.
func Fits(t Type, v any) bool {
	if v == nil {
		return true
	}
	switch t {
	case Checkbox:
		_, ok := v.(bool)
		return ok
	case Number:
		_, ok := asFloat(v)
		return ok
	case Date:
		s, ok := v.(string)
		return ok && isDate(s)
	case DateTime:
		s, ok := v.(string)
		return ok && (isDate(s) || isDateTime(s))
	case URL:
		s, ok := v.(string)
		return ok && isURL(s)
	case Select, Text:
		switch v.(type) {
		case string, bool:
			return true
		}
		_, ok := asFloat(v)
		return ok
	case MultiSelect:
		if _, ok := v.(string); ok {
			return true
		}
		list, ok := v.([]any)
		if !ok {
			return false
		}
		for _, item := range list {
			if _, ok := item.(string); !ok && item != nil {
				if _, num := asFloat(item); !num {
					return false
				}
			}
		}
		return true
	}
	return false
}

func isURL(s string) bool {
	return (strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")) && !strings.ContainsAny(s, " \n")
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// Projection is what the sidecar's title, tags, dueAt and done take from the
// frontmatter (D1). A nil field means the key is absent: leave the sidecar.
type Projection struct {
	Title *string
	Tags  *[]string
	DueAt *time.Time
	Done  *bool
}

// Project reads the four sidecar fields from properties.
func Project(p Props) Projection {
	var out Projection
	if v, ok := p["title"]; ok {
		if s, ok := v.(string); ok {
			out.Title = &s
		}
	}
	tags, hasTags := p["tags"]
	if !hasTags {
		tags, hasTags = p["tag"]
	}
	if hasTags {
		list := tagList(tags)
		out.Tags = &list
	}
	if v, ok := p["due"]; ok {
		if s, ok := v.(string); ok {
			if t, ok := parseMoment(s); ok {
				out.DueAt = &t
			}
		}
	}
	if v, ok := p["done"]; ok {
		if b, ok := v.(bool); ok {
			out.Done = &b
		}
	}
	return out
}

// tagList accepts a list or a string ("a, b" or "a b"), without leading `#`.
func tagList(v any) []string {
	var raw []string
	switch x := v.(type) {
	case string:
		raw = strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == ' ' })
	case []any:
		for _, item := range x {
			switch s := item.(type) {
			case string:
				raw = append(raw, s)
			case float64:
				raw = append(raw, numberText(s))
			}
		}
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, t := range raw {
		t = strings.TrimPrefix(strings.TrimSpace(t), "#")
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// parseMoment reads a date (midnight UTC) or a datetime.
func parseMoment(s string) (time.Time, bool) {
	for _, layout := range []string{time.DateOnly, time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
