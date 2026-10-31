package teams_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/spaces"
	"lumina/internal/modules/teams"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
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
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Fixture ids from parity/seed/teams.sql.
const (
	alpha   = "7EA51000-0000-4000-8000-000000000001"
	beta    = "7EA51000-0000-4000-8000-000000000002"
	delta   = "7EA51000-0000-4000-8000-000000000003"
	epsilon = "7EA51000-0000-4000-8000-000000000004"
	ancient = "7EA51000-0000-4000-8000-000000000005"

	alphaMain    = "7EA53000-0000-4000-8000-000000000001"
	alphaLab     = "7EA53000-0000-4000-8000-000000000002"
	alphaArchive = "7EA53000-0000-4000-8000-000000000003"
	betaMain     = "7EA53000-0000-4000-8000-000000000004"
	deltaMain    = "7EA53000-0000-4000-8000-000000000005"
)

var (
	olga = uuid.MustParse("7EA50000-0000-4000-8000-0000000000A1")
	adam = uuid.MustParse("7EA50000-0000-4000-8000-0000000000A2")
	mia  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000A3")
	out  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000A4")
	ivy  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000A5")
	una  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000A6")
	rex  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000A7")
	bea  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000A8")
	tom  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000AA")
	ada  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000AB")
	ned  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000AC")
	kim  = uuid.MustParse("7EA50000-0000-4000-8000-0000000000AD")
	keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

// fakeSender records what Swift would mail; err makes it fail.
type fakeSender struct {
	mu   sync.Mutex
	sent []sent
	err  error
}

type sent struct{ to, team, inviter, token string }

func (f *fakeSender) Send(_ context.Context, to, team, inviter, token string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sent{to, team, inviter, token})
	return nil
}

func (f *fakeSender) last() sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

type env struct {
	t      *testing.T
	e      *echo.Echo
	pool   *pgxpool.Pool
	paths  vaultpaths.Paths
	sender *fakeSender
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "teams.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	paths := vaultpaths.Paths{Root: t.TempDir()}
	sender := &fakeSender{}
	identify := auth.Identify(keys, auth.PGUsers(pool))
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{
			teams.New(pool, identify, ratelimit.NewStore(), paths, sender),
			spaces.New(pool, paths, identify),
		},
	})
	return env{t: t, e: e, pool: pool, paths: paths, sender: sender}
}

type resp struct {
	code int
	ct   string
	body []byte
}

func (r resp) json(t *testing.T) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(r.body, &v), string(r.body))
	return v
}

func (v env) do(user uuid.UUID, method, path, body string, hdr ...string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
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

func (v env) ok(user uuid.UUID, method, path, body string) map[string]any {
	v.t.Helper()
	r := v.do(user, method, path, body)
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	m, _ := r.json(v.t).(map[string]any)
	return m
}

func (v env) list(user uuid.UUID, path string) []map[string]any {
	v.t.Helper()
	r := v.do(user, http.MethodGet, path, "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out []map[string]any
	require.NoError(v.t, json.Unmarshal(r.body, &out))
	return out
}

func (v env) errIs(r resp, status int, msg string) {
	v.t.Helper()
	require.Equal(v.t, status, r.code, string(r.body))
	require.JSONEq(v.t, `{"error":{"message":`+mustJSON(msg)+`}}`, string(r.body))
	require.Equal(v.t, "application/json; charset=utf-8", r.ct)
}

func (v env) count(sql string, args ...any) int {
	v.t.Helper()
	var n int
	require.NoError(v.t, v.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func names(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i], _ = r["name"].(string)
	}
	return out
}

// Swift MultimodalIngestionAPIIntegrationTests / KnowledgeVaultScopeIntegrationTests
// createTeamVault: POST /v1/teams is 200 and decodes as TeamResponse; POST
// /v1/teams/{id}/vaults is 200 and decodes as VaultResponse.
func TestCreateTeamVaultHelper(t *testing.T) {
	v := setup(t)
	team := v.ok(ned, http.MethodPost, "/v1/teams", `{"name":"Ingestion Team 1a2b3c4d"}`)
	require.Equal(t, "owner", team["role"])
	require.NotContains(t, team, "archivedAt")
	id := team["id"].(string)
	require.Equal(t, strings.ToUpper(id), id)

	vault := v.ok(ned, http.MethodPost, "/v1/teams/"+id+"/vaults", `{"name":"Shared Capture"}`)
	require.Equal(t, id, vault["teamID"])
	require.Equal(t, "Shared Capture", vault["name"])
	require.Equal(t, false, vault["isPersonal"])
	require.Equal(t, "admin", vault["role"])
	require.Equal(t, true, vault["canUseAI"])
	vaultID := uuid.MustParse(vault["id"].(string))
	for _, d := range []string{v.paths.Raw(vaultID), v.paths.Wiki(vaultID), v.paths.Memories(vaultID)} {
		st, err := os.Stat(d)
		require.NoError(t, err)
		require.True(t, st.IsDir())
	}
	acts := v.list(ned, "/v1/vaults/"+vault["id"].(string)+"/activity")
	require.Len(t, acts, 1)
	require.Equal(t, "vault.created", acts[0]["action"])
	require.Equal(t, "team_ned", acts[0]["actorName"])
	require.Equal(t, "Shared Capture", acts[0]["targetTitle"])
}

func TestCheckOrderBeforeIdentity(t *testing.T) {
	v := setup(t)
	v.errIs(v.do(uuid.Nil, http.MethodGet, "/v1/teams/nope/vaults", ""), 400, "invalid teamID")
	v.errIs(v.do(uuid.Nil, http.MethodGet, "/v1/teams/"+alpha+"/vaults", ""), 401, "Authenticated identity is unavailable")
	v.errIs(v.do(uuid.Nil, http.MethodGet, "/v1/vaults/nope/members", ""), 400, "invalid vaultID")
	v.errIs(v.do(uuid.Nil, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/nope", "{}"), 401, "Authenticated identity is unavailable")
	v.errIs(v.do(uuid.Nil, http.MethodPost, "/v1/teams", "{}"), 401, "Authenticated identity is unavailable")
	v.errIs(v.do(uuid.Nil, http.MethodPost, "/v1/team-invitations/x/accept", ""), 401, "Authenticated identity is unavailable")
	// The admin check runs before the user id and the body.
	v.errIs(v.do(mia, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/nope", ""), 403, "vault admin permission required")
	v.errIs(v.do(mia, http.MethodPost, "/v1/teams/"+alpha+"/vaults", ""), 403, "team access denied")
	// Archived team: 404 before the body is read.
	v.errIs(v.do(tom, http.MethodPost, "/v1/teams/"+epsilon+"/vaults", "[]"), 404, "team not found")
}

func TestListTeamsPurgesExpiredArchivesFirst(t *testing.T) {
	v := setup(t)
	ancientVault := uuid.MustParse("7EA53000-0000-4000-8000-000000000007")
	require.NoError(t, v.paths.EnsureTenantDirectories(ancientVault))
	v.errIs(v.do(tom, http.MethodPost, "/v1/teams/"+ancient+"/restore", ""), 410, "team is not restorable")

	v.errIs(v.do(uuid.Nil, http.MethodGet, "/v1/teams", ""), 401, "Authenticated identity is unavailable")
	require.Zero(t, v.count(`SELECT count(*) FROM teams WHERE id = $1`, ancient))
	require.Zero(t, v.count(`SELECT count(*) FROM vaults WHERE id = $1`, ancientVault))
	_, err := os.Stat(v.paths.TenantRoot(ancientVault))
	require.True(t, errors.Is(err, os.ErrNotExist), "the vault folder is removed")
	require.Equal(t, 1, v.count(`SELECT count(*) FROM teams WHERE id = $1`, epsilon), "recently archived teams stay")

	got := v.list(tom, "/v1/teams")
	require.Equal(t, []string{"Team Delta", "Team Epsilon"}, names(got))
	require.NotContains(t, got[0], "archivedAt")
	require.Contains(t, got[1], "archivedAt")
	require.Empty(t, v.list(out, "/v1/teams"))
}

func TestCreateTeamValidation(t *testing.T) {
	v := setup(t)
	team := v.ok(ned, http.MethodPost, "/v1/teams", `{"name":"  Gamma Team \n"}`)
	require.Equal(t, "Gamma Team", team["name"])
	require.Equal(t, 1, v.count(`SELECT count(*) FROM team_memberships m JOIN teams t ON t.id = m.team_id
		WHERE t.name = 'Gamma Team' AND m.user_id = $1 AND m.role = 'owner' AND t.billing_sponsor_user_id = $1`, ned))

	v.ok(ned, http.MethodPost, "/v1/teams", `{"name":"👍🏽!"}`)
	v.ok(ned, http.MethodPost, "/v1/teams", `{"name":"`+strings.Repeat("y", 80)+`"}`)
	for _, name := range []string{"🇵🇹", " a ", "   ", strings.Repeat("x", 81)} {
		v.errIs(v.do(ned, http.MethodPost, "/v1/teams", `{"name":`+mustJSON(name)+`}`), 422, "team name must be 2-80 characters")
	}
	v.errIs(v.do(ned, http.MethodPost, "/v1/teams", `{"name":`), 400, "The given data was not valid input: The given data was not valid JSON.")
	v.errIs(v.do(ned, http.MethodPost, "/v1/teams", `{}`), 400, "Coding key `name` not found.")
	v.errIs(v.do(ned, http.MethodPost, "/v1/teams", `{"name":null}`), 400, "Value not found for `name` key.")
	v.errIs(v.do(ned, http.MethodPost, "/v1/teams", `{"name":42}`), 400, "Type mismatch for `name` key, expected `String` type.")
}

func TestListVaultsSortsPersonalThenCaseInsensitive(t *testing.T) {
	v := setup(t)
	got := v.list(olga, "/v1/vaults")
	require.Equal(t, []string{"team_olga's Vault", "Alpha Archive", "alpha lab", "Alpha Main"}, names(got))
	require.Equal(t, true, got[0]["isPersonal"])
	require.NotContains(t, got[0], "teamID")
	require.Equal(t, alpha, got[1]["teamID"])
	require.Contains(t, got[1], "archivedAt")

	require.Equal(t, []string{"team_mia's Vault", "Alpha Main"}, names(v.list(mia, "/v1/vaults")))
	require.Equal(t, []string{"Alpha Main"}, names(v.list(mia, "/v1/teams/"+alpha+"/vaults")))
	require.Equal(t, []string{"Alpha Main", "alpha lab", "Alpha Archive"},
		names(v.list(olga, "/v1/teams/"+strings.ToLower(alpha)+"/vaults")), "heap order, archived included")
	v.errIs(v.do(out, http.MethodGet, "/v1/teams/"+alpha+"/vaults", ""), 403, "team access denied")
	v.errIs(v.do(kim, http.MethodGet, "/v1/teams/"+alpha+"/vaults", ""), 403, "team access denied")
}

func TestMembers(t *testing.T) {
	v := setup(t)
	admins := v.list(adam, "/v1/vaults/"+alphaMain+"/members")
	require.Len(t, admins, 4)
	require.Equal(t, "olga@parity.test", admins[0]["email"])
	require.Equal(t, "7EA54100-0000-4000-8000-000000000001", admins[0]["id"])
	for _, m := range v.list(mia, "/v1/vaults/"+alphaMain+"/members") {
		require.Equal(t, "", m["email"], "viewers do not see emails")
	}
	require.Len(t, v.list(adam, "/v1/vaults/"+alphaMain+"/members"), 4)
	r := v.do(adam, http.MethodGet, "/v1/vaults/"+alphaMain+"/members", "", "X-Vault-ID", "nope")
	require.Equal(t, 200, r.code, "the path, not X-Vault-ID, names the vault")
	v.errIs(v.do(out, http.MethodGet, "/v1/vaults/"+alphaMain+"/members", ""), 403, "vault access denied")
	v.errIs(v.do(olga, http.MethodGet, "/v1/vaults/"+alphaArchive+"/members", ""), 404, "vault not found")

	upd := v.ok(adam, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/"+mia.String(), `{"role":"editor","canUseAI":true}`)
	require.Equal(t, "mia@parity.test", upd["email"])
	require.Equal(t, "editor", upd["role"])
	require.Equal(t, true, upd["canUseAI"])
	v.errIs(v.do(adam, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/nope", `{}`), 400, "invalid userID")
	v.errIs(v.do(adam, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/"+uuid.Nil.String(), `{"role":"owner","canUseAI":false}`), 422, "invalid vault role")
	v.errIs(v.do(adam, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/"+out.String(), `{"role":"viewer","canUseAI":false}`), 404, "vault member not found")
	v.errIs(v.do(adam, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/"+mia.String(), `{"role":"viewer"}`), 400, "Coding key `canUseAI` not found.")
	v.errIs(v.do(adam, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/"+mia.String(), `{"role":"viewer","canUseAI":null}`), 400, "Type mismatch for `canUseAI` key, expected `Bool` type.")

	v.errIs(v.do(adam, http.MethodDelete, "/v1/vaults/"+alphaMain+"/members/"+olga.String(), ""), 409, "transfer team ownership before removing the owner")
	v.errIs(v.do(out, http.MethodDelete, "/v1/vaults/"+out.String()+"/members/"+out.String(), ""), 409, "transfer team ownership before removing the owner")
	v.errIs(v.do(adam, http.MethodDelete, "/v1/vaults/"+alphaMain+"/members/"+out.String(), ""), 404, "vault member not found")
	r = v.do(adam, http.MethodDelete, "/v1/vaults/"+alphaMain+"/members/"+mia.String(), "")
	require.Equal(t, 204, r.code, string(r.body))
	v.errIs(v.do(mia, http.MethodGet, "/v1/vaults/"+alphaMain+"/members", ""), 403, "vault access denied")

	acts := v.list(kim, "/v1/vaults/"+alphaMain+"/activity")
	require.Equal(t, "membership.removed", acts[0]["action"])
	require.Equal(t, "team_mia", acts[0]["targetTitle"])
	require.Equal(t, "team_adam", acts[0]["actorName"])
	require.Equal(t, mia.String(), strings.ToLower(acts[0]["targetID"].(string)))
	require.Equal(t, "membership.updated", acts[1]["action"])
	require.Equal(t, "Former member", acts[2]["actorName"])
	require.NotContains(t, acts[2], "actorUserID")
	require.NotContains(t, acts[2], "targetID")
	require.Equal(t, "2026-01-02T00:00:30Z", acts[2]["createdAt"])
}

func TestTransferAndLeave(t *testing.T) {
	v := setup(t)
	v.errIs(v.do(bea, http.MethodPut, "/v1/teams/"+beta+"/owner", `{"userID":"`+bea.String()+`"}`), 403, "only the team owner can transfer ownership")
	v.errIs(v.do(rex, http.MethodPut, "/v1/teams/"+beta+"/owner", `{"userID":"nope"}`), 400, "The given data was not valid input: Attempted to decode UUID from invalid UUID string.")
	v.errIs(v.do(rex, http.MethodPut, "/v1/teams/"+beta+"/owner", `{"userID":7}`), 400, "Type mismatch for `userID` key, expected `String` type.")
	v.errIs(v.do(rex, http.MethodPut, "/v1/teams/"+beta+"/owner", `{"userID":"`+rex.String()+`"}`), 422, "new owner must be an existing team member")
	v.errIs(v.do(rex, http.MethodPut, "/v1/teams/"+beta+"/owner", `{"userID":"`+out.String()+`"}`), 422, "new owner must be an existing team member")

	got := v.ok(rex, http.MethodPut, "/v1/teams/"+beta+"/owner", `{"userID":"`+bea.String()+`"}`)
	require.Equal(t, map[string]any{"id": beta, "name": "Team Beta", "role": "admin"}, got)
	var owner, sponsor uuid.UUID
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT owner_user_id, billing_sponsor_user_id FROM teams WHERE id = $1`, beta).Scan(&owner, &sponsor))
	require.Equal(t, bea, owner)
	require.Equal(t, bea, sponsor)
	require.Equal(t, "owner", v.list(bea, "/v1/teams")[0]["role"])

	v.errIs(v.do(bea, http.MethodPost, "/v1/teams/"+beta+"/leave", ""), 409, "transfer ownership before leaving")
	r := v.do(rex, http.MethodPost, "/v1/teams/"+beta+"/leave", "")
	require.Equal(t, 204, r.code)
	require.Zero(t, v.count(`SELECT count(*) FROM vault_memberships WHERE vault_id = $1 AND user_id = $2`, betaMain, rex))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM vault_memberships WHERE user_id = $1`, rex), "personal seat stays")
	v.errIs(v.do(rex, http.MethodPost, "/v1/teams/"+beta+"/leave", ""), 403, "team access denied")
}

func TestArchiveAndRestore(t *testing.T) {
	v := setup(t)
	v.errIs(v.do(ada, http.MethodPost, "/v1/teams/"+delta+"/archive", ""), 403, "only the team owner can archive the team")
	v.errIs(v.do(tom, http.MethodPost, "/v1/teams/"+delta+"/restore", ""), 410, "team is not restorable")
	got := v.ok(tom, http.MethodPost, "/v1/teams/"+delta+"/archive", "")
	require.Equal(t, "owner", got["role"])
	require.Regexp(t, `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`, got["archivedAt"])
	require.Equal(t, 1, v.count(`SELECT count(*) FROM vaults WHERE id = $1 AND archived_at IS NOT NULL`, deltaMain))
	v.errIs(v.do(ada, http.MethodGet, "/v1/vaults/"+deltaMain+"/activity", ""), 404, "vault not found")
	v.errIs(v.do(tom, http.MethodPost, "/v1/teams/"+delta+"/invitations", `{}`), 404, "team not found")
	v.errIs(v.do(ada, http.MethodPost, "/v1/teams/"+delta+"/restore", ""), 410, "team is not restorable")

	got = v.ok(tom, http.MethodPost, "/v1/teams/"+delta+"/restore", "")
	require.NotContains(t, got, "archivedAt")
	require.Zero(t, v.count(`SELECT count(*) FROM vaults WHERE team_id = $1 AND archived_at IS NOT NULL`, delta))
	require.Empty(t, v.list(ada, "/v1/vaults/"+deltaMain+"/activity"))
	v.ok(tom, http.MethodPost, "/v1/teams/"+epsilon+"/restore", "")
}

func TestInviteResendRevokeAccept(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	inv := v.ok(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations",
		`{"email":"  New.Person@Parity.TEST ","vaultGrants":{"`+alphaMain+`":{"role":"editor","canUseAI":true}}}`)
	require.Equal(t, "new.person@parity.test", inv["email"])
	require.Equal(t, alpha, inv["teamID"])
	require.NotContains(t, inv, "token")
	expires, err := time.Parse(time.RFC3339, inv["expiresAt"].(string))
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(7*24*time.Hour), expires, time.Minute)

	s := v.sender.last()
	require.Equal(t, sent{"new.person@parity.test", "Team Alpha", "team_adam", s.token}, s)
	require.Regexp(t, `^[0-9A-F-]{72}$`, s.token)
	var hash, grants string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT token_hash, vault_grants FROM team_invitations WHERE email = 'new.person@parity.test'`).Scan(&hash, &grants))
	sum := sha256.Sum256([]byte(s.token))
	require.Equal(t, hex.EncodeToString(sum[:]), hash)
	require.Equal(t, `{"`+alphaMain+`":{"role":"editor","canUseAI":true}}`, grants, "Swift JSONEncoder bytes")

	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations", `{"email":"new.person@parity.test","vaultGrants":{"`+alphaMain+`":{"role":"viewer","canUseAI":false}}}`), 409, "a pending invitation already exists for this email")
	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations", `{"email":"nobody","vaultGrants":{"`+alphaMain+`":{"role":"viewer","canUseAI":false}}}`), 422, "email and at least one vault grant are required")
	for _, g := range []string{
		`{"` + betaMain + `":{"role":"viewer","canUseAI":false}}`,
		`{"` + adam.String() + `":{"role":"viewer","canUseAI":false}}`,
		`{"` + alphaMain + `":{"role":"owner","canUseAI":false}}`,
		`{"nope":{"role":"viewer","canUseAI":false}}`,
	} {
		v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations", `{"email":"g@parity.test","vaultGrants":`+g+`}`), 422, "invalid invitation vault grant")
	}
	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations", `{"email":"g@parity.test","vaultGrants":[]}`), 400, "Type mismatch for `vaultGrants` key, expected `Dictionary<String, Any>` type.")
	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations", `{"email":"g@parity.test","vaultGrants":{"`+alphaMain+`":{"role":"viewer"}}}`), 400, "Coding key `canUseAI` not found.")
	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations", `{"email":"g@parity.test","vaultGrants":{"`+alphaMain+`":{"role":3,"canUseAI":false}}}`), 400, "Type mismatch for `vaultGrants."+alphaMain+".role` key, expected `String` type.")

	// Pending, newest expiry first, the expired one included.
	list := v.list(adam, "/v1/teams/"+alpha+"/invitations")
	emails := make([]string, len(list))
	for i, r := range list {
		emails[i] = r["email"].(string)
	}
	require.Equal(t, []string{"new.person@parity.test", "revoke@parity.test", "una@parity.test", "ivy@parity.test", "resend@parity.test", "ivy@parity.test"}, emails)

	// Resend rotates the token; a failing mailer restores the old one.
	old := "parity-invite-8"
	v.sender.err = errors.New("smtp down")
	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations/7EA55000-0000-4000-8000-000000000008/resend", ""), 503, "invitation could not be resent")
	oldSum := sha256.Sum256([]byte(old))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM team_invitations WHERE token_hash = $1 AND expires_at < now() + interval '2 days'`, hex.EncodeToString(oldSum[:])))
	v.sender.err = nil
	v.ok(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations/7EA55000-0000-4000-8000-000000000008/resend", "")
	require.Zero(t, v.count(`SELECT count(*) FROM team_invitations WHERE token_hash = $1`, hex.EncodeToString(oldSum[:])))
	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations/7EA55000-0000-4000-8000-000000000006/resend", ""), 404, "pending invitation not found")
	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations/7EA55000-0000-4000-8000-00000000000A/resend", ""), 404, "pending invitation not found")
	v.errIs(v.do(tom, http.MethodPost, "/v1/teams/"+epsilon+"/invitations/7EA55000-0000-4000-8000-00000000000B/resend", ""), 404, "pending invitation not found")
	v.errIs(v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations/nope/resend", ""), 400, "invalid invitationID")

	require.Equal(t, 204, v.do(adam, http.MethodDelete, "/v1/teams/"+alpha+"/invitations/7EA55000-0000-4000-8000-000000000009", "").code)
	v.errIs(v.do(adam, http.MethodDelete, "/v1/teams/"+alpha+"/invitations/7EA55000-0000-4000-8000-000000000009", ""), 404, "pending invitation not found")

	// Accept.
	v.errIs(v.do(una, http.MethodPost, "/v1/team-invitations/parity-invite-3/accept", ""), 403, "verify email before accepting")
	for _, tok := range []string{"no-such-token", "parity-invite-4", "parity-invite-5", "parity-invite-6", "parity-invite-7"} {
		v.errIs(v.do(ivy, http.MethodPost, "/v1/team-invitations/"+tok+"/accept", ""), 410, "invitation is invalid or expired")
	}
	v.errIs(v.do(mia, http.MethodPost, "/v1/team-invitations/parity-invite-1/accept", ""), 410, "invitation is invalid or expired")
	got := v.ok(ivy, http.MethodPost, "/v1/team-invitations/parity-invite-1/accept", "")
	require.Equal(t, map[string]any{"id": alpha, "name": "Team Alpha", "role": "member"}, got)
	require.Equal(t, 1, v.count(`SELECT count(*) FROM team_memberships WHERE team_id = $1 AND user_id = $2 AND role = 'member'`, alpha, ivy))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM vault_memberships WHERE vault_id = $1 AND user_id = $2 AND role = 'editor' AND can_use_ai AND created_by_user_id = $3`, alphaLab, ivy, olga))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM vault_activity_events WHERE vault_id = $1 AND action = 'member.joined' AND actor_name = 'team_ivy'`, alphaLab))
	v.errIs(v.do(ivy, http.MethodPost, "/v1/team-invitations/parity-invite-1/accept", ""), 410, "invitation is invalid or expired")

	// An existing member keeps the team seat; the vault seat is updated.
	v.ok(ivy, http.MethodPost, "/v1/team-invitations/parity-invite-2/accept", "")
	require.Equal(t, 1, v.count(`SELECT count(*) FROM team_memberships WHERE team_id = $1 AND user_id = $2`, beta, ivy))
	require.Equal(t, 1, v.count(`SELECT count(*) FROM vault_memberships WHERE vault_id = $1 AND user_id = $2 AND role = 'admin' AND can_use_ai AND updated_at > created_at`, betaMain, ivy))
	require.Equal(t, []string{"team_ivy's Vault", "alpha lab", "Beta Main"}, names(v.list(ivy, "/v1/vaults")))

	// The token the mailer received for the new invitation is accepted by its addressee.
	_, err = v.pool.Exec(ctx, `UPDATE users SET email = 'New.Person@parity.test' WHERE id = $1`, out)
	require.NoError(t, err)
	v.ok(out, http.MethodPost, "/v1/team-invitations/"+s.token+"/accept", "")
	require.Equal(t, 1, v.count(`SELECT count(*) FROM vault_memberships WHERE vault_id = $1 AND user_id = $2 AND role = 'editor'`, alphaMain, out))
}

// A mailer failure on create deletes the invitation and surfaces the error.
func TestInviteWithFailingResend(t *testing.T) {
	v := setup(t)
	var got map[string]any
	var authz string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"bad from"}`)
	}))
	defer stub.Close()
	rs := &teams.ResendSender{
		APIKey: "re_test_not_real", FromAddress: "team@parity.test", ReplyTo: "help@parity.test",
		BaseURL: "https://app.parity.test", Endpoint: stub.URL, Client: stub.Client(), Log: slog.Default(),
	}
	err := rs.Send(context.Background(), "a@parity.test", "A <Team>", "olga", "TOKEN", time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC))
	require.ErrorContains(t, err, "team invitation email failed")
	require.Equal(t, "Bearer re_test_not_real", authz)
	require.Equal(t, "olga invited you to A <Team> on LuminaVault", got["subject"])
	require.Equal(t, "olga invited you to collaborate in A <Team>. Accept before 2026-11-07 09:30:00 +0000: https://app.parity.test/invitations/TOKEN", got["text"])
	require.Contains(t, got["html"], "<strong>A &lt;Team&gt;</strong>")
	require.Equal(t, "help@parity.test", got["reply_to"])
	require.Equal(t, []any{"a@parity.test"}, got["to"])

	unconfigured := teams.NewSender(teams.SenderConfig{Kind: "Resend"}, slog.Default())
	require.ErrorContains(t, unconfigured.Send(context.Background(), "a", "b", "c", "d", time.Now()), "team invitation email is not configured")

	v.sender.err = errors.New("boom")
	r := v.do(adam, http.MethodPost, "/v1/teams/"+alpha+"/invitations", `{"email":"x@parity.test","vaultGrants":{"`+alphaMain+`":{"role":"viewer","canUseAI":false}}}`)
	require.Equal(t, 500, r.code)
	require.Zero(t, v.count(`SELECT count(*) FROM team_invitations WHERE email = 'x@parity.test'`))
}

func TestActivityLimitAndErrors(t *testing.T) {
	v := setup(t)
	acts := v.list(kim, "/v1/vaults/"+alphaLab+"/activity")
	require.Len(t, acts, 100)
	require.Equal(t, "Space 101", acts[0]["targetTitle"])
	require.Equal(t, "Space 2", acts[99]["targetTitle"])
	require.Empty(t, v.list(kim, "/v1/vaults/"+kim.String()+"/activity"))
	v.errIs(v.do(kim, http.MethodGet, "/v1/vaults/nope/activity", ""), 400, "invalid vaultID")
	v.errIs(v.do(out, http.MethodGet, "/v1/vaults/"+alphaMain+"/activity", ""), 403, "vault access denied")
	v.errIs(v.do(kim, http.MethodGet, "/v1/vaults/"+uuid.Nil.String()+"/activity", ""), 404, "vault not found")
	v.errIs(v.do(kim, http.MethodGet, "/v1/vaults/nope/activity/stream", ""), 400, "invalid vaultID")
	v.errIs(v.do(out, http.MethodGet, "/v1/vaults/"+alphaMain+"/activity/stream", ""), 403, "vault access denied")
}

// The stream sends Swift's headers at once, then one frame per event recorded
// in this process for the vault, whichever module recorded it.
func TestActivityStream(t *testing.T) {
	v := setup(t)
	srv := httptest.NewServer(v.e)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tok, err := scenario.Mint(keys, kim)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/vaults/"+alphaMain+"/activity/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	require.Equal(t, 200, res.StatusCode)
	require.Equal(t, "text/event-stream", res.Header.Get("Content-Type"))
	require.Equal(t, "no-cache", res.Header.Get("Cache-Control"))
	require.Equal(t, "no", res.Header.Get("X-Accel-Buffering"))
	require.Empty(t, res.Header.Get("Content-Encoding"), "never gzipped")

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	frame := func() []string {
		var f []string
		for {
			select {
			case l := <-lines:
				if l == "" {
					return f
				}
				f = append(f, l)
			case <-time.After(10 * time.Second):
				t.Fatal("no frame")
			}
		}
	}

	// A Teams write.
	r := v.do(adam, http.MethodPut, "/v1/vaults/"+alphaMain+"/members/"+mia.String(), `{"role":"editor","canUseAI":false}`)
	require.Equal(t, 200, r.code, string(r.body))
	f := frame()
	require.Len(t, f, 3)
	id := strings.TrimPrefix(f[0], "id: ")
	require.Equal(t, strings.ToUpper(id), id)
	require.Equal(t, "event: activity", f[1])
	var evID string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT upper(id::text) FROM vault_activity_events WHERE action = 'membership.updated'`).Scan(&evID))
	require.Equal(t, evID, id)
	require.Regexp(t, `^data: \{"id":"`+id+`","vaultID":"`+alphaMain+`","actorUserID":"`+strings.ToUpper(adam.String())+
		`","actorName":"team_adam","action":"membership.updated","targetType":"member","targetID":"`+strings.ToUpper(mia.String())+
		`","targetTitle":"team_mia","createdAt":"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ"\}$`, f[2])

	// A Spaces write (another module's Recorder) reaches the same stream; "/" is escaped as Swift does.
	r = v.do(adam, http.MethodPost, "/v1/spaces", `{"name":"Notes/Ideas","slug":"notes"}`, "X-Vault-ID", alphaMain)
	require.Equal(t, 200, r.code, string(r.body))
	f = frame()
	require.Contains(t, f[2], `"action":"space.created","targetType":"space"`)
	require.Contains(t, f[2], `"targetTitle":"Notes\/Ideas"`)

	// Another vault's events are not delivered.
	r = v.do(olga, http.MethodPost, "/v1/teams/"+alpha+"/vaults", `{"name":"Elsewhere"}`)
	require.Equal(t, 200, r.code)
	r = v.do(adam, http.MethodDelete, "/v1/vaults/"+alphaMain+"/members/"+mia.String(), "")
	require.Equal(t, 204, r.code)
	require.Contains(t, frame()[2], `"action":"membership.removed"`)
}

// Every success body must validate against api/openapi.yaml (decisions.md D2).
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	for _, c := range []struct {
		user               uuid.UUID
		method, path, body string
	}{
		{olga, http.MethodGet, "/v1/teams", ""},
		{tom, http.MethodGet, "/v1/teams", ""},
		{ned, http.MethodPost, "/v1/teams", `{"name":"Spec Team"}`},
		{olga, http.MethodGet, "/v1/vaults", ""},
		{olga, http.MethodGet, "/v1/teams/" + alpha + "/vaults", ""},
		{olga, http.MethodPost, "/v1/teams/" + alpha + "/vaults", `{"name":"Spec Vault"}`},
		{adam, http.MethodGet, "/v1/vaults/" + alphaMain + "/members", ""},
		{adam, http.MethodPut, "/v1/vaults/" + alphaMain + "/members/" + mia.String(), `{"role":"editor","canUseAI":true}`},
		{adam, http.MethodDelete, "/v1/vaults/" + alphaMain + "/members/" + mia.String(), ""},
		{kim, http.MethodGet, "/v1/vaults/" + alphaMain + "/activity", ""},
		{adam, http.MethodPost, "/v1/teams/" + alpha + "/invitations", `{"email":"spec@parity.test","vaultGrants":{"` + alphaMain + `":{"role":"viewer","canUseAI":false}}}`},
		{adam, http.MethodGet, "/v1/teams/" + alpha + "/invitations", ""},
		{adam, http.MethodPost, "/v1/teams/" + alpha + "/invitations/7EA55000-0000-4000-8000-000000000008/resend", ""},
		{adam, http.MethodDelete, "/v1/teams/" + alpha + "/invitations/7EA55000-0000-4000-8000-000000000009", ""},
		{ivy, http.MethodPost, "/v1/team-invitations/parity-invite-1/accept", ""},
		{rex, http.MethodPut, "/v1/teams/" + beta + "/owner", `{"userID":"` + bea.String() + `"}`},
		{rex, http.MethodPost, "/v1/teams/" + beta + "/leave", ""},
		{tom, http.MethodPost, "/v1/teams/" + delta + "/archive", ""},
		{tom, http.MethodPost, "/v1/teams/" + delta + "/restore", ""},
	} {
		r := v.do(c.user, c.method, c.path, c.body)
		require.Less(t, r.code, 300, "%s %s: %s", c.method, c.path, r.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		outp := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		outp.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, outp), "%s %s: %s", c.method, c.path, r.body)
	}
}
