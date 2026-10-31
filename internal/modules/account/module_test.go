package account_test

import (
	"context"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"lumina/internal/modules/account"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// The golden bcrypt hash (testdata/golden/bcrypt.json) and its password.
const (
	password = "correct horse battery staple"
	hash     = "$2b$12$asMTTkraZGgSRkkBR0CDpe.fsvorxUmu0Ue0zUG9.53O.GnHPewau"
)

var (
	alice   = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	acctDel = uuid.MustParse("ACC70000-0000-4000-8000-0000000000A1")
	owner   = uuid.MustParse("ACC70000-0000-4000-8000-0000000000A7")
	self    = uuid.MustParse("ACC70000-0000-4000-8000-0000000000C9")
	keys    = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t      *testing.T
	e      *echo.Echo
	pool   *pgxpool.Pool
	vault  string
	hermes string
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "account.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	vault, hermes := t.TempDir(), t.TempDir()
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{account.New(pool, keys, vaultpaths.Paths{Root: vault}, hermes)},
	})
	return env{t: t, e: e, pool: pool, vault: vault, hermes: hermes}
}

type resp struct {
	code int
	ct   string
	body string
}

func (v env) doToken(token, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/v1/account", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()}
}

func (v env) do(user uuid.UUID, body string) resp {
	v.t.Helper()
	tok, err := scenario.Mint(keys, user)
	require.NoError(v.t, err)
	return v.doToken(tok, body)
}

// mint signs a session token with an explicit iat (nil: no iat claim).
func mint(t *testing.T, user uuid.UUID, iat *time.Time) string {
	t.Helper()
	claims := auth.Claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject: ids.Upper(user), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)), ID: ids.Upper(uuid.New()),
	}}
	if iat != nil {
		claims.IssuedAt = jwt.NewNumericDate(*iat)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tok.Header["kid"] = "t"
	s, err := tok.SignedString(keys.Active().Secret)
	require.NoError(t, err)
	return s
}

func (v env) newUser() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())`, id, id.String()+"@test", hash, "u-"+id.String()[:8])
	require.NoError(v.t, err)
	return id
}

func (v env) count(query string, args ...any) int {
	v.t.Helper()
	var n int
	require.NoError(v.t, v.pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (v env) userExists(id uuid.UUID) bool {
	return v.count(`SELECT COUNT(*)::int FROM users WHERE id = $1`, id) == 1
}

const unauthBody = `{"error":{"message":"Authenticated identity is unavailable"}}`

// Swift AccountDeletionTests "delete with correct password returns 204".
func TestDeleteWithCorrectPassword(t *testing.T) {
	v := setup(t)
	u := v.newUser()
	r := v.doToken(mint(t, u, nil), `{"password":"`+password+`"}`)
	require.Equal(t, http.StatusNoContent, r.code)
	require.Empty(t, r.body)
	require.False(t, v.userExists(u))
}

// Swift "delete with wrong password returns 401". A fresh token does not
// rescue a wrong password: the password is checked first.
func TestDeleteWithWrongPassword(t *testing.T) {
	v := setup(t)
	u := v.newUser()
	r := v.do(u, `{"password":"definitely-wrong"}`)
	require.Equal(t, http.StatusUnauthorized, r.code)
	require.JSONEq(t, `{"error":{"message":"password incorrect"}}`, r.body)
	require.True(t, v.userExists(u))
}

// Swift "delete with fresh JWT and no password succeeds".
func TestDeleteWithFreshJWT(t *testing.T) {
	v := setup(t)
	for _, body := range []string{`{}`, ``, `{"password":""}`, `{"password":null}`, `{"password":`, `[1]`, `{"password":5}`, `null`, `{"PASSWORD":"wrong"}`} {
		u := v.newUser()
		r := v.do(u, body)
		require.Equal(t, http.StatusNoContent, r.code, body)
		require.False(t, v.userExists(u), body)
	}
}

// Swift "delete with stale JWT and no password returns 401", plus a future
// iat (now - iat < 0) and a token without iat.
func TestDeleteWithoutFreshJWTNeedsPassword(t *testing.T) {
	v := setup(t)
	u := v.newUser()
	stale, future := time.Now().Add(-10*time.Minute), time.Now().Add(10*time.Minute)
	for _, tok := range []string{mint(t, u, &stale), mint(t, u, &future), mint(t, u, nil)} {
		r := v.doToken(tok, `{}`)
		require.Equal(t, http.StatusUnauthorized, r.code)
		require.JSONEq(t, `{"error":{"message":"account deletion requires password or a JWT issued within 300 seconds"}}`, r.body)
	}
	require.True(t, v.userExists(u))
	require.Equal(t, http.StatusNoContent, v.doToken(mint(t, u, &stale), `{"password":"`+password+`"}`).code)
}

// Swift "unauthenticated returns 401".
func TestUnauthenticated(t *testing.T) {
	v := setup(t)
	for _, tok := range []string{"", "not.a.jwt"} {
		r := v.doToken(tok, "")
		require.Equal(t, http.StatusUnauthorized, r.code)
		require.JSONEq(t, unauthBody, r.body)
	}
	// A deleted user's still-valid token is unauthenticated, not a second delete.
	require.Equal(t, http.StatusNoContent, v.do(acctDel, `{"password":"`+password+`"}`).code)
	require.Equal(t, http.StatusUnauthorized, v.do(acctDel, `{"password":"`+password+`"}`).code)
}

// Swift "delete cascades across all child tables".
func TestDeleteCascadesAcrossChildTables(t *testing.T) {
	v := setup(t)
	require.Equal(t, http.StatusNoContent, v.do(acctDel, `{"password":"`+password+`"}`).code)
	for table, col := range map[string]string{
		"users": "id", "refresh_tokens": "tenant_id", "password_reset_tokens": "tenant_id",
		"mfa_challenges": "tenant_id", "oauth_identities": "tenant_id", "memories": "tenant_id",
		"hermes_profiles": "tenant_id", "device_tokens": "tenant_id", "webauthn_credentials": "tenant_id",
		"spaces": "tenant_id", "vault_files": "tenant_id", "health_events": "tenant_id",
		"email_verification_tokens": "tenant_id", "onboarding_state": "tenant_id",
		"projects": "tenant_id", "vaults": "personal_owner_user_id",
	} {
		require.Zero(t, v.count(fmt.Sprintf(`SELECT COUNT(*)::int FROM %s WHERE %s = $1`, table, col), acctDel), table)
	}
	// SET NULL left to the FK.
	require.Equal(t, 1, v.count(`SELECT COUNT(*)::int FROM provider_failover_events WHERE tenant_id IS NULL AND provider = 'anthropic'`))
}

// Swift "tenant isolation cannot delete another user".
func TestTenantIsolation(t *testing.T) {
	v := setup(t)
	mallory := v.newUser()
	require.Equal(t, http.StatusNoContent, v.do(mallory, `{"password":"`+password+`"}`).code)
	require.True(t, v.userExists(alice))
	require.Equal(t, 1, v.count(`SELECT COUNT(*)::int FROM projects WHERE tenant_id = $1`, alice))
}

// seedUnkeyedRows is Swift's helper: one row per erased table (plus a room
// message, both spend-bucket spellings and a billing event).
func (v env) seedUnkeyedRows(tenant uuid.UUID) {
	v.t.Helper()
	ctx := context.Background()
	room := uuid.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO agent_rooms (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 'room', NOW(), NOW())`, []any{room, tenant}},
		{`INSERT INTO agent_room_messages (id, room_id, author_kind, body, created_at) VALUES (gen_random_uuid(), $1, 'user', 'private words', NOW())`, []any{room}},
		{`INSERT INTO device_command_queue (id, tenant_id, kind, payload, created_at, expires_at) VALUES (gen_random_uuid(), $1, 'reminder_create', '{"title":"x"}', NOW(), NOW() + interval '1 day')`, []any{tenant}},
		{`INSERT INTO idempotency_keys (id, tenant_id, key, request_hash, response_status, response_body, created_at, expires_at) VALUES (gen_random_uuid(), $1, gen_random_uuid(), 'h', 200, '\x7b7d'::bytea, NOW(), NOW() + interval '1 day')`, []any{tenant}},
		{`INSERT INTO embedding_usage (id, tenant_id, year_month, tokens_used, created_at, updated_at) VALUES (gen_random_uuid(), $1, '1999-01', 10, NOW(), NOW())`, []any{tenant}},
		{`INSERT INTO retrieval_telemetry_events (id, tenant_id, source_path, hit_count, zero_hit, limit_requested, created_at) VALUES (gen_random_uuid(), $1, 'chat', 1, false, 5, NOW())`, []any{tenant}},
		{`INSERT INTO retrieval_leak_reports (id, tenant_id, period_start, period_end, total_retrievals, zero_hit_count, zero_hit_rate, created_at) VALUES (gen_random_uuid(), $1, NOW() - interval '7 days', NOW(), 10, 1, 0.1, NOW())`, []any{tenant}},
		{`INSERT INTO workflow_spend_buckets (scope_key, period_kind, period_start) VALUES ($1, 'day', CURRENT_DATE)`, []any{"tenant:" + ids.Upper(tenant)}},
		{`INSERT INTO workflow_spend_buckets (scope_key, period_kind, period_start) VALUES ($1, 'day', CURRENT_DATE)`, []any{"freelane:tenant:" + ids.Upper(tenant)}},
		{`INSERT INTO billing_event_logs (id, event_id, event_type, user_id, processed_at) VALUES (gen_random_uuid(), $1, 'INITIAL_PURCHASE', $2, NOW())`, []any{"evt-" + tenant.String(), tenant}},
	} {
		_, err := v.pool.Exec(ctx, q.sql, q.args...)
		require.NoError(v.t, err, q.sql)
	}
}

// Swift "delete erases tenant rows that have no cascade and spares other tenants".
func TestDeleteErasesUnkeyedRowsAndSparesOthers(t *testing.T) {
	v := setup(t)
	a, b := v.newUser(), v.newUser()
	v.seedUnkeyedRows(a)
	v.seedUnkeyedRows(b)
	// a acted in b's (here: alice's) vault: the row stays but loses a's name.
	_, err := v.pool.Exec(context.Background(), `INSERT INTO vault_activity_events (id, vault_id, actor_user_id, actor_name, action, target_type, created_at)
		VALUES (gen_random_uuid(), $1, $2, 'a-display-name', 'note.created', 'note', NOW())`, alice, a)
	require.NoError(t, err)

	require.Equal(t, http.StatusNoContent, v.do(a, `{"password":"`+password+`"}`).code)

	for _, table := range account.ErasedTables {
		q := fmt.Sprintf(`SELECT COUNT(*)::int FROM %s WHERE tenant_id = $1`, table)
		require.Zero(t, v.count(q, a), "%s kept the deleted tenant's rows", table)
		require.GreaterOrEqual(t, v.count(q, b), 1, "%s lost another tenant's rows", table)
	}
	require.Equal(t, 1, v.count(`SELECT COUNT(*)::int FROM agent_room_messages m JOIN agent_rooms r ON r.id = m.room_id WHERE r.tenant_id = $1`, b))
	require.Zero(t, v.count(`SELECT COUNT(*)::int FROM agent_room_messages m WHERE NOT EXISTS (SELECT 1 FROM agent_rooms r WHERE r.id = m.room_id)`))
	bucket := `SELECT COUNT(*)::int FROM workflow_spend_buckets WHERE lower(scope_key) LIKE '%' || lower($1::text)`
	require.Zero(t, v.count(bucket, a.String()))
	require.Equal(t, 2, v.count(bucket, b.String()))
	require.Zero(t, v.count(`SELECT COUNT(*)::int FROM billing_event_logs WHERE user_id = $1`, a))
	require.Equal(t, 1, v.count(`SELECT COUNT(*)::int FROM billing_event_logs WHERE event_id = $1 AND user_id IS NULL`, "evt-"+a.String()))
	require.Equal(t, 1, v.count(`SELECT COUNT(*)::int FROM billing_event_logs WHERE user_id = $1`, b))
	require.Zero(t, v.count(`SELECT COUNT(*)::int FROM vault_activity_events WHERE actor_name = 'a-display-name'`))
	require.Equal(t, 1, v.count(`SELECT COUNT(*)::int FROM vault_activity_events WHERE vault_id = $1 AND actor_user_id IS NULL AND actor_name = 'Deleted user'`, alice))
}

// Swift "every tenant table is erased or cascades".
func TestEveryTenantTableIsErasedOrCascades(t *testing.T) {
	v := setup(t)
	rows, err := v.pool.Query(context.Background(), `
		WITH RECURSIVE fk AS (
			SELECT c.conrelid::regclass::text AS child, c.confrelid::regclass::text AS parent, c.confdeltype::text AS del
			FROM pg_constraint c WHERE c.contype = 'f'
		),
		reach(tbl) AS (
			SELECT 'users'::text
			UNION
			SELECT fk.child FROM fk JOIN reach r ON fk.parent = r.tbl WHERE fk.del = 'c'
		)
		SELECT DISTINCT c.table_name::text
		FROM information_schema.columns c
		WHERE c.table_schema = 'public'
		  AND c.column_name IN ('tenant_id', 'user_id')
		  AND c.table_name::text NOT IN (SELECT tbl FROM reach)
		ORDER BY 1`)
	require.NoError(t, err)
	covered := map[string]bool{}
	for _, list := range [][]string{account.ErasedTables, account.AnonymizedTables, account.HandledElsewhere} {
		for _, tbl := range list {
			covered[tbl] = true
		}
	}
	var uncovered []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		if !covered[name] {
			uncovered = append(uncovered, name)
		}
	}
	require.NoError(t, rows.Err())
	require.Empty(t, uncovered, "tenant tables with no cascade from users and no entry in the eraser")
}

// Swift "delete soft-renames the Hermes profile directory under profiles",
// and the vault folder beside it.
func TestDeleteSoftRenamesFolders(t *testing.T) {
	v := setup(t)
	live := filepath.Join(v.hermes, "profiles", "acct-del")
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "SOUL.md"), []byte("soul"), 0o600))
	tenantRoot := vaultpaths.Paths{Root: v.vault}.TenantRoot(acctDel)
	require.NoError(t, os.MkdirAll(filepath.Join(tenantRoot, "raw"), 0o755))

	before := time.Now().Unix()
	require.Equal(t, http.StatusNoContent, v.do(acctDel, `{"password":"`+password+`"}`).code)
	after := time.Now().Unix()

	for _, dir := range []string{live, tenantRoot} {
		_, err := os.Stat(dir)
		require.True(t, os.IsNotExist(err), "%s survived deletion", dir)
		names, err := os.ReadDir(filepath.Dir(dir))
		require.NoError(t, err)
		require.Len(t, names, 1)
		var ts int64
		_, err = fmt.Sscanf(names[0].Name(), "_deleted_%d_"+ids.Upper(acctDel), &ts)
		require.NoError(t, err, names[0].Name())
		require.Equal(t, account.DirectoryName(acctDel, time.Unix(ts, 0)), names[0].Name())
		require.True(t, ts >= before && ts <= after)
	}
	_, err := os.Stat(filepath.Join(v.hermes, "profiles", account.DirectoryName(acctDel, time.Unix(before, 0)), "SOUL.md"))
	if err != nil { // the second may have ticked
		_, err = os.Stat(filepath.Join(v.hermes, "profiles", account.DirectoryName(acctDel, time.Unix(after, 0)), "SOUL.md"))
	}
	require.NoError(t, err)
}

// A missing folder is skipped, and a rename that cannot happen is logged,
// not returned: the rows are gone either way.
func TestRenameFailureDoesNotFailTheRequest(t *testing.T) {
	v := setup(t)
	tenantRoot := vaultpaths.Paths{Root: v.vault}.TenantRoot(acctDel)
	require.NoError(t, os.MkdirAll(tenantRoot, 0o755))
	now := time.Now()
	// Occupy both possible targets (this second and the next).
	for _, ts := range []time.Time{now, now.Add(time.Second)} {
		require.NoError(t, os.MkdirAll(filepath.Join(v.vault, "tenants", account.DirectoryName(acctDel, ts)), 0o755))
	}
	require.Equal(t, http.StatusNoContent, v.do(acctDel, `{"password":"`+password+`"}`).code)
	_, err := os.Stat(tenantRoot)
	require.NoError(t, err, "the folder stays under its live name")
	require.False(t, v.userExists(acctDel))
}

// teams.owner_user_id is ON DELETE RESTRICT: the delete fails with a bare 500
// and the eraser's work rolls back with it.
func TestRestrictRollsBackTheEraser(t *testing.T) {
	v := setup(t)
	r := v.do(owner, `{"password":"`+password+`"}`)
	require.Equal(t, http.StatusInternalServerError, r.code)
	require.Empty(t, r.body)
	require.True(t, v.userExists(owner))
	require.Equal(t, 1, v.count(`SELECT COUNT(*)::int FROM device_command_queue WHERE tenant_id = $1`, owner))
}

// Swift bug, reproduced: activity in one's own personal vault blocks deletion.
// The users delete fires actor_user_id SET NULL and the users -> vaults ->
// vault_activity_events CASCADE on the same row; the SET NULL update runs
// after the vault is gone and fails its vault_id check.
func TestOwnVaultActivityBlocksDeletion(t *testing.T) {
	v := setup(t)
	r := v.do(self, `{"password":"`+password+`"}`)
	require.Equal(t, http.StatusInternalServerError, r.code)
	require.Empty(t, r.body)
	require.True(t, v.userExists(self))
}

func TestProfileNamesAndDirectoryName(t *testing.T) {
	for _, ok := range []string{"acct-del", "a.b_c-D9", strings.Repeat("a", 128)} {
		require.True(t, account.IsSafeProfileName(ok), ok)
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../x", "é", "a b", strings.Repeat("a", 129)} {
		require.False(t, account.IsSafeProfileName(bad), bad)
	}
	require.Equal(t, "_deleted_1767225600_ACC70000-0000-4000-8000-0000000000A1",
		account.DirectoryName(acctDel, time.Date(2026, 1, 1, 0, 0, 0, 900, time.UTC)))
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	body := `{"password":"` + password + `"}`
	r := v.do(acctDel, body)
	req := httptest.NewRequest(http.MethodDelete, "/v1/account", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	route, params, err := router.FindRoute(req)
	require.NoError(t, err)
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	out := &openapi3filter.ResponseValidationInput{RequestValidationInput: in, Status: r.code, Header: http.Header{"Content-Type": {r.ct}}}
	out.SetBodyBytes([]byte(r.body))
	require.NoError(t, openapi3filter.ValidateResponse(ctx, out))
}
