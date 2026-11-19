package connectedapps_test

import (
	"context"
	"lumina/internal/connectedapps"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// testMasterKey is base64 of 32 bytes of 0x01. It is a copy of
// calendar.TestMasterKey, which lives in a _test.go file and cannot be imported.
const testMasterKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
)

func newStore(t *testing.T) (*connectedapps.Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, u := range []struct {
		id   uuid.UUID
		name string
	}{{alice, "alice"}, {bob, "bob"}} {
		_, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, password_hash, username) VALUES ($1, $2, 'x', $3)`,
			u.id, u.name+"@example.test", u.name)
		require.NoError(t, err)
	}
	box, err := secretbox.New(testMasterKey)
	require.NoError(t, err)
	return connectedapps.NewStore(pool, box), pool
}

func saveAaru(t *testing.T, s *connectedapps.Store, user uuid.UUID) {
	t.Helper()
	scope := "read"
	require.NoError(t, s.Save(context.Background(), user, connectedapps.Account{
		AppID: "aaru", AccessToken: "access-123", RefreshToken: "refresh-456", Scope: &scope,
	}))
}

func TestStoreRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	saveAaru(t, s, alice)
	got, err := s.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "access-123", got.AccessToken)
	require.Equal(t, "refresh-456", got.RefreshToken)
	require.Equal(t, "connected", got.Status)
	require.Equal(t, "read", *got.Scope)
	require.False(t, got.ConnectedAt.IsZero())

	// Upsert replaces tokens and a missing refresh token stores NULLs.
	require.NoError(t, s.Save(context.Background(), alice, connectedapps.Account{AppID: "aaru", AccessToken: "new"}))
	got, err = s.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "new", got.AccessToken)
	require.Empty(t, got.RefreshToken)
}

func TestStoreIsolatesUsers(t *testing.T) {
	s, _ := newStore(t)
	saveAaru(t, s, alice)
	_, err := s.Get(context.Background(), bob, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
	list, err := s.List(context.Background(), bob)
	require.NoError(t, err)
	require.Empty(t, list)
}

func TestSealedRowMovedToOtherUserFailsOpen(t *testing.T) {
	s, pool := newStore(t)
	saveAaru(t, s, alice)
	_, err := pool.Exec(context.Background(), `UPDATE connected_app_accounts SET user_id = $1`, bob)
	require.NoError(t, err)
	_, err = s.Get(context.Background(), bob, "aaru")
	require.ErrorIs(t, err, secretbox.ErrDecryptionFailed)
}

func TestListNeverReturnsTokens(t *testing.T) {
	s, _ := newStore(t)
	saveAaru(t, s, alice)
	list, err := s.List(context.Background(), alice)
	require.NoError(t, err)
	require.Len(t, list, 1)
	for _, a := range list {
		require.Equal(t, "aaru", a.AppID)
		require.Empty(t, a.AccessToken)
		require.Empty(t, a.RefreshToken)
	}
}

func TestDeleteRemovesRow(t *testing.T) {
	s, _ := newStore(t)
	saveAaru(t, s, alice)
	require.NoError(t, s.Delete(context.Background(), alice, "aaru"))
	_, err := s.Get(context.Background(), alice, "aaru")
	require.ErrorIs(t, err, connectedapps.ErrNotConnected)
}

func TestMarkNeedsReauth(t *testing.T) {
	s, _ := newStore(t)
	saveAaru(t, s, alice)
	require.NoError(t, s.MarkNeedsReauth(context.Background(), alice, "aaru", "invalid_grant"))
	got, err := s.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "needs_reauth", got.Status)
	// Saving again (re-connect) returns the account to connected.
	saveAaru(t, s, alice)
	got, err = s.Get(context.Background(), alice, "aaru")
	require.NoError(t, err)
	require.Equal(t, "connected", got.Status)
}
