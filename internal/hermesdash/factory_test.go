package hermesdash_test

import (
	"context"
	"encoding/base64"
	"lumina/internal/dbtest"
	"lumina/internal/hermesdash"
	"lumina/internal/hermesdash/hermesdashtest"
	"lumina/internal/hermesresolve"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MirrorTransportFallbackTests: a BYO gateway that is configured but unusable
// is raised, never swapped for the managed transport on our own disk.

type fallbackEnv struct {
	pool    *pgxpool.Pool
	box     *secretbox.Box
	factory *hermesdash.Factory
}

// evil.test resolves into RFC1918, which the guard rejects once private
// ranges are closed: the verdict production reaches for a ClusterIP.
func newFallbackEnv(t *testing.T) fallbackEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(master))
	require.NoError(t, err)
	answers := map[string][]string{
		"managed.hermes.test": {"93.184.216.34"},
		"good.hermes.test":    {"93.184.216.34"},
		"dash.hermes.test":    {"93.184.216.34"},
		"evil.test":           {"10.0.0.1"},
	}
	guard := ssrfguard.Guard{Resolver: func(_ context.Context, host string) ([]string, error) { return answers[host], nil }}
	managed, _ := url.Parse("http://managed.hermes.test")
	stub := hermesdashtest.New()
	tmp := t.TempDir()
	return fallbackEnv{pool: pool, box: box, factory: &hermesdash.Factory{
		Pool: pool, Box: box, Guard: guard, Resolver: hermesresolve.New(pool, box, guard, managed),
		PerTenantDataRootBase: filepath.Join(tmp, "lv-test-tenants"), ManagedHermesRoot: filepath.Join(tmp, "lv-test-managed-hermes"),
		HTTP: stub,
	}}
}

func (e fallbackEnv) tenant(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	_, err := e.pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", "m"+strings.ReplaceAll(id.String(), "-", "")[:12])
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'Personal', now(), now())`, id)
	require.NoError(t, err)
	return id
}

func (e fallbackEnv) config(t *testing.T, tenant uuid.UUID, base string, dashboard bool) {
	t.Helper()
	var dashURL *string
	var ct, nonce []byte
	if dashboard {
		u := "https://dash.hermes.test"
		dashURL = &u
		sealed, err := e.box.Seal("dashboard-token", tenant)
		require.NoError(t, err)
		ct, nonce = sealed.Ciphertext, sealed.Nonce
	}
	_, err := e.pool.Exec(context.Background(), `INSERT INTO user_hermes_config (id, tenant_id, base_url, cron_dashboard_url, cron_dashboard_token_ciphertext, cron_dashboard_token_nonce)
		VALUES ($1, $2, $3, $4, $5, $6)`, uuid.New(), tenant, base, dashURL, ct, nonce)
	require.NoError(t, err)
}

func TestMirrorTransportFallback(t *testing.T) {
	e := newFallbackEnv(t)
	ctx := context.Background()

	t.Run("a gateway that no longer resolves is raised, not swapped for our own disk", func(t *testing.T) {
		tenant := e.tenant(t)
		e.config(t, tenant, "https://evil.test", false)
		_, err := e.factory.Transport(ctx, tenant)
		var resolution *hermesresolve.ResolutionError
		assert.ErrorAs(t, err, &resolution)
	})

	t.Run("that tenant is reported as none rather than as managed", func(t *testing.T) {
		tenant := e.tenant(t)
		e.config(t, tenant, "https://evil.test", false)
		assert.Equal(t, hermesdash.KindNone, e.factory.Kind(ctx, tenant))
	})

	t.Run("a broken gateway does not take the dashboard down with it", func(t *testing.T) {
		tenant := e.tenant(t)
		e.config(t, tenant, "https://evil.test", true)
		transport, err := e.factory.Transport(ctx, tenant)
		require.NoError(t, err)
		assert.Equal(t, hermesdash.KindRemote, transport.Kind())
	})

	t.Run("a tenant who configured nothing still gets the managed transport", func(t *testing.T) {
		tenant := e.tenant(t)
		transport, err := e.factory.Transport(ctx, tenant)
		require.NoError(t, err)
		assert.Equal(t, hermesdash.KindManaged, transport.Kind())
		assert.Equal(t, hermesdash.KindManaged, e.factory.Kind(ctx, tenant))
	})

	t.Run("a gateway that still resolves is untouched", func(t *testing.T) {
		tenant := e.tenant(t)
		e.config(t, tenant, "https://good.hermes.test", false)
		transport, err := e.factory.Transport(ctx, tenant)
		require.NoError(t, err)
		assert.Equal(t, hermesdash.KindRemote, transport.Kind())
		assert.Equal(t, hermesdash.KindRemote, e.factory.Kind(ctx, tenant))
	})
}
