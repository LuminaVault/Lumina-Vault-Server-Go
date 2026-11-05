package memorystore

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOriginSummaryFallsBackToTheColumns(t *testing.T) {
	id := uuid.New()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	provider, model, source := "openrouter", "m", "ref"
	s := OriginSummary(id, "chat", &source, &provider, &model, &at)
	require.NotNil(t, s.CreatedBy)
	assert.Equal(t, id, s.CreatedBy.ID)
	assert.Equal(t, OperationCreate, s.CreatedBy.Operation)
	assert.Equal(t, SourceChat, s.CreatedBy.Source)
	require.NotNil(t, s.CreatedBy.Model)
	assert.Equal(t, "openrouter", s.CreatedBy.Model.Provider)
	assert.Equal(t, []ModelRef{{Provider: "openrouter", Model: "m"}}, s.Contributors)

	legacy := OriginSummary(id, "not-a-kind", nil, nil, &model, nil)
	assert.Equal(t, SourceLegacy, legacy.CreatedBy.Source)
	assert.Nil(t, legacy.CreatedBy.Model, "a model with no provider names nothing")
	assert.Empty(t, legacy.Contributors)
}

func TestProvenanceAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	r := New(pool, nil)
	tenant, other := newProvenanceTenant(t, pool), newProvenanceTenant(t, pool)

	m, err := r.Create(ctx, CreateParams{Tenant: tenant, Content: "provenance"})
	require.NoError(t, err)
	foreign, err := r.Create(ctx, CreateParams{Tenant: other, Content: "theirs"})
	require.NoError(t, err)
	// Create writes its own row at the current time; these tests pin the order.
	_, err = pool.Exec(ctx, `DELETE FROM memory_contributions WHERE memory_id = $1`, m.ID)
	require.NoError(t, err)
	insert := func(mem uuid.UUID, op, actor, source string, provider, model *string, at string) {
		_, err := pool.Exec(ctx, `INSERT INTO memory_contributions (id, tenant_id, memory_id, operation, actor_kind, source_kind, provider, model, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			uuid.New(), tenant, mem, op, actor, source, provider, model, at)
		require.NoError(t, err)
	}
	p1, m1, p2, m2 := "openrouter", "one", "mcp:codex", "two"
	insert(m.ID, OperationCreate, ActorUser, SourceManual, nil, nil, "2026-01-01T00:00:00Z")
	insert(m.ID, OperationUpdate, ActorModel, SourceChat, &p1, &m1, "2026-01-02T00:00:00Z")
	insert(m.ID, OperationUpdate, ActorModel, SourceSkill, &p2, &m2, "2026-01-03T00:00:00Z")
	insert(m.ID, OperationUpdate, ActorModel, SourceChat, &p1, &m1, "2026-01-04T00:00:00Z")

	t.Run("Timeline is oldest first and tenant scoped", func(t *testing.T) {
		rows, exists, err := r.Timeline(ctx, tenant, m.ID)
		require.NoError(t, err)
		assert.True(t, exists)
		require.Len(t, rows, 4)
		assert.Equal(t, OperationCreate, rows[0].Operation)
		assert.Nil(t, rows[0].Model)
		assert.Equal(t, "two", rows[2].Model.Model)
		_, exists, err = r.Timeline(ctx, tenant, foreign.ID)
		require.NoError(t, err)
		assert.False(t, exists)
	})

	t.Run("Summaries name the first and last writer and each model once", func(t *testing.T) {
		got, err := r.Summaries(ctx, tenant, []uuid.UUID{m.ID, foreign.ID})
		require.NoError(t, err)
		require.Contains(t, got, m.ID)
		assert.NotContains(t, got, foreign.ID)
		s := got[m.ID]
		assert.Equal(t, ActorUser, s.CreatedBy.Actor)
		assert.Equal(t, "one", s.LastUpdatedBy.Model.Model)
		assert.ElementsMatch(t, []ModelRef{{Provider: "openrouter", Model: "one"}, {Provider: "mcp:codex", Model: "two"}}, s.Contributors)
		none, err := r.Summaries(ctx, tenant, nil)
		require.NoError(t, err)
		assert.Empty(t, none)
	})

	t.Run("Facets count the origin columns", func(t *testing.T) {
		_, err := pool.Exec(ctx, `UPDATE memories SET origin_provider = 'openrouter', origin_model = 'one', origin_kind = 'chat', created_at = '2026-01-05T00:00:00Z' WHERE id = $1`, m.ID)
		require.NoError(t, err)
		f, err := r.Facets(ctx, tenant)
		require.NoError(t, err)
		assert.Equal(t, []Facet{{Value: "openrouter", Count: 1}}, f.Providers)
		assert.Equal(t, []Facet{{Value: "one", Count: 1}}, f.Models)
		assert.Equal(t, []Facet{{Value: "chat", Count: 1}}, f.Sources)
		require.NotNil(t, f.OldestAt)
		assert.True(t, f.OldestAt.Equal(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)))
	})

	t.Run("EnqueueOutput drops a second answer and SuppressJob stops it", func(t *testing.T) {
		provider := "openrouter"
		first := EnqueueOutputParams{Tenant: tenant, Source: SourceChat, SourceID: "msg", Content: "first", Provider: &provider}
		require.NoError(t, r.EnqueueOutput(ctx, first))
		second := first
		second.Content = "second"
		require.NoError(t, r.EnqueueOutput(ctx, second))
		var content, status string
		require.NoError(t, pool.QueryRow(ctx, `SELECT content, status FROM memory_index_jobs WHERE tenant_id = $1 AND source_id = 'msg'`, tenant).Scan(&content, &status))
		assert.Equal(t, "first", content)
		assert.Equal(t, "pending", status)

		sourceID := "msg"
		require.NoError(t, r.SuppressJob(ctx, tenant, &Memory{OriginKind: SourceChat, OriginSourceID: &sourceID}))
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM memory_index_jobs WHERE tenant_id = $1 AND source_id = 'msg'`, tenant).Scan(&status))
		assert.Equal(t, "suppressed", status)
		require.NoError(t, r.SuppressJob(ctx, tenant, &Memory{OriginKind: SourceChat}), "no source id, nothing to do")
	})

	t.Run("RecordRejected is idempotent", func(t *testing.T) {
		hash := ContentHash("x")
		require.NoError(t, r.RecordRejected(ctx, tenant, hash, nil))
		require.NoError(t, r.RecordRejected(ctx, tenant, hash, nil))
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM kb_compile_reject_list WHERE tenant_id = $1`, tenant).Scan(&n))
		assert.Equal(t, 1, n)
	})

	t.Run("FindMany is tenant scoped", func(t *testing.T) {
		rows, err := r.FindMany(ctx, tenant, []uuid.UUID{m.ID, foreign.ID})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, m.ID, rows[0].ID)
	})
}

func newProvenanceTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_verified, username, created_at, updated_at) VALUES ($1, $2, 'x', true, $2, now(), now())`, id, id.String())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(t, err)
	return id
}
