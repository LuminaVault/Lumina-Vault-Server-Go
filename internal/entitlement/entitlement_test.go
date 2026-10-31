package entitlement

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Swift EntitlementChecker: override never lowers, unknown tier is lapsed.
func TestEffectiveTier(t *testing.T) {
	for _, c := range []struct{ tier, override, want string }{
		{"free", "none", "free"},
		{"ultimate", "pro", "ultimate"},
		{"lapsed", "pro", "pro"},
		{"archived", "ultimate", "ultimate"},
		{"bogus", "none", "lapsed"},
		{"trial", "bogus", "trial"},
	} {
		require.Equal(t, c.want, EffectiveTier(c.tier, c.override), c)
	}
}

func TestEntitledMatrix(t *testing.T) {
	tiers := []string{"free", "trial", "pro", "ultimate", "lapsed", "archived"}
	want := map[Capability][]bool{
		VaultRead:     {true, true, true, true, true, false},
		Chat:          {true, true, true, true, true, false},
		HealthIngest:  {false, true, true, true, false, false},
		KBCompile:     {false, true, true, true, false, false},
		SkillVaultRun: {false, false, false, true, false, false},
	}
	for c, row := range want {
		for i, tier := range tiers {
			require.Equal(t, row[i], Entitled(tier, "none", c), "%s %s", c, tier)
		}
	}
	require.True(t, Entitled("bogus", "none", Chat), "unknown tier reads as lapsed")
	require.False(t, Entitled("bogus", "none", HealthIngest))
}

func TestEnforcementEnabled(t *testing.T) {
	for raw, want := range map[string]bool{"true": true, "TRUE": true, "True": true, "1": false, "yes": false, "": false, " true": false} {
		require.Equal(t, want, EnforcementEnabled(raw), raw)
	}
}

// byoHasUsableCredential: an API key anywhere counts; a bare base URL counts
// only for ollama and custom; pool keys count; undecryptable rows do not.
func TestPGCredentials(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	box, err := secretbox.New("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	require.NoError(t, err)

	newUser := func() uuid.UUID {
		id := uuid.New()
		_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username) VALUES ($1, $2, 'x', $3)`, id, id.String(), id.String())
		require.NoError(t, err)
		return id
	}
	primary := func(tenant uuid.UUID, provider string, key, baseURL *string) {
		var ct, nonce []byte
		if key != nil {
			s, err := box.Seal(*key, tenant)
			require.NoError(t, err)
			ct, nonce = s.Ciphertext, s.Nonce
		}
		_, err := pool.Exec(ctx, `INSERT INTO user_provider_credentials (id, tenant_id, provider, credential_kind, ciphertext, nonce, base_url)
			VALUES (gen_random_uuid(), $1, $2, 'apiKey', $3, $4, $5)`, tenant, provider, ct, nonce, baseURL)
		require.NoError(t, err)
	}
	str := func(s string) *string { return &s }
	has := PGCredentials(pool, box)

	none := newUser()
	require.False(t, has(ctx, none))

	withKey := newUser()
	primary(withKey, "anthropic", str("sk-1"), nil)
	require.True(t, has(ctx, withKey))

	emptyKey := newUser()
	primary(emptyKey, "openai", str(""), nil)
	require.False(t, has(ctx, emptyKey))

	urlOnly := newUser()
	primary(urlOnly, "openai", nil, str("https://api.example.com"))
	require.False(t, has(ctx, urlOnly))

	ollama := newUser()
	primary(ollama, "ollama", nil, str("http://10.0.0.2:11434"))
	require.True(t, has(ctx, ollama))

	notTarget := newUser()
	primary(notTarget, "groq", str("gsk"), nil)
	require.False(t, has(ctx, notTarget))

	pooled := newUser()
	primary(pooled, "openai", nil, str("https://api.example.com"))
	s, err := box.Seal("sk-pool", pooled)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO user_provider_credential_pool_keys (id, tenant_id, provider, ciphertext, nonce, created_at)
		VALUES (gen_random_uuid(), $1, 'openai', $2, $3, now())`, pooled, s.Ciphertext, s.Nonce)
	require.NoError(t, err)
	require.True(t, has(ctx, pooled))

	foreign := newUser()
	sealed, err := box.Seal("sk-x", withKey) // sealed for another tenant: does not open
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO user_provider_credentials (id, tenant_id, provider, credential_kind, ciphertext, nonce)
		VALUES (gen_random_uuid(), $1, 'anthropic', 'apiKey', $2, $3)`, foreign, sealed.Ciphertext, sealed.Nonce)
	require.NoError(t, err)
	require.False(t, has(ctx, foreign))

	require.False(t, PGCredentials(pool, nil)(ctx, withKey), "no master key, no store")
}
