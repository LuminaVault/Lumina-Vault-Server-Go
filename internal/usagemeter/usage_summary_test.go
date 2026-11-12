package usagemeter_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/dashboard"
	"lumina/internal/usagemeter"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// UsageSummaryAggregationTests: "month-to-date llm + embedding tokens aggregate
// per tenant". The Swift suite replays AnalyticsController's SQL by hand; here
// the rows are written by the meter that writes them in production and read back
// through GET /v1/analytics/usage-summary, which runs that SQL.
func TestUsageSummaryAggregatesPerTenant(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	user := func() uuid.UUID {
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
			VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@test.luminavault", "usg"+id.String()[:8])
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
			VALUES ($1, $1, 'v', now(), now())`, id)
		require.NoError(t, err)
		return id
	}
	tenant, other := user(), user()

	// Distinct models per row: the key is (tenant_id, day, model).
	meter := usagemeter.New(pool)
	meter.Record(ctx, tenant, "model-a", 100, 40)
	meter.Record(ctx, tenant, "model-b", 50, 10)
	meter.Record(ctx, other, "model-a", 999, 999)
	_, err := pool.Exec(ctx, `INSERT INTO embedding_usage (id, tenant_id, year_month, tokens_used, created_at, updated_at)
		VALUES ($1, $2, $3, 777, now(), now())`, uuid.New(), tenant, time.Now().UTC().Format("2006-01"))
	require.NoError(t, err)

	keys := auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	users := auth.PGUsers(pool)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, users),
		Modules: []httpserver.Module{dashboard.New(pool, auth.Identify(keys, users))},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/analytics/usage-summary", nil)
	token, err := keys.MintAccess(tenant, time.Now(), "")
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got struct {
		LLMTokensIn     int `json:"llmTokensIn"`
		LLMTokensOut    int `json:"llmTokensOut"`
		EmbeddingTokens int `json:"embeddingTokens"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, 150, got.LLMTokensIn, "100 + 50, the other tenant excluded")
	require.Equal(t, 50, got.LLMTokensOut, "40 + 10")
	require.Equal(t, 777, got.EmbeddingTokens)
}
