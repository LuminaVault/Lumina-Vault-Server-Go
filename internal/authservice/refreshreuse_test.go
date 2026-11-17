package authservice_test

import (
	"context"
	"lumina/internal/authservice"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Post-archive fixes to refresh (backlog: auth weaknesses kept by #85): Swift
// read then revoked outside a transaction and had no reuse detection.
func TestRefreshRotationIsAtomicAndDetectsReuse(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	signUp := func(t *testing.T, h harness) (string, *authservice.AuthResponse) {
		t.Helper()
		email := randomEmail("refresh")
		reg, err := h.svc.Register(ctx, email, randomUsername("refresh"), password)
		require.NoError(t, err)
		return email, reg
	}
	live := func(t *testing.T, email string) int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens r JOIN users u ON u.id = r.tenant_id
			WHERE u.email = $1 AND r.revoked_at IS NULL`, email).Scan(&n))
		return n
	}

	t.Run("two refreshes of one token: exactly one wins", func(t *testing.T) {
		h := build(t, pool, harness{})
		_, reg := signUp(t, h)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if _, err := h.svc.Refresh(ctx, reg.RefreshToken); err == nil {
					wins.Add(1)
				} else {
					assert.ErrorIs(t, err, authservice.ErrInvalidRefresh)
				}
			})
		}
		wg.Wait()
		assert.Equal(t, int32(1), wins.Load())
	})

	t.Run("racing refreshes finish on a pool smaller than the race", func(t *testing.T) {
		// Each refresh holds one connection for its transaction. If it then asked
		// the pool for another, the winner would wait behind the losers that are
		// waiting on its row lock, and nobody would finish.
		cfg := pool.Config().Copy()
		cfg.MaxConns = 2
		small, err := pgxpool.NewWithConfig(ctx, cfg)
		require.NoError(t, err)
		t.Cleanup(small.Close)
		h := build(t, small, harness{tierOverride: func(string) (string, bool) { return "pro", true }})
		_, reg := signUp(t, h)
		raceCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var wins atomic.Int32
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				if _, err := h.svc.Refresh(raceCtx, reg.RefreshToken); err == nil {
					wins.Add(1)
				} else {
					assert.ErrorIs(t, err, authservice.ErrInvalidRefresh)
				}
			})
		}
		wg.Wait()
		assert.Equal(t, int32(1), wins.Load())
	})

	t.Run("reuse within the grace is a 401 and nothing more", func(t *testing.T) {
		h := build(t, pool, harness{})
		email, reg := signUp(t, h)
		other, err := h.svc.Login(ctx, email, password, false)
		require.NoError(t, err)
		_, err = h.svc.Refresh(ctx, reg.RefreshToken)
		require.NoError(t, err)
		_, err = h.svc.Refresh(ctx, reg.RefreshToken)
		require.ErrorIs(t, err, authservice.ErrInvalidRefresh)
		_, err = h.svc.Refresh(ctx, other.RefreshToken)
		require.NoError(t, err, "the other session is untouched")
	})

	t.Run("reuse after the grace revokes every refresh token of the tenant", func(t *testing.T) {
		h := build(t, pool, harness{})
		email, reg := signUp(t, h)
		other, err := h.svc.Login(ctx, email, password, false)
		require.NoError(t, err)
		rotated, err := h.svc.Refresh(ctx, reg.RefreshToken)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() - interval '1 minute' WHERE revoked_at IS NOT NULL
			AND tenant_id = (SELECT id FROM users WHERE email = $1)`, email)
		require.NoError(t, err)
		require.Equal(t, 2, live(t, email))

		_, err = h.svc.Refresh(ctx, reg.RefreshToken)
		require.ErrorIs(t, err, authservice.ErrInvalidRefresh)
		assert.Zero(t, live(t, email), "the revocation is committed though the refresh failed")
		_, err = h.svc.Refresh(ctx, rotated.RefreshToken)
		assert.ErrorIs(t, err, authservice.ErrInvalidRefresh)
		_, err = h.svc.Refresh(ctx, other.RefreshToken)
		assert.ErrorIs(t, err, authservice.ErrInvalidRefresh)
		_, err = h.svc.Login(ctx, email, password, false)
		require.NoError(t, err, "signing in again works")
	})

	t.Run("a failure while issuing leaves the old token usable", func(t *testing.T) {
		h := build(t, pool, harness{})
		email, reg := signUp(t, h)
		_, err := pool.Exec(ctx, `CREATE FUNCTION fail_refresh_insert() RETURNS trigger LANGUAGE plpgsql AS
			$$ BEGIN RAISE EXCEPTION 'refresh insert refused for the test'; END $$;
			CREATE TRIGGER fail_refresh_insert BEFORE INSERT ON refresh_tokens FOR EACH ROW EXECUTE FUNCTION fail_refresh_insert();`)
		require.NoError(t, err)
		_, err = h.svc.Refresh(ctx, reg.RefreshToken)
		require.Error(t, err)
		require.NotErrorIs(t, err, authservice.ErrInvalidRefresh)
		_, err = pool.Exec(ctx, `DROP TRIGGER fail_refresh_insert ON refresh_tokens; DROP FUNCTION fail_refresh_insert();`)
		require.NoError(t, err)
		assert.Equal(t, 1, live(t, email), "the consume was rolled back")
		_, err = h.svc.Refresh(ctx, reg.RefreshToken)
		require.NoError(t, err)
	})
}
