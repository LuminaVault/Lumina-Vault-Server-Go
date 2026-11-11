package hermesmirror

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/hermesdash"
	"path"
	"sort"
	"sync"

	"github.com/google/uuid"
)

// fakeTransport is FakeHermesMirrorTransport: in memory, scriptable per
// operation, and it records what reached "Hermes".
type fakeTransport struct {
	mu         sync.Mutex
	kind       hermesdash.TransportKind
	skills     []hermesdash.Skill
	contents   map[string]string
	jobs       []hermesdash.Job
	files      map[string]string
	dirs       map[string]bool
	sessions   []hermesdash.Session
	messages   map[string][]hermesdash.Message
	runs       map[string][]hermesdash.JobRun
	runOutputs map[string]*string
	status     hermesdash.Status
	failures   map[string]error
	calls      []string
}

func newFake(kind hermesdash.TransportKind) *fakeTransport {
	cwd, version, auth := "/home/hermes", "0.20.0", false
	return &fakeTransport{
		kind: kind, contents: map[string]string{}, files: map[string]string{}, dirs: map[string]bool{},
		messages: map[string][]hermesdash.Message{}, runs: map[string][]hermesdash.JobRun{},
		runOutputs: map[string]*string{}, failures: map[string]error{},
		status: hermesdash.Status{Reachable: true, AuthRequired: &auth, Version: &version, DefaultCwd: &cwd},
	}
}

// fixedProvider is FixedTransports.
type fixedProvider struct{ t *fakeTransport }

func (p fixedProvider) Kind(context.Context, uuid.UUID) hermesdash.TransportKind { return p.t.kind }
func (p fixedProvider) Transport(context.Context, uuid.UUID) (hermesdash.Transport, error) {
	return p.t, nil
}

func (f *fakeTransport) fail(op string, err error) { f.mu.Lock(); f.failures[op] = err; f.mu.Unlock() }

func (f *fakeTransport) setSkills(s ...hermesdash.Skill) { f.mu.Lock(); f.skills = s; f.mu.Unlock() }
func (f *fakeTransport) setJobs(j ...hermesdash.Job)     { f.mu.Lock(); f.jobs = j; f.mu.Unlock() }
func (f *fakeTransport) setSessions(s ...hermesdash.Session) {
	f.mu.Lock()
	f.sessions = s
	f.mu.Unlock()
}

func (f *fakeTransport) setMessages(id string, m ...hermesdash.Message) {
	f.mu.Lock()
	f.messages[id] = m
	f.mu.Unlock()
}

func (f *fakeTransport) setRuns(job string, r ...hermesdash.JobRun) {
	f.mu.Lock()
	f.runs[job] = r
	f.mu.Unlock()
}

func (f *fakeTransport) setRunOutput(job, key, out string) {
	f.mu.Lock()
	f.runOutputs[job+"/"+key] = &out
	f.mu.Unlock()
}

func (f *fakeTransport) addDirectory(p string) { f.mu.Lock(); f.dirs[p] = true; f.mu.Unlock() }

func (f *fakeTransport) addFile(p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = content
	for parent := path.Dir(p); len(parent) > 1; parent = path.Dir(parent) {
		f.dirs[parent] = true
	}
	f.dirs["/"] = true
}

func (f *fakeTransport) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeTransport) jobList() []hermesdash.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hermesdash.Job(nil), f.jobs...)
}

func (f *fakeTransport) fileContents() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.files {
		out[k] = v
	}
	return out
}

// check records the call and returns the scripted failure; callers hold mu.
func (f *fakeTransport) check(op string) error {
	f.calls = append(f.calls, op)
	return f.failures[op]
}

func (f *fakeTransport) Kind() hermesdash.TransportKind { return f.kind }

func (f *fakeTransport) Status(context.Context) (hermesdash.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("status"); err != nil {
		return hermesdash.Status{}, err
	}
	return f.status, nil
}

func (f *fakeTransport) ListSkills(context.Context) ([]hermesdash.Skill, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("listSkills"); err != nil {
		return nil, err
	}
	return append([]hermesdash.Skill(nil), f.skills...), nil
}

func (f *fakeTransport) SkillContent(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("skillContent"); err != nil {
		return "", err
	}
	c, ok := f.contents[name]
	if !ok {
		return "", hermesdash.NotFound("skill:" + name)
	}
	return c, nil
}

func (f *fakeTransport) ToggleSkill(_ context.Context, name string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(fmt.Sprintf("toggleSkill:%s:%t", name, enabled)); err != nil {
		return err
	}
	for i, s := range f.skills {
		if s.Name == name {
			f.skills[i].Enabled = enabled
			return nil
		}
	}
	return hermesdash.NotFound("skill:" + name)
}

func (f *fakeTransport) CreateSkill(_ context.Context, name, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("createSkill:" + name); err != nil {
		return err
	}
	f.contents[name] = content
	f.skills = append(f.skills, hermesdash.Skill{Name: name, Description: hermesdash.FrontmatterDescription(content), Enabled: true, Source: "custom"})
	return nil
}

func (f *fakeTransport) ListJobs(context.Context) ([]hermesdash.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("listJobs"); err != nil {
		return nil, err
	}
	return append([]hermesdash.Job(nil), f.jobs...), nil
}

func (f *fakeTransport) CreateJob(_ context.Context, spec hermesdash.JobSpec) (hermesdash.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("createJob:" + spec.Name); err != nil {
		return hermesdash.Job{}, err
	}
	raw, _ := json.Marshal(map[string]string{"name": spec.Name, "schedule": spec.Schedule})
	name, schedule, prompt := spec.Name, spec.Schedule, spec.Prompt
	job := hermesdash.Job{ID: fmt.Sprintf("job-%d", len(f.jobs)+1), Name: &name, Schedule: &schedule, Prompt: &prompt, Raw: raw}
	f.jobs = append(f.jobs, job)
	return job, nil
}

func (f *fakeTransport) mutate(id string, patch func(*hermesdash.Job)) (hermesdash.Job, error) {
	for i := range f.jobs {
		if f.jobs[i].ID == id {
			patch(&f.jobs[i])
			return f.jobs[i], nil
		}
	}
	return hermesdash.Job{}, hermesdash.NotFound("job:" + id)
}

func (f *fakeTransport) UpdateJob(_ context.Context, id string, u hermesdash.JobUpdate) (hermesdash.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("updateJob:" + id); err != nil {
		return hermesdash.Job{}, err
	}
	return f.mutate(id, func(j *hermesdash.Job) {
		if u.Name != nil {
			j.Name = u.Name
		}
		if u.Schedule != nil {
			j.Schedule = u.Schedule
		}
		if u.Prompt != nil {
			j.Prompt = u.Prompt
		}
		if u.Enabled != nil {
			j.Paused = !*u.Enabled
		}
	})
}

func (f *fakeTransport) setPaused(op, id string, paused bool) (hermesdash.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(op + ":" + id); err != nil {
		return hermesdash.Job{}, err
	}
	return f.mutate(id, func(j *hermesdash.Job) { j.Paused = paused })
}

func (f *fakeTransport) PauseJob(_ context.Context, id string) (hermesdash.Job, error) {
	return f.setPaused("pauseJob", id, true)
}

func (f *fakeTransport) ResumeJob(_ context.Context, id string) (hermesdash.Job, error) {
	return f.setPaused("resumeJob", id, false)
}

func (f *fakeTransport) TriggerJob(_ context.Context, id string) (hermesdash.Job, error) {
	return f.setPaused("triggerJob", id, false)
}

func (f *fakeTransport) DeleteJob(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("deleteJob:" + id); err != nil {
		return err
	}
	for i, j := range f.jobs {
		if j.ID == id {
			f.jobs = append(f.jobs[:i], f.jobs[i+1:]...)
			delete(f.runs, id)
			return nil
		}
	}
	return hermesdash.NotFound("job:" + id)
}

func (f *fakeTransport) JobRuns(_ context.Context, jobID string, limit int) ([]hermesdash.JobRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("jobRuns:" + jobID); err != nil {
		return nil, err
	}
	rows := append([]hermesdash.JobRun(nil), f.runs[jobID]...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartedAt.After(rows[j].StartedAt) })
	if n := max(1, limit); len(rows) > n {
		rows = rows[:n]
	}
	return rows, nil
}

func (f *fakeTransport) JobRunOutput(_ context.Context, jobID, runKey string) (*string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("jobRunOutput:" + jobID + ":" + runKey); err != nil {
		return nil, err
	}
	return f.runOutputs[jobID+"/"+runKey], nil
}

func (f *fakeTransport) ListFiles(_ context.Context, p string) ([]hermesdash.FileEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("listFiles:" + p); err != nil {
		return nil, err
	}
	validated, err := hermesdash.ValidatePath(p)
	if err != nil {
		return nil, err
	}
	if !f.dirs[validated] {
		return nil, hermesdash.NotFound(validated)
	}
	var entries []hermesdash.FileEntry
	for d := range f.dirs {
		if d != validated && path.Dir(d) == validated {
			entries = append(entries, hermesdash.FileEntry{Name: path.Base(d), Path: d, IsDirectory: true})
		}
	}
	for file := range f.files {
		if path.Dir(file) == validated {
			entries = append(entries, hermesdash.FileEntry{Name: path.Base(file), Path: file})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (f *fakeTransport) ReadText(_ context.Context, p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("readText:" + p); err != nil {
		return "", err
	}
	c, ok := f.files[p]
	if !ok {
		return "", hermesdash.NotFound(p)
	}
	return c, nil
}

func (f *fakeTransport) WriteText(_ context.Context, p, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("writeText:" + p); err != nil {
		return err
	}
	f.files[p] = content
	return nil
}

func (f *fakeTransport) Mkdir(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("mkdir:" + p); err != nil {
		return err
	}
	v, err := hermesdash.ValidatePath(p)
	if err != nil {
		return err
	}
	f.dirs[v] = true
	return nil
}

func (f *fakeTransport) ListSessions(_ context.Context, offset, limit int) (hermesdash.SessionPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check(fmt.Sprintf("listSessions:%d:%d", offset, limit)); err != nil {
		return hermesdash.SessionPage{}, err
	}
	total := len(f.sessions)
	start := min(offset, total)
	end := min(start+limit, total)
	return hermesdash.SessionPage{Sessions: append([]hermesdash.Session(nil), f.sessions[start:end]...), Total: &total}, nil
}

func (f *fakeTransport) SessionMessages(_ context.Context, id string) ([]hermesdash.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.check("sessionMessages:" + id); err != nil {
		return nil, err
	}
	m, ok := f.messages[id]
	if !ok {
		return nil, hermesdash.NotFound("session:" + id)
	}
	return m, nil
}
