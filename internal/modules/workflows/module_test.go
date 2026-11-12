package workflows

import (
	"bufio"
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/selfimprove"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

var (
	wfAnn = uuid.MustParse("F10F0000-0000-4000-8000-0000000000A1")
	wfCat = uuid.MustParse("F10F0000-0000-4000-8000-0000000000A3")
	wfFay = uuid.MustParse("F10F0000-0000-4000-8000-0000000000A4")
	wfTia = uuid.MustParse("F10F0000-0000-4000-8000-0000000000A5")
	wfArc = uuid.MustParse("F10F0000-0000-4000-8000-0000000000A7")
	wfByo = uuid.MustParse("F10F0000-0000-4000-8000-0000000000A9")
	wfRun = uuid.MustParse("F10F0000-0000-4000-8000-0000000000AA")
	wfEvt = uuid.MustParse("F10F0000-0000-4000-8000-0000000000AD")

	testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

const (
	parityMasterKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	runE1           = "F10C0000-0000-4000-8000-0000000000E1"
	runE2           = "F10C0000-0000-4000-8000-0000000000E2"
	runE4           = "F10C0000-0000-4000-8000-0000000000E4"
	runB1           = "F10C0000-0000-4000-8000-0000000000B1"
	runA1           = "F10C0000-0000-4000-8000-0000000000A1"
	hookOnLimit     = "F1080000-0000-4000-8000-0000000000AB"
)

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	m    *Module
	h    http.Handler
}

func setup(t *testing.T, change func(*Options)) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, file := range []string{"base.sql", "workflows.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + file)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, file)
	}
	box, err := secretbox.New(parityMasterKey)
	require.NoError(t, err)
	opts := Options{
		Identify:    auth.Identify(testKeys, auth.PGUsers(pool)),
		Limits:      ratelimit.NewStore(),
		Box:         box,
		Preferences: llm.NewPreferenceRepository(pool),
		Activity:    selfimprove.NewActivity(pool, true, nil),
	}
	if change != nil {
		change(&opts)
	}
	m := New(pool, opts)
	e := httpserver.New(config.Config{}, httpserver.Deps{Modules: []httpserver.Module{m}})
	var h http.Handler = e
	return env{t: t, pool: pool, m: m, h: h}
}

func (v env) do(user uuid.UUID, method, path, body string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		token, err := scenario.Mint(testKeys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.h.ServeHTTP(rec, req)
	return rec
}

func webhookDefinition() WorkflowDefinitionDTO {
	trigger, output := testNode(Trigger, nil), testNode(Output, StringMap{"value": "{{event}}"})
	return testDefinition(Webhook, []WorkflowNodeDTO{trigger, output}, []WorkflowEdgeDTO{testEdge(trigger, output)})
}

// TestCreatesPublishesDeduplicatesAndCancelsRun ports WorkflowLifecycleIntegrationTests
// (creates publishes deduplicates and cancels run). The
// engine's reserve and reconcile stay in Swift, so the 12345 micros they would book
// is written as they write it.
func TestCreatesPublishesDeduplicatesAndCancelsRun(t *testing.T) {
	v := setup(t, func(o *Options) { o.ManagedInferenceAvailable = true })
	ctx := context.Background()
	created, err := v.m.createWorkflow(ctx, wfAnn, "Webhook", nil, webhookDefinition())
	require.NoError(t, err)
	id := created.Workflow.Id.UUID
	_, err = v.m.publishWorkflow(ctx, wfAnn, id)
	require.NoError(t, err)

	key := "webhook:event-1"
	first, err := v.m.enqueue(ctx, wfAnn, id, Webhook, StringMap{"event": "created"}, nil, &key)
	require.NoError(t, err)
	duplicate, err := v.m.enqueue(ctx, wfAnn, id, Webhook, StringMap{"event": "created"}, nil, &key)
	require.NoError(t, err)
	require.Equal(t, first.Id, duplicate.Id)

	_, err = v.pool.Exec(ctx, `UPDATE workflow_runs SET managed_spend_usd_micros = 12345 WHERE id = $1`, first.Id.UUID)
	require.NoError(t, err)
	day, month := periodStarts(time.Now())
	for kind, start := range map[string]time.Time{"day": day, "month": month} {
		_, err = v.pool.Exec(ctx, `INSERT INTO workflow_spend_buckets (scope_key, period_kind, period_start, spent_usd_micros, reserved_usd_micros)
			VALUES ($1, $2, $3, 12345, 0)`, "tenant:"+strings.ToUpper(wfAnn.String()), kind, start)
		require.NoError(t, err)
	}
	limits := v.m.limitsFor(ctx, wfAnn, tierPro)
	require.Equal(t, 1, limits.ActiveRuns)
	require.EqualValues(t, 12345, limits.DailySpentUsdMicros)
	require.EqualValues(t, 12345, limits.MonthlySpentUsdMicros)

	detail, err := v.m.runByID(ctx, wfAnn, first.Id.UUID)
	require.NoError(t, err)
	require.EqualValues(t, 12345, *detail.ManagedSpendUsdMicros)
	require.EqualValues(t, 200_000, *detail.ManagedSpendLimitUsdMicros)

	require.NoError(t, v.m.cancelRun(ctx, wfAnn, first.Id.UUID))

	// Two replicas race on one delivery. The unique index is the final guard.
	raced := "webhook:event-2"
	results := make([]WorkflowRunDTO, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			results[i], errs[i] = v.m.enqueue(ctx, wfAnn, id, Webhook, StringMap{"event": "raced"}, nil, &raced)
		})
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.Equal(t, results[0].Id, results[1].Id)
	require.NoError(t, v.m.cancelRun(ctx, wfAnn, results[0].Id.UUID))

	runs, err := v.m.listRuns(ctx, wfAnn, &id)
	require.NoError(t, err)
	require.Equal(t, WorkflowRunStatusCancelled, runs.Runs[0].Status)
}

// TestResumeRespectsActiveRunLimit ports WorkflowLifecycleIntegrationTests (resume
// respects active run limit).
func TestResumeRespectsActiveRunLimit(t *testing.T) {
	v := setup(t, nil)
	ctx := context.Background()
	created, err := v.m.createWorkflow(ctx, wfAnn, "Resume Limit", nil, webhookDefinition())
	require.NoError(t, err)
	id := created.Workflow.Id.UUID
	_, err = v.m.publishWorkflow(ctx, wfAnn, id)
	require.NoError(t, err)
	active, err := v.m.enqueue(ctx, wfAnn, id, Manual, StringMap{"event": "active"}, nil, nil)
	require.NoError(t, err)

	paused := uuid.New()
	_, err = v.pool.Exec(ctx, `INSERT INTO workflow_runs (id, tenant_id, workflow_id, version_id, status, trigger_kind, input, pause_reason, managed_spend_usd_micros, managed_spend_limit_usd_micros, created_at, updated_at)
		SELECT $1, tenant_id, workflow_id, version_id, 'paused', 'manual', '{"event":"paused"}', 'dailySpendLimit', 0, 200000, now(), now() FROM workflow_runs WHERE id = $2`, paused, active.Id.UUID)
	require.NoError(t, err)

	_, err = v.m.resumeRun(ctx, wfAnn, paused)
	require.ErrorIs(t, err, errActiveRunLimit)
	var status, reason string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT status, pause_reason FROM workflow_runs WHERE id = $1`, paused).Scan(&status, &reason))
	require.Equal(t, "paused", status)
	require.Equal(t, "dailySpendLimit", reason)
	require.NoError(t, v.m.cancelRun(ctx, wfAnn, active.Id.UUID))
}

func TestEventRoutesFailOnARowLikeSwift(t *testing.T) {
	v := setup(t, nil)
	require.Equal(t, http.StatusInternalServerError, v.do(wfEvt, http.MethodGet, "/v1/workflows/runs/"+runE1+"/events", "").Code)
	require.Equal(t, http.StatusOK, v.do(wfEvt, http.MethodGet, "/v1/workflows/runs/F10C0000-0000-4000-8000-0000000000E3/events", "").Code)
	stream := v.do(wfEvt, http.MethodGet, "/v1/workflows/runs/"+runE1+"/events/stream", "")
	require.Equal(t, http.StatusOK, stream.Code)
	require.Empty(t, stream.Body.String(), "the stream catches the failure and finishes")
}

func TestEventFramesWithDecodingOn(t *testing.T) {
	v := setup(t, func(o *Options) { o.DecodeEventRows = true })
	list := v.do(wfEvt, http.MethodGet, "/v1/workflows/runs/"+runE1+"/events", "")
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	var out struct {
		Events []struct {
			Kind    string            `json:"kind"`
			Message *string           `json:"message"`
			Data    map[string]string `json:"data"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(list.Body.Bytes(), &out))
	require.Len(t, out.Events, 2)
	require.Equal(t, "runQueued", out.Events[0].Kind)
	require.Equal(t, map[string]string{"kept": "yes"}, out.Events[1].Data, "the provider keys are scrubbed")
	require.Equal(t, "Managed provider unavailable; retrying on a backup route.", *out.Events[1].Message)

	stream := v.do(wfEvt, http.MethodGet, "/v1/workflows/runs/"+runE4+"/events/stream", "")
	require.Equal(t, http.StatusOK, stream.Code)
	require.Equal(t, "text/event-stream", stream.Header().Get("Content-Type"))
	require.Equal(t, "no", stream.Header().Get("X-Accel-Buffering"))
	body := stream.Body.String()
	require.Contains(t, body, "event: runCompleted\n")
	require.Contains(t, body, "id: ")
	require.Contains(t, body, "data: {")
}

func TestStreamEndsWhenTheClientLeaves(t *testing.T) {
	v := setup(t, nil)
	srv := httptest.NewServer(v.h)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/workflows/runs/"+runE2+"/events/stream", nil)
	require.NoError(t, err)
	token, err := scenario.Mint(testKeys, wfEvt)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, http.StatusOK, res.StatusCode)
	line, err := bufio.NewReader(res.Body).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, ": keep-alive\n", line)
	cancel()
	done := make(chan struct{})
	go func() { srv.CloseClientConnections(); srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler is still running")
	}
}

func TestModelDisclosure(t *testing.T) {
	v := setup(t, nil)
	hidden := v.do(wfRun, http.MethodGet, "/v1/workflows/runs/"+runA1, "")
	require.Equal(t, http.StatusOK, hidden.Code, hidden.Body.String())
	require.NotContains(t, hidden.Body.String(), "deepseek")
	require.NotContains(t, hidden.Body.String(), "openRouter")
	shown := v.do(wfByo, http.MethodGet, "/v1/workflows/runs/"+runB1, "")
	require.Equal(t, http.StatusOK, shown.Code, shown.Body.String())
	require.Contains(t, shown.Body.String(), "claude-x")
}

func TestWebhookRoutesWithoutAMasterKey(t *testing.T) {
	v := setup(t, func(o *Options) { o.Box = nil })
	require.Equal(t, http.StatusServiceUnavailable, v.do(uuid.Nil, http.MethodPost, "/v1/workflow-hooks/"+hookOnLimit, "{}").Code)
	require.Equal(t, http.StatusServiceUnavailable, v.do(wfRun, http.MethodPost, "/v1/workflows/F10A0000-0000-4000-8000-0000000000AA/webhook/rotate", "").Code)
	require.Equal(t, http.StatusOK, v.do(wfRun, http.MethodGet, "/v1/workflows/limits", "").Code)
}

func TestRateLimitBacksOffPerTier(t *testing.T) {
	v := setup(t, nil)
	last := 0
	for range 7 {
		last = v.do(wfArc, http.MethodGet, "/v1/workflows/runs", "").Code
	}
	require.Equal(t, http.StatusTooManyRequests, last, "archived is 60 x 0.1")
	require.NotEqual(t, http.StatusTooManyRequests, v.do(wfCat, http.MethodGet, "/v1/workflows/runs", "").Code)
	require.Equal(t, http.StatusForbidden, v.do(wfArc, http.MethodGet, "/v1/workflow-templates", "").Code, "templates have no limit, and archived can't read them")
}

func TestOrderBodyThenTokenThenID(t *testing.T) {
	v := setup(t, nil)
	require.Equal(t, http.StatusBadRequest, v.do(uuid.Nil, http.MethodPost, "/v1/workflows", "{").Code)
	require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/workflows/nope", "").Code)
	require.Equal(t, http.StatusBadRequest, v.do(wfCat, http.MethodGet, "/v1/workflows/nope", "").Code)
}

func TestTiers(t *testing.T) {
	v := setup(t, nil)
	var limits WorkflowLimitsDTO
	require.NoError(t, json.Unmarshal(v.do(wfFay, http.MethodGet, "/v1/workflows/limits", "").Body.Bytes(), &limits))
	require.False(t, limits.CanAuthor)
	require.Equal(t, http.StatusOK, v.do(wfFay, http.MethodGet, "/v1/workflows", "").Code)
	require.Equal(t, http.StatusForbidden, v.do(wfFay, http.MethodPost, "/v1/workflows", `{"name":"x","definition":{"trigger":"manual","nodes":[],"edges":[]}}`).Code)
	require.Equal(t, http.StatusInternalServerError, v.do(wfTia, http.MethodGet, "/v1/workflows", "").Code, "trial's list has no mapErrors")
	require.Equal(t, http.StatusForbidden, v.do(wfTia, http.MethodGet, "/v1/workflows/F10A0000-0000-4000-8000-0000000000A5", "").Code)
}

func TestIsolation(t *testing.T) {
	v := setup(t, nil)
	foreign := "/v1/workflows/F10A0000-0000-4000-8000-0000000000AA"
	require.Equal(t, http.StatusNotFound, v.do(wfCat, http.MethodGet, foreign, "").Code)
	require.Equal(t, http.StatusNotFound, v.do(wfCat, http.MethodPost, foreign+"/publish", "").Code)
	require.Equal(t, http.StatusNotFound, v.do(wfCat, http.MethodGet, "/v1/workflows/runs/"+runA1, "").Code)
	require.Equal(t, http.StatusNotFound, v.do(wfCat, http.MethodPost, "/v1/workflows/runs/"+runA1+"/cancel", "").Code)
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, func(o *Options) { o.DecodeEventRows = true })
	definition := `{"trigger":"manual","nodes":[{"id":"F10D0000-0000-4000-8000-000000000001","kind":"trigger","name":"Start","x":0,"y":0,"configuration":{}},{"id":"F10D0000-0000-4000-8000-000000000002","kind":"output","name":"End","x":1.5,"y":0,"configuration":{}}],"edges":[{"id":"F10E0000-0000-4000-8000-000000000001","sourceNodeID":"F10D0000-0000-4000-8000-000000000001","sourcePort":"output","targetNodeID":"F10D0000-0000-4000-8000-000000000002","targetPort":"input"}]}`
	created := v.do(wfCat, http.MethodPost, "/v1/workflows", `{"name":"Spec","definition":`+definition+`}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var detail WorkflowDetailDTO
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &detail))
	id := detail.Workflow.Id.String()
	require.Equal(t, http.StatusOK, v.do(wfCat, http.MethodPost, "/v1/workflows/"+id+"/publish", "").Code)
	ran := v.do(wfCat, http.MethodPost, "/v1/workflows/"+id+"/runs", `{"input":{"a":"b"}}`)
	require.Equal(t, http.StatusOK, ran.Code, ran.Body.String())

	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
	}{
		{wfCat, http.MethodGet, "/v1/workflows", ""},
		{wfCat, http.MethodGet, "/v1/workflows/" + id, ""},
		{wfCat, http.MethodPut, "/v1/workflows/" + id + "/draft", `{"expectedRevision":1,"definition":` + definition + `}`},
		{wfCat, http.MethodGet, "/v1/workflows/" + id + "/versions", ""},
		{wfCat, http.MethodGet, "/v1/workflows/" + id + "/runs", ""},
		{wfCat, http.MethodPost, "/v1/workflows/validate", definition},
		{wfCat, http.MethodPost, "/v1/workflows/" + id + "/webhook/rotate", ""},
		{wfCat, http.MethodGet, "/v1/workflows/limits", ""},
		{wfCat, http.MethodGet, "/v1/workflows/runs", ""},
		{wfCat, http.MethodGet, "/v1/workflow-templates", ""},
		{wfRun, http.MethodGet, "/v1/workflows/runs/" + runA1, ""},
		{wfEvt, http.MethodGet, "/v1/workflows/runs/" + runE1 + "/events", ""},
		{uuid.MustParse("F10F0000-0000-4000-8000-0000000000AC"), http.MethodGet, "/v1/workflows/approvals", ""},
	} {
		r := v.do(c.user, c.method, c.path, c.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.Code,
			Header: http.Header{"Content-Type": {r.Header().Get("Content-Type")}},
		}
		out.SetBodyBytes(r.Body.Bytes())
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.Body.String())
	}
}
