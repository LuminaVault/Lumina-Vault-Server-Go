package auth_test

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPGAudioUsers(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	users := auth.PGAudioUsers(pool)

	_, found, err := users.AudioTokenEpoch(ctx, uuid.New())
	require.NoError(t, err)
	require.False(t, found, "no row")

	id := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username) VALUES ($1, $2, 'x', $2)`, id, id.String())
	require.NoError(t, err)
	epoch, found, err := users.AudioTokenEpoch(ctx, id)
	require.NoError(t, err)
	require.True(t, found)
	require.Nil(t, epoch, "never revoked")

	at := time.Date(2026, 11, 5, 12, 0, 0, 123456000, time.UTC)
	_, err = pool.Exec(ctx, `UPDATE users SET hermes_audio_token_epoch = $2 WHERE id = $1`, id, at)
	require.NoError(t, err)
	epoch, found, err = users.AudioTokenEpoch(ctx, id)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, epoch.Equal(at), "microseconds survive, because the comparison is strict")
}
