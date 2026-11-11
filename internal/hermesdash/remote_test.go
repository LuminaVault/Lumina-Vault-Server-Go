package hermesdash

import (
	"lumina/internal/hermesdash/hermesdashtest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GatewayOnlyMirrorTests: a BYO tenant who linked only their gateway reads
// skills, jobs and sessions over it and mutates jobs over /api/jobs.

func gatewayOnly(stub *hermesdashtest.Stub) *Remote {
	base, _ := url.Parse("http://100.105.117.67:8642")
	auth := "Bearer gateway-key"
	return &Remote{GatewayBaseURL: base, GatewayAuthHeader: &auth, Gateway: NewGateway(base, &auth, stub), SkillsHTTP: stub}
}

func TestGatewayOnlySkillsComeFromTheGatewayWhenThereIsNoDashboard(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/v1/skills", 200, `{"skills":[
	  {"name":"kb-compile","description":"Compile raw into wiki"},
	  {"name":"brandkit","description":"Brand assets"}
	]}`)
	skills, err := gatewayOnly(stub).ListSkills(ctx)
	require.NoError(t, err)
	// The catalog parser title-cases the slug and sorts by display name.
	require.Len(t, skills, 2)
	assert.Equal(t, []string{"Brandkit", "Kb Compile"}, []string{skills[0].Name, skills[1].Name})
	assert.Equal(t, "Compile raw into wiki", skills[1].Description)
}

func TestGatewayOnlyJobsComeFromTheGatewayAPIJobsRoute(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/jobs", 200, `{"jobs":[
	  {"id":"digest","name":"Daily Digest","schedule":"0 9 * * *","prompt":"summarise","enabled":true},
	  {"id":"compile","name":"Nightly compile","schedule":"0 3 * * *","prompt":"kb-compile","enabled":true}
	]}`)
	jobs, err := gatewayOnly(stub).ListJobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, []string{"digest", "compile"}, []string{jobs[0].ID, jobs[1].ID})
	assert.Equal(t, "0 9 * * *", *jobs[0].Schedule)
}

func TestGatewayOnlySessionsComeFromTheGatewayAPISessionsRoute(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/sessions", 200, `{"sessions":[{"id":"s1","title":"Planning","message_count":12,"updated_at":"2026-09-01T10:00:00Z"}],"total":1}`)
	page, err := gatewayOnly(stub).ListSessions(ctx, 0, 50)
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "s1", page.Sessions[0].ID)
}

func TestGatewayOnlyFilesystemWritesFailWithTheAuthModeError(t *testing.T) {
	transport := gatewayOnly(hermesdashtest.New())
	assert.Equal(t, ErrAuthModeUnsupported, transport.Mkdir(ctx, "/home/hermes/kb"))
	assert.Equal(t, ErrAuthModeUnsupported, transport.WriteText(ctx, "/home/hermes/kb/note.md", "x"))
}

func hasRequest(stub *hermesdashtest.Stub, method, fragment string) bool {
	for _, r := range stub.Requests() {
		if r.Method == method && strings.Contains(r.URL, fragment) {
			return true
		}
	}
	return false
}

func TestGatewayOnlyPauseGoesToTheGatewayAPIJobsRoute(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("POST", "/api/jobs/digest/pause", 200, `{"job":{"id":"digest","name":"Daily Digest","schedule":"0 9 * * *","prompt":"summarise","enabled":false}}`)
	job, err := gatewayOnly(stub).PauseJob(ctx, "digest")
	require.NoError(t, err)
	assert.Equal(t, "digest", job.ID)
	assert.True(t, job.Paused)
	assert.True(t, hasRequest(stub, "POST", "/api/jobs/digest/pause"))
}

func TestGatewayOnlyTriggerMapsToTheGatewayRunAction(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("POST", "/api/jobs/digest/run", 200, `{"job":{"id":"digest","name":"Daily Digest","schedule":"0 9 * * *","prompt":"summarise","enabled":true}}`)
	job, err := gatewayOnly(stub).TriggerJob(ctx, "digest")
	require.NoError(t, err)
	assert.Equal(t, "digest", job.ID)
	assert.True(t, hasRequest(stub, "POST", "/api/jobs/digest/run"))
}

func TestGatewayOnlyCreatePostsTheJobBodyToTheGateway(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("POST", "/api/jobs", 200, `{"job":{"id":"digest","name":"Daily Digest","schedule":"0 9 * * *","prompt":"summarise","enabled":true}}`)
	job, err := gatewayOnly(stub).CreateJob(ctx, JobSpec{Name: "Daily Digest", Schedule: "0 9 * * *", Prompt: "summarise", Deliver: "local"})
	require.NoError(t, err)
	assert.Equal(t, "digest", job.ID)
	var body string
	for _, r := range stub.Requests() {
		if r.Method == "POST" && strings.HasSuffix(strings.SplitN(r.URL, "?", 2)[0], "/api/jobs") {
			body = r.Body
		}
	}
	assert.Contains(t, body, "Daily Digest")
}

func TestGatewayOnlyUpdatePatchesFlattenedFieldsNotADashboardUpdatesWrapper(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("PATCH", "/api/jobs/digest", 200, `{"job":{"id":"digest","name":"Evening Digest","schedule":"0 18 * * *","prompt":"summarise","enabled":true}}`)
	job, err := gatewayOnly(stub).UpdateJob(ctx, "digest", JobUpdate{Name: ptrTo("Evening Digest"), Schedule: ptrTo("0 18 * * *")})
	require.NoError(t, err)
	assert.Equal(t, "Evening Digest", *job.Name)
	var body string
	for _, r := range stub.Requests() {
		if r.Method == "PATCH" {
			body = r.Body
		}
	}
	assert.NotContains(t, body, `"updates"`)
	assert.Contains(t, body, "Evening Digest")
}

func TestGatewayOnlyDeleteHitsTheGatewayAndSucceedsWithoutADashboard(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("DELETE", "/api/jobs/digest", 200, `{"ok":true}`)
	require.NoError(t, gatewayOnly(stub).DeleteJob(ctx, "digest"))
	assert.True(t, hasRequest(stub, "DELETE", "/api/jobs/digest"))
}

func TestGatewayOnlyStatusReportsReachableFromTheGatewayAlone(t *testing.T) {
	status, err := gatewayOnly(hermesdashtest.New()).Status(ctx)
	require.NoError(t, err)
	assert.True(t, status.Reachable)
}
