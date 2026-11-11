package hermesruns

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/hermesresolve"
	"lumina/internal/hermesruns"
	"lumina/internal/hermesruns/hermesrunstest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/ratelimit"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Ported from AppTests/Hermes/Runs/HermesRunsControllerTests.swift (11 tests).

var testKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type env struct {
	t       *testing.T
	e       *echo.Echo
	pool    *pgxpool.Pool
	service *hermesruns.Service
}

func newEnv(t *testing.T, pool *pgxpool.Pool) env {
	t.Helper()
	gateway := hermesrunstest.Accepting(t, "run_1")
	service := hermesruns.NewService(hermesruns.Options{
		Pool: pool, Notifier: &hermesrunstest.Notifier{},
		Resolve: func(context.Context, uuid.UUID) (hermesresolve.Resolution, error) {
			return hermesresolve.Resolution{BaseURL: gateway.URL()}, nil
		},
		MakeClient: func(_ hermesresolve.Resolution, key string) *hermesruns.Client { return gateway.Client(key) },
	})
	t.Cleanup(func() { service.StopAll() })
	users := auth.PGUsers(pool)
	module := New(Options{Identify: auth.Identify(testKeys, users), Limits: ratelimit.NewStore(), Service: service, PollInterval: 50 * time.Millisecond})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(testKeys, users), Modules: []httpserver.Module{module}})
	return env{t: t, e: e, pool: pool, service: service}
}

func (v env) tenant() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	short := strings.ToLower(id.String()[:8])
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'trial', now(), now())`, id, "runs-"+short+"@test.luminavault", "runs-"+short)
	require.NoError(v.t, err)
	return id
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
	v.e.ServeHTTP(rec, req)
	return rec
}

// seedRun is HermesRunsControllerTests.seedRun: a run with events already persisted.
func (v env) seedRun(tenant uuid.UUID, status hermesruns.Status, pending *hermesruns.PendingApproval, events ...string) uuid.UUID {
	v.t.Helper()
	r := &hermesruns.Run{
		ID: uuid.New(), Tenant: tenant, HermesRunID: "run_" + uuid.NewString()[:8], Status: status,
		Prompt: "seeded", StartedAt: time.Now(), PendingApproval: pending,
	}
	if status == hermesruns.Completed {
		summary := "all done"
		r.Summary = &summary
	}
	require.NoError(v.t, v.service.Store().Insert(context.Background(), r))
	for _, name := range events {
		payload := map[string]any{"event": name}
		if name == "run.completed" {
			payload["output"] = "all done"
		}
		_, err := v.service.Store().Append(context.Background(), r.ID, name, payload, time.Now(), nil)
		require.NoError(v.t, err)
	}
	return r.ID
}

func sseEvents(t *testing.T, body string) (names []string, events []hermesruns.EventDTO) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			names = append(names, strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			var e hermesruns.EventDTO
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e))
			events = append(events, e)
		}
	}
	return
}

func decodeRun(t *testing.T, rec *httptest.ResponseRecorder) hermesruns.DTO {
	t.Helper()
	var run hermesruns.DTO
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run), rec.Body.String())
	return run
}

func TestController(t *testing.T) {
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	v := newEnv(t, pool)

	t.Run("every runs route requires a token", func(t *testing.T) {
		id := uuid.NewString()
		for _, route := range [][2]string{
			{http.MethodGet, "/v1/hermes/runs"},
			{http.MethodPost, "/v1/hermes/runs"},
			{http.MethodGet, "/v1/hermes/runs/" + id},
			{http.MethodGet, "/v1/hermes/runs/" + id + "/events"},
			{http.MethodPost, "/v1/hermes/runs/" + id + "/approval"},
			{http.MethodPost, "/v1/hermes/runs/" + id + "/stop"},
		} {
			require.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, route[0], route[1], "").Code, route)
		}
	})

	t.Run("list returns only the caller's runs", func(t *testing.T) {
		tenant := v.tenant()
		rec := v.do(tenant, http.MethodGet, "/v1/hermes/runs", "")
		require.Equal(t, http.StatusOK, rec.Code)
		require.JSONEq(t, `{"runs":[]}`, rec.Body.String())
		first := v.seedRun(tenant, hermesruns.Completed, nil)
		second := v.seedRun(tenant, hermesruns.Completed, nil)
		v.seedRun(v.tenant(), hermesruns.Completed, nil)
		rec = v.do(tenant, http.MethodGet, "/v1/hermes/runs", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var list struct{ Runs []hermesruns.DTO }
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.ElementsMatch(t, []uuid.UUID{first, second}, []uuid.UUID{list.Runs[0].ID.UUID, list.Runs[1].ID.UUID})
		require.Len(t, list.Runs, 2)
		for _, r := range list.Runs {
			require.Equal(t, hermesruns.Completed, r.Status)
		}
	})

	t.Run("get returns the run and 404s for another tenant or an unknown id", func(t *testing.T) {
		tenant := v.tenant()
		id := v.seedRun(tenant, hermesruns.Completed, nil)
		rec := v.do(tenant, http.MethodGet, "/v1/hermes/runs/"+id.String(), "")
		require.Equal(t, http.StatusOK, rec.Code)
		run := decodeRun(t, rec)
		require.Equal(t, id, run.ID.UUID)
		require.Equal(t, "all done", *run.Summary)
		require.Equal(t, hermesruns.Completed, run.Status)
		require.Equal(t, http.StatusNotFound, v.do(v.tenant(), http.MethodGet, "/v1/hermes/runs/"+id.String(), "").Code)
		require.Equal(t, http.StatusNotFound, v.do(tenant, http.MethodGet, "/v1/hermes/runs/"+uuid.NewString(), "").Code)
	})

	t.Run("events replays a finished run as SSE and closes", func(t *testing.T) {
		tenant := v.tenant()
		id := v.seedRun(tenant, hermesruns.Completed, nil, "run.started", "tool.started", "run.completed")
		rec := v.do(tenant, http.MethodGet, "/v1/hermes/runs/"+id.String()+"/events", "")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
		names, events := sseEvents(t, rec.Body.String())
		require.Equal(t, []string{eventName, eventName, eventName}, names)
		require.Len(t, events, 3)
		require.Equal(t, []int{1, 2, 3}, []int{events[0].Seq, events[1].Seq, events[2].Seq})
		require.Equal(t, []string{"run.started", "tool.started", "run.completed"}, []string{events[0].Event, events[1].Event, events[2].Event})
		for _, e := range events {
			require.Equal(t, id, e.RunID.UUID)
		}
		require.Equal(t, "all done", events[2].Payload.(map[string]any)["output"])
	})

	t.Run("events resumes after the given cursor", func(t *testing.T) {
		tenant := v.tenant()
		id := v.seedRun(tenant, hermesruns.Completed, nil, "run.started", "tool.started", "run.completed")
		rec := v.do(tenant, http.MethodGet, "/v1/hermes/runs/"+id.String()+"/events?after=2", "")
		require.Equal(t, http.StatusOK, rec.Code)
		_, events := sseEvents(t, rec.Body.String())
		require.Len(t, events, 1)
		require.Equal(t, 3, events[0].Seq)
		rec = v.do(tenant, http.MethodGet, "/v1/hermes/runs/"+id.String()+"/events?after=3", "")
		require.Equal(t, http.StatusOK, rec.Code)
		_, events = sseEvents(t, rec.Body.String())
		require.Empty(t, events)
	})

	t.Run("events for an unknown run 404s before streaming", func(t *testing.T) {
		rec := v.do(v.tenant(), http.MethodGet, "/v1/hermes/runs/"+uuid.NewString()+"/events", "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.NotEqual(t, "text/event-stream", rec.Header().Get("Content-Type"))
	})

	t.Run("start rejects an empty prompt without touching hermes", func(t *testing.T) {
		rec := v.do(v.tenant(), http.MethodPost, "/v1/hermes/runs", `{"prompt":"   "}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "prompt_required")
	})

	t.Run("approval on a run with nothing pending is a conflict", func(t *testing.T) {
		tenant := v.tenant()
		id := v.seedRun(tenant, hermesruns.Running, nil)
		require.Equal(t, http.StatusConflict, v.do(tenant, http.MethodPost, "/v1/hermes/runs/"+id.String()+"/approval", `{"choice":"once"}`).Code)
		require.Equal(t, http.StatusNotFound, v.do(tenant, http.MethodPost, "/v1/hermes/runs/"+uuid.NewString()+"/approval", `{"choice":"deny"}`).Code)
		// The seeded run is not watched; settle it so later re-attach-free cases stay quiet.
		_, err := v.service.Store().Finish(context.Background(), id, hermesruns.Completed, nil)
		require.NoError(t, err)
	})

	t.Run("stopping an already terminal run returns it unchanged", func(t *testing.T) {
		tenant := v.tenant()
		id := v.seedRun(tenant, hermesruns.Completed, nil)
		rec := v.do(tenant, http.MethodPost, "/v1/hermes/runs/"+id.String()+"/stop", "")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, hermesruns.Completed, decodeRun(t, rec).Status)
		require.Equal(t, http.StatusNotFound, v.do(tenant, http.MethodPost, "/v1/hermes/runs/"+uuid.NewString()+"/stop", "").Code)
	})

	t.Run("a pending approval is serialised to the client", func(t *testing.T) {
		tenant := v.tenant()
		cmd := "rm -rf ***"
		id := v.seedRun(tenant, hermesruns.WaitingForApproval, &hermesruns.PendingApproval{
			Command: &cmd, Choices: hermesruns.AllChoices(), RequestedAt: time.Unix(1_700_000_000, 0),
			Extra: map[string]any{"tool": "shell"},
		})
		rec := v.do(tenant, http.MethodGet, "/v1/hermes/runs/"+id.String(), "")
		require.Equal(t, http.StatusOK, rec.Code)
		run := decodeRun(t, rec)
		require.Equal(t, hermesruns.WaitingForApproval, run.Status)
		require.NotNil(t, run.PendingApproval)
		require.Equal(t, cmd, *run.PendingApproval.Command)
		require.Equal(t, hermesruns.AllChoices(), run.PendingApproval.Choices)
		require.Equal(t, "shell", run.PendingApproval.Extra["tool"])
	})
}

func TestErrorMapping(t *testing.T) {
	status := func(err error) int { return mapErr(err).(interface{ StatusCode() int }).StatusCode() }
	require.Equal(t, http.StatusNotFound, status(hermesruns.ErrRunNotFound))
	require.Equal(t, http.StatusNotFound, status(hermesruns.ErrConversationNotFound))
	require.Equal(t, http.StatusTooManyRequests, status(hermesruns.ErrTooManyActiveRuns))
	require.Equal(t, http.StatusConflict, status(hermesruns.ErrApprovalNotPending))
	require.Equal(t, http.StatusConflict, status(hermesruns.ErrRunNotActive))
	require.Equal(t, http.StatusBadRequest, status(hermesruns.ErrEmptyPrompt))
	require.Equal(t, http.StatusNotImplemented, status(&hermesruns.ClientError{Kind: hermesruns.ErrUnsupported}))
	require.Equal(t, http.StatusGone, status(&hermesruns.ClientError{Kind: hermesruns.ErrRunExpired}))
	require.Equal(t, http.StatusConflict, status(&hermesruns.ClientError{Kind: hermesruns.ErrApprovalNotPendingUpstream}))
	require.Equal(t, http.StatusBadGateway, status(&hermesruns.ClientError{Kind: hermesruns.ErrUpstream, Status: 500}))
	require.Equal(t, http.StatusBadGateway, status(&hermesruns.ClientError{Kind: hermesruns.ErrStreamIdle}))
	require.Equal(t, "hermes_runs_unsupported", (&hermesruns.ClientError{Kind: hermesruns.ErrUnsupported}).StableCode())
	require.Equal(t, "hermes_run_expired", (&hermesruns.ClientError{Kind: hermesruns.ErrRunExpired}).StableCode())
	require.Equal(t, "hermes_runs_limit", hermesruns.ErrTooManyActiveRuns.StableCode())
}
