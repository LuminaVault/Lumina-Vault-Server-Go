package system_test

import (
	"bufio"
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/system"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Ported from HermesUpdateServiceTests.swift. Its happy-path and
// unhealthy-version tests drive docker to success; production has no docker
// (decision O1), so they are proposed as waived and the no-docker failure is
// tested instead.

var (
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	alice = uuid.MustParse("a11ce000-0000-4000-8000-000000000001")
)

const token = "admin-secret"

// gate blocks docker until released, so a job stays running.
type gate struct{ release chan struct{} }

func (g gate) Run(ctx context.Context, args []string) (string, int, error) {
	if args[0] == "inspect" {
		return "", 1, nil
	}
	<-g.release
	return "", -1, system.ErrNoDocker
}

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
	wg   *sync.WaitGroup
}

func setup(t *testing.T, docker system.Docker, adminToken string) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)
	wg := &sync.WaitGroup{}
	identify := auth.Identify(keys, auth.PGUsers(pool))
	m := system.New(pool, system.Options{
		Identify: identify, AdminToken: adminToken, PerTenant: true, Docker: docker, BackgroundWait: wg,
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{m},
	})
	t.Cleanup(wg.Wait)
	return env{t: t, e: e, pool: pool, wg: wg}
}

func (v env) do(method, path, body string, admin string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if admin != "" {
		req.Header.Set("X-Admin-Token", admin)
	}
	tok, err := scenario.Mint(keys, alice)
	require.NoError(v.t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func (v env) errIs(r *httptest.ResponseRecorder, status int, msg string) {
	v.t.Helper()
	require.Equal(v.t, status, r.Code, r.Body.String())
	b, _ := json.Marshal(msg)
	require.JSONEq(v.t, `{"error":{"message":`+string(b)+`}}`, r.Body.String())
}

func obj(t *testing.T, r *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &m), r.Body.String())
	return m
}

func TestAdminTokenGate(t *testing.T) {
	v := setup(t, nil, "")
	v.errIs(v.do(http.MethodGet, "/v1/system/hermes/version", "", "x"), 404, "admin disabled")
	v = setup(t, nil, token)
	v.errIs(v.do(http.MethodGet, "/v1/system/hermes/version", "", ""), 401, "admin token invalid")
	v.errIs(v.do(http.MethodGet, "/v1/system/hermes/version", "", "wrong"), 401, "admin token invalid")
}

func TestVersionWithoutDocker(t *testing.T) {
	v := setup(t, nil, token)
	r := v.do(http.MethodGet, "/v1/system/hermes/version", "", token)
	require.Equal(t, 200, r.Code, r.Body.String())
	require.JSONEq(t, `{"info":{"currentRef":"ghcr.io/luminavault/luminavault-hermes:latest","currentLabel":"latest","updateAvailable":false}}`, r.Body.String())
}

func TestNoUpdateJobAndBadIDs(t *testing.T) {
	v := setup(t, nil, token)
	v.errIs(v.do(http.MethodGet, "/v1/system/hermes/update/current", "", token), 404, "no_update_job")
	v.errIs(v.do(http.MethodGet, "/v1/system/hermes/update/nope", "", token), 400, "invalid_job_id")
	v.errIs(v.do(http.MethodGet, "/v1/system/hermes/update/"+uuid.NewString(), "", token), 404, "job_not_found")
	v.errIs(v.do(http.MethodGet, "/v1/system/hermes/update/"+uuid.NewString()+"/stream", "", token), 404, "job_not_found")
	v.errIs(v.do(http.MethodPost, "/v1/system/hermes/update/"+uuid.NewString()+"/rollback", "", token), 400,
		"rollback_unavailable:no recorded previous version")
}

func TestUpdateFailsAtPreflightWithoutDocker(t *testing.T) {
	v := setup(t, nil, token)
	r := v.do(http.MethodPost, "/v1/system/hermes/update", `{"targetTag":" v2 "}`, token)
	require.Equal(t, 200, r.Code, r.Body.String())
	started := obj(t, r)
	require.Equal(t, "running", started["state"])
	id, _ := started["jobID"].(string)
	v.wg.Wait()

	r = v.do(http.MethodGet, "/v1/system/hermes/update/"+id, "", token)
	require.Equal(t, 200, r.Code, r.Body.String())
	status, _ := obj(t, r)["status"].(map[string]any)
	require.Equal(t, "failed", status["state"])
	require.Equal(t, "ghcr.io/luminavault/luminavault-hermes:v2", status["toVersion"])
	require.NotContains(t, status, "fromVersion")
	require.Contains(t, status["errorMessage"], "The update failed: ")
	steps, _ := status["steps"].([]any)
	require.Len(t, steps, 9)
	first, _ := steps[0].(map[string]any)
	require.Equal(t, "preflight", first["id"])
	require.Equal(t, "failed", first["state"])
	require.Contains(t, first, "startedAt")
	require.Contains(t, first, "finishedAt")
	for _, s := range steps[1:] {
		require.Equal(t, "pending", s.(map[string]any)["state"])
	}

	// current falls back to the newest job; the stream of a finished job is
	// its steps then done.
	r = v.do(http.MethodGet, "/v1/system/hermes/update/current", "", token)
	require.Equal(t, 200, r.Code)
	r = v.do(http.MethodGet, "/v1/system/hermes/update/"+id+"/stream", "", token)
	require.Equal(t, 200, r.Code)
	require.Equal(t, "text/event-stream", r.Header().Get("Content-Type"))
	require.Equal(t, "no", r.Header().Get("X-Accel-Buffering"))
	var types []string
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct{ Type string }
		require.NoError(t, json.Unmarshal([]byte(line), &ev))
		types = append(types, ev.Type)
	}
	require.Len(t, types, 10)
	require.Equal(t, "done", types[9])

	// No fromVersion recorded, so no rollback.
	v.errIs(v.do(http.MethodPost, "/v1/system/hermes/update/"+id+"/rollback", "", token), 400,
		"rollback_unavailable:no recorded previous version")
}

// `second update while one is running is rejected`.
func TestSecondUpdateWhileRunningIsRejected(t *testing.T) {
	g := gate{release: make(chan struct{})}
	v := setup(t, g, token)
	r := v.do(http.MethodPost, "/v1/system/hermes/update", "", token)
	require.Equal(t, 200, r.Code, r.Body.String())
	v.errIs(v.do(http.MethodPost, "/v1/system/hermes/update", `{}`, token), 409, "update_in_progress")
	v.errIs(v.do(http.MethodPost, "/v1/system/hermes/update/"+uuid.NewString()+"/rollback", "", token), 409, "update_in_progress")
	r = v.do(http.MethodGet, "/v1/system/hermes/update/current", "", token)
	status, _ := obj(t, r)["status"].(map[string]any)
	require.Equal(t, "running", status["state"])
	close(g.release)
	v.wg.Wait()
}

func TestManualRollbackFailsWithoutDocker(t *testing.T) {
	v := setup(t, nil, token)
	failed := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO hermes_update_jobs
		(id, state, steps_json, from_version, to_version, created_at, updated_at)
		VALUES ($1, 'failed', '[]', 'img:old', 'img:new', now(), now())`, failed)
	require.NoError(t, err)
	r := v.do(http.MethodPost, "/v1/system/hermes/update/"+failed.String()+"/rollback", "", token)
	require.Equal(t, 200, r.Code, r.Body.String())
	id, _ := obj(t, r)["jobID"].(string)
	v.wg.Wait()
	r = v.do(http.MethodGet, "/v1/system/hermes/update/"+id, "", token)
	status, _ := obj(t, r)["status"].(map[string]any)
	require.Equal(t, "failed", status["state"])
	require.Equal(t, "img:new", status["fromVersion"])
	require.Equal(t, "img:old", status["toVersion"])
	require.Equal(t, "Restore did not succeed. Manual recovery may be required.", status["errorMessage"])
}

func TestReprovision(t *testing.T) {
	v := setup(t, nil, token)
	r := v.do(http.MethodPost, "/v1/system/hermes/reprovision", "", token)
	require.Equal(t, 500, r.Code, r.Body.String())
}
