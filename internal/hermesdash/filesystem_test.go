package hermesdash

import (
	"context"
	"encoding/json"
	"lumina/internal/hermesdash/hermesdashtest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FilesystemHermesTransportTests: the managed transport reads and writes the
// Hermes home and never leaves it.

func writeFile(t *testing.T, root, relative, content string) {
	t.Helper()
	p := filepath.Join(root, relative)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

func fixedFS(root string, at time.Time) *Filesystem {
	f := NewFilesystem(root, nil)
	f.Now = func() time.Time { return at }
	return f
}

func jobRowOnDisk(t *testing.T, root, id string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "cron", "jobs.json"))
	require.NoError(t, err)
	var doc struct {
		Jobs []map[string]any `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	for _, row := range doc.Jobs {
		if row["id"] == id {
			return row
		}
	}
	return nil
}

func ptrTo[T any](v T) *T { return &v }

var ctx = context.Background()

func TestFilesystemListsSkillsFromSkillMDWithFrontmatterDescriptionsAndDisabledConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "skills/kb-compile/SKILL.md", "---\nname: kb-compile\ndescription: Compile raw into wiki\ntrigger: /kb-compile\n---\n# body\n")
	writeFile(t, root, "skills/weather/SKILL.md", "Fetch the weather.\n")
	writeFile(t, root, "skills/.hidden/SKILL.md", "nope")
	writeFile(t, root, "config.yaml", "model:\n  default: gpt\nskills:\n  disabled:\n    - weather\n")
	transport := NewFilesystem(root, nil)
	skills, err := transport.ListSkills(ctx)
	require.NoError(t, err)
	require.Len(t, skills, 2)
	assert.Equal(t, "kb-compile", skills[0].Name)
	assert.Equal(t, "weather", skills[1].Name)
	assert.Equal(t, "Compile raw into wiki", skills[0].Description)
	assert.True(t, skills[0].Enabled)
	require.NotNil(t, skills[0].ContentHash)
	assert.Len(t, *skills[0].ContentHash, 64)
	assert.Equal(t, "Fetch the weather.", skills[1].Description)
	assert.False(t, skills[1].Enabled)
	content, err := transport.SkillContent(ctx, "weather")
	require.NoError(t, err)
	assert.Equal(t, "Fetch the weather.\n", content)
	status, err := transport.Status(ctx)
	require.NoError(t, err)
	assert.True(t, status.Reachable)
}

func TestFilesystemToggleSkillRewritesSkillsDisabledAndKeepsOtherKeys(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "skills/a/SKILL.md", "A")
	writeFile(t, root, "config.yaml", "model:\n  default: gpt\nskills:\n  disabled: []\n")
	transport := NewFilesystem(root, nil)
	require.NoError(t, transport.ToggleSkill(ctx, "a", false))
	skills, err := transport.ListSkills(ctx)
	require.NoError(t, err)
	assert.False(t, skills[0].Enabled)
	config := loadConfig(filepath.Join(root, "config.yaml"))
	model, _ := config["model"].(map[string]any)
	assert.Equal(t, "gpt", model["default"])
	require.NoError(t, transport.ToggleSkill(ctx, "a", true))
	skills, err = transport.ListSkills(ctx)
	require.NoError(t, err)
	assert.True(t, skills[0].Enabled)
	assert.Equal(t, InvalidPath("skill:../x"), transport.ToggleSkill(ctx, "../x", true))
}

func TestFilesystemCreateSkillWritesSkillMDUnderSkills(t *testing.T) {
	root := t.TempDir()
	transport := NewFilesystem(root, nil)
	require.NoError(t, transport.CreateSkill(ctx, "kb-ingest", "---\nname: kb-ingest\ndescription: Ingest\n---\n"))
	written, err := os.ReadFile(filepath.Join(root, "skills", "kb-ingest", "SKILL.md"))
	require.NoError(t, err)
	assert.Contains(t, string(written), "description: Ingest")
	skills, err := transport.ListSkills(ctx)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Equal(t, "kb-ingest", skills[0].Name)
}

func TestFilesystemCreateJobAppendsToCronJobsJSONInTheSchedulerShape(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cron/jobs.json", `{"jobs":[{"id":"old1","name":"Existing","schedule":{"kind":"cron","expr":"0 9 * * *","display":"0 9 * * *"},"enabled":true}],"updated_at":"x"}`)
	transport := fixedFS(root, time.Unix(1_756_800_000, 0))
	job, err := transport.CreateJob(ctx, JobSpec{Name: "luminavault-nightly-compile", Schedule: "0 3 * * *", Prompt: "compile", Deliver: "origin", Skills: []string{"kb-compile"}})
	require.NoError(t, err)
	assert.Equal(t, "luminavault-nightly-compile", *job.Name)
	assert.Equal(t, "0 3 * * *", *job.Schedule)
	assert.Len(t, job.ID, 12)
	assert.NotNil(t, job.NextRunAt)
	jobs, err := transport.ListJobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, []string{"old1", job.ID}, []string{jobs[0].ID, jobs[1].ID})
	row := jobRowOnDisk(t, root, job.ID)
	assert.Equal(t, "scheduled", row["state"])
	assert.Equal(t, "origin", row["deliver"])
	assert.Equal(t, []any{"kb-compile"}, row["skills"])
	schedule, _ := row["schedule"].(map[string]any)
	assert.Equal(t, "0 3 * * *", schedule["expr"])
	_, err = transport.CreateJob(ctx, JobSpec{Name: "bad", Schedule: "not a cron", Deliver: "origin"})
	assert.Error(t, err)
}

func TestFilesystemCreateJobCarriesTheOptionalCronJobCreateFields(t *testing.T) {
	root := t.TempDir()
	transport := fixedFS(root, time.Unix(1_756_800_000, 0))
	job, err := transport.CreateJob(ctx, JobSpec{
		Name: "digest", Schedule: "0 3 * * *", Prompt: "summarise", Deliver: "origin", Skills: []string{"kb-compile"},
		Model: ptrTo("gpt-5"), Provider: ptrTo("openai"), BaseURL: ptrTo("https://api.example"), Script: ptrTo("run.sh"),
		ContextFrom: []string{"j1"}, HasContextFrom: true, EnabledToolsets: []string{"files"}, HasToolsets: true,
		Workdir: ptrTo("/home/hermes/work"), NoAgent: true,
	})
	require.NoError(t, err)
	row := jobRowOnDisk(t, root, job.ID)
	assert.Equal(t, "gpt-5", row["model"])
	assert.Equal(t, "openai", row["provider"])
	assert.Equal(t, "https://api.example", row["base_url"])
	assert.Equal(t, "run.sh", row["script"])
	assert.Equal(t, true, row["no_agent"])
	assert.Equal(t, []any{"j1"}, row["context_from"])
	assert.Equal(t, []any{"files"}, row["enabled_toolsets"])
	assert.Equal(t, "/home/hermes/work", row["workdir"])
}

func TestFilesystemUpdateJobPatchesFieldsRederivesTheScheduleAndLeavesTheIDAlone(t *testing.T) {
	root := t.TempDir()
	transport := fixedFS(root, time.Unix(1_756_800_000, 0))
	created, err := transport.CreateJob(ctx, JobSpec{Name: "old", Schedule: "0 3 * * *", Prompt: "p", Deliver: "origin", Skills: []string{"a"}})
	require.NoError(t, err)
	updated, err := transport.UpdateJob(ctx, created.ID, JobUpdate{
		Name: ptrTo("new"), Schedule: ptrTo("30 7 * * *"), Prompt: ptrTo("q"), Skills: &[]string{"b"}, Model: ptrTo("gpt-5"),
	})
	require.NoError(t, err)
	assert.Equal(t, created.ID, updated.ID)
	assert.Equal(t, "new", *updated.Name)
	assert.Equal(t, "30 7 * * *", *updated.Schedule)
	assert.Equal(t, "q", *updated.Prompt)
	row := jobRowOnDisk(t, root, created.ID)
	require.NotNil(t, row)
	assert.Equal(t, "gpt-5", row["model"])
	assert.Equal(t, []any{"b"}, row["skills"])
	// `skill` (singular legacy field) is cleared when `skills` is set.
	skill, present := row["skill"]
	assert.True(t, present)
	assert.Nil(t, skill)
	schedule, _ := row["schedule"].(map[string]any)
	assert.Equal(t, "30 7 * * *", schedule["expr"])
	assert.Equal(t, "30 7 * * *", row["schedule_display"])
	assert.IsType(t, "", row["next_run_at"])
	_, err = transport.UpdateJob(ctx, "missing", JobUpdate{Name: ptrTo("x")})
	assert.Equal(t, NotFound("job:missing"), err)
	_, err = transport.UpdateJob(ctx, "../etc", JobUpdate{Name: ptrTo("x")})
	assert.Equal(t, InvalidPath("job:../etc"), err)
}

func TestFilesystemPauseResumeTriggerAndDeleteRewriteJobsJSONUnderTheLock(t *testing.T) {
	root := t.TempDir()
	fixed := time.Unix(1_756_800_000, 0)
	transport := fixedFS(root, fixed)
	job, err := transport.CreateJob(ctx, JobSpec{Name: "j", Schedule: "0 3 * * *", Prompt: "p", Deliver: "origin"})
	require.NoError(t, err)

	paused, err := transport.PauseJob(ctx, job.ID)
	require.NoError(t, err)
	assert.True(t, paused.Paused)
	row := jobRowOnDisk(t, root, job.ID)
	assert.Equal(t, "paused", row["state"])
	assert.IsType(t, "", row["paused_at"])

	resumed, err := transport.ResumeJob(ctx, job.ID)
	require.NoError(t, err)
	assert.False(t, resumed.Paused)
	row = jobRowOnDisk(t, root, job.ID)
	assert.Equal(t, "scheduled", row["state"])
	pausedAt, present := row["paused_at"]
	assert.True(t, present)
	assert.Nil(t, pausedAt)

	_, err = transport.TriggerJob(ctx, job.ID)
	require.NoError(t, err)
	row = jobRowOnDisk(t, root, job.ID)
	assert.Equal(t, ISO(fixed), row["next_run_at"])

	// The lock is exclusive per mutation, so concurrent writers serialise
	// instead of clobbering: 8 pause/resume pairs leave exactly one job.
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = transport.PauseJob(ctx, job.ID)
			} else {
				_, _ = transport.ResumeJob(ctx, job.ID)
			}
		}()
	}
	wg.Wait()
	jobs, err := transport.ListJobs(ctx)
	require.NoError(t, err)
	assert.Len(t, jobs, 1)

	require.NoError(t, os.MkdirAll(transport.jobOutputDirectory(job.ID), 0o750))
	require.NoError(t, transport.DeleteJob(ctx, job.ID))
	jobs, err = transport.ListJobs(ctx)
	require.NoError(t, err)
	assert.Empty(t, jobs)
	_, err = os.Stat(transport.jobOutputDirectory(job.ID))
	assert.True(t, os.IsNotExist(err))
	assert.Equal(t, NotFound("job:"+job.ID), transport.DeleteJob(ctx, job.ID))
}

func TestFilesystemJobRunsListsCronOutputFilesNewestFirstAndReadsOneOutput(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cron/output/digest/2026-09-01_03-00-00.md", "# Monday\n")
	writeFile(t, root, "cron/output/digest/2026-09-02_03-00-00.md", "# Tuesday\n")
	writeFile(t, root, "cron/output/digest/notes.txt", "ignored")
	writeFile(t, root, "cron/output/digest/.hidden.md", "ignored")
	transport := NewFilesystem(root, nil)
	runs, err := transport.JobRuns(ctx, "digest", 10)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "2026-09-02_03-00-00", runs[0].Key)
	assert.Equal(t, "2026-09-01_03-00-00", runs[1].Key)
	for _, r := range runs {
		assert.Equal(t, RunOK, r.Status)
	}
	// The stem is the run's UTC start instant, not a local-time reading.
	assert.True(t, runs[0].StartedAt.Equal(*ParseISO("2026-09-02T03:00:00+00:00")))
	out, err := transport.JobRunOutput(ctx, "digest", runs[0].Key)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "# Tuesday\n", *out)
	missing, err := transport.JobRunOutput(ctx, "digest", "missing")
	require.NoError(t, err)
	assert.Nil(t, missing)
	one, err := transport.JobRuns(ctx, "digest", 1)
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, "2026-09-02_03-00-00", one[0].Key)
	unknown, err := transport.JobRuns(ctx, "unknown-job", 10)
	require.NoError(t, err)
	assert.Empty(t, unknown)
	_, err = transport.JobRuns(ctx, "../x", 10)
	assert.Equal(t, InvalidPath("job:../x"), err)
}

func TestFilesystemAFailedRunWithNoOutputFileSurfacesAsASyntheticErrorRun(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cron/output/digest/2026-09-01_03-00-00.md", "# Monday\n")
	writeFile(t, root, "cron/jobs.json", `{"jobs":[{"id":"digest","name":"Digest","schedule":{"kind":"cron","expr":"0 3 * * *"},"enabled":true,
		"last_run_at":"2026-09-02T03:00:00+00:00","last_status":"error","last_error":"provider timeout"}]}`)
	transport := NewFilesystem(root, nil)
	runs, err := transport.JobRuns(ctx, "digest", 10)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, RunError, runs[0].Status)
	require.NotNil(t, runs[0].Error)
	assert.Equal(t, "provider timeout", *runs[0].Error)
	assert.True(t, strings.HasPrefix(runs[0].Key, "error-"))
	assert.Equal(t, RunOK, runs[1].Status)
	// No synthetic run once a later output file exists for that failure.
	writeFile(t, root, "cron/output/digest/2026-09-02_03-00-01.md", "# Recovered\n")
	recovered, err := transport.JobRuns(ctx, "digest", 10)
	require.NoError(t, err)
	require.Len(t, recovered, 2)
	for _, r := range recovered {
		assert.Equal(t, RunOK, r.Status)
	}
}

func TestNextRunFindsTheNextMatchingMinute(t *testing.T) {
	expr, err := parseCron("0 3 * * *")
	require.NoError(t, err)
	from := time.Unix(1_756_800_000, 0)
	next := NextRun(from, expr)
	require.NotNil(t, next)
	assert.Equal(t, 3, next.UTC().Hour())
	assert.Equal(t, 0, next.UTC().Minute())
	assert.True(t, next.After(from))
}

func TestFilesystemOperationsStayInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	transport := NewFilesystem(root, nil)
	kb := filepath.Join(root, "kb")
	require.NoError(t, transport.Mkdir(ctx, kb+"/raw"))
	require.NoError(t, transport.WriteText(ctx, kb+"/raw/note.md", "# Note\n"))
	text, err := transport.ReadText(ctx, kb+"/raw/note.md")
	require.NoError(t, err)
	assert.Equal(t, "# Note\n", text)
	entries, err := transport.ListFiles(ctx, kb)
	require.NoError(t, err)
	assert.Equal(t, []FileEntry{{Name: "raw", Path: kb + "/raw", IsDirectory: true}}, entries)

	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	_, err = transport.ListFiles(ctx, outside)
	assert.Equal(t, InvalidPath(outside), err)
	_, err = transport.ReadText(ctx, kb+"/../../etc/passwd")
	assert.Equal(t, InvalidPath(kb+"/../../etc/passwd"), err)
	assert.Equal(t, NotFound(kb+"/nope"), transport.WriteText(ctx, kb+"/nope/x.md", "x"))
	_, err = transport.ListFiles(ctx, kb+"/missing")
	assert.Equal(t, NotFound(kb+"/missing"), err)
}

func TestFilesystemSessionsAreUnsupportedWithoutAGatewayClient(t *testing.T) {
	transport := NewFilesystem("/tmp/lv-none", nil)
	_, err := transport.ListSessions(ctx, 0, 10)
	assert.Equal(t, Unsupported("sessions"), err)
}

func TestFilesystemGatewaySessionsClientUsesTheAPIServerPathsWithTheAuthHeader(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/sessions", 200, `{"sessions":[{"id":"s1","message_count":2}],"total":1}`)
	stub.Respond("GET", "/api/sessions/s1/messages", 200, `{"messages":[{"role":"user","content":"hi"}]}`)
	base, _ := url.Parse("http://hermes:8642")
	auth := "Bearer key"
	transport := NewFilesystem("/tmp/lv-none", NewGateway(base, &auth, stub))
	page, err := transport.ListSessions(ctx, 0, 5)
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "s1", page.Sessions[0].ID)
	messages, err := transport.SessionMessages(ctx, "s1")
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Equal(t, "hi", messages[0].Content)
	requests := stub.Requests()
	require.NotEmpty(t, requests)
	assert.True(t, strings.HasPrefix(requests[0].URL, "http://hermes:8642/api/sessions?"), requests[0].URL)
	assert.Equal(t, "Bearer key", requests[0].Header.Get("Authorization"))
}
