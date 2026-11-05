package hermescontainers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"lumina/internal/dbtest"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// HermesContainerManagerTests has no test of handle: all seven go through
// ensureRunning, evictIdle or reprovisionAll, which are wave 6. These are new.
func TestHandle(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x01", 32)))
	box, err := secretbox.New(key)
	require.NoError(t, err)
	m := New(pool, box)
	port := 9000

	row := func(t *testing.T, tenant uuid.UUID, sealed secretbox.Sealed, xai, nous *time.Time) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash) VALUES ($1, $2, $3, 'x')`,
			tenant, tenant.String()+"@test.luminavault", "u"+strings.ReplaceAll(tenant.String(), "-", "")[:20])
		require.NoError(t, err)
		port++
		_, err = pool.Exec(ctx, `INSERT INTO hermes_tenant_containers
			(id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, xai_connected_at, nous_connected_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			uuid.New(), tenant, "hermes-tenant-"+ids.Upper(tenant), port, sealed.Ciphertext, sealed.Nonce, xai, nous)
		require.NoError(t, err)
	}

	t.Run("no row is nil with no error", func(t *testing.T) {
		h, err := m.Handle(ctx, uuid.New())
		require.NoError(t, err)
		require.Nil(t, h)
	})

	t.Run("a row comes back with its key open and its markers", func(t *testing.T) {
		tenant := uuid.New()
		sealed, err := box.Seal("the-api-key", tenant)
		require.NoError(t, err)
		connected := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
		row(t, tenant, sealed, &connected, nil)

		h, err := m.Handle(ctx, tenant)
		require.NoError(t, err)
		require.Equal(t, tenant, h.TenantID)
		require.Equal(t, "hermes-tenant-"+ids.Upper(tenant), h.ContainerName)
		require.Equal(t, port, h.Port)
		require.Equal(t, "the-api-key", h.APIServerKey)
		require.WithinDuration(t, connected, *h.XaiConnectedAt, 0)
		require.Nil(t, h.NousConnectedAt)
		require.Equal(t, "http://hermes-tenant-"+ids.Upper(tenant)+":8642", h.BaseURL())
	})

	t.Run("a value Swift sealed opens", func(t *testing.T) {
		raw, err := os.ReadFile("../../testdata/golden/secretbox.json")
		require.NoError(t, err)
		var g struct {
			MasterKeyB64, TenantID, Plaintext, CiphertextB64, NonceB64 string
		}
		require.NoError(t, json.Unmarshal(raw, &g))
		goldenBox, err := secretbox.New(g.MasterKeyB64)
		require.NoError(t, err)
		ciphertext, err := base64.StdEncoding.DecodeString(g.CiphertextB64)
		require.NoError(t, err)
		nonce, err := base64.StdEncoding.DecodeString(g.NonceB64)
		require.NoError(t, err)
		tenant := uuid.MustParse(g.TenantID)
		row(t, tenant, secretbox.Sealed{Ciphertext: ciphertext, Nonce: nonce}, nil, nil)

		h, err := New(pool, goldenBox).Handle(ctx, tenant)
		require.NoError(t, err)
		require.Equal(t, g.Plaintext, h.APIServerKey)
	})

	t.Run("a key sealed for another tenant is an error, not no container", func(t *testing.T) {
		tenant := uuid.New()
		sealed, err := box.Seal("the-api-key", uuid.New())
		require.NoError(t, err)
		row(t, tenant, sealed, nil, nil)

		h, err := m.Handle(ctx, tenant)
		require.ErrorIs(t, err, secretbox.ErrDecryptionFailed)
		require.Nil(t, h)
	})

	t.Run("it writes nothing", func(t *testing.T) {
		tenant := uuid.New()
		sealed, err := box.Seal("the-api-key", tenant)
		require.NoError(t, err)
		row(t, tenant, sealed, nil, nil)
		before := snapshot(t, pool, tenant)

		_, err = m.Handle(ctx, tenant)
		require.NoError(t, err)
		require.Equal(t, before, snapshot(t, pool, tenant))
		require.Nil(t, before.lastUsedAt, "handle doesn't bump last_used_at")
	})
}

type rowState struct {
	xmin       string
	lastUsedAt *time.Time
}

// snapshot reads xmin, which changes on any update to the row.
func snapshot(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) rowState {
	t.Helper()
	var s rowState
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT xmin::text, last_used_at FROM hermes_tenant_containers WHERE tenant_id = $1`, tenant).Scan(&s.xmin, &s.lastUsedAt))
	return s
}
