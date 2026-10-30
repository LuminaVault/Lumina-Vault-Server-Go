package vaultaccess_test

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/dbtest"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultactivity"
	"lumina/internal/wire"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	team  = uuid.MustParse("7EA70000-0000-4000-8000-000000000001")
	tv    = uuid.MustParse("7EA70000-0000-4000-8000-0000000000F1")
)

// Alice owns a team vault; bob is a member with the given role.
const teamSeed = `
INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
VALUES ('7ea70000-0000-4000-8000-000000000001', 'T', 'a11ce000-0000-4000-8000-000000000001', 'a11ce000-0000-4000-8000-000000000001', now(), now());
INSERT INTO vaults (id, team_id, name, created_at, updated_at)
VALUES ('7ea70000-0000-4000-8000-0000000000f1', '7ea70000-0000-4000-8000-000000000001', 'Team vault', now(), now());
`

func setup(t *testing.T) (*vaultaccess.Service, func(sql string, args ...any)) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed)+teamSeed)
	require.NoError(t, err)
	exec := func(sql string, args ...any) {
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	return vaultaccess.New(pool), exec
}

func member(exec func(string, ...any), role string) {
	exec(`INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
	      VALUES ($1, $2, $3, $4, false, $5, now(), now())`, uuid.New(), tv, bob, role, alice)
}

func requireErr(t *testing.T, err error, status int, msg string) {
	t.Helper()
	var we *wire.Error
	require.ErrorAs(t, err, &we)
	require.Equal(t, status, we.Status)
	require.Equal(t, msg, we.Message)
}

func TestPersonalVaultIsAdmin(t *testing.T) {
	s, _ := setup(t)
	a, err := s.ResolveVault(context.Background(), alice, alice, vaultaccess.Admin)
	require.NoError(t, err)
	require.True(t, a.IsPersonal)
	require.Equal(t, "admin", a.Role)
	require.Equal(t, alice, a.BillingSponsorUserID)
}

func TestOtherPersonalVaultDenied(t *testing.T) {
	s, _ := setup(t)
	_, err := s.ResolveVault(context.Background(), bob, alice, vaultaccess.Read)
	requireErr(t, err, 403, "vault access denied")
}

func TestUnknownVaultNotFound(t *testing.T) {
	s, _ := setup(t)
	_, err := s.ResolveVault(context.Background(), alice, uuid.New(), vaultaccess.Read)
	requireErr(t, err, 404, "vault not found")
}

func TestViewerCanReadNotWrite(t *testing.T) {
	s, exec := setup(t)
	member(exec, "viewer")
	a, err := s.ResolveVault(context.Background(), bob, tv, vaultaccess.Read)
	require.NoError(t, err)
	require.Equal(t, team, *a.TeamID)
	require.Equal(t, alice, a.BillingSponsorUserID)
	_, err = s.ResolveVault(context.Background(), bob, tv, vaultaccess.Write)
	requireErr(t, err, 403, "vault write permission required")
}

func TestEditorWritesNotAdmin(t *testing.T) {
	s, exec := setup(t)
	member(exec, "editor")
	_, err := s.ResolveVault(context.Background(), bob, tv, vaultaccess.Write)
	require.NoError(t, err)
	_, err = s.ResolveVault(context.Background(), bob, tv, vaultaccess.Admin)
	requireErr(t, err, 403, "vault admin permission required")
}

func TestArchivedVaultAndTeam(t *testing.T) {
	s, exec := setup(t)
	member(exec, "admin")
	exec(`UPDATE teams SET archived_at = now()`)
	_, err := s.ResolveVault(context.Background(), bob, tv, vaultaccess.Read)
	requireErr(t, err, 404, "team vault not found")
	exec(`UPDATE vaults SET archived_at = now() WHERE id = $1`, tv)
	_, err = s.ResolveVault(context.Background(), bob, tv, vaultaccess.Read)
	requireErr(t, err, 404, "vault not found")
}

func TestRecordActivityUsesUsername(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)

	title := "Inbox"
	target := uuid.New()
	require.NoError(t, vaultactivity.New(pool).Record(ctx, vaultactivity.Event{
		VaultID: alice, Actor: alice, Action: "created", TargetType: "space", TargetID: &target, TargetTitle: &title,
	}))
	var name, action string
	require.NoError(t, pool.QueryRow(ctx, `SELECT actor_name, action FROM vault_activity_events WHERE target_id = $1`, target).Scan(&name, &action))
	require.Equal(t, "alice", name)
	require.Equal(t, "created", action)
}

func TestAIPermission(t *testing.T) {
	s, exec := setup(t)
	member(exec, "viewer")
	_, err := s.ResolveVault(context.Background(), bob, tv, vaultaccess.AI)
	requireErr(t, err, 403, "vault AI access required")

	exec(`UPDATE vault_memberships SET can_use_ai = true`)
	a, err := s.ResolveVault(context.Background(), bob, tv, vaultaccess.AI)
	require.NoError(t, err, "AI does not imply write")
	require.False(t, a.CanWrite())

	exec(`UPDATE users SET tier = 'archived' WHERE id = $1`, alice)
	_, err = s.ResolveVault(context.Background(), bob, tv, vaultaccess.AI)
	requireErr(t, err, 402, "team owner subscription does not include AI access")

	exec(`UPDATE users SET tier_override = 'pro' WHERE id = $1`, alice)
	_, err = s.ResolveVault(context.Background(), bob, tv, vaultaccess.AI)
	require.NoError(t, err)
}

// The X-Vault-ID header is checked before identity, as in Swift.
func TestResolveHeaderBeforeIdentity(t *testing.T) {
	s, _ := setup(t)
	keys := auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	e := echo.New()
	e.HTTPErrorHandler = wire.HTTPErrorHandler
	e.GET("/x", func(c *echo.Context) error {
		a, err := s.Resolve(c, vaultaccess.Read)
		if err != nil {
			return err
		}
		return c.String(http.StatusOK, ids.Upper(a.VaultID))
	}, auth.Identify(keys, stubUsers{}))

	call := func(user uuid.UUID, header string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		if header != "" {
			req.Header.Set(vaultaccess.Header, header)
		}
		if user != uuid.Nil {
			tok, err := scenario.Mint(keys, user)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := call(uuid.Nil, "nope")
	require.Equal(t, 400, code)
	require.Contains(t, body, "invalid X-Vault-ID")
	code, body = call(uuid.Nil, "")
	require.Equal(t, 401, code)
	require.Contains(t, body, "Authenticated identity is unavailable")
	code, body = call(alice, "")
	require.Equal(t, 200, code)
	require.Equal(t, ids.Upper(alice), body)
	code, _ = call(alice, strings.ToLower(ids.Upper(tv)))
	require.Equal(t, 403, code)
}

type stubUsers struct{}

func (stubUsers) Exists(context.Context, uuid.UUID) (bool, error) { return true, nil }
