package authservice_test

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/authservice"
	"lumina/internal/hermesprofile"
	"lumina/internal/memorystore"
	"lumina/internal/vaultpaths"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TenantIsolationTests: two tenants, and every tenant-owned row reads, writes and
// deletes only within its owner. Swift checks this through
// TenantModel.query(on:tenantID:), its generic tenant filter. Go has no such
// type: each query carries its own `WHERE tenant_id`, so these tests drive the
// Go paths that read and write the same tables (refresh tokens, memories,
// OAuth identities, MFA challenges, Hermes profiles) and assert the partition
// both through those paths and on the rows themselves.

func tenantPair(t *testing.T, h harness, tag string) (*authservice.User, *authservice.User) {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword(password, 4)
	require.NoError(t, err)
	// CreateUser provisions the personal vault the way signup does (Swift's saveTenant).
	u1, err := h.svc.CreateUser(ctx, randomEmail(tag+"1"), randomUsername(tag+"1"), hash)
	require.NoError(t, err)
	u2, err := h.svc.CreateUser(ctx, randomEmail(tag+"2"), randomUsername(tag+"2"), hash)
	require.NoError(t, err)
	return u1, u2
}

func countFor(t *testing.T, h harness, table string, tenant uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, tenant).Scan(&n))
	return n
}

func TestTenantIsolation(t *testing.T) {
	h := build(t, newPool(t), harness{})
	ctx := context.Background()

	// TenantIsolationTests: refresh token query filters by tenant.
	t.Run("refresh token query filters by tenant", func(t *testing.T) {
		u1, u2 := tenantPair(t, h, "ru")
		now := time.Now()
		r1, err := auth.IssueRefreshToken(ctx, h.pool, u1.ID, now)
		require.NoError(t, err)
		r2, err := auth.IssueRefreshToken(ctx, h.pool, u2.ID, now)
		require.NoError(t, err)
		assert.Equal(t, 1, countFor(t, h, "refresh_tokens", u1.ID))
		assert.Equal(t, 1, countFor(t, h, "refresh_tokens", u2.ID))

		got1, err := auth.ConsumeRefreshToken(ctx, h.pool, r1, now)
		require.NoError(t, err)
		got2, err := auth.ConsumeRefreshToken(ctx, h.pool, r2, now)
		require.NoError(t, err)
		assert.Equal(t, u1.ID, got1)
		assert.Equal(t, u2.ID, got2)
	})

	// TenantIsolationTests: memory query filters by tenant.
	t.Run("memory query filters by tenant", func(t *testing.T) {
		u1, u2 := tenantPair(t, h, "mu")
		repo := memorystore.New(h.pool, nil)
		_, err := repo.CreateBare(ctx, u1.ID, "alice secret")
		require.NoError(t, err)
		_, err = repo.CreateBare(ctx, u2.ID, "bob secret")
		require.NoError(t, err)

		contents := func(tenant uuid.UUID) []string {
			list, err := repo.List(ctx, tenant)
			require.NoError(t, err)
			var out []string
			for _, m := range list {
				assert.Equal(t, tenant, m.TenantID)
				out = append(out, m.Content)
			}
			return out
		}
		assert.Equal(t, []string{"alice secret"}, contents(u1.ID))
		assert.Equal(t, []string{"bob secret"}, contents(u2.ID))
	})

	// TenantIsolationTests: oauth identity query filters by tenant.
	t.Run("oauth identity query filters by tenant", func(t *testing.T) {
		u1, u2 := tenantPair(t, h, "ou")
		google, apple := uuid.NewString(), uuid.NewString()
		got1, err := h.svc.UpsertOAuthUser(ctx, "google", google, u1.Email, true)
		require.NoError(t, err)
		got2, err := h.svc.UpsertOAuthUser(ctx, "apple", apple, u2.Email, true)
		require.NoError(t, err)
		assert.Equal(t, u1.ID, got1.ID)
		assert.Equal(t, u2.ID, got2.ID)

		providers := func(tenant uuid.UUID) []string {
			rows, err := h.pool.Query(ctx, `SELECT provider FROM oauth_identities WHERE tenant_id = $1`, tenant)
			require.NoError(t, err)
			defer rows.Close()
			var out []string
			for rows.Next() {
				var p string
				require.NoError(t, rows.Scan(&p))
				out = append(out, p)
			}
			require.NoError(t, rows.Err())
			return out
		}
		assert.Equal(t, []string{"google"}, providers(u1.ID))
		assert.Equal(t, []string{"apple"}, providers(u2.ID))

		// The identity resolves back to its own tenant, whatever email comes with it.
		again, err := h.svc.UpsertOAuthUser(ctx, "google", google, u2.Email, true)
		require.NoError(t, err)
		assert.Equal(t, u1.ID, again.ID)
	})

	// TenantIsolationTests: mfa challenge query filters by tenant.
	t.Run("mfa challenge query filters by tenant", func(t *testing.T) {
		u1, u2 := tenantPair(t, h, "mfa")
		p1, err := h.svc.Login(ctx, u1.Email, password, true)
		require.NoError(t, err)
		p2, err := h.svc.Login(ctx, u2.Email, password, true)
		require.NoError(t, err)
		require.NotNil(t, p1.MFAChallengeID)
		require.NotNil(t, p2.MFAChallengeID)

		destination := func(tenant uuid.UUID) []string {
			rows, err := h.pool.Query(ctx, `SELECT destination FROM mfa_challenges WHERE tenant_id = $1`, tenant)
			require.NoError(t, err)
			defer rows.Close()
			var out []string
			for rows.Next() {
				var d string
				require.NoError(t, rows.Scan(&d))
				out = append(out, d)
			}
			require.NoError(t, rows.Err())
			return out
		}
		assert.Equal(t, []string{u1.Email}, destination(u1.ID))
		assert.Equal(t, []string{u2.Email}, destination(u2.ID))

		// Each challenge signs in its own tenant only.
		a1, err := h.svc.VerifyMFA(ctx, p1.MFAChallengeID.UUID, "123456")
		require.NoError(t, err)
		a2, err := h.svc.VerifyMFA(ctx, p2.MFAChallengeID.UUID, "123456")
		require.NoError(t, err)
		assert.Equal(t, u1.ID, a1.UserID.UUID)
		assert.Equal(t, u2.ID, a2.UserID.UUID)
	})

	// TenantIsolationTests: cross tenant delete only affects owner. Go's
	// tenant-wide refresh-token write is the revoke in ResetPassword.
	t.Run("cross tenant delete only affects owner", func(t *testing.T) {
		u1, u2 := tenantPair(t, h, "del")
		now := time.Now()
		r1, err := auth.IssueRefreshToken(ctx, h.pool, u1.ID, now)
		require.NoError(t, err)
		r2, err := auth.IssueRefreshToken(ctx, h.pool, u2.ID, now)
		require.NoError(t, err)

		require.NoError(t, h.svc.ForgotPassword(ctx, u1.Email))
		_, err = h.svc.ResetPassword(ctx, u1.Email, "654321", "AnotherCorrectHorse2!")
		require.NoError(t, err)

		_, err = auth.ConsumeRefreshToken(ctx, h.pool, r1, now)
		require.ErrorIs(t, err, auth.ErrInvalidRefresh, "the owner's token is gone")
		got, err := auth.ConsumeRefreshToken(ctx, h.pool, r2, now)
		require.NoError(t, err, "the other tenant's token survives")
		assert.Equal(t, u2.ID, got)
	})

	// TenantIsolationTests: hermes profile service ensure scopes by tenant.
	t.Run("hermes profile service ensure scopes by tenant", func(t *testing.T) {
		u1, u2 := tenantPair(t, h, "hps")
		svc := hermesprofile.New(h.pool, hermesprofile.LoggingGateway{}, vaultpaths.Paths{Root: t.TempDir()})
		require.NoError(t, svc.Ensure(ctx, u1.ID))
		require.NoError(t, svc.Ensure(ctx, u2.ID))

		p1, err := svc.Find(ctx, u1.ID)
		require.NoError(t, err)
		p2, err := svc.Find(ctx, u2.ID)
		require.NoError(t, err)
		require.NotNil(t, p1)
		require.NotNil(t, p2)
		assert.Equal(t, "hermes-"+u1.Username, p1.HermesProfileID)
		assert.Equal(t, "hermes-"+u2.Username, p2.HermesProfileID)
		assert.Equal(t, 1, countFor(t, h, "hermes_profiles", u1.ID))
		assert.Equal(t, 1, countFor(t, h, "hermes_profiles", u2.ID))

		require.NoError(t, svc.Ensure(ctx, u1.ID))
		again, err := svc.Find(ctx, u1.ID)
		require.NoError(t, err)
		require.NotNil(t, again)
		assert.Equal(t, p1.ID, again.ID)
		assert.Equal(t, 1, countFor(t, h, "hermes_profiles", u1.ID))
	})
}
