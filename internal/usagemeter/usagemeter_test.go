package usagemeter

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestMeter(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	tenant := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, 'u@test', 'x', 'u', now(), now())`, tenant)
	require.NoError(t, err)
	m := New(pool)
	row := func(model string) (in, out, chars int64, rows int) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `SELECT COALESCE(sum(mtok_in),0)::bigint, COALESCE(sum(mtok_out),0)::bigint,
			COALESCE(sum(chars_out),0)::bigint, count(*) FROM usage_meter WHERE tenant_id = $1 AND model = $2 AND day = CURRENT_DATE`,
			tenant, model).Scan(&in, &out, &chars, &rows))
		return in, out, chars, rows
	}

	// One row a day per model; a second embed adds to it.
	vision := VisionEmbedModel("cohere")
	require.Equal(t, "vision.embed:cohere", vision)
	for range 2 {
		done := make(chan struct{})
		m.RecordVisionEmbed(tenant, "cohere", func() { close(done) })
		<-done
	}
	in, out, chars, rows := row(vision)
	require.Equal(t, []int64{2, 0, 0}, []int64{in, out, chars})
	require.Equal(t, 1, rows)

	// Both counts zero writes nothing.
	m.Record(ctx, tenant, "none", 0, 0)
	_, _, _, rows = row("none")
	require.Zero(t, rows)

	// Failures are logged, not returned, and nil is safe.
	require.NotPanics(t, func() {
		m.Record(ctx, uuid.New(), "fk-violation", 1, 0)
		var none *Meter
		none.Record(ctx, tenant, "m", 1, 0)
		called := false
		none.RecordVisionEmbed(tenant, "k", func() { called = true })
		require.True(t, called)
	})
}
