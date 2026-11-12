package skillscron

import "encoding/json"

// Job is HermesCronJob: the subset of a Hermes cron job the app shows. nil
// fields are left out of the JSON, as Swift's synthesized Codable does.
type Job struct {
	ID       string  `json:"id"`
	Name     *string `json:"name,omitempty"`
	Schedule *string `json:"schedule,omitempty"`
	Deliver  *string `json:"deliver,omitempty"`
	LastRun  *string `json:"lastRun,omitempty"`
	Status   *string `json:"status,omitempty"`
	Mode     *string `json:"mode,omitempty"`
}

// ListResponse is HermesCronListResponse. Source is "managed" or "byo".
type ListResponse struct {
	Source string `json:"source"`
	Jobs   []Job  `json:"jobs"`
}

// CreateSpec is CronCreateSpec: the create body and the preview answer.
type CreateSpec struct {
	Schedule string    `json:"schedule"`
	Prompt   *string   `json:"prompt,omitempty"`
	Name     *string   `json:"name,omitempty"`
	Deliver  *string   `json:"deliver,omitempty"`
	Skills   *[]string `json:"skills,omitempty"`
}

// objects is `as? [[String: Any]]`: a list whose every element is an object.
func objects(v any) ([]map[string]any, bool) {
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

// stringAt is `m[key] as? String`.
func stringAt(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

func stringPtr(m map[string]any, key string) *string {
	if s, ok := stringAt(m, key); ok {
		return &s
	}
	return nil
}

// boolAt is `m[key] as? Bool` over JSONSerialization's output: a JSON true or
// false, and also a number equal to 0 or 1, since an NSNumber holding 0 or 1
// bridges to Bool (measured: {"enabled": 0} is paused in Swift).
func boolAt(m map[string]any, key string) (value, ok bool) {
	switch v := m[key].(type) {
	case bool:
		return v, true
	case float64:
		if v == 0 || v == 1 {
			return v == 1, true
		}
	}
	return false, false
}

func ptr(s string) *string { return &s }

// Parse is CronBridgeService.parse: jobs.json's {"jobs": [...]} or the
// dashboard's bare array, robust to field shape. Anything else is no jobs.
func Parse(data []byte) []Job {
	var raw any
	if json.Unmarshal(data, &raw) != nil {
		return []Job{}
	}
	var rows []map[string]any
	if dict, ok := raw.(map[string]any); ok {
		list, ok := objects(dict["jobs"])
		if !ok {
			return []Job{}
		}
		rows = list // jobs.json shape
	} else if list, ok := objects(raw); ok {
		rows = list // dashboard /api/cron/jobs shape (bare array)
	} else {
		return []Job{}
	}
	jobs := []Job{}
	for _, j := range rows {
		id, ok := stringAt(j, "id")
		if !ok {
			continue
		}
		// schedule: the dashboard sends schedule_display and an object
		// schedule {kind, expr, display}; jobs.json may send a plain string.
		var schedule *string
		if sd, ok := stringAt(j, "schedule_display"); ok {
			schedule = &sd
		} else if so, ok := j["schedule"].(map[string]any); ok {
			schedule = stringPtr(so, "display")
			if schedule == nil {
				schedule = stringPtr(so, "expr")
			}
		} else {
			schedule = stringPtr(j, "schedule")
		}
		// last run: last_run_at (+ last_status) or the legacy last_run.
		var lastRun *string
		if lr, ok := stringAt(j, "last_run_at"); ok {
			if st, ok := stringAt(j, "last_status"); ok {
				lastRun = ptr(lr + " (" + st + ")")
			} else {
				lastRun = &lr
			}
		} else if s, ok := stringAt(j, "last_run"); ok {
			lastRun = &s
		} else if d, ok := j["last_run"].(map[string]any); ok {
			lastRun = stringPtr(d, "at")
			if lastRun == nil {
				lastRun = stringPtr(d, "status")
			}
		}
		// status: the paused flag wins, else state/status, else active.
		status := "active"
		if enabled, ok := boolAt(j, "enabled"); ok && !enabled {
			status = "paused"
		} else if s, ok := stringAt(j, "state"); ok {
			status = s
		} else if s, ok := stringAt(j, "status"); ok {
			status = s
		}
		mode := "agent"
		if noAgent, ok := boolAt(j, "no_agent"); ok && noAgent {
			mode = "script"
		}
		jobs = append(jobs, Job{
			ID: id, Name: stringPtr(j, "name"), Schedule: schedule, Deliver: stringPtr(j, "deliver"),
			LastRun: lastRun, Status: &status, Mode: &mode,
		})
	}
	return jobs
}
