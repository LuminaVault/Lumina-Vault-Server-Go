package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/embedding"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The tools a skill's allowed-tools may name, in SkillRunner.AvailableTool's
// allCases order (the order the definitions are sent in). Tool results only
// ever reach the model, so their JSON is not wire contract.

type propertySchema struct {
	Type        string  `json:"type"`
	Description *string `json:"description,omitempty"`
}

type parameterSchema struct {
	Type       string                    `json:"type"`
	Properties map[string]propertySchema `json:"properties"`
	Required   []string                  `json:"required"`
}

type toolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  parameterSchema `json:"parameters"`
	} `json:"function"`
}

type toolProperty struct{ name, kind, description string }

type availableTool struct {
	name, description string
	properties        []toolProperty
	required          []string
}

func (t availableTool) definition() toolDefinition {
	var d toolDefinition
	d.Type = "function"
	d.Function.Name = t.name
	d.Function.Description = t.description
	d.Function.Parameters = parameterSchema{Type: "object", Properties: map[string]propertySchema{}, Required: []string{}}
	for _, p := range t.properties {
		d.Function.Parameters.Properties[p.name] = propertySchema{Type: p.kind, Description: strPtr(p.description)}
	}
	if t.required != nil {
		d.Function.Parameters.Required = t.required
	}
	return d
}

const (
	toolMemoryUpsert    = "memory_upsert"
	toolSessionSearch   = "session_search"
	toolVaultRead       = "vault_read"
	toolHealthQuery     = "health_query"
	toolReminderCreate  = "reminder_create"
	toolCalendarCreate  = "calendar_create"
	toolCalendarQuery   = "calendar_query"
	toolRemindersList   = "reminders_list"
	toolPhotosSearch    = "photos_search"
	toolLocationRecent  = "location_recent"
	toolFilesPick       = "files_pick"
	toolWeatherForecast = "weather_forecast"
	toolMailInboxRecent = "mail_inbox_recent"
)

var availableTools = []availableTool{
	{name: toolMemoryUpsert, description: "Persist a memory for the current tenant.", properties: []toolProperty{
		{"content", "string", "Memory text to persist."},
		{"source_vault_file_id", "string", "Optional source vault file UUID."},
	}, required: []string{"content"}},
	{name: toolSessionSearch, description: "Semantic-search the current tenant's memories.", properties: []toolProperty{
		{"query", "string", "Natural language query."},
		{"limit", "integer", "Maximum memories, 1-20."},
	}, required: []string{"query"}},
	{name: toolVaultRead, description: "Read one current-tenant vault file by path or id.", properties: []toolProperty{
		{"path", "string", "Vault path relative to raw root."},
		{"vault_file_id", "string", "Vault file UUID."},
	}},
	{name: toolHealthQuery, description: "Read the user's Apple Health data as daily aggregates (for trends/correlation). Requires the user to have allowed Health access.", properties: []toolProperty{
		{"metric", "string", "Optional event type to filter (e.g. step_count, sleep, heart_rate). Omit for all."},
		{"days", "integer", "Lookback window in days, 1-365 (default 30)."},
	}},
	{name: toolReminderCreate, description: "Create a reminder in the user's Apple Reminders. Requires the user to have allowed Reminders access with changes enabled.", properties: []toolProperty{
		{"title", "string", "Reminder title."},
		{"notes", "string", "Optional notes/body."},
		{"due", "string", "Optional ISO-8601 due date-time."},
	}, required: []string{"title"}},
	{name: toolCalendarCreate, description: "Create an event in the user's Apple Calendar. Requires the user to have allowed Calendar access with changes enabled.", properties: []toolProperty{
		{"title", "string", "Event title."},
		{"start", "string", "ISO-8601 start date-time."},
		{"end", "string", "ISO-8601 end date-time."},
		{"location", "string", "Optional location."},
	}, required: []string{"title", "start"}},
	{name: toolCalendarQuery, description: "List the user's upcoming calendar events from the synced server cache (Apple EventKit + Google Calendar), falling back to a live device read when the cache is empty. Requires the user to have allowed Calendar access. Returns a JSON array of {title,start,end,location}.", properties: []toolProperty{
		{"days", "integer", "How many days ahead to include, 1-90 (default 7)."},
	}},
	{name: toolRemindersList, description: "List the user's open (incomplete) Apple Reminders, overdue and upcoming items soonest-due first. Served from the synced server-side cache when available (falls back to a live device fetch). Requires the user to have allowed Reminders access. Returns a JSON array of {title,due,notes}."},
	{name: toolPhotosSearch, description: "Search the user's photos by their derived text. Pass `query` for a semantic search over previously-indexed OCR text + scene tags (e.g. \"the screenshot about the flight\", \"a receipt\") — only derived text + metadata are stored server-side, never the images. Omit `query` to analyze the most recent photos live on-device. Requires Photos access. Returns a JSON array of {taken_at, is_screenshot, ocr_text, scene_tags, score}.", properties: []toolProperty{
		{"query", "string", "Natural-language description of the photo/screenshot to find. Omit to list recent photos instead."},
		{"limit", "integer", "Maximum results, 1-30 (default 10)."},
	}},
	{name: toolLocationRecent, description: "Get the user's current location with a place name. Requires Location access. Returns a JSON array of {lat,lng,place,at}."},
	{name: toolFilesPick, description: "Prompt the user to pick one or more documents on their device; extracted text (from PDFs and plain-text files) is read on-device and only that text is returned, never the file bytes. Requires Files access and the app to be open. Returns a JSON array of {name,type,chars,text}."},
	{name: toolWeatherForecast, description: "7-day daily weather forecast for the user's location (live phone location when available, otherwise the last known one). Requires Location access. Returns {location:{place,lat,lng,source,captured_at}, days:[{date,summary,precipitation_mm,weather_code,dry}], dry_streak_days} where dry means under 1 mm of rain and dry_streak_days counts consecutive dry days starting today."},
	{name: toolMailInboxRecent, description: "The user's Gmail inbox from the last 24 hours as metadata only: sender, subject, Google's short snippet, unread/important flags. No message bodies. Requires the user to have connected Gmail. Returns {count, messages:[{from,subject,snippet,date,unread,important}]}.", properties: []toolProperty{
		{"limit", "integer", "Maximum messages, 1-25 (default 15)."},
	}},
}

func encodeToolJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"status":"error","reason":"could not encode tool result"}`
	}
	return string(b)
}

func toolErrorJSON(reason string) string {
	return encodeToolJSON(map[string]string{"status": "error", "reason": reason})
}

func clampInt(v *int, def, lo, hi int) int {
	n := def
	if v != nil {
		n = *v
	}
	return max(lo, min(n, hi))
}

// swiftFloat renders a Float the way Swift's String(Float) does for the
// ordinary range: shortest digits, always with a fractional part.
func swiftFloat(f float32) string {
	s := strconv.FormatFloat(float64(f), 'g', -1, 32)
	if !strings.ContainsAny(s, ".eEn") {
		s += ".0"
	}
	return s
}

// dispatch is SkillRunner.dispatch: one tool call, answered as JSON for the model.
func (r *Runner) dispatch(ctx context.Context, tenant uuid.UUID, allowed map[string]bool, call toolCall) string {
	name := call.Function.Name
	if !allowed[name] {
		return toolErrorJSON("tool " + name + " not allowed by this skill manifest")
	}
	args := []byte(call.Function.Arguments)
	switch name {
	case toolMemoryUpsert:
		var a struct {
			Content           string     `json:"content"`
			SourceVaultFileID *wire.UUID `json:"source_vault_file_id,omitempty"`
		}
		if err := wire.DecodeInto(args, &a); err != nil {
			return toolErrorJSON("memory_upsert failed: " + err.Error())
		}
		var source *uuid.UUID
		if a.SourceVaultFileID != nil {
			source = &a.SourceVaultFileID.UUID
		}
		saved, err := r.persistMemory(ctx, tenant, a.Content, source)
		if err != nil {
			return toolErrorJSON("memory_upsert failed: " + err.Error())
		}
		return encodeToolJSON(map[string]string{"status": "ok", "id": strings.ToUpper(saved.ID.String())})
	case toolSessionSearch:
		var a struct {
			Query string `json:"query"`
			Limit *int   `json:"limit,omitempty"`
		}
		if err := wire.DecodeInto(args, &a); err != nil {
			return toolErrorJSON("session_search failed: " + err.Error())
		}
		vec, err := embedding.EmbedForQuery(ctx, r.embeddings, a.Query, tenant, "skill.sessionSearch")
		if err != nil {
			return toolErrorJSON("session_search failed: " + err.Error())
		}
		hits, err := r.memories.SearchDocuments(ctx, tenant, a.Query, vec, clampInt(a.Limit, 5, 1, 20), memorystore.SearchOptions{})
		if err != nil {
			return toolErrorJSON("session_search failed: " + err.Error())
		}
		results := make([]map[string]string, len(hits))
		for i, h := range hits {
			results[i] = map[string]string{"id": strings.ToUpper(h.ID.String()), "content": h.Content, "distance": swiftFloat(h.Distance)}
		}
		return encodeToolJSON(map[string]any{"status": "ok", "results": results})
	case toolVaultRead:
		var a struct {
			Path        *string    `json:"path,omitempty"`
			VaultFileID *wire.UUID `json:"vault_file_id,omitempty"`
		}
		if err := wire.DecodeInto(args, &a); err != nil {
			return toolErrorJSON("vault_read failed: " + err.Error())
		}
		var id *uuid.UUID
		if a.VaultFileID != nil {
			id = &a.VaultFileID.UUID
		}
		path, err := r.resolveVaultFile(ctx, tenant, id, a.Path)
		if err != nil {
			return toolErrorJSON("vault_read failed: " + err.Error())
		}
		content, err := r.readVaultFile(tenant, path)
		if err != nil {
			return toolErrorJSON("vault_read failed: " + err.Error())
		}
		return encodeToolJSON(map[string]string{"status": "ok", "path": path, "content": content})
	}
	if r.device == nil {
		switch name {
		case toolHealthQuery, toolReminderCreate, toolCalendarCreate, toolCalendarQuery, toolRemindersList, toolLocationRecent, toolFilesPick:
			return toolErrorJSON("device tools are not available on this server")
		}
	}
	switch name {
	case toolHealthQuery:
		var a struct {
			Metric *string `json:"metric,omitempty"`
			Days   *int    `json:"days,omitempty"`
		}
		if wire.DecodeInto(args, &a) != nil {
			a.Metric, a.Days = nil, nil
		}
		return r.device.HealthQuery(ctx, tenant, a.Metric, a.Days)
	case toolReminderCreate:
		var a struct {
			Title string  `json:"title"`
			Notes *string `json:"notes,omitempty"`
			Due   *string `json:"due,omitempty"`
		}
		if err := wire.DecodeInto(args, &a); err != nil {
			return toolErrorJSON("reminder_create failed: " + err.Error())
		}
		return r.device.ReminderCreate(ctx, tenant, a.Title, a.Notes, a.Due)
	case toolCalendarCreate:
		var a struct {
			Title    string  `json:"title"`
			Start    string  `json:"start"`
			End      *string `json:"end,omitempty"`
			Location *string `json:"location,omitempty"`
		}
		if err := wire.DecodeInto(args, &a); err != nil {
			return toolErrorJSON("calendar_create failed: " + err.Error())
		}
		return r.device.CalendarCreate(ctx, tenant, a.Title, a.Start, a.End, a.Location)
	case toolCalendarQuery:
		var a struct {
			Days *int `json:"days,omitempty"`
		}
		if wire.DecodeInto(args, &a) != nil {
			a.Days = nil
		}
		return r.device.CalendarQuery(ctx, tenant, a.Days)
	case toolRemindersList:
		return r.device.RemindersList(ctx, tenant)
	case toolPhotosSearch:
		var a struct {
			Limit *int    `json:"limit,omitempty"`
			Query *string `json:"query,omitempty"`
		}
		if wire.DecodeInto(args, &a) != nil {
			a.Limit, a.Query = nil, nil
		}
		return r.photosSearch(ctx, tenant, a.Query, clampInt(a.Limit, 10, 1, 30))
	case toolLocationRecent:
		result := r.device.DeviceRead(ctx, tenant, "location", map[string]string{})
		if fix := fixFromDeviceRead(result, time.Now()); fix != nil {
			if err := saveFix(ctx, r.pool, tenant, *fix); err != nil {
				r.log.WarnContext(ctx, fmt.Sprintf("location_recent: could not keep fix tenant=%s: %v", strings.ToUpper(tenant.String()), err))
			}
		}
		return result
	case toolWeatherForecast:
		return r.weather(ctx, tenant)
	case toolMailInboxRecent:
		var a struct {
			Limit *int `json:"limit,omitempty"`
		}
		if wire.DecodeInto(args, &a) != nil {
			a.Limit = nil
		}
		if r.google == nil {
			return toolErrorJSON("Gmail is not available on this server")
		}
		return r.gmailInbox(ctx, tenant, clampInt(a.Limit, 15, 1, 25))
	case toolFilesPick:
		return r.device.DeviceRead(ctx, tenant, "files", map[string]string{})
	}
	return toolErrorJSON("unknown tool " + name)
}

var (
	errVaultFileNotFound = wire.NewError(http.StatusNotFound, "vault file not found")
	errVaultFileRequired = wire.NewError(http.StatusBadRequest, "vault file id or path required")
)

// resolveVaultFile finds the tenant's vault file by id, else by path, and
// returns its stored path.
func (r *Runner) resolveVaultFile(ctx context.Context, tenant uuid.UUID, id *uuid.UUID, path *string) (string, error) {
	var found string
	if id != nil {
		err := r.pool.QueryRow(ctx, `SELECT path FROM vault_files WHERE tenant_id = $1 AND id = $2 LIMIT 1`, tenant, *id).Scan(&found)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", errVaultFileNotFound
		}
		return found, err
	}
	if path != nil {
		safe, err := validateRelativePath(*path)
		if err != nil {
			return "", err
		}
		err = r.pool.QueryRow(ctx, `SELECT path FROM vault_files WHERE tenant_id = $1 AND path = $2 LIMIT 1`, tenant, safe).Scan(&found)
		if err == nil {
			return found, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	return "", errVaultFileRequired
}

func (r *Runner) readVaultFile(tenant uuid.UUID, path string) (string, error) {
	safe, err := validateRelativePath(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(r.paths.Raw(tenant), safe)) //nolint:gosec // validated relative path under the tenant's raw root
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// photosSearch is SkillRunner.photosSearch: consent-gated cosine search over
// photo_index when there is a query and an index, else a live device read.
func (r *Runner) photosSearch(ctx context.Context, tenant uuid.UUID, query *string, limit int) string {
	if !r.consentAllows(ctx, tenant, "photos") {
		return toolErrorJSON("photos access not allowed by the user")
	}
	trimmed := ""
	if query != nil {
		trimmed = swiftstr.TrimWhitespacesAndNewlines(*query)
	}
	var n int
	_ = r.pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM photo_index WHERE tenant_id = $1`, tenant).Scan(&n)
	if trimmed != "" && n > 0 {
		items, err := r.photoHits(ctx, tenant, trimmed, limit)
		if err != nil {
			return toolErrorJSON("photos_search failed: " + err.Error())
		}
		return encodeToolJSON(map[string]any{"status": "ok", "source": "index", "items": items})
	}
	if r.device == nil {
		return toolErrorJSON("device tools are not available on this server")
	}
	return r.device.DeviceRead(ctx, tenant, "photos", map[string]string{"limit": strconv.Itoa(limit)})
}

func (r *Runner) photoHits(ctx context.Context, tenant uuid.UUID, query string, limit int) ([]map[string]string, error) {
	vec, err := r.embeddings.Embed(ctx, query, tenant)
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT taken_at, is_screenshot, ocr_text, scene_tags, (embedding <=> $2::vector)::real AS distance
		FROM photo_index WHERE tenant_id = $1 AND embedding IS NOT NULL ORDER BY distance ASC LIMIT $3`,
		tenant, embedding.FormatVector(vec), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]string{}
	for rows.Next() {
		var takenAt *time.Time
		var screenshot bool
		var ocr *string
		var tags []string
		var distance float32
		if err := rows.Scan(&takenAt, &screenshot, &ocr, &tags, &distance); err != nil {
			return nil, err
		}
		item := map[string]string{
			"taken_at": "", "is_screenshot": strconv.FormatBool(screenshot), "ocr_text": "",
			"scene_tags": strings.Join(tags, ", "),
			"score":      strconv.FormatFloat(math.Max(0, 1-float64(distance)/2), 'f', 3, 64),
		}
		if takenAt != nil {
			item["taken_at"] = dateStamp(*takenAt)
		}
		if ocr != nil {
			item["ocr_text"] = *ocr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
