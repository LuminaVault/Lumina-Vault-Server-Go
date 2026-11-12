package hermesdash

import (
	"context"
	"errors"
	"lumina/internal/hermesdash/hermesdashtest"
	"lumina/internal/ssrfguard"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HermesDashboardClientTests: the client speaks web_server.py's /api/*
// contract, detects the OAuth-only auth mode, caps and validates paths, and
// never leaks the token.

func dashboardOn(stub *hermesdashtest.Stub) *Dashboard {
	d := NewDashboard("http://127.0.0.1:9119/", "dash-secret", ssrfguard.Guard{AllowPrivateRanges: true, AllowTailnetHTTP: true}, stub)
	d.Sleep = func(context.Context, time.Duration) error { return nil }
	return d
}

func requestsWith(stub *hermesdashtest.Stub, method string) []hermesdashtest.Recorded {
	var out []hermesdashtest.Recorded
	for _, r := range stub.Requests() {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func TestDashboardRequestsCarryTheBearerAndSessionTokenHeadersOnTheJoinedURL(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/skills", 200, "[]")
	_, err := dashboardOn(stub).ListSkills(ctx)
	require.NoError(t, err)
	requests := stub.Requests()
	require.NotEmpty(t, requests)
	assert.Equal(t, "http://127.0.0.1:9119/api/skills", requests[0].URL)
	assert.Equal(t, "Bearer dash-secret", requests[0].Header.Get("Authorization"))
	assert.Equal(t, "dash-secret", requests[0].Header.Get("X-Hermes-Session-Token"))
}

func TestDashboardListSkillsMapsProvenanceAndEnabledFlags(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/skills", 200, `[{"name":"kb-compile","description":"Compile","enabled":true,"provenance":"agent"},
	 {"name":"weather","description":"","enabled":false,"provenance":"hub"},
	 {"name":"bundled-one","enabled":true,"provenance":"bundled"},
	 {"description":"no name"}]`)
	skills, err := dashboardOn(stub).ListSkills(ctx)
	require.NoError(t, err)
	require.Len(t, skills, 3)
	assert.Equal(t, []string{"kb-compile", "weather", "bundled-one"}, []string{skills[0].Name, skills[1].Name, skills[2].Name})
	assert.Equal(t, "custom", skills[0].Source)
	assert.Equal(t, "hub", skills[1].Source)
	assert.False(t, skills[1].Enabled)
	assert.Equal(t, "builtin", skills[2].Source)
}

func TestDashboardAuthProbeDistinguishesBearerOAuthOnlyUnauthorizedAndUnreachable(t *testing.T) {
	no, yes := false, true
	ok := hermesdashtest.New()
	ok.Respond("GET", "/api/skills", 200, "[]")
	assert.Equal(t, AuthBearer, dashboardOn(ok).ProbeAuthMode(ctx, &no))

	redirect := hermesdashtest.New()
	redirect.Respond("GET", "/api/skills", 302, "", "Location", "/login")
	assert.Equal(t, AuthOAuthOnly, dashboardOn(redirect).ProbeAuthMode(ctx, nil))

	gated := hermesdashtest.New()
	gated.Respond("GET", "/api/skills", 401, `{"detail":"Unauthorized"}`)
	assert.Equal(t, AuthOAuthOnly, dashboardOn(gated).ProbeAuthMode(ctx, &yes))

	badToken := hermesdashtest.New()
	badToken.Respond("GET", "/api/skills", 401, `{"detail":"Unauthorized"}`)
	assert.Equal(t, AuthUnauthorized, dashboardOn(badToken).ProbeAuthMode(ctx, &no))

	down := hermesdashtest.New()
	down.Failure = errors.New("cannot connect to host")
	assert.Equal(t, AuthUnreachable, dashboardOn(down).ProbeAuthMode(ctx, &no))
}

func TestDashboardProtectedCallsSurfaceTheAuthModeErrorOnALoginRedirect(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/cron/jobs", 303, "", "Location", "/login")
	_, err := dashboardOn(stub).ListJobs(ctx)
	assert.Equal(t, ErrAuthModeUnsupported, err)
}

func TestDashboardStatusReadsVersionAndAuthRequiredAndTheDefaultCwd(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/status", 200, `{"version":"0.21.0","auth_required":true,"gateway_running":true}`)
	stub.Respond("GET", "/api/fs/default-cwd", 200, `{"cwd":"/home/hermes"}`)
	status, err := dashboardOn(stub).Status(ctx)
	require.NoError(t, err)
	assert.True(t, status.Reachable)
	require.NotNil(t, status.AuthRequired)
	assert.True(t, *status.AuthRequired)
	assert.Equal(t, ptrTo("0.21.0"), status.Version)
	assert.Equal(t, ptrTo("/home/hermes"), status.DefaultCwd)
}

func TestDashboardCronJobsParseTheDashboardShapeAndCreatePostsTheSpec(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/cron/jobs", 200, `[{"id":"8628eaf81dda","name":"Digest","prompt":"p","enabled":true,"state":"scheduled",
	  "schedule":{"kind":"cron","expr":"0 9 * * 1-5","display":"0 9 * * 1-5"},"schedule_display":"0 9 * * 1-5",
	  "last_run_at":"2026-06-12T09:03:08.536535+00:00","next_run_at":"2026-06-15T09:00:00+00:00"},
	 {"id":"paused1","enabled":false}]`)
	stub.Respond("POST", "/api/cron/jobs", 200, `{"id":"new1","name":"luminavault-nightly-compile","schedule":"0 3 * * *","enabled":true}`)
	client := dashboardOn(stub)
	jobs, err := client.ListJobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, "0 9 * * 1-5", *jobs[0].Schedule)
	assert.False(t, jobs[0].Paused)
	assert.NotNil(t, jobs[0].LastRunAt)
	assert.NotNil(t, jobs[0].NextRunAt)
	assert.True(t, jobs[1].Paused)

	created, err := client.CreateJob(ctx, JobSpec{Name: "luminavault-nightly-compile", Schedule: "0 3 * * *", Prompt: "compile", Deliver: "origin", Skills: []string{"kb-compile"}})
	require.NoError(t, err)
	assert.Equal(t, "new1", created.ID)
	posts := requestsWith(stub, "POST")
	require.NotEmpty(t, posts)
	assert.Contains(t, posts[0].Body, `"schedule":"0 3 * * *"`)
	assert.Contains(t, posts[0].Body, `"skills":["kb-compile"]`)
	assert.Contains(t, posts[0].Body, `"deliver":"origin"`)
}

func TestDashboardJobMutationsHitTheCronRoutesAndMap404ToNotFound(t *testing.T) {
	stub := hermesdashtest.New()
	job := `{"id":"j1","name":"Digest","enabled":true,"schedule":{"kind":"cron","expr":"0 3 * * *"}}`
	stub.Respond("PUT", "/api/cron/jobs/j1", 200, job)
	stub.Respond("POST", "/api/cron/jobs/j1/pause", 200, `{"id":"j1","enabled":false}`)
	stub.Respond("POST", "/api/cron/jobs/j1/resume", 200, job)
	stub.Respond("POST", "/api/cron/jobs/j1/trigger", 200, job)
	stub.Respond("DELETE", "/api/cron/jobs/j1", 200, `{"ok":true}`)
	client := dashboardOn(stub)

	updated, err := client.UpdateJob(ctx, "j1", JobUpdate{Name: ptrTo("Digest"), Schedule: ptrTo("0 3 * * *"), Enabled: ptrTo(true)})
	require.NoError(t, err)
	assert.Equal(t, "j1", updated.ID)
	puts := requestsWith(stub, "PUT")
	require.NotEmpty(t, puts)
	// CronJobUpdate nests the patch under `updates`.
	assert.Contains(t, puts[0].Body, `"updates"`)
	assert.Contains(t, puts[0].Body, `"schedule":"0 3 * * *"`)
	assert.Contains(t, puts[0].Body, `"enabled":true`)

	paused, err := client.PauseJob(ctx, "j1")
	require.NoError(t, err)
	assert.True(t, paused.Paused)
	resumed, err := client.ResumeJob(ctx, "j1")
	require.NoError(t, err)
	assert.False(t, resumed.Paused)
	triggered, err := client.TriggerJob(ctx, "j1")
	require.NoError(t, err)
	assert.Equal(t, "j1", triggered.ID)
	require.NoError(t, client.DeleteJob(ctx, "j1"))

	// Unstubbed ids answer 404 from the stub → stable notFound.
	_, err = client.PauseJob(ctx, "gone")
	assert.Equal(t, NotFound("job:gone"), err)
	assert.Equal(t, NotFound("job:gone"), client.DeleteJob(ctx, "gone"))
	assert.Equal(t, InvalidPath("job:../etc"), client.DeleteJob(ctx, "../etc"))
}

func TestDashboardJobRunsParsesTheRunSessionRowsAndClampsTheLimit(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/cron/jobs/digest/runs", 200, `{"runs":[{"id":"cron_digest_1756800000","started_at":"2026-09-02T03:00:00+00:00","ended_at":"2026-09-02T03:01:00+00:00",
	          "input_tokens":120,"output_tokens":340,"is_active":false},
	         {"id":"cron_digest_1756713600","created_at":"2026-09-01T03:00:00+00:00","is_active":true},
	         {"started_at":"2026-09-01T03:00:00+00:00"}]}`)
	runs, err := dashboardOn(stub).JobRuns(ctx, "digest", 5000)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "cron_digest_1756800000", runs[0].Key)
	assert.Equal(t, "cron_digest_1756713600", runs[1].Key)
	assert.Equal(t, RunOK, runs[0].Status)
	assert.NotNil(t, runs[0].FinishedAt)
	assert.Equal(t, ptrTo(120), runs[0].TokensIn)
	assert.Equal(t, ptrTo(340), runs[0].TokensOut)
	// Still running: no finish time, and the collector leaves it for later.
	assert.Equal(t, RunRunning, runs[1].Status)
	assert.Nil(t, runs[1].FinishedAt)
	assert.Contains(t, stub.Requests()[0].URL, "limit=100")
}

func TestDashboardJobRunsTellsAnUnknownJobFromAMissingRunsRoute(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/cron/jobs/gone/runs", 404, `{"detail":"Job not found"}`)
	// The gateway api_server has no /api/cron/*; its 404 is aiohttp's.
	stub.Respond("GET", "/api/cron/jobs/digest/runs", 404, `{"detail":"Not Found"}`)
	client := dashboardOn(stub)
	_, err := client.JobRuns(ctx, "gone", 5)
	assert.Equal(t, NotFound("job:gone"), err)
	_, err = client.JobRuns(ctx, "digest", 5)
	assert.Equal(t, Unsupported("jobRuns"), err)
}

func TestDashboardJobRunOutputReturnsTheRunsFinalAssistantMessage(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/sessions/cron_digest_1/messages", 200, `{"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"# Digest\n"},{"role":"assistant","content":"  "}]}`)
	out, err := dashboardOn(stub).JobRunOutput(ctx, "digest", "cron_digest_1")
	require.NoError(t, err)
	assert.Equal(t, ptrTo("# Digest"), out)
	assert.Nil(t, FinalAssistantText(nil))
}

func TestDashboardFSListMapsEntriesAndRejectsTraversalBeforeAnyRequest(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/fs/list", 200, `{"entries":[{"name":"raw","path":"/kb/raw","isDirectory":true},{"name":"a.md","path":"/kb/a.md","isDirectory":false}]}`)
	client := dashboardOn(stub)
	entries, err := client.ListFiles(ctx, "/kb/")
	require.NoError(t, err)
	assert.Equal(t, []FileEntry{
		{Name: "raw", Path: "/kb/raw", IsDirectory: true},
		{Name: "a.md", Path: "/kb/a.md", IsDirectory: false},
	}, entries)
	assert.Contains(t, stub.Requests()[0].URL, "path=/kb")

	_, err = client.ListFiles(ctx, "/kb/../etc")
	assert.Equal(t, InvalidPath("/kb/../etc"), err)
	_, err = client.ReadText(ctx, "relative/path")
	assert.Equal(t, InvalidPath("relative/path"), err)
	assert.Len(t, stub.Requests(), 1)
}

func TestDashboardFSReadTextMapsENOENTAnd413AndBinary(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/fs/list", 200, `{"entries":[],"error":"ENOENT"}`)
	stub.Respond("GET", "/api/fs/read-text", 413, `{"detail":"File too large"}`)
	client := dashboardOn(stub)
	_, err := client.ListFiles(ctx, "/missing")
	assert.Equal(t, NotFound("/missing"), err)
	_, err = client.ReadText(ctx, "/big.md")
	assert.Equal(t, BodyTooLarge("/big.md", FileBodyCap), err)
	binary := hermesdashtest.New()
	binary.Respond("GET", "/api/fs/read-text", 200, `{"binary":true,"text":" "}`)
	_, err = dashboardOn(binary).ReadText(ctx, "/img.png")
	assert.Equal(t, InvalidResponse("binary:/img.png"), err)
}

func TestDashboardWriteTextRefusesOversizedBodiesWithoutARequest(t *testing.T) {
	stub := hermesdashtest.New()
	big := strings.Repeat("x", FileBodyCap+1)
	assert.Equal(t, BodyTooLarge("/kb/big.md", FileBodyCap), dashboardOn(stub).WriteText(ctx, "/kb/big.md", big))
	assert.Empty(t, stub.Requests())
}

func TestDashboardSessionsAndMessagesParseTheDashboardShapes(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/sessions", 200, `{"sessions":[{"id":"s1","title":"Plan","source":"cli","started_at":1756800000.5,"last_active":1756803600,"message_count":4},
	             {"id":"s2","display_name":"Chat","started_at":"2026-09-01T10:00:00Z","message_count":0}],"total":2,"limit":20,"offset":0}`)
	stub.Respond("GET", "/api/sessions/s1/messages", 200, `{"session_id":"s1","messages":[{"role":"user","content":"hi","timestamp":1756800001},
	                                {"role":"assistant","content":[{"type":"text","text":"hello"}]},
	                                {"content":"no role"}]}`)
	client := dashboardOn(stub)
	page, err := client.ListSessions(ctx, 0, 20)
	require.NoError(t, err)
	assert.Equal(t, ptrTo(2), page.Total)
	require.Len(t, page.Sessions, 2)
	assert.Equal(t, "s1", page.Sessions[0].ID)
	assert.Equal(t, "s2", page.Sessions[1].ID)
	assert.Equal(t, ptrTo("Plan"), page.Sessions[0].Title)
	assert.Equal(t, 4, page.Sessions[0].MessageCount)
	require.NotNil(t, page.Sessions[0].LastActiveAt)
	assert.True(t, page.Sessions[0].LastActiveAt.Equal(time.Unix(1_756_803_600, 0)))
	assert.Equal(t, ptrTo("Chat"), page.Sessions[1].Title)
	assert.NotNil(t, page.Sessions[1].StartedAt)
	listURL := stub.Requests()[0].URL
	assert.Contains(t, listURL, "order=recent")
	assert.Contains(t, listURL, "min_messages=1")

	messages, err := client.SessionMessages(ctx, "s1")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, []string{"user", "assistant"}, []string{messages[0].Role, messages[1].Role})
	assert.Equal(t, "hello", messages[1].Content)
}

func TestDashboard429BacksOffUsingRetryAfterThenSucceeds(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/skills", 429, "{}", "Retry-After", "0.01")
	stub.Respond("GET", "/api/skills", 200, `[{"name":"a"}]`)
	var slept []time.Duration
	client := dashboardOn(stub)
	client.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	skills, err := client.ListSkills(ctx)
	require.NoError(t, err)
	require.Len(t, skills, 1)
	assert.Equal(t, "a", skills[0].Name)
	assert.Len(t, stub.Requests(), 2)
	assert.Equal(t, []time.Duration{10 * time.Millisecond}, slept)
	assert.Equal(t, 2*time.Second, RetryDelay("", 2))
	assert.Equal(t, 30*time.Second, RetryDelay("120", 1))
}

func TestDashboardSSRFRejectionStopsTheCallBeforeAnyRequest(t *testing.T) {
	stub := hermesdashtest.New()
	client := NewDashboard("http://169.254.169.254/", "t", ssrfguard.Guard{}, stub)
	_, err := client.ListSkills(ctx)
	assert.Error(t, err)
	assert.Empty(t, stub.Requests())
}

func TestPathValidationNormalisesTrailingSlashesAndRejectsEscapes(t *testing.T) {
	p, err := ValidatePath("/kb/raw/")
	require.NoError(t, err)
	assert.Equal(t, "/kb/raw", p)
	p, err = ValidatePath("/")
	require.NoError(t, err)
	assert.Equal(t, "/", p)
	_, err = ValidatePath("")
	assert.Equal(t, InvalidPath(""), err)
	_, err = ValidatePath("/a/./b")
	assert.Equal(t, InvalidPath("/a/./b"), err)
	nul := "/a/\x00"
	_, err = ValidatePath(nul)
	assert.Equal(t, InvalidPath(nul), err)
	assert.True(t, IsInside("/kb/raw/x.md", "/kb"))
	assert.False(t, IsInside("/kbx/raw", "/kb"))
}

func TestDatesParseISOWithAndWithoutOffsetsAndEpochSeconds(t *testing.T) {
	assert.NotNil(t, ParseDate("2026-06-12T09:03:08.536535+00:00"))
	assert.NotNil(t, ParseDate("2026-06-15T09:00:00+00:00"))
	assert.NotNil(t, ParseDate("2026-06-15T09:00:00.1"))
	epoch := ParseDate(1_756_800_000.5)
	require.NotNil(t, epoch)
	assert.True(t, epoch.Equal(time.Unix(1_756_800_000, 500_000_000)))
	assert.Nil(t, ParseDate(float64(0)))
	assert.Nil(t, ParseDate("nope"))
}

// HermesWorkspacePathTests: the path guards on the workspace surface.

func TestWorkspacePlainRelativePathIsAcceptedAndReturnedUnchanged(t *testing.T) {
	for in, want := range map[string]string{"Sources/App/main.swift": "Sources/App/main.swift", "README.md": "README.md", "  a/b.swift  ": "a/b.swift"} {
		got, err := ValidateRelative(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got)
	}
}

func TestWorkspaceTraversalSegmentsAreRejected(t *testing.T) {
	for _, p := range []string{"../etc/passwd", "a/../../etc/passwd", "a/./b", "..", ".", "a/.."} {
		_, err := ValidateRelative(p)
		assert.Error(t, err, "accepted %q", p)
	}
}

func TestWorkspaceAbsolutePathsAreRejected(t *testing.T) {
	for _, p := range []string{"/etc/passwd", "/", "/a/b"} {
		_, err := ValidateRelative(p)
		assert.Error(t, err, "accepted %q", p)
	}
}

func TestWorkspaceEmptyBlankAndNullBearingPathsAreRejected(t *testing.T) {
	for _, p := range []string{"", "   ", "a\x00b", "a//b"} {
		_, err := ValidateRelative(p)
		assert.Error(t, err, "accepted %q", p)
	}
}

func TestWorkspaceTheAbsoluteValidatorStillDemandsALeadingSlash(t *testing.T) {
	p, err := ValidatePath("/work/repo")
	require.NoError(t, err)
	assert.Equal(t, "/work/repo", p)
	_, err = ValidatePath("work/repo")
	assert.Error(t, err)
	_, err = ValidatePath("/work/../etc")
	assert.Error(t, err)
}

// JSONValueFoundationBridgeTests: the raw job document keeps booleans and
// numbers apart (Go's decoder is the bridge here).

func TestJSONBooleansSurviveAsBooleansNotAsOneAndZero(t *testing.T) {
	fields, ok := decodeAny([]byte(`{"paused": true, "enabled": false}`)).(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, fields["paused"])
	assert.Equal(t, false, fields["enabled"])
}

func TestJSONNumbersStayNumbersIncludingOneAndZero(t *testing.T) {
	fields, ok := decodeAny([]byte(`{"one": 1, "zero": 0, "pi": 3.5, "big": 1756800000}`)).(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(1), fields["one"])
	assert.Equal(t, float64(0), fields["zero"])
	assert.Equal(t, 3.5, fields["pi"])
	assert.Equal(t, float64(1_756_800_000), fields["big"])
}

func TestJSONStringsNullAndNestingRoundTrip(t *testing.T) {
	fields, ok := decodeAny([]byte(`{"name": "digest", "gone": null, "tags": ["a", 2, true]}`)).(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "digest", fields["name"])
	gone, present := fields["gone"]
	assert.True(t, present)
	assert.Nil(t, gone)
	assert.Equal(t, []any{"a", float64(2), true}, fields["tags"])
}

func TestJSONACronRowKeepsPausedBooleanAndScheduleStringApart(t *testing.T) {
	raw := []byte(`{"id":"digest","name":"Digest","paused":false,"schedule":"0 3 * * *","runs":12}`)
	fields, ok := decodeAny(raw).(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, fields["paused"])
	assert.NotEqual(t, float64(0), fields["paused"])
	assert.Equal(t, "0 3 * * *", fields["schedule"])
	assert.Equal(t, float64(12), fields["runs"])
	// And the stored raw column round-trips the same document.
	job, ok := ParseJob(fields)
	require.True(t, ok)
	assert.JSONEq(t, string(raw), string(job.Raw))
}
