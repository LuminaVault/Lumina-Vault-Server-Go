package hermesdash

import (
	"encoding/json"
	"lumina/internal/swiftstr"
	"math"
	"strings"
)

// The static parsers of HermesDashboardClient. Hermes' JSON is loose, so every
// reader tolerates absence, the way JSONSerialization plus `as?` casts do.

// decodeAny is JSONSerialization.jsonObject: nil for anything unparseable.
func decodeAny(data []byte) any {
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil
	}
	return v
}

func asObject(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// asObjects is `as? [[String: Any]]`: every element must be an object.
func asObjects(v any) ([]map[string]any, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}

// asStrings is `as? [String]`.
func asStrings(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func str(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

func strPtr(m map[string]any, key string) *string {
	if s, ok := m[key].(string); ok {
		return &s
	}
	return nil
}

func boolPtr(m map[string]any, key string) *bool {
	if b, ok := m[key].(bool); ok {
		return &b
	}
	return nil
}

// nsInt is `(x as? NSNumber)?.intValue`: numbers truncate, booleans are 0 or 1.
func nsInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return int(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// exactInt is `x as? Int`: an integral number in range.
func exactInt(v any) (int, bool) {
	x, ok := v.(float64)
	if !ok || x != math.Trunc(x) || math.Abs(x) > 1<<62 {
		return 0, false
	}
	return int(x), true
}

// first is Swift's `row["a"] ?? row["b"]` over Any?: the first key present,
// even when its value is JSON null (NSNull is not nil).
func first(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

// AdminConfigRW reads admin_config_rw off /api/status.
func AdminConfigRW(status map[string]any) *bool {
	if b, ok := status["admin_config_rw"].(bool); ok {
		return &b
	}
	if caps, ok := asStrings(status["capabilities"]); ok {
		has := false
		for _, c := range caps {
			if c == "admin_config_rw" {
				has = true
			}
		}
		return &has
	}
	if caps, ok := status["capabilities"].(map[string]any); ok {
		if b, ok := caps["admin_config_rw"].(bool); ok {
			return &b
		}
	}
	return nil
}

// ParseSkill is parseSkill.
func ParseSkill(row map[string]any) (Skill, bool) {
	name, ok := str(row, "name")
	if !ok || name == "" {
		return Skill{}, false
	}
	source := "custom"
	switch p, _ := str(row, "provenance"); p {
	case "hub":
		source = "hub"
	case "bundled":
		source = "builtin"
	}
	desc, _ := str(row, "description")
	enabled := true
	if b, ok := row["enabled"].(bool); ok {
		enabled = b
	}
	return Skill{Name: name, Description: desc, Enabled: enabled, Source: source}, true
}

// ParseJobs is parseJobs: {"jobs": [...]} or a bare list.
func ParseJobs(data []byte) []Job {
	raw := decodeAny(data)
	var rows []map[string]any
	if obj, ok := asObject(raw); ok {
		list, ok := asObjects(obj["jobs"])
		if !ok {
			return []Job{}
		}
		rows = list
	} else if list, ok := asObjects(raw); ok {
		rows = list
	} else {
		return []Job{}
	}
	jobs := []Job{}
	for _, row := range rows {
		if j, ok := ParseJob(row); ok {
			jobs = append(jobs, j)
		}
	}
	return jobs
}

// ParseJobDocument is parseJobDocument: {"job": {...}} or the job itself.
func ParseJobDocument(v any) (Job, bool) {
	obj, ok := asObject(v)
	if !ok {
		return Job{}, false
	}
	if wrapped, ok := asObject(obj["job"]); ok {
		return ParseJob(wrapped)
	}
	return ParseJob(obj)
}

// ParseJob is parseJob.
func ParseJob(row map[string]any) (Job, bool) {
	id, ok := str(row, "id")
	if !ok || id == "" {
		return Job{}, false
	}
	var schedule *string
	if display, ok := str(row, "schedule_display"); ok {
		schedule = &display
	} else if obj, ok := asObject(row["schedule"]); ok {
		schedule = strPtr(obj, "display")
		if schedule == nil {
			schedule = strPtr(obj, "expr")
		}
	} else {
		schedule = strPtr(row, "schedule")
	}
	paused := false
	if b, ok := row["enabled"].(bool); ok && !b {
		paused = true
	}
	if s, _ := str(row, "state"); s == "paused" {
		paused = true
	}
	if s, _ := str(row, "status"); s == "paused" {
		paused = true
	}
	raw, _ := json.Marshal(row)
	return Job{
		ID: id, Name: strPtr(row, "name"), Schedule: schedule, Prompt: strPtr(row, "prompt"),
		Paused: paused, LastRunAt: ParseDate(first(row, "last_run_at", "last_run")),
		NextRunAt: ParseDate(row["next_run_at"]), Raw: raw,
	}, true
}

// ParseRuns is parseRuns: {"runs": [...]}, {"sessions": [...]} or a list.
func ParseRuns(v any) []JobRun {
	var rows []map[string]any
	if obj, ok := asObject(v); ok {
		if list, ok := asObjects(obj["runs"]); ok {
			rows = list
		} else if list, ok := asObjects(obj["sessions"]); ok {
			rows = list
		}
	} else if list, ok := asObjects(v); ok {
		rows = list
	}
	runs := []JobRun{}
	for _, row := range rows {
		key, ok := str(row, "id")
		if !ok || key == "" {
			continue
		}
		started := ParseDate(first(row, "started_at", "created_at"))
		if started == nil {
			continue
		}
		ended := ParseDate(row["ended_at"])
		lastActive := ParseDate(first(row, "last_active", "updated_at"))
		active, _ := row["is_active"].(bool)
		run := JobRun{Key: key, Status: RunOK, StartedAt: *started}
		if active {
			run.Status = RunRunning
		} else {
			finished := started
			if lastActive != nil {
				finished = lastActive
			}
			if ended != nil {
				finished = ended
			}
			run.FinishedAt = finished
		}
		if n, ok := nsInt(row["input_tokens"]); ok {
			run.TokensIn = &n
		}
		if n, ok := nsInt(row["output_tokens"]); ok {
			run.TokensOut = &n
		}
		runs = append(runs, run)
	}
	return runs
}

// FinalAssistantText is finalAssistantText: the last non-blank assistant message.
func FinalAssistantText(messages []Message) *string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		if text := swiftstr.TrimWhitespacesAndNewlines(messages[i].Content); text != "" {
			return &text
		}
	}
	return nil
}

// ParseSessions is parseSessions.
func ParseSessions(v any) SessionPage {
	var rows []map[string]any
	var total *int
	if obj, ok := asObject(v); ok {
		for _, key := range []string{"sessions", "items", "data"} {
			if list, ok := asObjects(obj[key]); ok {
				rows = list
				break
			}
		}
		if n, ok := nsInt(obj["total"]); ok {
			total = &n
		}
	} else if list, ok := asObjects(v); ok {
		rows = list
	}
	page := SessionPage{Sessions: []Session{}, Total: total}
	for _, row := range rows {
		id, ok := str(row, "id")
		if !ok || id == "" {
			continue
		}
		title := strPtr(row, "title")
		if title == nil {
			title = strPtr(row, "display_name")
		}
		if title == nil {
			title = strPtr(row, "preview")
		}
		count, _ := nsInt(row["message_count"])
		page.Sessions = append(page.Sessions, Session{
			ID: id, Title: title, Source: strPtr(row, "source"),
			StartedAt:    ParseDate(first(row, "started_at", "created_at")),
			LastActiveAt: ParseDate(first(row, "last_active", "updated_at", "ended_at")),
			MessageCount: count,
		})
	}
	return page
}

// ParseMessages is parseMessages.
func ParseMessages(v any) []Message {
	var rows []map[string]any
	if obj, ok := asObject(v); ok {
		rows, _ = asObjects(obj["messages"])
	} else if list, ok := asObjects(v); ok {
		rows = list
	}
	messages := []Message{}
	for _, row := range rows {
		role, ok := str(row, "role")
		if !ok {
			continue
		}
		content := ""
		if text, ok := str(row, "content"); ok {
			content = text
		} else if parts, ok := asObjects(row["content"]); ok {
			texts := []string{}
			for _, p := range parts {
				if t, ok := str(p, "text"); ok {
					texts = append(texts, t)
				}
			}
			content = strings.Join(texts, "\n")
		}
		messages = append(messages, Message{Role: role, Content: content, Timestamp: ParseDate(row["timestamp"])})
	}
	return messages
}
