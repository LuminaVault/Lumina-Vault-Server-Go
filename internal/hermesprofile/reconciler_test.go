package hermesprofile_test

import (
	"context"
	"errors"
	"lumina/internal/hermesprofile"
	"lumina/internal/ids"
	"lumina/internal/vaultpaths"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func (v env) service(gw hermesprofile.Gateway) *hermesprofile.Service {
	return hermesprofile.New(v.pool, gw, vaultpaths.Paths{Root: v.vaults})
}

func (v env) reconciler(gw hermesprofile.Gateway) *hermesprofile.Reconciler {
	return hermesprofile.NewReconciler(v.pool, v.service(gw), v.dataRoot, "http://127.0.0.1:9", hermesprofile.NewGatewayProbe(""))
}

func (v env) rows(tenant uuid.UUID) []uuid.UUID {
	v.t.Helper()
	rows, err := v.pool.Query(context.Background(), `SELECT id FROM hermes_profiles WHERE tenant_id = $1`, tenant)
	require.NoError(v.t, err)
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(v.t, rows.Scan(&id))
		out = append(out, id)
	}
	return out
}

// Ports HermesProfileServiceTests.swift (5).
func TestHermesProfileService(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	logging := hermesprofile.LoggingGateway{}

	t.Run("ensure twice returns the same ready row", func(t *testing.T) {
		svc := v.service(logging)
		u, _ := v.user()
		require.NoError(t, svc.Ensure(ctx, u))
		first, err := svc.Find(ctx, u)
		require.NoError(t, err)
		require.NoError(t, svc.Ensure(ctx, u))
		second, err := svc.Find(ctx, u)
		require.NoError(t, err)
		require.Equal(t, first.ID, second.ID)
		require.Equal(t, "ready", second.Status)
		require.Len(t, v.rows(u), 1)
	})
	t.Run("gateway failure leaves an error row and user untouched", func(t *testing.T) {
		svc := v.service(gatewayFunc(func(context.Context, uuid.UUID, string) (string, error) {
			return "", errors.New("gateway is intentionally down for the test")
		}))
		u, _ := v.user()
		require.Error(t, svc.Ensure(ctx, u))
		p := v.profile(u)
		require.Equal(t, "error", p.Status)
		require.NotNil(t, p.LastError)
		require.True(t, strings.HasPrefix(p.HermesID, "pending-"))
		var n int
		require.NoError(t, v.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE id = $1`, u).Scan(&n))
		require.Equal(t, 1, n)
	})
	t.Run("ensure recovers an error row to ready", func(t *testing.T) {
		svc := v.service(logging)
		u, name := v.user()
		stale := uuid.New()
		_, err := v.pool.Exec(ctx, `INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status, last_error) VALUES ($1, $2, $3, 'error', 'boom from a previous attempt')`,
			stale, u, "pending-"+ids.Upper(u))
		require.NoError(t, err)
		require.NoError(t, svc.Ensure(ctx, u))
		p := v.profile(u)
		require.Equal(t, "ready", p.Status)
		require.Nil(t, p.LastError)
		require.Equal(t, "hermes-"+name, p.HermesID)
		require.Equal(t, []uuid.UUID{stale}, v.rows(u))
	})
	t.Run("ensure transitions provisioning row to ready", func(t *testing.T) {
		svc := v.service(logging)
		u, name := v.user()
		stuck := uuid.New()
		_, err := v.pool.Exec(ctx, `INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status) VALUES ($1, $2, $3, 'provisioning')`,
			stuck, u, "pending-"+ids.Upper(u))
		require.NoError(t, err)
		require.NoError(t, svc.Ensure(ctx, u))
		p := v.profile(u)
		require.Equal(t, "ready", p.Status)
		require.Equal(t, "hermes-"+name, p.HermesID)
		require.Equal(t, []uuid.UUID{stuck}, v.rows(u))
	})
	t.Run("find returns nil for user without profile", func(t *testing.T) {
		u, _ := v.user()
		found, err := v.service(logging).Find(ctx, u)
		require.NoError(t, err)
		require.Nil(t, found)
	})
}

// Ports HermesProfileReconcilerTests.swift (4). Each test has its own database,
// so the counts are exact where Swift's shared database allowed only >=.
func TestHermesProfileReconciler(t *testing.T) {
	ctx := context.Background()

	t.Run("reconcile creates missing profiles for existing users", func(t *testing.T) {
		v := setup(t)
		recon := v.reconciler(hermesprofile.NewGateway("filesystem", v.dataRoot))
		u1, n1 := v.user()
		u2, n2 := v.user()
		s, err := recon.Reconcile(ctx)
		require.NoError(t, err)
		require.Equal(t, hermesprofile.ReconcileSummary{UsersScanned: 2, ProfilesCreated: 2, Failures: []string{}}, s)
		for u, name := range map[uuid.UUID]string{u1: n1, u2: n2} {
			p := v.profile(u)
			require.Equal(t, "ready", p.Status)
			require.Equal(t, name, p.HermesID)
		}
		again, err := recon.Reconcile(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, again.ProfilesAlreadyOK)
	})
	t.Run("reconcile counts recovered rows and failures", func(t *testing.T) {
		v := setup(t)
		healthy, _ := v.user()
		_, err := v.pool.Exec(ctx, `INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status) VALUES ($1, $2, 'x', 'error')`, uuid.New(), healthy)
		require.NoError(t, err)
		broken, brokenName := v.user()
		recon := v.reconciler(gatewayFunc(func(_ context.Context, tenant uuid.UUID, username string) (string, error) {
			if tenant == broken {
				return "", errors.New("down")
			}
			return "hermes-" + username, nil
		}))
		s, err := recon.Reconcile(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, s.ProfilesRecovered)
		require.Equal(t, []string{brokenName + ": down"}, s.Failures)
	})
	t.Run("reap orphans soft deletes unknown dirs", func(t *testing.T) {
		v := setup(t)
		recon := v.reconciler(hermesprofile.LoggingGateway{})
		v.readyUser("live-one")
		profiles := filepath.Join(v.dataRoot, "profiles")
		require.NoError(t, os.MkdirAll(filepath.Join(profiles, "live-one"), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(profiles, "ghost-abc"), 0o750))
		require.NoError(t, os.MkdirAll(filepath.Join(profiles, "_deleted_1_old"), 0o750))

		s, err := recon.ReapOrphans(ctx)
		require.NoError(t, err)
		require.Equal(t, hermesprofile.ReapSummary{DirsScanned: 3, OrphansSoftDeleted: []string{"ghost-abc"}, ActiveKept: 1}, s)
		require.NoDirExists(t, filepath.Join(profiles, "ghost-abc"))
		entries, err := os.ReadDir(profiles)
		require.NoError(t, err)
		var reaped, live bool
		for _, e := range entries {
			reaped = reaped || (strings.HasPrefix(e.Name(), "_deleted_") && strings.HasSuffix(e.Name(), "_ghost-abc"))
			live = live || e.Name() == "live-one"
		}
		require.True(t, reaped)
		require.True(t, live)
	})
	t.Run("reap with no profiles directory scans nothing", func(t *testing.T) {
		v := setup(t)
		s, err := v.reconciler(hermesprofile.LoggingGateway{}).ReapOrphans(ctx)
		require.NoError(t, err)
		require.Equal(t, hermesprofile.ReapSummary{OrphansSoftDeleted: []string{}}, s)
	})
	t.Run("health reports accurate counts", func(t *testing.T) {
		v := setup(t)
		recon := v.reconciler(hermesprofile.NewGateway("filesystem", v.dataRoot))
		v.user()
		v.user()
		_, err := recon.Reconcile(ctx)
		require.NoError(t, err)
		v.user()
		require.NoError(t, os.MkdirAll(filepath.Join(v.dataRoot, "profiles", "orphan-x"), 0o750))
		h, err := recon.Health(ctx)
		require.NoError(t, err)
		require.Equal(t, hermesprofile.Health{TotalUsers: 3, ProfilesReady: 2, UsersWithoutProfile: 1, OrphanFilesystemDirs: 1}, h)
	})
	t.Run("health is zero orphans after reap", func(t *testing.T) {
		v := setup(t)
		recon := v.reconciler(hermesprofile.NewGateway("filesystem", v.dataRoot))
		v.user()
		_, err := recon.Reconcile(ctx)
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Join(v.dataRoot, "profiles", "trash-x"), 0o750))
		before, err := recon.Health(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, before.OrphanFilesystemDirs)
		_, err = recon.ReapOrphans(ctx)
		require.NoError(t, err)
		after, err := recon.Health(ctx)
		require.NoError(t, err)
		require.Zero(t, after.OrphanFilesystemDirs)
	})
}

func TestGatewayProbe(t *testing.T) {
	var hits atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization") + " " + r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := hermesprofile.NewGatewayProbe("k")
	r := p.Probe(context.Background(), srv.URL)
	require.True(t, r.Reachable, "any HTTP response is reachable")
	require.NotNil(t, r.LatencyMs)
	require.Equal(t, "Bearer k /v1/models", auth.Load())
	p.Probe(context.Background(), srv.URL)
	require.Equal(t, int32(1), hits.Load(), "cached for 30 s")

	down := hermesprofile.NewGatewayProbe("").Probe(context.Background(), "http://127.0.0.1:9")
	require.False(t, down.Reachable)
	require.Nil(t, down.LatencyMs)
}
