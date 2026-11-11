package hermesdash

import (
	"context"
	"log/slog"
	"lumina/internal/swiftstr"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Gateway is HermesGatewayReadClient: jobs and sessions over the gateway
// api_server (/api/jobs, /api/sessions, Bearer API key). Same wire shapes as
// the dashboard, so the parsers are shared.
type Gateway struct {
	BaseURL    *url.URL
	AuthHeader *string
	HTTP       Doer
}

func NewGateway(base *url.URL, auth *string, doer Doer) *Gateway {
	if doer == nil {
		doer = NewClient()
	}
	return &Gateway{BaseURL: base, AuthHeader: auth, HTTP: doer}
}

func (g *Gateway) ListSessions(ctx context.Context, offset, limit int) (SessionPage, error) {
	resp, err := g.send(ctx, http.MethodGet, "api/sessions", sessionQuery(offset, limit), nil, ListBodyCap)
	if err != nil {
		return SessionPage{}, err
	}
	if !resp.IsSuccess() {
		return SessionPage{}, HTTPStatus(resp.Status, "/api/sessions")
	}
	return ParseSessions(resp.JSON()), nil
}

// ListJobs is GET /api/jobs: the cron rows the dashboard serves, with the gateway key.
func (g *Gateway) ListJobs(ctx context.Context) ([]Job, error) {
	resp, err := g.send(ctx, http.MethodGet, "api/jobs", nil, nil, ListBodyCap)
	if err != nil {
		return nil, err
	}
	if !resp.IsSuccess() {
		return nil, HTTPStatus(resp.Status, "/api/jobs")
	}
	return ParseJobs(resp.Body), nil
}

func (g *Gateway) CreateJob(ctx context.Context, spec JobSpec) (Job, error) {
	resp, err := g.send(ctx, http.MethodPost, "api/jobs", nil, spec.Body(), FileBodyCap)
	if err != nil {
		return Job{}, err
	}
	return gatewayJobDocument(resp, "/api/jobs", "")
}

func (g *Gateway) UpdateJob(ctx context.Context, id string, updates JobUpdate) (Job, error) {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return Job{}, err
	}
	resp, err := g.send(ctx, http.MethodPatch, "api/jobs/"+jobID, nil, updates.Updates(), FileBodyCap)
	if err != nil {
		return Job{}, err
	}
	return gatewayJobDocument(resp, "/api/jobs/{id}", jobID)
}

func (g *Gateway) PauseJob(ctx context.Context, id string) (Job, error) {
	return g.jobAction(ctx, id, "pause")
}

func (g *Gateway) ResumeJob(ctx context.Context, id string) (Job, error) {
	return g.jobAction(ctx, id, "resume")
}

// TriggerJob is POST /api/jobs/{id}/run (the dashboard says /trigger).
func (g *Gateway) TriggerJob(ctx context.Context, id string) (Job, error) {
	return g.jobAction(ctx, id, "run")
}

func (g *Gateway) DeleteJob(ctx context.Context, id string) error {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return err
	}
	resp, err := g.send(ctx, http.MethodDelete, "api/jobs/"+jobID, nil, nil, FileBodyCap)
	if err != nil {
		return err
	}
	if resp.Status == 404 {
		return NotFound("job:" + jobID)
	}
	if !resp.IsSuccess() {
		return HTTPStatus(resp.Status, "/api/jobs/{id}")
	}
	return nil
}

func (g *Gateway) SessionMessages(ctx context.Context, id string) ([]Message, error) {
	resp, err := g.send(ctx, http.MethodGet, "api/sessions/"+escapePath(id)+"/messages", query{{"limit", "500"}}, nil, ListBodyCap)
	if err != nil {
		return nil, err
	}
	if resp.Status == 404 {
		return nil, NotFound("session:" + id)
	}
	if !resp.IsSuccess() {
		return nil, HTTPStatus(resp.Status, "/api/sessions/{id}/messages")
	}
	return ParseMessages(resp.JSON()), nil
}

func (g *Gateway) jobAction(ctx context.Context, id, action string) (Job, error) {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return Job{}, err
	}
	resp, err := g.send(ctx, http.MethodPost, "api/jobs/"+jobID+"/"+action, nil, nil, FileBodyCap)
	if err != nil {
		return Job{}, err
	}
	return gatewayJobDocument(resp, "/api/jobs/{id}/"+action, jobID)
}

func gatewayJobDocument(resp Response, path, id string) (Job, error) {
	if resp.Status == 404 && id != "" {
		return Job{}, NotFound("job:" + id)
	}
	if resp.Status == 400 {
		return Job{}, InvalidResponse(path + ":rejected")
	}
	if !resp.IsSuccess() {
		return Job{}, HTTPStatus(resp.Status, path)
	}
	job, ok := ParseJobDocument(resp.JSON())
	if !ok {
		return Job{}, InvalidResponse(path)
	}
	return job, nil
}

// send is URL.appendingPathComponent(path) plus query items. Note the path
// joins after the base's own path, the way Foundation appends a component.
func (g *Gateway) send(ctx context.Context, method, path string, q query, body map[string]any, cap int) (Response, error) {
	target := appendPathComponent(g.BaseURL, path)
	if len(q) > 0 {
		target += "?" + encodeQuery(q)
	}
	header := http.Header{}
	header.Set("Accept", "application/json")
	if g.AuthHeader != nil && *g.AuthHeader != "" {
		header.Set("Authorization", *g.AuthHeader)
	}
	var payload any
	if body != nil {
		header.Set("Content-Type", "application/json")
		payload = body
	}
	resp, err := execute(ctx, g.HTTP, method, target, header, payload, Timeout, cap)
	if err != nil {
		if _, ok := err.(*Error); !ok {
			slog.DebugContext(ctx, "hermes gateway request failed", "path", path)
		}
		return Response{}, asTransportError(err, path)
	}
	return resp, nil
}

// appendPathComponent is URL.appendingPathComponent: the component is
// percent-escaped and joined to the base path with one "/"; the query and
// fragment are dropped. An id the caller already escaped is escaped again
// ("%" becomes "%25"), as in Swift.
func appendPathComponent(base *url.URL, component string) string {
	u := *base
	u.RawQuery, u.Fragment = "", ""
	path := u.EscapedPath()
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	path += escapePath(component)
	u.Path, u.RawPath = "", ""
	return u.String() + path
}

// CatalogEntry is the part of PluginCatalogEntryDTO the mirror reads.
type CatalogEntry struct {
	Slug, Name, Summary, Description string
}

// SkillsTimeout is HermesSkillsClient's default timeout.
const SkillsTimeout = 5 * time.Second

// InstalledSkills is HermesSkillsClient.installedSkills: GET /v1/skills,
// best-effort (any failure is an empty list).
func InstalledSkills(ctx context.Context, doer Doer, base *url.URL, authHeader *string) []CatalogEntry {
	if doer == nil {
		doer = NewClient()
	}
	header := http.Header{}
	header.Set("Accept", "application/json")
	if authHeader != nil && *authHeader != "" {
		header.Set("Authorization", *authHeader)
	}
	u := *base
	u.RawQuery, u.Fragment, u.RawPath = "", "", ""
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/skills"
	resp, err := execute(ctx, doer, http.MethodGet, u.String(), header, nil, SkillsTimeout, FileBodyCap)
	if err != nil || !resp.IsSuccess() {
		return []CatalogEntry{}
	}
	return ParseHermesSkills(resp.Body)
}

// ParseHermesSkills is SkillPluginCatalog.parseHermesSkills.
func ParseHermesSkills(data []byte) []CatalogEntry {
	raw := decodeAny(data)
	var list []any
	switch v := raw.(type) {
	case []any:
		list = v
	case map[string]any:
		for _, key := range []string{"skills", "data", "items"} {
			if l, ok := v[key].([]any); ok {
				list = l
				break
			}
		}
	}
	entries := []CatalogEntry{}
	seen := map[string]bool{}
	for _, item := range list {
		var name string
		var description *string
		switch v := item.(type) {
		case string:
			name = v
		case map[string]any:
			if n, ok := str(v, "name"); ok {
				name = n
			} else if n, ok := str(v, "id"); ok {
				name = n
			}
			description = strPtr(v, "description")
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		entries = append(entries, hermesEntry(name, description))
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	return entries
}

func hermesEntry(name string, description *string) CatalogEntry {
	desc := "Installed in your Hermes agent."
	if description != nil {
		desc = *description
	}
	return CatalogEntry{Slug: "hermes-" + name, Name: titleize(name), Summary: summarize(desc), Description: desc}
}

// titleize is "pattern-detector" → "Pattern Detector".
func titleize(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		chars := swiftstr.Characters(p)
		if len(chars) > 0 {
			parts[i] = strings.ToUpper(chars[0]) + strings.Join(chars[1:], "")
		}
	}
	return strings.Join(parts, " ")
}

// summarize is the first line, trimmed, capped at 140 characters.
func summarize(text string) string {
	line := text
	for _, l := range strings.Split(text, "\n") {
		if l != "" {
			line = l
			break
		}
	}
	trimmed := swiftstr.TrimWhitespaces(line)
	if swiftstr.Count(trimmed) <= 140 {
		return trimmed
	}
	return swiftstr.Prefix(trimmed, 139) + "…"
}
