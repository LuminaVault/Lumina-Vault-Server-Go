package hermesdash

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/cronexpr"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Filesystem is FilesystemHermesTransport: the managed Hermes and the API pod
// share one volume, so skills, cron jobs, config and the KB vault are files
// under the Hermes home. Sessions live in Hermes' SQLite, so they come from
// the tenant's gateway; without one they are unsupported.
type Filesystem struct {
	Root     string
	Sessions *Gateway
	Now      func() time.Time
}

// NewFilesystem builds the transport rooted at rootPath (standardized).
func NewFilesystem(rootPath string, sessions *Gateway) *Filesystem {
	return &Filesystem{Root: standardize(rootPath), Sessions: sessions, Now: time.Now}
}

// standardize is URL(fileURLWithPath:).standardizedFileURL.path: "." and ".."
// resolved, no trailing slash.
func standardize(p string) string {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		if wd, err := os.Getwd(); err == nil {
			p = filepath.Join(wd, p)
		}
	}
	return filepath.Clean(p)
}

func (f *Filesystem) Kind() TransportKind { return KindManaged }

func (f *Filesystem) skillsRoot() string   { return filepath.Join(f.Root, "skills") }
func (f *Filesystem) configPath() string   { return filepath.Join(f.Root, "config.yaml") }
func (f *Filesystem) cronJobsPath() string { return filepath.Join(f.Root, "cron", "jobs.json") }

func (f *Filesystem) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Status: a managed Hermes keeps its config on the volume we write to, so
// admin config read/write is the transport itself.
func (f *Filesystem) Status(context.Context) (Status, error) {
	exists := isDir(f.Root)
	root := f.Root
	authRequired := false
	return Status{Reachable: exists, AuthRequired: &authRequired, DefaultCwd: &root, AdminConfigRW: &exists}, nil
}

// sortedNames is contentsOfDirectory(atPath:).sorted() (byte order of the names).
func sortedNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (f *Filesystem) ListSkills(context.Context) ([]Skill, error) {
	root := f.skillsRoot()
	if _, err := os.Stat(root); err != nil {
		return []Skill{}, nil
	}
	disabled := readDisabledSkills(f.configPath())
	names, err := sortedNames(root)
	if err != nil {
		return nil, err
	}
	skills := []Skill{}
	for _, name := range names {
		if strings.HasPrefix(name, ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name, "SKILL.md")) //nolint:gosec // name is a directory entry of the skills root
		if err != nil || !utf8.Valid(data) {
			continue
		}
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		skills = append(skills, Skill{
			Name: name, Description: FrontmatterDescription(string(data)),
			Enabled: !disabled[name], Source: "custom", ContentHash: &digest,
		})
	}
	return skills, nil
}

func (f *Filesystem) skillFile(name string) (string, error) {
	if !isSafeSkillName(name) {
		return "", InvalidPath("skill:" + name)
	}
	return filepath.Join(f.skillsRoot(), name, "SKILL.md"), nil
}

func (f *Filesystem) SkillContent(_ context.Context, name string) (string, error) {
	file, err := f.skillFile(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(file) //nolint:gosec // resolved inside the Hermes root
	if err != nil || !utf8.Valid(data) {
		return "", NotFound("skill:" + name)
	}
	return string(data), nil
}

func (f *Filesystem) ToggleSkill(_ context.Context, name string, enabled bool) error {
	if _, err := f.skillFile(name); err != nil {
		return err
	}
	config := loadConfig(f.configPath())
	section, _ := config["skills"].(map[string]any)
	if section == nil {
		section = map[string]any{}
	}
	disabled := map[string]bool{}
	if list, ok := section["disabled"].([]any); ok {
		all := true
		names := []string{}
		for _, v := range list {
			s, ok := v.(string)
			if !ok {
				all = false
				break
			}
			names = append(names, s)
		}
		if all {
			for _, n := range names {
				disabled[n] = true
			}
		}
	}
	if enabled {
		delete(disabled, name)
	} else {
		disabled[name] = true
	}
	sortedList := make([]string, 0, len(disabled))
	for n := range disabled {
		sortedList = append(sortedList, n)
	}
	sort.Strings(sortedList)
	section["disabled"] = sortedList
	config["skills"] = section
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(config); err != nil {
		return err
	}
	_ = enc.Close()
	return atomicWrite(buf.Bytes(), f.configPath())
}

func (f *Filesystem) CreateSkill(_ context.Context, name, content string) error {
	file, err := f.skillFile(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil { //nolint:gosec // Hermes reads these as another user, as Swift writes them
		return err
	}
	return atomicWrite([]byte(content), file)
}

// FrontmatterDescription is the frontmatter's description: line, else the first
// non-heading, non-key line.
func FrontmatterDescription(content string) string {
	lines := strings.Split(content, "\n")
	trimSpace := func(s string) string { return strings.Trim(s, " \t") }
	if len(lines) > 0 && trimSpace(lines[0]) == "---" {
		lines = lines[1:]
		for _, line := range lines {
			if trimSpace(line) == "---" {
				break
			}
			if strings.HasPrefix(line, "description:") {
				return trimSpace(strings.TrimPrefix(line, "description:"))
			}
		}
	}
	for _, line := range lines {
		t := trimSpace(line)
		if t != "" && !strings.HasPrefix(t, "#") && t != "---" && !strings.Contains(t, ":") {
			return t
		}
	}
	return ""
}

func loadConfig(path string) map[string]any {
	data, err := os.ReadFile(path) //nolint:gosec // resolved inside the Hermes root
	if err != nil || !utf8.Valid(data) {
		return map[string]any{}
	}
	var loaded map[string]any
	if yaml.Unmarshal(data, &loaded) != nil || loaded == nil {
		return map[string]any{}
	}
	return loaded
}

func readDisabledSkills(path string) map[string]bool {
	section, _ := loadConfig(path)["skills"].(map[string]any)
	out := map[string]bool{}
	list, ok := section["disabled"].([]any)
	if !ok {
		return out
	}
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return map[string]bool{}
		}
		out[s] = true
	}
	return out
}

// ListJobs reads cron/jobs.json.
func (f *Filesystem) ListJobs(context.Context) ([]Job, error) {
	data, err := os.ReadFile(f.cronJobsPath())
	if err != nil {
		return []Job{}, nil
	}
	return ParseJobs(data), nil
}

// CronParseError is CronExpression.ParseError: not a transport error, so the
// route answers 500 as Swift's does.
type CronParseError struct{ Raw string }

func (e *CronParseError) Error() string { return "invalid cron expression: " + e.Raw }

func parseCron(raw string) (cronexpr.Expression, error) {
	expr, ok := cronexpr.Parse(raw)
	if !ok {
		return expr, &CronParseError{Raw: raw}
	}
	return expr, nil
}

func newJobID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])[:12]
}

func optString(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func optList(has bool, v []string) any {
	if !has {
		return nil
	}
	return nonNil(v)
}

// CreateJob appends a job document in the shape cron/jobs.py writes.
func (f *Filesystem) CreateJob(_ context.Context, spec JobSpec) (Job, error) {
	expr, err := parseCron(spec.Schedule)
	if err != nil {
		return Job{}, err
	}
	now := f.now()
	var next any
	if n := NextRun(now, expr); n != nil {
		next = ISO(*n)
	}
	document := map[string]any{
		"id": newJobID(), "name": spec.Name, "prompt": spec.Prompt, "skills": nonNil(spec.Skills),
		"skill": nil, "model": optString(spec.Model), "provider": optString(spec.Provider),
		"base_url": optString(spec.BaseURL), "script": optString(spec.Script), "no_agent": spec.NoAgent,
		"context_from":     optList(spec.HasContextFrom, spec.ContextFrom),
		"enabled_toolsets": optList(spec.HasToolsets, spec.EnabledToolsets),
		"workdir":          optString(spec.Workdir), "deliver": spec.Deliver,
		"schedule":         map[string]any{"kind": "cron", "expr": spec.Schedule, "display": spec.Schedule},
		"schedule_display": spec.Schedule,
		"repeat":           map[string]any{"times": nil, "completed": 0},
		"enabled":          true, "state": "scheduled", "paused_at": nil, "paused_reason": nil,
		"created_at": ISO(now), "next_run_at": next, "last_run_at": nil, "last_status": nil, "last_error": nil,
	}
	// Round-trip through JSON so the stored document holds JSON types only.
	encoded, err := json.Marshal(document)
	if err != nil {
		return Job{}, err
	}
	return f.mutateJobs(now, func(jobs *[]map[string]any) (Job, error) {
		var doc map[string]any
		if json.Unmarshal(encoded, &doc) != nil {
			return Job{}, InvalidResponse("jobs.json")
		}
		*jobs = append(*jobs, doc)
		job, ok := ParseJob(doc)
		if !ok {
			return Job{}, InvalidResponse("jobs.json")
		}
		return job, nil
	})
}

func indexOfJob(jobs []map[string]any, id string) int {
	for i, j := range jobs {
		if s, ok := j["id"].(string); ok && s == id {
			return i
		}
	}
	return -1
}

// UpdateJob mirrors cron/jobs.py:update_job: immutable id, schedule re-derived,
// enabled false pauses.
func (f *Filesystem) UpdateJob(_ context.Context, id string, updates JobUpdate) (Job, error) {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return Job{}, err
	}
	now := f.now()
	var schedulePatch map[string]any
	if updates.Schedule != nil {
		expr, err := parseCron(*updates.Schedule)
		if err != nil {
			return Job{}, err
		}
		var next any
		if n := NextRun(now, expr); n != nil {
			next = ISO(*n)
		}
		s := *updates.Schedule
		schedulePatch = map[string]any{
			"schedule":         map[string]any{"kind": "cron", "expr": s, "display": s},
			"schedule_display": s, "next_run_at": next,
		}
	}
	encoded, _ := json.Marshal(updates.Updates())
	var fields map[string]any
	_ = json.Unmarshal(encoded, &fields)
	return f.mutateJobs(now, func(jobs *[]map[string]any) (Job, error) {
		index := indexOfJob(*jobs, jobID)
		if index < 0 {
			return Job{}, NotFound("job:" + jobID)
		}
		job := (*jobs)[index]
		for k, v := range fields {
			if k != "schedule" && k != "id" {
				job[k] = v
			}
		}
		if _, ok := fields["skills"]; ok {
			job["skill"] = nil
		}
		for k, v := range schedulePatch {
			job[k] = v
		}
		if enabled, ok := fields["enabled"].(bool); ok {
			if enabled {
				job["state"], job["paused_at"], job["paused_reason"] = "scheduled", nil, nil
			} else {
				job["state"], job["paused_at"] = "paused", ISO(now)
			}
		}
		(*jobs)[index] = job
		parsed, ok := ParseJob(job)
		if !ok {
			return Job{}, InvalidResponse("jobs.json")
		}
		return parsed, nil
	})
}

func (f *Filesystem) PauseJob(_ context.Context, id string) (Job, error) {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return Job{}, err
	}
	now := f.now()
	return f.mutateJobs(now, func(jobs *[]map[string]any) (Job, error) {
		return patchJob(jobs, jobID, func(job map[string]any) {
			job["enabled"], job["state"], job["paused_at"], job["paused_reason"] = false, "paused", ISO(now), nil
		})
	})
}

func (f *Filesystem) ResumeJob(_ context.Context, id string) (Job, error) {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return Job{}, err
	}
	now := f.now()
	return f.mutateJobs(now, func(jobs *[]map[string]any) (Job, error) {
		return patchJob(jobs, jobID, func(job map[string]any) {
			job["enabled"], job["state"], job["paused_at"], job["paused_reason"] = true, "scheduled", nil, nil
			if sched, ok := job["schedule"].(map[string]any); ok {
				if raw, ok := sched["expr"].(string); ok {
					if expr, ok := cronexpr.Parse(raw); ok {
						if next := NextRun(now, expr); next != nil {
							job["next_run_at"] = ISO(*next)
						}
					}
				}
			}
		})
	})
}

func (f *Filesystem) TriggerJob(_ context.Context, id string) (Job, error) {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return Job{}, err
	}
	now := f.now()
	return f.mutateJobs(now, func(jobs *[]map[string]any) (Job, error) {
		return patchJob(jobs, jobID, func(job map[string]any) {
			job["enabled"], job["state"], job["paused_at"], job["paused_reason"] = true, "scheduled", nil, nil
			job["next_run_at"] = ISO(now)
		})
	})
}

// DeleteJob removes the job and its cron/output/<id>/ directory.
func (f *Filesystem) DeleteJob(_ context.Context, id string) error {
	jobID, err := ValidateJobID(id)
	if err != nil {
		return err
	}
	out := f.jobOutputDirectory(jobID)
	_, err = f.mutateJobs(f.now(), func(jobs *[]map[string]any) (Job, error) {
		index := indexOfJob(*jobs, jobID)
		if index < 0 {
			return Job{}, NotFound("job:" + jobID)
		}
		*jobs = append((*jobs)[:index], (*jobs)[index+1:]...)
		_ = os.RemoveAll(out)
		return Job{}, nil
	})
	return err
}

func patchJob(jobs *[]map[string]any, id string, patch func(map[string]any)) (Job, error) {
	index := indexOfJob(*jobs, id)
	if index < 0 {
		return Job{}, NotFound("job:" + id)
	}
	job := (*jobs)[index]
	patch(job)
	parsed, ok := ParseJob(job)
	if !ok {
		return Job{}, InvalidResponse("jobs.json")
	}
	return parsed, nil
}

// mutateJobs is load, mutate, atomic save of cron/jobs.json under the advisory
// lock the Hermes scheduler and CLI take (cron/.jobs.lock).
func (f *Filesystem) mutateJobs(now time.Time, body func(*[]map[string]any) (Job, error)) (Job, error) {
	path := f.cronJobsPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // Hermes reads these as another user, as Swift writes them
		return Job{}, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".jobs.lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // resolved inside the Hermes root
	if err != nil {
		return Job{}, InvalidResponse("jobs.lock")
	}
	defer func() { _ = lock.Close() }()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX) != nil {
		return Job{}, InvalidResponse("jobs.lock")
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	envelope := map[string]any{"jobs": []any{}}
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // resolved inside the Hermes root
		var existing map[string]any
		if json.Unmarshal(data, &existing) == nil && existing != nil {
			envelope = existing
		}
	}
	jobs, _ := asObjects(envelope["jobs"])
	if jobs == nil {
		jobs = []map[string]any{}
	}
	result, err := body(&jobs)
	if err != nil {
		return Job{}, err
	}
	envelope["jobs"] = jobs
	envelope["updated_at"] = ISO(now)
	payload, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return Job{}, err
	}
	return result, atomicWrite(payload, path)
}

func (f *Filesystem) jobOutputDirectory(jobID string) string {
	return filepath.Join(f.Root, "cron", "output", jobID)
}

// JobRuns: every cron/output/<job>/<stamp>.md is one completed run; a job whose
// last_status is error after its newest output gets one synthetic error run.
func (f *Filesystem) JobRuns(_ context.Context, jobID string, limit int) ([]JobRun, error) {
	id, err := ValidateJobID(jobID)
	if err != nil {
		return nil, err
	}
	bounded := max(1, min(limit, 100))
	runs := []JobRun{}
	if names, err := sortedNames(f.jobOutputDirectory(id)); err == nil {
		for _, name := range names {
			if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
				continue
			}
			stem := strings.TrimSuffix(name, ".md")
			started := ParseOutputStamp(stem)
			if started == nil {
				continue
			}
			runs = append(runs, JobRun{Key: stem, Status: RunOK, StartedAt: *started, FinishedAt: started})
		}
	}
	if data, err := os.ReadFile(f.cronJobsPath()); err == nil {
		var obj map[string]any
		if json.Unmarshal(data, &obj) == nil {
			jobs, _ := asObjects(obj["jobs"])
			if i := indexOfJob(jobs, id); i >= 0 {
				job := jobs[i]
				if s, _ := job["last_status"].(string); s == "error" {
					if lastRun := ParseDate(job["last_run_at"]); lastRun != nil {
						allBefore := true
						for _, r := range runs {
							if !r.StartedAt.Before(*lastRun) {
								allBefore = false
							}
						}
						if allBefore {
							msg := "run failed"
							if e, ok := job["last_error"].(string); ok {
								msg = e
							}
							runs = append(runs, JobRun{Key: "error-" + ISO(*lastRun), Status: RunError, StartedAt: *lastRun, FinishedAt: lastRun, Error: &msg})
						}
					}
				}
			}
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	if len(runs) > bounded {
		runs = runs[:bounded]
	}
	return runs, nil
}

func (f *Filesystem) JobRunOutput(_ context.Context, jobID, runKey string) (*string, error) {
	id, err := ValidateJobID(jobID)
	if err != nil {
		return nil, err
	}
	if ParseOutputStamp(runKey) == nil {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(f.jobOutputDirectory(id), runKey+".md")) //nolint:gosec // resolved inside the Hermes root
	if err != nil || !utf8.Valid(data) {
		return nil, nil
	}
	s := string(data)
	return &s, nil
}

// ParseOutputStamp reads yyyy-MM-dd_HH-mm-ss (save_job_output) as UTC.
func ParseOutputStamp(stem string) *time.Time {
	parts := strings.Split(stem, "_")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	date, time1 := ints(parts[0]), ints(parts[1])
	if len(date) != 3 || len(time1) != 3 {
		return nil
	}
	t := time.Date(date[0], time.Month(date[1]), date[2], time1[0], time1[1], time1[2], 0, time.UTC)
	return &t
}

// ints is split(separator: "-").compactMap { Int($0) }.
func ints(s string) []int {
	out := []int{}
	for _, p := range strings.Split(s, "-") {
		if p == "" {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// NextRun is the next minute (UTC) matching the expression within 366 days.
func NextRun(after time.Time, expr cronexpr.Expression) *time.Time {
	candidate := after.UTC().Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if expr.Matches(candidate, time.UTC) {
			return &candidate
		}
		candidate = candidate.Add(time.Minute)
	}
	return nil
}

func (f *Filesystem) ListFiles(_ context.Context, path string) ([]FileEntry, error) {
	dir, err := f.resolveInsideRoot(path)
	if err != nil {
		return nil, err
	}
	if !isDir(dir) {
		return nil, NotFound(dir)
	}
	names, err := sortedNames(dir)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(names))
	for _, name := range names {
		full := JoinPath(dir, name)
		out = append(out, FileEntry{Name: name, Path: full, IsDirectory: isDir(full)})
	}
	return out, nil
}

func (f *Filesystem) ReadText(_ context.Context, path string) (string, error) {
	file, err := f.resolveInsideRoot(path)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(file); err == nil && st.Size() > FileBodyCap {
		return "", BodyTooLarge(file, FileBodyCap)
	}
	data, err := os.ReadFile(file) //nolint:gosec // resolved inside the Hermes root
	if err != nil {
		return "", NotFound(file)
	}
	if !utf8.Valid(data) {
		return "", InvalidResponse("binary:" + file)
	}
	return string(data), nil
}

func (f *Filesystem) WriteText(_ context.Context, path, content string) error {
	file, err := f.resolveInsideRoot(path)
	if err != nil {
		return err
	}
	if len(content) > FileBodyCap {
		return BodyTooLarge(file, FileBodyCap)
	}
	parent := filepath.Dir(file)
	if !isDir(parent) {
		return NotFound(parent)
	}
	return atomicWrite([]byte(content), file)
}

func (f *Filesystem) Mkdir(_ context.Context, path string) error {
	dir, err := f.resolveInsideRoot(path)
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755) //nolint:gosec // Hermes reads these as another user, as Swift writes them
}

// resolveInsideRoot validates the path and pins it under the root. Symlinks
// are not followed for the check (the volume is ours).
func (f *Filesystem) resolveInsideRoot(path string) (string, error) {
	validated, err := ValidatePath(path)
	if err != nil {
		return "", err
	}
	root := f.Root
	for len(root) > 1 && strings.HasSuffix(root, "/") {
		root = root[:len(root)-1]
	}
	if !IsInside(validated, root) {
		return "", InvalidPath(path)
	}
	return validated, nil
}

func (f *Filesystem) ListSessions(ctx context.Context, offset, limit int) (SessionPage, error) {
	if f.Sessions == nil {
		return SessionPage{}, Unsupported("sessions")
	}
	return f.Sessions.ListSessions(ctx, offset, limit)
}

func (f *Filesystem) SessionMessages(ctx context.Context, id string) ([]Message, error) {
	if f.Sessions == nil {
		return nil, Unsupported("sessions")
	}
	return f.Sessions.SessionMessages(ctx, id)
}

// atomicWrite writes a sibling temp file and renames it over the target.
func atomicWrite(data []byte, path string) error {
	var b [16]byte
	_, _ = rand.Read(b[:])
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".lv-tmp-"+strings.ToUpper(hex.EncodeToString(b[:])))
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // Hermes reads these as another user, as Swift writes them
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		var errno syscall.Errno
		code := 0
		if errors.As(err, &errno) {
			code = int(errno)
		}
		return InvalidResponse(fmt.Sprintf("atomic write of %s failed (errno %d)", filepath.Base(path), code))
	}
	return nil
}
