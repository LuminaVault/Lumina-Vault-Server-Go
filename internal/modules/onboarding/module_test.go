package onboarding_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/onboarding"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	ann  = uuid.MustParse("0B0A0000-0000-4000-8000-0000000000A1")
	bob  = uuid.MustParse("0B0A0000-0000-4000-8000-0000000000B2")
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "onboarding.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{onboarding.New(pool)},
	})
	return env{t: t, e: e, pool: pool}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) do(user uuid.UUID, method, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, "/v1/onboarding", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

// dto decodes the wire shape; a missing date stays nil.
type dto struct {
	SignupCompleted            *bool
	SignupCompletedAt          *string
	EmailVerifiedCompleted     *bool
	EmailVerifiedCompletedAt   *string
	SoulConfiguredCompleted    *bool
	SoulConfiguredCompletedAt  *string
	FirstCaptureCompleted      *bool
	FirstCaptureCompletedAt    *string
	FirstKBCompileCompleted    *bool
	FirstKBCompileCompletedAt  *string
	FirstQueryCompleted        *bool
	FirstQueryCompletedAt      *string
	BrainConfiguredCompleted   *bool
	BrainConfiguredCompletedAt *string
	GuidedStartDismissedAt     *string
}

func (v env) state(user uuid.UUID, method, body string, want int) dto {
	v.t.Helper()
	r := v.do(user, method, body)
	require.Equal(v.t, want, r.code, string(r.body))
	var d dto
	if want == http.StatusOK {
		require.Equal(v.t, "application/json; charset=utf-8", r.ct)
		require.NoError(v.t, json.Unmarshal(r.body, &d))
	}
	return d
}

func (v env) get(user uuid.UUID) dto { return v.state(user, http.MethodGet, "", http.StatusOK) }

func (v env) patch(user uuid.UUID, body string) dto {
	return v.state(user, http.MethodPatch, body, http.StatusOK)
}

func isTrue(b *bool) bool  { return b != nil && *b }
func isFalse(b *bool) bool { return b != nil && !*b }

// Swift OnboardingTests "get returns fresh state for new user", and
// GuidedStartTests "guidedStartDismissedAt round-trips through GET and starts nil".
func TestGetReturnsFreshState(t *testing.T) {
	v := setup(t)
	s := v.get(ann)
	require.True(t, isTrue(s.SignupCompleted))
	require.NotNil(t, s.SignupCompletedAt)
	for _, f := range []*bool{s.EmailVerifiedCompleted, s.SoulConfiguredCompleted, s.FirstCaptureCompleted, s.FirstKBCompileCompleted, s.FirstQueryCompleted, s.BrainConfiguredCompleted} {
		require.True(t, isFalse(f), "every other flag is present and false")
	}
	require.Nil(t, s.GuidedStartDismissedAt)
	require.Nil(t, s.BrainConfiguredCompletedAt)
	require.Nil(t, s.EmailVerifiedCompletedAt)

	v.patch(ann, `{"guidedStartDismissed":true}`)
	require.NotNil(t, v.get(ann).GuidedStartDismissedAt)
}

// Swift "patch sets flag and timestamp".
func TestPatchSetsFlagAndTimestamp(t *testing.T) {
	v := setup(t)
	s := v.patch(ann, `{"emailVerifiedCompleted":true,"soulConfiguredCompleted":true}`)
	require.True(t, isTrue(s.EmailVerifiedCompleted))
	require.NotNil(t, s.EmailVerifiedCompletedAt)
	require.True(t, isTrue(s.SoulConfiguredCompleted))
	require.NotNil(t, s.SoulConfiguredCompletedAt)
	require.True(t, isFalse(s.FirstCaptureCompleted))
}

// Swift "patch is idempotent does not overwrite timestamp".
func TestPatchIsIdempotent(t *testing.T) {
	v := setup(t)
	first := v.patch(ann, `{"firstCaptureCompleted":true}`)
	require.True(t, isTrue(first.FirstCaptureCompleted))
	require.NotNil(t, first.FirstCaptureCompletedAt)
	time.Sleep(1100 * time.Millisecond)
	second := v.patch(ann, `{"firstCaptureCompleted":true}`)
	require.Equal(t, *first.FirstCaptureCompletedAt, *second.FirstCaptureCompletedAt)
}

// Swift "patch rejects false values", "patch rejects brainConfiguredCompleted
// false" and GuidedStartTests "the seven latches still reject false".
func TestPatchRejectsFalse(t *testing.T) {
	v := setup(t)
	for _, f := range []string{"signupCompleted", "emailVerifiedCompleted", "soulConfiguredCompleted", "firstCaptureCompleted", "firstKBCompileCompleted", "firstQueryCompleted", "brainConfiguredCompleted"} {
		r := v.do(ann, http.MethodPatch, `{"`+f+`":false}`)
		require.Equal(t, http.StatusBadRequest, r.code)
		require.JSONEq(t, `{"error":{"message":"`+f+`: onboarding flags are one-way; only true is accepted"}}`, string(r.body))
	}
	// Checked in declaration order, not body order; nothing is created first.
	r := v.do(bob, http.MethodPatch, `{"brainConfiguredCompleted":false,"emailVerifiedCompleted":true,"signupCompleted":false}`)
	require.JSONEq(t, `{"error":{"message":"signupCompleted: onboarding flags are one-way; only true is accepted"}}`, string(r.body))
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT COUNT(*)::int FROM onboarding_state`).Scan(&n))
	require.Zero(t, n)
	// The exception is genuinely an exception.
	v.patch(ann, `{"guidedStartDismissed":false}`)
}

// Swift "unauthenticated returns 401".
func TestUnauthenticated(t *testing.T) {
	v := setup(t)
	for _, m := range []string{http.MethodGet, http.MethodPatch} {
		r := v.do(uuid.Nil, m, `{"signupCompleted":true}`)
		require.Equal(t, http.StatusUnauthorized, r.code)
		require.JSONEq(t, `{"error":{"message":"Authenticated identity is unavailable"}}`, string(r.body))
	}
}

// Swift "brain configured starts false on new user" and "patch latches brainConfiguredCompleted".
func TestBrainConfigured(t *testing.T) {
	v := setup(t)
	s := v.get(ann)
	require.True(t, isFalse(s.BrainConfiguredCompleted))
	require.Nil(t, s.BrainConfiguredCompletedAt)
	s = v.patch(ann, `{"brainConfiguredCompleted":true}`)
	require.True(t, isTrue(s.BrainConfiguredCompleted))
	require.NotNil(t, s.BrainConfiguredCompletedAt)
}

// Swift "get is stable across calls".
func TestGetIsStable(t *testing.T) {
	v := setup(t)
	one, two := v.get(ann), v.get(ann)
	require.Equal(t, *one.SignupCompletedAt, *two.SignupCompletedAt)
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT COUNT(*)::int FROM onboarding_state WHERE tenant_id = $1`, ann).Scan(&n))
	require.Equal(t, 1, n)
}

// GuidedStartTests "patch guidedStartDismissed true stamps the timestamp",
// "... false clears the timestamp" and "dismissing twice keeps the original timestamp".
func TestGuidedStartDismissal(t *testing.T) {
	v := setup(t)
	first := v.patch(ann, `{"guidedStartDismissed":true}`)
	require.NotNil(t, first.GuidedStartDismissedAt)
	time.Sleep(1100 * time.Millisecond)
	second := v.patch(ann, `{"guidedStartDismissed":true}`)
	require.Equal(t, *first.GuidedStartDismissedAt, *second.GuidedStartDismissedAt)

	cleared := v.patch(ann, `{"guidedStartDismissed":false}`)
	require.Nil(t, cleared.GuidedStartDismissedAt)
	require.Nil(t, v.get(ann).GuidedStartDismissedAt)
}

// firstKBCompileCompleted dual-writes the memory_compile source of truth and
// the legacy kb pair (HER-240); the DTO reads memory_compile.
func TestKBCompileDualWrites(t *testing.T) {
	v := setup(t)
	s := v.patch(ann, `{"firstKBCompileCompleted":true}`)
	require.True(t, isTrue(s.FirstKBCompileCompleted))
	var mem, kb bool
	var memAt, kbAt *time.Time
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT first_memory_compile_completed, first_kb_compile_completed,
		first_memory_compile_completed_at, first_kb_compile_completed_at FROM onboarding_state WHERE tenant_id = $1`, ann).Scan(&mem, &kb, &memAt, &kbAt))
	require.True(t, mem && kb)
	require.Equal(t, *memAt, *kbAt)

	// A row stranded with only the legacy pair latched still reads false and
	// is latched by PATCH.
	_, err := v.pool.Exec(context.Background(), `UPDATE onboarding_state SET first_memory_compile_completed = false, first_memory_compile_completed_at = NULL WHERE tenant_id = $1`, ann)
	require.NoError(t, err)
	require.True(t, isFalse(v.get(ann).FirstKBCompileCompleted))
	require.True(t, isTrue(v.patch(ann, `{"firstKBCompileCompleted":true}`).FirstKBCompileCompleted))
}

// PATCH writes only what it assigned, as Fluent does: a latch flipped by
// another writer between our read and write survives.
func TestPatchWritesOnlyAssignedColumns(t *testing.T) {
	v := setup(t)
	v.get(ann)
	_, err := v.pool.Exec(context.Background(), `UPDATE onboarding_state SET first_query_completed = true, first_query_completed_at = now() WHERE tenant_id = $1`, ann)
	require.NoError(t, err)
	v.patch(ann, `{"emailVerifiedCompleted":true}`)
	s := v.get(ann)
	require.True(t, isTrue(s.FirstQueryCompleted))
	require.True(t, isTrue(s.EmailVerifiedCompleted))
	// An empty PATCH writes nothing, not even updated_at.
	var before, after time.Time
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT updated_at FROM onboarding_state WHERE tenant_id = $1`, ann).Scan(&before))
	v.patch(ann, `{}`)
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT updated_at FROM onboarding_state WHERE tenant_id = $1`, ann).Scan(&after))
	require.Equal(t, before, after)
}

func TestBodyErrors(t *testing.T) {
	v := setup(t)
	for body, msg := range map[string]string{
		``:                             "The given data was not valid input: The given data was not valid JSON.",
		`{"signupCompleted":`:          "The given data was not valid input: The given data was not valid JSON.",
		`[]`:                           "Type mismatch for `` key, expected `Dictionary<String, Any>` type.",
		`{"signupCompleted":"yes"}`:    "Type mismatch for `signupCompleted` key, expected `Bool` type.",
		`{"guidedStartDismissed":"x"}`: "Type mismatch for `guidedStartDismissed` key, expected `Bool` type.",
	} {
		r := v.do(ann, http.MethodPatch, body)
		require.Equal(t, http.StatusBadRequest, r.code, body)
		b, _ := json.Marshal(map[string]map[string]string{"error": {"message": msg}})
		require.JSONEq(t, string(b), string(r.body), body)
	}
	// null is absent; unknown keys are ignored.
	v.patch(ann, `{"signupCompleted":null,"bogus":1}`)
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	for _, c := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPatch, `{"emailVerifiedCompleted":true,"guidedStartDismissed":true}`},
		{http.MethodPatch, `{"guidedStartDismissed":false}`},
	} {
		r := v.do(ann, c.method, c.body)
		req := httptest.NewRequest(c.method, "/v1/onboarding", strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: r.code, Header: http.Header{"Content-Type": {r.ct}}}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s: %s", c.method, r.body)
	}
}
