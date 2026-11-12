// Package hermesdash is P2 of the Go migration: the mirror transports over a
// tenant's Hermes. It ports Hermes/Mirror/HermesMirrorTransport.swift (the
// transport seam, its value types, errors, path and date rules),
// HermesDashboardClient.swift, RemoteHermesTransport.swift,
// FilesystemHermesTransport.swift (with HermesGatewayReadClient),
// HermesMirrorTransportFactory.swift and Plugins/HermesSkillsClient.swift.
//
// The mirror service (internal/hermesmirror) and the capabilities probe
// (internal/hermescaps) both sit on top of it.
package hermesdash

import (
	"context"
	"fmt"
	"lumina/internal/swiftstr"
	"strings"
	"time"
	"unicode"
)

// TransportKind is HermesMirrorTransportKind.
type TransportKind string

const (
	KindManaged TransportKind = "managed"
	KindRemote  TransportKind = "remote"
	KindNone    TransportKind = "none"
)

// Transport is HermesMirrorTransport.
type Transport interface {
	Kind() TransportKind
	Status(ctx context.Context) (Status, error)
	ListSkills(ctx context.Context) ([]Skill, error)
	SkillContent(ctx context.Context, name string) (string, error)
	ToggleSkill(ctx context.Context, name string, enabled bool) error
	CreateSkill(ctx context.Context, name, content string) error
	ListJobs(ctx context.Context) ([]Job, error)
	CreateJob(ctx context.Context, spec JobSpec) (Job, error)
	UpdateJob(ctx context.Context, id string, updates JobUpdate) (Job, error)
	PauseJob(ctx context.Context, id string) (Job, error)
	ResumeJob(ctx context.Context, id string) (Job, error)
	TriggerJob(ctx context.Context, id string) (Job, error)
	DeleteJob(ctx context.Context, id string) error
	JobRuns(ctx context.Context, jobID string, limit int) ([]JobRun, error)
	JobRunOutput(ctx context.Context, jobID, runKey string) (*string, error)
	ListFiles(ctx context.Context, path string) ([]FileEntry, error)
	ReadText(ctx context.Context, path string) (string, error)
	WriteText(ctx context.Context, path, content string) error
	Mkdir(ctx context.Context, path string) error
	ListSessions(ctx context.Context, offset, limit int) (SessionPage, error)
	SessionMessages(ctx context.Context, id string) ([]Message, error)
}

// Status is HermesDashboardStatus.
type Status struct {
	Reachable     bool
	AuthRequired  *bool
	Version       *string
	DefaultCwd    *string
	AdminConfigRW *bool
}

// Skill is HermesMirrorSkill. Source is a HermesMirroredSkillSource raw value.
type Skill struct {
	Name        string
	Description string
	Enabled     bool
	Source      string
	ContentHash *string
}

// Job is HermesMirrorJob. Raw is the source document as JSON (the jsonb column).
type Job struct {
	ID        string
	Name      *string
	Schedule  *string
	Prompt    *string
	Paused    bool
	LastRunAt *time.Time
	NextRunAt *time.Time
	Raw       []byte
}

// JobRun is HermesMirrorJobRun. Status is a HermesJobRunStatus raw value.
type JobRun struct {
	Key        string
	Status     string
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      *string
	TokensIn   *int
	TokensOut  *int
}

// Run statuses (HermesJobRunStatus).
const (
	RunRunning = "running"
	RunOK      = "ok"
	RunError   = "error"
)

// FileEntry is HermesMirrorFileEntry.
type FileEntry struct {
	Name        string
	Path        string
	IsDirectory bool
}

// Session is HermesMirrorSession.
type Session struct {
	ID           string
	Title        *string
	Source       *string
	StartedAt    *time.Time
	LastActiveAt *time.Time
	MessageCount int
}

// SessionPage is HermesMirrorSessionPage.
type SessionPage struct {
	Sessions []Session
	Total    *int
}

// Message is HermesMirrorSessionMessage.
type Message struct {
	Role      string
	Content   string
	Timestamp *time.Time
}

// JobSpec is HermesMirrorJobSpec: the full CronJobCreate body.
type JobSpec struct {
	Name, Schedule, Prompt, Deliver string
	Skills                          []string
	Model, Provider, BaseURL        *string
	Script                          *string
	ContextFrom, EnabledToolsets    []string
	HasContextFrom, HasToolsets     bool
	Workdir                         *string
	NoAgent                         bool
}

// Body is the wire body for POST /api/cron/jobs; nil optionals are left out.
func (s JobSpec) Body() map[string]any {
	body := map[string]any{
		"name": s.Name, "schedule": s.Schedule, "prompt": s.Prompt, "deliver": s.Deliver,
		"skills": nonNil(s.Skills), "no_agent": s.NoAgent,
	}
	putString(body, "model", s.Model)
	putString(body, "provider", s.Provider)
	putString(body, "base_url", s.BaseURL)
	putString(body, "script", s.Script)
	if s.HasContextFrom {
		body["context_from"] = nonNil(s.ContextFrom)
	}
	if s.HasToolsets {
		body["enabled_toolsets"] = nonNil(s.EnabledToolsets)
	}
	putString(body, "workdir", s.Workdir)
	return body
}

// JobUpdate is HermesMirrorJobUpdate: only set fields are sent.
type JobUpdate struct {
	Name, Schedule, Prompt, Deliver *string
	Skills                          *[]string
	Model, Provider, BaseURL        *string
	Script                          *string
	ContextFrom, EnabledToolsets    *[]string
	Workdir                         *string
	NoAgent, Enabled                *bool
}

// Updates is the Hermes-side field names to values.
func (u JobUpdate) Updates() map[string]any {
	m := map[string]any{}
	putString(m, "name", u.Name)
	putString(m, "schedule", u.Schedule)
	putString(m, "prompt", u.Prompt)
	putString(m, "deliver", u.Deliver)
	if u.Skills != nil {
		m["skills"] = nonNil(*u.Skills)
	}
	putString(m, "model", u.Model)
	putString(m, "provider", u.Provider)
	putString(m, "base_url", u.BaseURL)
	putString(m, "script", u.Script)
	if u.ContextFrom != nil {
		m["context_from"] = nonNil(*u.ContextFrom)
	}
	if u.EnabledToolsets != nil {
		m["enabled_toolsets"] = nonNil(*u.EnabledToolsets)
	}
	putString(m, "workdir", u.Workdir)
	if u.NoAgent != nil {
		m["no_agent"] = *u.NoAgent
	}
	if u.Enabled != nil {
		m["enabled"] = *u.Enabled
	}
	return m
}

func putString(m map[string]any, key string, v *string) {
	if v != nil {
		m[key] = *v
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Workspace shapes (HermesWorkspace).
type (
	Repo struct {
		Root       string
		Branch     *string
		IsDetached bool
	}
	Branch struct {
		Name      string
		IsCurrent bool
	}
	Worktree struct {
		Path      string
		Branch    *string
		IsPrimary bool
	}
	ChangedFile struct {
		Path           string
		Added, Removed int
		Status         string
	}
	FileDiff struct {
		Path      string
		Diff      string
		Truncated bool
	}
)

// ErrorKind is a HermesMirrorTransportError case.
type ErrorKind int

const (
	ErrUnsupported ErrorKind = iota
	ErrHTTP
	ErrBodyTooLarge
	ErrInvalidPath
	ErrNotFound
	ErrInvalidResponse
	ErrDashboardAuthModeUnsupported
	ErrDashboardUnauthorized
	ErrDashboardUnreachable
	ErrNotConfigured
)

// Error is HermesMirrorTransportError.
type Error struct {
	Kind ErrorKind
	// Detail is the case's string payload: the op, path, what or reason.
	Detail string
	Status int
	Limit  int
}

func Unsupported(op string) *Error { return &Error{Kind: ErrUnsupported, Detail: op} }
func HTTPStatus(status int, path string) *Error {
	return &Error{Kind: ErrHTTP, Status: status, Detail: path}
}

func BodyTooLarge(path string, limit int) *Error {
	return &Error{Kind: ErrBodyTooLarge, Detail: path, Limit: limit}
}
func InvalidPath(path string) *Error     { return &Error{Kind: ErrInvalidPath, Detail: path} }
func NotFound(what string) *Error        { return &Error{Kind: ErrNotFound, Detail: what} }
func InvalidResponse(what string) *Error { return &Error{Kind: ErrInvalidResponse, Detail: what} }
func DashboardUnreachable(why string) *Error {
	return &Error{Kind: ErrDashboardUnreachable, Detail: why}
}

var (
	ErrAuthModeUnsupported = &Error{Kind: ErrDashboardAuthModeUnsupported}
	ErrUnauthorized        = &Error{Kind: ErrDashboardUnauthorized}
	ErrNotConfiguredErr    = &Error{Kind: ErrNotConfigured}
)

// Error is description.
func (e *Error) Error() string {
	switch e.Kind {
	case ErrUnsupported:
		return "hermes_mirror_unsupported:" + e.Detail
	case ErrHTTP:
		return fmt.Sprintf("hermes_mirror_http_%d:%s", e.Status, e.Detail)
	case ErrBodyTooLarge:
		return fmt.Sprintf("hermes_mirror_body_too_large:%s:%d", e.Detail, e.Limit)
	case ErrInvalidPath:
		return "hermes_mirror_invalid_path:" + e.Detail
	case ErrNotFound:
		return "hermes_mirror_not_found:" + e.Detail
	case ErrInvalidResponse:
		return "hermes_mirror_invalid_response:" + e.Detail
	case ErrDashboardAuthModeUnsupported:
		return "hermes_dashboard_auth_mode_unsupported"
	case ErrDashboardUnauthorized:
		return "hermes_dashboard_unauthorized"
	case ErrDashboardUnreachable:
		return "hermes_dashboard_unreachable:" + e.Detail
	default:
		return "hermes_mirror_not_configured"
	}
}

// Code is the stable client-facing code (no host names, no reasons).
func (e *Error) Code() string {
	switch e.Kind {
	case ErrUnsupported:
		return "hermes_mirror_unsupported"
	case ErrHTTP:
		return "hermes_mirror_upstream_error"
	case ErrBodyTooLarge:
		return "hermes_mirror_body_too_large"
	case ErrInvalidPath:
		return "hermes_mirror_invalid_path"
	case ErrNotFound:
		return "hermes_mirror_not_found"
	case ErrInvalidResponse:
		return "hermes_mirror_invalid_response"
	case ErrDashboardAuthModeUnsupported:
		return "hermes_dashboard_auth_mode_unsupported"
	case ErrDashboardUnauthorized:
		return "hermes_dashboard_unauthorized"
	case ErrDashboardUnreachable:
		return "hermes_dashboard_unreachable"
	default:
		return "hermes_mirror_not_configured"
	}
}

// IsTenantWide says the operation cannot work for this tenant at all.
func (e *Error) IsTenantWide() bool {
	switch e.Kind {
	case ErrUnsupported, ErrDashboardAuthModeUnsupported, ErrDashboardUnauthorized, ErrNotConfigured:
		return true
	default:
		return false
	}
}

// ValidatePath is HermesMirrorPath.validate: absolute, no NUL, no "." or ".."
// segments; trailing slashes dropped.
func ValidatePath(raw string) (string, error) {
	path := swiftstr.TrimWhitespacesAndNewlines(raw)
	if path == "" || !strings.HasPrefix(path, "/") || strings.Contains(path, "\x00") {
		return "", InvalidPath(raw)
	}
	for _, seg := range strings.Split(path, "/")[1:] {
		if seg == "." || seg == ".." {
			return "", InvalidPath(raw)
		}
	}
	for len(path) > 1 && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}
	return path, nil
}

// ValidateRelative is HermesMirrorPath.validateRelative.
func ValidateRelative(raw string) (string, error) {
	path := swiftstr.TrimWhitespacesAndNewlines(raw)
	if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "\x00") {
		return "", InvalidPath(raw)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "." || seg == ".." || seg == "" {
			return "", InvalidPath(raw)
		}
	}
	return path, nil
}

// JoinPath is HermesMirrorPath.join.
func JoinPath(base, component string) string {
	if strings.HasSuffix(base, "/") {
		return base + component
	}
	return base + "/" + component
}

// IsInside is HermesMirrorPath.isInside.
func IsInside(path, root string) bool {
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return path == root || strings.HasPrefix(path, prefix)
}

// ValidateJobID is HermesJobID.validate: one path component of letters,
// numbers, "-", "_" or ".", at most 128 characters.
func ValidateJobID(raw string) (string, error) {
	id := swiftstr.TrimWhitespacesAndNewlines(raw)
	if id == "" || swiftstr.Count(id) > 128 || id == "." || id == ".." {
		return "", InvalidPath("job:" + raw)
	}
	for _, c := range swiftstr.Characters(id) {
		if c == "-" || c == "_" || c == "." || swiftstr.IsLetterOrNumber(c) {
			continue
		}
		return "", InvalidPath("job:" + raw)
	}
	return id, nil
}

// isSafeSkillName is FilesystemHermesTransport.isSafeSkillName.
func isSafeSkillName(name string) bool {
	if name == "" || swiftstr.Count(name) > 128 || strings.HasPrefix(name, ".") {
		return false
	}
	for _, c := range swiftstr.Characters(name) {
		if c == "-" || c == "_" || swiftstr.IsLetterOrNumber(c) {
			continue
		}
		return false
	}
	return true
}

// ParseDate is HermesDates.parse: epoch seconds (> 0) or an ISO-8601 string.
func ParseDate(v any) *time.Time {
	switch x := v.(type) {
	case float64:
		if x > 0 {
			t := epoch(x)
			return &t
		}
	case bool: // JSONSerialization boxes a bool in an NSNumber
		if x {
			t := epoch(1)
			return &t
		}
	case string:
		return ParseISO(x)
	}
	return nil
}

func epoch(seconds float64) time.Time {
	whole := int64(seconds)
	return time.Unix(whole, int64((seconds-float64(whole))*1e9)).UTC()
}

// ParseISO is HermesDates.parseISO: ISO-8601 with or without fraction and
// offset, or Python isoformat() without an offset (read as UTC).
func ParseISO(raw string) *time.Time {
	s := strings.TrimFunc(raw, func(r rune) bool { return r == ' ' || r == '\t' || unicode.Is(unicode.Zs, r) })
	if s == "" {
		return nil
	}
	for _, candidate := range []string{s, s + "Z"} {
		if t, err := time.Parse(time.RFC3339Nano, candidate); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// ISO is HermesDates.iso: ISO-8601 with milliseconds, UTC.
func ISO(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z07:00")
}
