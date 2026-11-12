package hermesresolve

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/ssrfguard"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// UserHermesConfigTests: "model uses expected schema name" and "model conforms
// to TenantModel". Go has no Fluent model: the resolver reads the table by name
// and scopes by tenant_id itself. These pin that the table it names exists, that
// tenant_id is its tenant key (unique, a foreign key to users), and that a row is
// only ever read for its own tenant.
func TestUserHermesConfigIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	var regclass *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.user_hermes_config')::text`).Scan(&regclass))
	require.NotNil(t, regclass, "schema name is user_hermes_config")

	var unique, fk int
	require.NoError(t, pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE c.contype = 'u'),
		count(*) FILTER (WHERE c.contype = 'f' AND c.confrelid = 'users'::regclass)
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		WHERE c.conrelid = 'user_hermes_config'::regclass AND a.attname = 'tenant_id' AND cardinality(c.conkey) = 1`).
		Scan(&unique, &fk))
	require.Equal(t, 1, unique, "tenant_id is unique")
	require.Equal(t, 1, fk, "tenant_id references users")

	owner, other := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{owner, other} {
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
			VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", id.String())
		require.NoError(t, err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO user_hermes_config (id, tenant_id, base_url) VALUES ($1, $2, 'not-a-url')`, uuid.New(), owner)
	require.NoError(t, err)

	defaultURL, _ := url.Parse("http://default.hermes.test")
	r := New(pool, nil, ssrfguard.Guard{Resolver: stubResolver(nil)}, defaultURL)
	res, err := r.Resolve(ctx, other)
	require.NoError(t, err, "another tenant's row is never read")
	require.Equal(t, defaultURL, res.BaseURL)
	_, err = r.Resolve(ctx, owner)
	require.Error(t, err, "the owner's row is read")
}
