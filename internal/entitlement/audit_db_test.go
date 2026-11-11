package entitlement_test

import (
	"encoding/json"
	"lumina/internal/entitlement"
	"testing"

	"github.com/stretchr/testify/require"
)

// EnforcementTests.swift and EntitlementCheckerTests.swift cases that need the
// database (see audit_test.go for the rest of the 7A audit).

func TestEnforcementMiddleware(t *testing.T) {
	g := newGateEnv(t)
	lapsed := g.user("lapsed", "none")

	code, _ := g.probe(entitlement.Middleware(g.pool, entitlement.HealthIngest, false, nil), lapsed)
	require.Equal(t, 200, code, "enforcement disabled is a no-op for a lapsed user")

	code, body := g.probe(entitlement.Middleware(g.pool, entitlement.HealthIngest, true, nil), lapsed)
	require.Equal(t, 402, code)
	require.JSONEq(t, `{"paywall":true,"paywallId":"default","requiredTier":"pro"}`, body)

	code, _ = g.probe(entitlement.Middleware(g.pool, entitlement.HealthIngest, true, nil), g.user("lapsed", "ultimate"))
	require.Equal(t, 200, code, "ultimate override wins even when the stored tier is lapsed")

	code, _ = g.probe(entitlement.Middleware(g.pool, entitlement.VaultExport, true, nil), lapsed)
	require.Equal(t, 200, code, "vault export remains available to a lapsed user")

	code, body = g.probe(entitlement.Middleware(g.pool, entitlement.SkillVaultRun, true, nil), lapsed)
	require.Equal(t, 402, code)
	require.JSONEq(t, `{"paywall":true,"paywallId":"ultimate_upsell","requiredTier":"ultimate"}`, body)
}

// The paywall hint and the required tier never disagree, and the tier is always
// one you can buy: every capability's 402 for an archived user.
func TestEnforcementPaywallHints(t *testing.T) {
	g := newGateEnv(t)
	archived := g.user("archived", "none")
	for _, c := range []entitlement.Capability{
		entitlement.VaultRead, entitlement.VaultExport, entitlement.Capture, entitlement.HealthIngest, entitlement.Chat,
		entitlement.MemoryQuery, entitlement.MemoGenerator, entitlement.SkillBuiltinRun, entitlement.SkillVaultRun,
		entitlement.KBCompile, entitlement.MemoryCompile, entitlement.PrivacyBYOKey, entitlement.PrivacyContextRouter,
		entitlement.MLXOnDevice, entitlement.WorkflowAutomation,
	} {
		code, body := g.probe(entitlement.Middleware(g.pool, c, true, nil), archived)
		require.Equal(t, 402, code, c)
		var p struct{ PaywallID, RequiredTier string }
		require.NoError(t, json.Unmarshal([]byte(body), &p))
		require.Equal(t, p.PaywallID == "ultimate_upsell", p.RequiredTier == "ultimate", "hint and tier agree: %s", c)
		require.Equal(t, c.RequiresUltimate(), p.RequiredTier == "ultimate", c)
		require.Contains(t, []string{"pro", "ultimate"}, p.RequiredTier, "purchasable: %s", c)
	}
}

// A row created with the database defaults is trial with no override and no
// expiry (EntitlementCheckerTests' post-init defaults).
func TestEntitlementCheckerRowDefaults(t *testing.T) {
	g := newGateEnv(t)
	var tier, override string
	var expires *string
	_, err := g.pool.Exec(t.Context(), `INSERT INTO users (id, email, password_hash, username) VALUES (gen_random_uuid(), 'd@x', 'x', 'd')`)
	require.NoError(t, err)
	require.NoError(t, g.pool.QueryRow(t.Context(), `SELECT tier, tier_override, tier_expires_at::text FROM users WHERE email = 'd@x'`).
		Scan(&tier, &override, &expires))
	require.Equal(t, "trial", tier)
	require.Equal(t, "none", override)
	require.Nil(t, expires)
	require.True(t, entitlement.Entitled(tier, override, entitlement.Chat))
	require.False(t, entitlement.Entitled(tier, override, entitlement.SkillVaultRun))
}
