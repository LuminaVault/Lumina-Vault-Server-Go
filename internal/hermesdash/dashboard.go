package hermesdash

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/ssrfguard"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AuthMode is HermesDashboardAuthMode.
type AuthMode string

const (
	AuthBearer       AuthMode = "bearer"
	AuthOAuthOnly    AuthMode = "oauth_only"
	AuthUnauthorized AuthMode = "unauthorized"
	AuthUnreachable  AuthMode = "unreachable"
)

// Dashboard is HermesDashboardClient: the one client for a tenant's Hermes
// dashboard (/api/*, sealed dashboard token). SSRF validation runs on every
// call, bodies are capped, timeouts are 15 s, the token is never logged, and a
// 429 backs off and retries up to three times.
type Dashboard struct {
	BaseURL string
	Token   string
	Guard   ssrfguard.Guard
	HTTP    Doer
	// Sleep is the 429 back-off; nil sleeps for real.
	Sleep func(context.Context, time.Duration) error
}

// NewDashboard builds a client on the shared no-redirect HTTP client.
func NewDashboard(baseURL, token string, guard ssrfguard.Guard, doer Doer) *Dashboard {
	if doer == nil {
		doer = NewClient()
	}
	return &Dashboard{BaseURL: baseURL, Token: token, Guard: guard, HTTP: doer}
}

type query [][2]string

// Status is status(): /api/status (public in both auth modes) plus the default cwd.
func (d *Dashboard) Status(ctx context.Context) (Status, error) {
	resp, err := d.get(ctx, "/api/status", nil, FileBodyCap)
	if err != nil {
		return Status{}, err
	}
	obj, ok := resp.Object()
	if !resp.IsSuccess() || !ok {
		return Status{}, HTTPStatus(resp.Status, "/api/status")
	}
	cwd, _ := d.DefaultCwd(ctx)
	return Status{
		Reachable: true, AuthRequired: boolPtr(obj, "auth_required"), Version: strPtr(obj, "version"),
		DefaultCwd: cwd, AdminConfigRW: AdminConfigRW(obj),
	}, nil
}

// ProbeAuthMode hits a protected route to tell the two dashboard auth modes apart.
func (d *Dashboard) ProbeAuthMode(ctx context.Context, authRequired *bool) AuthMode {
	resp, err := d.get(ctx, "/api/skills", nil, ListBodyCap)
	switch {
	case err != nil:
		return AuthUnreachable
	case resp.IsSuccess():
		return AuthBearer
	case resp.IsRedirect():
		return AuthOAuthOnly
	case resp.Status == 401 || resp.Status == 403:
		if authRequired != nil && *authRequired {
			return AuthOAuthOnly
		}
		return AuthUnauthorized
	default:
		return AuthUnreachable
	}
}

// DefaultCwd is GET /api/fs/default-cwd.
func (d *Dashboard) DefaultCwd(ctx context.Context) (*string, error) {
	resp, err := d.get(ctx, "/api/fs/default-cwd", nil, FileBodyCap)
	if err != nil {
		return nil, err
	}
	if !resp.IsSuccess() {
		return nil, nil
	}
	obj, _ := resp.Object()
	if v := strPtr(obj, "cwd"); v != nil {
		return v, nil
	}
	return strPtr(obj, "path"), nil
}

// ListSkills is GET /api/skills.
func (d *Dashboard) ListSkills(ctx context.Context) ([]Skill, error) {
	resp, err := d.get(ctx, "/api/skills", nil, ListBodyCap)
	if err != nil {
		return nil, err
	}
	if err := requireSuccess(resp, "/api/skills"); err != nil {
		return nil, err
	}
	rows, ok := asObjects(resp.JSON())
	if !ok {
		return nil, InvalidResponse("/api/skills")
	}
	skills := []Skill{}
	for _, row := range rows {
		if s, ok := ParseSkill(row); ok {
			skills = append(skills, s)
		}
	}
	return skills, nil
}

func (d *Dashboard) SkillContent(ctx context.Context, name string) (string, error) {
	resp, err := d.get(ctx, "/api/skills/content", query{{"name", name}}, FileBodyCap)
	if err != nil {
		return "", err
	}
	if resp.Status == 404 {
		return "", NotFound("skill:" + name)
	}
	if err := requireSuccess(resp, "/api/skills/content"); err != nil {
		return "", err
	}
	obj, _ := resp.Object()
	content, ok := str(obj, "content")
	if !ok {
		return "", InvalidResponse("/api/skills/content")
	}
	return content, nil
}

func (d *Dashboard) ToggleSkill(ctx context.Context, name string, enabled bool) error {
	resp, err := d.send(ctx, http.MethodPut, "/api/skills/toggle", nil, map[string]any{"name": name, "enabled": enabled}, FileBodyCap)
	if err != nil {
		return err
	}
	return requireSuccess(resp, "/api/skills/toggle")
}

func (d *Dashboard) CreateSkill(ctx context.Context, name, content string) error {
	resp, err := d.send(ctx, http.MethodPost, "/api/skills", nil, map[string]any{"name": name, "content": content}, FileBodyCap)
	if err != nil {
		return err
	}
	return requireSuccess(resp, "/api/skills")
}

// ListCronJobsRaw is the raw GET /api/cron/jobs body.
func (d *Dashboard) ListCronJobsRaw(ctx context.Context) ([]byte, error) {
	resp, err := d.get(ctx, "/api/cron/jobs", nil, ListBodyCap)
	if err != nil {
		return nil, err
	}
	if err := requireSuccess(resp, "/api/cron/jobs"); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// CreateCronJobRaw is the raw POST /api/cron/jobs response body.
func (d *Dashboard) CreateCronJobRaw(ctx context.Context, body map[string]any) ([]byte, error) {
	resp, err := d.send(ctx, http.MethodPost, "/api/cron/jobs", nil, body, FileBodyCap)
	if err != nil {
		return nil, err
	}
	if err := requireSuccess(resp, "/api/cron/jobs"); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (d *Dashboard) ListJobs(ctx context.Context) ([]Job, error) {
	raw, err := d.ListCronJobsRaw(ctx)
	if err != nil {
		return nil, err
	}
	return ParseJobs(raw), nil
}

// CreateJob falls back to re-listing and matching by name when Hermes answers
// {"ok": true} instead of the job.
func (d *Dashboard) CreateJob(ctx context.Context, spec JobSpec) (Job, error) {
	raw, err := d.CreateCronJobRaw(ctx, spec.Body())
	if err != nil {
		return Job{}, err
	}
	if obj, ok := asObject(decodeAny(raw)); ok {
		if job, ok := ParseJob(obj); ok {
			return job, nil
		}
	}
	jobs, err := d.ListJobs(ctx)
	if err != nil {
		return Job{}, err
	}
	for _, j := range jobs {
		if j.Name != nil && *j.Name == spec.Name {
			return j, nil
		}
	}
	return Job{}, InvalidResponse("/api/cron/jobs")
}

// UpdateJob is PUT /api/cron/jobs/{id} with {"updates": {...}}.
func (d *Dashboard) UpdateJob(ctx context.Context, id string, updates JobUpdate) (Job, error) {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return Job{}, err
	}
	resp, err := d.send(ctx, http.MethodPut, "/api/cron/jobs/"+jobID, nil, map[string]any{"updates": updates.Updates()}, FileBodyCap)
	if err != nil {
		return Job{}, err
	}
	return dashboardJobResponse(resp, "/api/cron/jobs/{id}", jobID)
}

func (d *Dashboard) PauseJob(ctx context.Context, id string) (Job, error) {
	return d.jobAction(ctx, id, "pause")
}

func (d *Dashboard) ResumeJob(ctx context.Context, id string) (Job, error) {
	return d.jobAction(ctx, id, "resume")
}

func (d *Dashboard) TriggerJob(ctx context.Context, id string) (Job, error) {
	return d.jobAction(ctx, id, "trigger")
}

func (d *Dashboard) DeleteJob(ctx context.Context, id string) error {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return err
	}
	resp, err := d.send(ctx, http.MethodDelete, "/api/cron/jobs/"+jobID, nil, nil, FileBodyCap)
	if err != nil {
		return err
	}
	if resp.Status == 404 {
		return NotFound("job:" + jobID)
	}
	return requireSuccess(resp, "/api/cron/jobs/{id}")
}

func (d *Dashboard) jobAction(ctx context.Context, id, action string) (Job, error) {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return Job{}, err
	}
	resp, err := d.send(ctx, http.MethodPost, "/api/cron/jobs/"+jobID+"/"+action, nil, nil, FileBodyCap)
	if err != nil {
		return Job{}, err
	}
	return dashboardJobResponse(resp, "/api/cron/jobs/{id}/"+action, jobID)
}

func dashboardJobResponse(resp Response, path, id string) (Job, error) {
	if resp.Status == 404 {
		return Job{}, NotFound("job:" + id)
	}
	if resp.Status == 400 {
		return Job{}, InvalidResponse(path + ":rejected")
	}
	if err := requireSuccess(resp, path); err != nil {
		return Job{}, err
	}
	obj, ok := resp.Object()
	if !ok {
		return Job{}, InvalidResponse(path)
	}
	job, ok := ParseJob(obj)
	if !ok {
		return Job{}, InvalidResponse(path)
	}
	return job, nil
}

// JobRuns is GET /api/cron/jobs/{id}/runs?limit=.
func (d *Dashboard) JobRuns(ctx context.Context, jobID string, limit int) ([]JobRun, error) {
	id, err := ValidateJobID(jobID)
	if err != nil {
		return nil, err
	}
	bounded := max(1, min(limit, 100))
	resp, err := d.get(ctx, "/api/cron/jobs/"+id+"/runs", query{{"limit", strconv.Itoa(bounded)}}, ListBodyCap)
	if err != nil {
		return nil, err
	}
	if resp.Status == 404 {
		obj, _ := resp.Object()
		if detail, _ := str(obj, "detail"); detail == "Job not found" {
			return nil, NotFound("job:" + id)
		}
		return nil, Unsupported("jobRuns")
	}
	if err := requireSuccess(resp, "/api/cron/jobs/{id}/runs"); err != nil {
		return nil, err
	}
	return ParseRuns(resp.JSON()), nil
}

// JobRunOutput is the run's final assistant message.
func (d *Dashboard) JobRunOutput(ctx context.Context, _ string, runKey string) (*string, error) {
	messages, err := d.SessionMessages(ctx, runKey)
	if err != nil {
		return nil, err
	}
	return FinalAssistantText(messages), nil
}

func (d *Dashboard) ListFiles(ctx context.Context, path string) ([]FileEntry, error) {
	validated, err := ValidatePath(path)
	if err != nil {
		return nil, err
	}
	resp, err := d.get(ctx, "/api/fs/list", query{{"path", validated}}, ListBodyCap)
	if err != nil {
		return nil, err
	}
	if err := requireSuccess(resp, "/api/fs/list"); err != nil {
		return nil, err
	}
	obj, ok := resp.Object()
	if !ok {
		return nil, InvalidResponse("/api/fs/list")
	}
	if e, ok := str(obj, "error"); ok {
		if e == "ENOENT" || e == "ENOTDIR" {
			return nil, NotFound(validated)
		}
		return nil, InvalidResponse("/api/fs/list:" + e)
	}
	entries, _ := asObjects(obj["entries"])
	out := []FileEntry{}
	for _, e := range entries {
		name, ok1 := str(e, "name")
		p, ok2 := str(e, "path")
		if !ok1 || !ok2 {
			continue
		}
		isDir, _ := e["isDirectory"].(bool)
		out = append(out, FileEntry{Name: name, Path: p, IsDirectory: isDir})
	}
	return out, nil
}

func (d *Dashboard) ReadText(ctx context.Context, path string) (string, error) {
	validated, err := ValidatePath(path)
	if err != nil {
		return "", err
	}
	resp, err := d.get(ctx, "/api/fs/read-text", query{{"path", validated}}, FileBodyCap)
	if err != nil {
		return "", err
	}
	if resp.Status == 404 {
		return "", NotFound(validated)
	}
	if resp.Status == 413 {
		return "", BodyTooLarge(validated, FileBodyCap)
	}
	if err := requireSuccess(resp, "/api/fs/read-text"); err != nil {
		return "", err
	}
	obj, _ := resp.Object()
	text, ok := str(obj, "text")
	if !ok {
		return "", InvalidResponse("/api/fs/read-text")
	}
	if b, _ := obj["binary"].(bool); b {
		return "", InvalidResponse("binary:" + validated)
	}
	return text, nil
}

func (d *Dashboard) WriteText(ctx context.Context, path, content string) error {
	validated, err := ValidatePath(path)
	if err != nil {
		return err
	}
	if len(content) > FileBodyCap {
		return BodyTooLarge(validated, FileBodyCap)
	}
	resp, err := d.send(ctx, http.MethodPost, "/api/fs/write-text", nil, map[string]any{"path": validated, "content": content}, FileBodyCap)
	if err != nil {
		return err
	}
	return requireSuccess(resp, "/api/fs/write-text")
}

func (d *Dashboard) Mkdir(ctx context.Context, path string) error {
	validated, err := ValidatePath(path)
	if err != nil {
		return err
	}
	resp, err := d.send(ctx, http.MethodPost, "/api/files/mkdir", nil, map[string]any{"path": validated}, FileBodyCap)
	if err != nil {
		return err
	}
	return requireSuccess(resp, "/api/files/mkdir")
}

func sessionQuery(offset, limit int) query {
	return query{{"limit", strconv.Itoa(limit)}, {"offset", strconv.Itoa(offset)}, {"order", "recent"}, {"min_messages", "1"}}
}

func (d *Dashboard) ListSessions(ctx context.Context, offset, limit int) (SessionPage, error) {
	resp, err := d.get(ctx, "/api/sessions", sessionQuery(offset, limit), ListBodyCap)
	if err != nil {
		return SessionPage{}, err
	}
	if err := requireSuccess(resp, "/api/sessions"); err != nil {
		return SessionPage{}, err
	}
	return ParseSessions(resp.JSON()), nil
}

func (d *Dashboard) SessionMessages(ctx context.Context, id string) ([]Message, error) {
	resp, err := d.get(ctx, "/api/sessions/"+escapePath(id)+"/messages", query{{"limit", "500"}}, ListBodyCap)
	if err != nil {
		return nil, err
	}
	if resp.Status == 404 {
		return nil, NotFound("session:" + id)
	}
	if err := requireSuccess(resp, "/api/sessions/{id}/messages"); err != nil {
		return nil, err
	}
	return ParseMessages(resp.JSON()), nil
}

// GitStatus is /api/git/status.
func (d *Dashboard) GitStatus(ctx context.Context, path string) (Repo, error) {
	validated, err := ValidatePath(path)
	if err != nil {
		return Repo{}, err
	}
	obj, err := d.getObject(ctx, "/api/git/status", query{{"path", validated}})
	if err != nil {
		return Repo{}, err
	}
	var branch *string
	if b, ok := str(obj, "branch"); ok && b != "" {
		branch = &b
	}
	root := validated
	if r, ok := str(obj, "root"); ok {
		root = r
	}
	detached := branch == nil
	if b, ok := obj["detached"].(bool); ok {
		detached = b
	}
	return Repo{Root: root, Branch: branch, IsDetached: detached}, nil
}

func (d *Dashboard) GitBranches(ctx context.Context, path string) ([]Branch, error) {
	validated, err := ValidatePath(path)
	if err != nil {
		return nil, err
	}
	obj, err := d.getObject(ctx, "/api/git/branches", query{{"path", validated}})
	if err != nil {
		return nil, err
	}
	current := strPtr(obj, "current")
	isCurrent := func(name string) bool { return current != nil && *current == name }
	rows, _ := obj["branches"].([]any)
	out := []Branch{}
	for _, row := range rows {
		if name, ok := row.(string); ok {
			out = append(out, Branch{Name: name, IsCurrent: isCurrent(name)})
			continue
		}
		m, ok := row.(map[string]any)
		if !ok {
			continue
		}
		name, ok := str(m, "name")
		if !ok {
			continue
		}
		cur := isCurrent(name)
		if b, ok := m["current"].(bool); ok {
			cur = b
		}
		out = append(out, Branch{Name: name, IsCurrent: cur})
	}
	return out, nil
}

func (d *Dashboard) GitWorktrees(ctx context.Context, path string) ([]Worktree, error) {
	validated, err := ValidatePath(path)
	if err != nil {
		return nil, err
	}
	obj, err := d.getObject(ctx, "/api/git/worktrees", query{{"path", validated}})
	if err != nil {
		return nil, err
	}
	rows, _ := asObjects(obj["worktrees"])
	out := []Worktree{}
	for _, row := range rows {
		p, ok := str(row, "path")
		if !ok {
			if p, ok = str(row, "worktree"); !ok {
				continue
			}
		}
		var branch *string
		if b, ok := str(row, "branch"); ok && b != "" {
			branch = &b
		}
		primary := p == validated
		if b, ok := row["primary"].(bool); ok {
			primary = b
		}
		out = append(out, Worktree{Path: p, Branch: branch, IsPrimary: primary})
	}
	return out, nil
}

func (d *Dashboard) GitChangedFiles(ctx context.Context, path string) ([]ChangedFile, error) {
	validated, err := ValidatePath(path)
	if err != nil {
		return nil, err
	}
	obj, err := d.getObject(ctx, "/api/git/review/list", query{{"path", validated}})
	if err != nil {
		return nil, err
	}
	rows, _ := asObjects(obj["files"])
	out := []ChangedFile{}
	for _, row := range rows {
		p, ok := str(row, "path")
		if !ok {
			continue
		}
		added, _ := exactInt(row["added"])
		removed, _ := exactInt(row["removed"])
		status := "modified"
		if s, ok := str(row, "status"); ok {
			status = s
		}
		out = append(out, ChangedFile{Path: p, Added: added, Removed: removed, Status: status})
	}
	return out, nil
}

func (d *Dashboard) GitFileDiff(ctx context.Context, repoPath, file string) (FileDiff, error) {
	repo, err := ValidatePath(repoPath)
	if err != nil {
		return FileDiff{}, err
	}
	rel, err := ValidateRelative(file)
	if err != nil {
		return FileDiff{}, err
	}
	resp, err := d.get(ctx, "/api/git/file-diff", query{{"path", repo}, {"file", rel}}, FileBodyCap)
	if err != nil {
		return FileDiff{}, err
	}
	if err := requireSuccess(resp, "/api/git/file-diff"); err != nil {
		return FileDiff{}, err
	}
	var text string
	if obj, ok := resp.Object(); ok {
		if e, ok := str(obj, "error"); ok {
			return FileDiff{}, InvalidResponse("/api/git/file-diff:" + e)
		}
		if t, ok := str(obj, "diff"); ok {
			text = t
		} else if t, ok := str(obj, "patch"); ok {
			text = t
		}
	} else {
		text = strings.ToValidUTF8(string(resp.Body), "�")
	}
	return FileDiff{Path: rel, Diff: text, Truncated: len(resp.Body) >= FileBodyCap}, nil
}

func (d *Dashboard) getObject(ctx context.Context, path string, q query) (map[string]any, error) {
	resp, err := d.get(ctx, path, q, ListBodyCap)
	if err != nil {
		return nil, err
	}
	if err := requireSuccess(resp, path); err != nil {
		return nil, err
	}
	obj, ok := resp.Object()
	if !ok {
		return nil, InvalidResponse(path)
	}
	if e, ok := str(obj, "error"); ok {
		return nil, InvalidResponse(path + ":" + e)
	}
	return obj, nil
}

func (d *Dashboard) get(ctx context.Context, path string, q query, cap int) (Response, error) {
	return d.send(ctx, http.MethodGet, path, q, nil, cap)
}

// send validates the base URL on every call (DNS can rebind), sends both token
// headers, and retries a 429 up to three attempts in all.
func (d *Dashboard) send(ctx context.Context, method, path string, q query, body map[string]any, cap int) (Response, error) {
	validated, err := d.Guard.Validate(ctx, d.BaseURL)
	if err != nil {
		return Response{}, err
	}
	u := *validated
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath, u.Fragment = "", ""
	u.RawQuery = encodeQuery(q)
	target := u.String()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+d.Token)
	header.Set("X-Hermes-Session-Token", d.Token)
	header.Set("Accept", "application/json")
	var payload any
	if body != nil {
		header.Set("Content-Type", "application/json")
		payload = body
	}
	sleep := d.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	for attempt := 1; ; attempt++ {
		resp, err := execute(ctx, d.HTTP, method, target, header, payload, Timeout, cap)
		if err != nil {
			if te := (*Error)(nil); !errors.As(err, &te) {
				slog.DebugContext(ctx, "hermes dashboard request failed", "path", path)
			}
			return Response{}, asTransportError(err, path)
		}
		if resp.Status == http.StatusTooManyRequests && attempt < MaxRetries {
			if err := sleep(ctx, RetryDelay(resp.Header.Get("Retry-After"), attempt)); err != nil {
				return Response{}, err
			}
			continue
		}
		return resp, nil
	}
}

// RetryDelay is retryDelay: Retry-After seconds (capped at 30), else attempt seconds.
func RetryDelay(retryAfter string, attempt int) time.Duration {
	if seconds, err := strconv.ParseFloat(retryAfter, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 30) * float64(time.Second))
	}
	return time.Duration(attempt) * time.Second
}

func requireSuccess(resp Response, path string) error {
	switch {
	case resp.IsSuccess():
		return nil
	case resp.IsRedirect():
		return ErrAuthModeUnsupported
	case resp.Status == 401 || resp.Status == 403:
		return ErrUnauthorized
	default:
		return HTTPStatus(resp.Status, path)
	}
}

// encodeQuery is URLComponents.queryItems: names and values percent-encoded
// with the URL query allowed set, minus the delimiters "&", "=" and "#".
func encodeQuery(q query) string {
	if len(q) == 0 {
		return ""
	}
	parts := make([]string, 0, len(q))
	for _, kv := range q {
		parts = append(parts, escapeQuery(kv[0])+"="+escapeQuery(kv[1]))
	}
	return strings.Join(parts, "&")
}

func escapeQuery(s string) string {
	return escapeWith(s, func(c byte) bool {
		return isUnreservedByte(c) || strings.IndexByte("!$'()*+,;:@/?", c) >= 0
	})
}

// escapePath is addingPercentEncoding(withAllowedCharacters: .urlPathAllowed).
func escapePath(s string) string {
	return escapeWith(s, func(c byte) bool {
		return isUnreservedByte(c) || strings.IndexByte("!$&'()*+,;=:@/", c) >= 0
	})
}

func isUnreservedByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func escapeWith(s string, allowed func(byte) bool) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if allowed(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
