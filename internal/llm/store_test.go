package llm

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 32 bytes of 0x01, the fixture internal/entitlement uses. Not a real key.
const testMasterKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func llmDB(t *testing.T) (context.Context, *pgxpool.Pool, func() uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	return ctx, pool, func() uuid.UUID {
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username) VALUES ($1, $2, 'x', $3)`, id, id.String(), id.String())
		require.NoError(t, err)
		return id
	}
}

// One database for every store test: a container per test costs more than the
// rows do, and each case below owns its own tenant.
func TestStores(t *testing.T) {
	ctx, pool, newUser := llmDB(t)
	meters := PGMeters{Pool: pool}
	box, err := secretbox.New(testMasterKey)
	require.NoError(t, err)

	t.Run("usage_meter accumulates per day and model", func(t *testing.T) {
		tenant := newUser()
		meter := meters.UsageMeter()
		meter.Record(ctx, tenant, "m1", 10, 2)
		meter.Record(ctx, tenant, "m1", 5, 1)
		meter.Record(ctx, tenant, "m2", 7, 0)
		meter.Record(ctx, tenant, "m3", 0, 0)
		var in, out int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT mtok_in, mtok_out FROM usage_meter WHERE tenant_id = $1 AND model = 'm1' AND day = CURRENT_DATE`, tenant).Scan(&in, &out))
		assert.Equal(t, int64(15), in)
		assert.Equal(t, int64(3), out)
		var rows int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM usage_meter WHERE tenant_id = $1`, tenant).Scan(&rows))
		assert.Equal(t, 2, rows, "zero tokens write no row")
	})
	t.Run("cost_ledger accumulates dollars and calls per provider", func(t *testing.T) {
		tenant := newUser()
		ledger := meters.CostLedger()
		ledger.Record(ctx, tenant, ProviderID(Anthropic), 100)
		ledger.Record(ctx, tenant, ProviderID(Anthropic), 50)
		ledger.Record(ctx, tenant, ProviderID(OpenAI), 7)
		var usd, calls int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT usd_micros, calls FROM cost_ledger WHERE tenant_id = $1 AND provider = 'anthropic' AND day = CURRENT_DATE`, tenant).Scan(&usd, &calls))
		assert.Equal(t, int64(150), usd)
		assert.Equal(t, int64(2), calls)
	})

	// AgentTurnTraceRoundTripTests: the write half.
	t.Run("tool names survive the jsonb round trip as a list", func(t *testing.T) {
		tenant := newUser()
		meters.TraceRecorder().Record(ctx, TraceTurn{
			TenantID: tenant, Provider: ProviderID(OpenAI), Model: "gpt-4o-mini",
			ToolNames: []string{"search_memory", "read_vault_file"}, FailoverCount: 2,
			TokensIn: 1200, TokensOut: 340, EstimatedCostUsdMicros: 384, LatencyMs: 9, CredentialMode: ModeManaged,
		})
		var (
			provider, model, mode     string
			raw                       []byte
			failovers                 int
			tokensIn, tokensOut, cost int64
		)
		require.NoError(t, pool.QueryRow(ctx, `SELECT provider, model, tool_calls, failover_count, tokens_in, tokens_out, estimated_cost_usd_micros, credential_mode
			FROM agent_turn_traces WHERE tenant_id = $1`, tenant).Scan(&provider, &model, &raw, &failovers, &tokensIn, &tokensOut, &cost, &mode))
		assert.Equal(t, "openai", provider)
		assert.Equal(t, "gpt-4o-mini", model)
		assert.JSONEq(t, `{"names":["search_memory","read_vault_file"],"count":2}`, string(raw), "the keyed wrapper, not a bare array")
		assert.Equal(t, 2, failovers)
		assert.Equal(t, int64(1200), tokensIn)
		assert.Equal(t, int64(340), tokensOut)
		assert.Equal(t, int64(384), cost)
		assert.Equal(t, "managed", mode)
	})
	t.Run("an empty tool list is distinct from an absent one", func(t *testing.T) {
		tenant := newUser()
		base := TraceTurn{TenantID: tenant, Provider: ProviderID(OpenAI), Model: "m"}
		empty, absent := base, base
		empty.ToolNames = []string{}
		meters.TraceRecorder().Record(ctx, empty)
		meters.TraceRecorder().Record(ctx, absent)
		var counts struct{ empty, absent int }
		require.NoError(t, pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE tool_calls = '{"names":[],"count":0}'::jsonb),
			count(*) FILTER (WHERE tool_calls IS NULL)
			FROM agent_turn_traces WHERE tenant_id = $1`, tenant).Scan(&counts.empty, &counts.absent))
		assert.Equal(t, 1, counts.empty)
		assert.Equal(t, 1, counts.absent)
	})
	t.Run("a count without names is what the streaming path records", func(t *testing.T) {
		tenant := newUser()
		three := 3
		meters.TraceRecorder().Record(ctx, TraceTurn{TenantID: tenant, Provider: ProviderID(Anthropic), Model: "m", ToolCallCount: &three, FailoverCount: -1, CredentialMode: ModeBYOK})
		var raw []byte
		var failovers int
		var mode *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT tool_calls, failover_count, credential_mode FROM agent_turn_traces WHERE tenant_id = $1`, tenant).Scan(&raw, &failovers, &mode))
		var calls AgentToolCalls
		require.NoError(t, json.Unmarshal(raw, &calls))
		assert.Equal(t, 3, calls.Count)
		assert.Empty(t, calls.Names, "the streaming path never learned the names")
		assert.Zero(t, failovers, "negative values clamp")
		assert.Equal(t, "byok", *mode)
	})
	t.Run("an unset credential mode is NULL", func(t *testing.T) {
		tenant := newUser()
		meters.TraceRecorder().Record(ctx, TraceTurn{TenantID: tenant, Provider: ProviderID(OpenAI), Model: "m"})
		var mode *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT credential_mode FROM agent_turn_traces WHERE tenant_id = $1`, tenant).Scan(&mode))
		assert.Nil(t, mode)
	})

	t.Run("provider_failover_events takes a tenant or none", func(t *testing.T) {
		tenant := newUser()
		status := 402
		notice := FailoverNotice{
			OriginalProvider: XAI, OriginalModel: "grok-4", FallbackProvider: OpenRouter, FallbackModel: "q",
			ReasonCode: "credit_exhausted", StatusCode: &status, Source: FailoverBYO,
		}
		meters.FailoverLogger().Record(ctx, notice, &tenant)
		var provider, model, code, fallbackProvider, fallbackModel, source string
		var gotStatus *int
		require.NoError(t, pool.QueryRow(ctx, `SELECT provider, model, status_code, error_code, fallback_provider, fallback_model, source
			FROM provider_failover_events WHERE tenant_id = $1`, tenant).Scan(&provider, &model, &gotStatus, &code, &fallbackProvider, &fallbackModel, &source))
		assert.Equal(t, []string{"xai", "grok-4", "credit_exhausted", "openRouter", "q", "byo"}, []string{provider, model, code, fallbackProvider, fallbackModel, source})
		assert.Equal(t, 402, *gotStatus)

		notice.OriginalModel, notice.StatusCode = "no-tenant-model", nil
		meters.FailoverLogger().Record(ctx, notice, nil)
		var rows int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM provider_failover_events WHERE model = 'no-tenant-model' AND tenant_id IS NULL AND status_code IS NULL`).Scan(&rows))
		assert.Equal(t, 1, rows)
	})
	t.Run("a failed meter write never reaches the caller", func(t *testing.T) {
		// The tenant has no users row, so each insert breaks its foreign key.
		ghost := uuid.New()
		meters.UsageMeter().Record(ctx, ghost, "m", 1, 1)
		meters.CostLedger().Record(ctx, ghost, ProviderID(OpenAI), 1)
		meters.TraceRecorder().Record(ctx, TraceTurn{TenantID: ghost, Provider: ProviderID(OpenAI), Model: "m"})
		meters.FailoverLogger().Record(ctx, FailoverNotice{}, &ghost)
	})

	t.Run("the transport's writes reach the tables", func(t *testing.T) {
		tenant := newUser()
		transport := transportFor(Decision{Primary: Route{OpenAI, "gpt-4o-mini"}, CredentialMode: ModeManaged},
			&scriptedAdapter{kind: OpenAI, outcomes: []outcome{reply(`{"usage":{"prompt_tokens":1000000,"completion_tokens":0},"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)}})
		transport.UsageMeter, transport.CostLedger, transport.TraceRecorder = meters.UsageMeter(), meters.CostLedger(), meters.TraceRecorder()
		callCtx := WithValues(ctx, func(v *Values) { v.CurrentUser = &User{ID: tenant} })
		_, err := transport.ChatCompletions(callCtx, []byte(emptyPayload), tenant.String(), "")
		require.NoError(t, err)
		transport.Wait()
		var usd int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT usd_micros FROM cost_ledger WHERE tenant_id = $1 AND provider = 'openai'`, tenant).Scan(&usd))
		assert.Equal(t, int64(150_000), usd)
		var traced, metered int
		require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_turn_traces WHERE tenant_id = $1), (SELECT count(*) FROM usage_meter WHERE tenant_id = $1)`, tenant).Scan(&traced, &metered))
		assert.Equal(t, 1, traced)
		assert.Equal(t, 1, metered)
	})

	insertPreference := func(t *testing.T, tenant uuid.UUID, mode, provider, model, chain string, allowed, blocked *string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO user_llm_preferences (id, tenant_id, primary_provider, primary_model, fallback_chain, mode, allowed_providers, blocked_providers)
			VALUES (gen_random_uuid(), $1, $2, $3, $4::jsonb, $5, $6::jsonb, $7::jsonb)`, tenant, provider, model, chain, mode, allowed, blocked)
		require.NoError(t, err)
	}
	t.Run("a preference is read, decoded and cached", func(t *testing.T) {
		tenant := newUser()
		repo := NewPreferenceRepository(pool)
		missing, err := repo.Get(ctx, tenant)
		require.NoError(t, err)
		assert.Nil(t, missing)

		insertPreference(t, tenant, "byok", "xai", "grok-4", `{"steps":[{"provider":"openai","model":"gpt"}]}`, strPtr(`{"providers":["xai","openai"]}`), strPtr(`{"providers":["groq"]}`))
		still, err := repo.Get(ctx, tenant)
		require.NoError(t, err)
		assert.Nil(t, still, "the empty result is cached too")

		repo.Invalidate(tenant)
		got, err := repo.Get(ctx, tenant)
		require.NoError(t, err)
		assert.Equal(t, &Preference{
			Mode: ModeBYOK, PrimaryProvider: XAI, PrimaryModel: "grok-4",
			FallbackChain: []PreferenceStep{{OpenAI, "gpt"}},
			Allowed:       []ProviderKind{XAI, OpenAI},
			Blocked:       []ProviderKind{Groq},
		}, got)

		// Past the TTL the row is read again.
		_, err = pool.Exec(ctx, `UPDATE user_llm_preferences SET mode = 'managed' WHERE tenant_id = $1`, tenant)
		require.NoError(t, err)
		cached, err := repo.Get(ctx, tenant)
		require.NoError(t, err)
		assert.Equal(t, ModeBYOK, cached.Mode)
		repo.now = func() time.Time { return time.Now().Add(cacheTTL + time.Second) }
		fresh, err := repo.Get(ctx, tenant)
		require.NoError(t, err)
		assert.Equal(t, ModeManaged, fresh.Mode)
	})
	t.Run("the preference router steers a real row", func(t *testing.T) {
		tenant := newUser()
		insertPreference(t, tenant, "byok", "xai", "grok-4", `{"steps":[]}`, nil, nil)
		router := &UserPreferenceRouter{Preferences: NewPreferenceRepository(pool), Fallback: fixedRouter{Decision{Primary: hermesRoute}}}
		d := router.Pick(ctx, "", CapabilityMedium, &User{ID: tenant})
		assert.Equal(t, []Route{{XAI, "grok-4"}, hermesRoute}, d.Candidates())
		assert.Equal(t, ModeBYOK, d.CredentialMode)
	})

	insertPrimary := func(t *testing.T, tenant uuid.UUID, provider, kind string, key, baseURL, label *string) {
		t.Helper()
		var ct, nonce []byte
		if key != nil {
			sealed, err := box.Seal(*key, tenant)
			require.NoError(t, err)
			ct, nonce = sealed.Ciphertext, sealed.Nonce
		}
		_, err := pool.Exec(ctx, `INSERT INTO user_provider_credentials (id, tenant_id, provider, credential_kind, ciphertext, nonce, base_url, label)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)`, tenant, provider, kind, ct, nonce, baseURL, label)
		require.NoError(t, err)
	}
	insertPool := func(t *testing.T, tenant uuid.UUID, provider, key, label string, at time.Time) {
		t.Helper()
		sealed, err := box.Seal(key, tenant)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO user_provider_credential_pool_keys (id, tenant_id, provider, ciphertext, nonce, label, created_at)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6)`, tenant, provider, sealed.Ciphertext, sealed.Nonce, label, at)
		require.NoError(t, err)
	}
	t.Run("no row is no credential", func(t *testing.T) {
		got, err := NewCredentialStore(pool, box, nil).Credential(ctx, OpenAI, newUser())
		require.NoError(t, err)
		assert.Nil(t, got)
	})
	t.Run("a stored key is decrypted with its URL and label", func(t *testing.T) {
		tenant := newUser()
		insertPrimary(t, tenant, "openai", "apiKey", strPtr("sk-test-1"), strPtr("https://proxy.example.com"), strPtr("work"))
		got, err := NewCredentialStore(pool, box, nil).Credential(ctx, OpenAI, tenant)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "sk-test-1", *got.APIKey)
		assert.Equal(t, "https://proxy.example.com", got.BaseURL.String())
		assert.Equal(t, "work", *got.Label)
	})
	t.Run("a keyless row with a URL still counts, one with neither does not", func(t *testing.T) {
		withURL, neither := newUser(), newUser()
		insertPrimary(t, withURL, "custom", "apiKey", nil, strPtr("https://ollama.example.com"), nil)
		insertPrimary(t, neither, "custom", "apiKey", nil, nil, nil)
		store := NewCredentialStore(pool, box, nil)
		got, err := store.Credential(ctx, Custom, withURL)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Nil(t, got.APIKey)
		got, err = store.Credential(ctx, Custom, neither)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
	t.Run("a key sealed for another tenant fails rather than leaking", func(t *testing.T) {
		tenant, other := newUser(), newUser()
		sealed, err := box.Seal("sk-other", other)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO user_provider_credentials (id, tenant_id, provider, credential_kind, ciphertext, nonce)
			VALUES (gen_random_uuid(), $1, 'openai', 'apiKey', $2, $3)`, tenant, sealed.Ciphertext, sealed.Nonce)
		require.NoError(t, err)
		_, err = NewCredentialStore(pool, box, nil).Credential(ctx, OpenAI, tenant)
		require.ErrorIs(t, err, secretbox.ErrDecryptionFailed)
	})
	t.Run("pool keys rotate per call in creation order, sharing the primary's URL", func(t *testing.T) {
		tenant := newUser()
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		insertPrimary(t, tenant, "openRouter", "apiKey", strPtr("primary"), strPtr("https://or.example.com"), nil)
		insertPool(t, tenant, "openRouter", "pool-b", "b", base.Add(time.Hour))
		insertPool(t, tenant, "openRouter", "pool-a", "a", base)
		store := NewCredentialStore(pool, box, nil)
		var seen []string
		for range 4 {
			got, err := store.Credential(ctx, OpenRouter, tenant)
			require.NoError(t, err)
			seen = append(seen, *got.APIKey)
			assert.Equal(t, "https://or.example.com", got.BaseURL.String())
		}
		assert.Equal(t, []string{"primary", "pool-a", "pool-b", "primary"}, seen)
	})
	t.Run("the candidate list is cached until invalidated or expired", func(t *testing.T) {
		tenant := newUser()
		store := NewCredentialStore(pool, box, nil)
		got, err := store.Credential(ctx, Groq, tenant)
		require.NoError(t, err)
		assert.Nil(t, got)
		insertPrimary(t, tenant, "groq", "apiKey", strPtr("new"), nil, nil)
		got, err = store.Credential(ctx, Groq, tenant)
		require.NoError(t, err)
		assert.Nil(t, got, "still the cached absence")
		store.Invalidate(tenant, Groq)
		got, err = store.Credential(ctx, Groq, tenant)
		require.NoError(t, err)
		assert.Equal(t, "new", *got.APIKey)

		_, err = pool.Exec(ctx, `DELETE FROM user_provider_credentials WHERE tenant_id = $1`, tenant)
		require.NoError(t, err)
		store.now = func() time.Time { return time.Now().Add(cacheTTL + time.Second) }
		got, err = store.Credential(ctx, Groq, tenant)
		require.NoError(t, err)
		assert.Nil(t, got, "re-read past the TTL")
	})
	t.Run("a SuperGrok marker row spends the tenant's gateway", func(t *testing.T) {
		tenant := newUser()
		insertPrimary(t, tenant, "xai", "oauth", nil, nil, strPtr("SuperGrok"))
		connected := time.Now()
		linked := NewCredentialStore(pool, box, func(context.Context, uuid.UUID) *ContainerHandle {
			return &ContainerHandle{BaseURL: "https://gw.example.com", APIServerKey: "container-key", XAIConnectedAt: &connected}
		})
		got, err := linked.Credential(ctx, XAI, tenant)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "container-key", *got.APIKey)
		assert.Equal(t, "https://gw.example.com", got.BaseURL.String())

		unlinked := NewCredentialStore(pool, box, func(context.Context, uuid.UUID) *ContainerHandle { return nil })
		got, err = unlinked.Credential(ctx, XAI, tenant)
		require.NoError(t, err)
		assert.Nil(t, got, "the marker carries no secret of its own")
	})
	t.Run("the store feeds an adapter end to end", func(t *testing.T) {
		tenant := newUser()
		insertPrimary(t, tenant, "openai", "apiKey", strPtr("sk-tenant"), nil, nil)
		stub := newWireStub(t, 200, completionOK)
		adapter := &OpenAICompatibleAdapter{ProviderKind: OpenAI, APIKey: platformKey, BaseURL: stub.url(t), Credentials: NewCredentialStore(pool, box, nil)}
		_, err := adapter.ChatCompletions(modeCtx(ModeBYOK, &User{ID: tenant}), []byte(chatPayload), "a", "")
		require.NoError(t, err)
		assert.Equal(t, "Bearer sk-tenant", stub.last(t).header.Get("Authorization"))
	})
	t.Run("a closed pool is an error to the store, not a panic", func(t *testing.T) {
		broken, err := pgxpool.New(ctx, pool.Config().ConnString())
		require.NoError(t, err)
		broken.Close()
		_, err = NewCredentialStore(broken, box, nil).Credential(ctx, OpenAI, uuid.New())
		require.Error(t, err)
		_, err = NewPreferenceRepository(broken).Get(ctx, uuid.New())
		require.Error(t, err)
		require.False(t, errors.Is(err, context.Canceled))
	})
}
