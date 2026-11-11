package entitlement

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Wave 7A audit: ports EntitlementCheckerTests.swift (27), the middleware and
// paywall cases of EnforcementTests.swift (the /me/billing and admin
// tier-override cases are in internal/modules/billing) and
// BYOEntitlementPolicyTests.swift (7) against this package.

var (
	allTiers        = []string{"free", "trial", "pro", "ultimate", "lapsed", "archived"}
	allCapabilities = []Capability{
		VaultRead, VaultExport, Capture, HealthIngest, Chat, MemoryQuery, MemoGenerator, SkillBuiltinRun,
		SkillVaultRun, KBCompile, MemoryCompile, PrivacyBYOKey, PrivacyContextRouter, MLXOnDevice, WorkflowAutomation,
	}
	ultimateOnly = []Capability{SkillVaultRun, PrivacyBYOKey, PrivacyContextRouter, MLXOnDevice}
)

func allowedTiers(c Capability) map[string]bool {
	out := map[string]bool{}
	for _, tier := range allTiers {
		out[tier] = Entitled(tier, "none", c)
	}
	return out
}

func tiersSet(tiers ...string) map[string]bool {
	out := map[string]bool{}
	for _, tier := range allTiers {
		out[tier] = false
	}
	for _, tier := range tiers {
		out[tier] = true
	}
	return out
}

// EntitlementCheckerTests: the per-capability matrix (workflow automation,
// the always-on five, the active-tier four, the ultimate-only four).
func TestEntitlementCheckerCapabilityMatrix(t *testing.T) {
	everyoneButArchived := tiersSet("free", "trial", "pro", "ultimate", "lapsed")
	active := tiersSet("trial", "pro", "ultimate")
	ultimate := tiersSet("ultimate")
	want := map[Capability]map[string]bool{
		WorkflowAutomation:   active,
		VaultRead:            everyoneButArchived,
		VaultExport:          everyoneButArchived,
		Chat:                 everyoneButArchived,
		MemoryQuery:          everyoneButArchived,
		Capture:              everyoneButArchived,
		SkillBuiltinRun:      active,
		KBCompile:            active,
		MemoryCompile:        active,
		SkillVaultRun:        ultimate,
		PrivacyBYOKey:        ultimate,
		PrivacyContextRouter: ultimate,
		MLXOnDevice:          ultimate,
	}
	for c, tiers := range want {
		require.Equal(t, tiers, allowedTiers(c), c)
	}
	// memory query tracks chat exactly.
	require.Equal(t, allowedTiers(Chat), allowedTiers(MemoryQuery))
}

func TestEntitlementCheckerTierSummaries(t *testing.T) {
	free := map[Capability]bool{VaultRead: true, VaultExport: true, Chat: true, MemoryQuery: true, Capture: true}
	for _, c := range allCapabilities {
		require.Equal(t, free[c], Entitled("free", "none", c), "free gets chat memory and capture and nothing else: %s", c)
		require.Equal(t, Entitled("free", "none", c), Entitled("lapsed", "none", c), "lapsed matches free exactly: %s", c)
		require.False(t, Entitled("archived", "none", c), "archived gets nothing: %s", c)
	}
}

func TestEntitlementCheckerOverrides(t *testing.T) {
	for _, tier := range allTiers {
		for _, c := range allCapabilities {
			require.True(t, Entitled(tier, "ultimate", c), "override ultimate unlocks everything: %s %s", tier, c)
		}
		require.Equal(t, tier, EffectiveTier(tier, "none"), "override none is passthrough")
		require.Equal(t, "ultimate", EffectiveTier(tier, "ultimate"), "ultimate always wins")
	}
	// override pro raises free to pro
	require.True(t, Entitled("free", "pro", SkillBuiltinRun))
	require.True(t, Entitled("free", "pro", WorkflowAutomation))
	require.False(t, Entitled("free", "pro", SkillVaultRun))
	require.Equal(t, "pro", EffectiveTier("free", "pro"))
	require.Equal(t, "ultimate", EffectiveTier("free", "ultimate"))
	// override pro raises lapsed to pro
	require.True(t, Entitled("lapsed", "pro", Chat))
	require.True(t, Entitled("lapsed", "pro", SkillBuiltinRun))
	require.False(t, Entitled("lapsed", "pro", SkillVaultRun))
	// override pro does not downgrade ultimate
	require.True(t, Entitled("ultimate", "pro", SkillVaultRun))
	require.True(t, Entitled("ultimate", "pro", PrivacyBYOKey))
	// effective tier pro raises the floor, never downgrades
	for tier, want := range map[string]string{"lapsed": "pro", "archived": "pro", "trial": "pro", "pro": "pro", "ultimate": "ultimate"} {
		require.Equal(t, want, EffectiveTier(tier, "pro"), tier)
	}
}

// User extension cases: an unrecognised tier reads as lapsed, an unrecognised
// override as none, and a new row (DB defaults) is trial with no override.
func TestEntitlementCheckerUserDecoding(t *testing.T) {
	require.Equal(t, "lapsed", EffectiveTier("garbage", "none"))
	require.True(t, Entitled("garbage", "none", VaultRead))
	require.True(t, Entitled("garbage", "none", Chat))
	require.False(t, Entitled("garbage", "none", SkillBuiltinRun))
	require.False(t, Entitled("garbage", "none", WorkflowAutomation))
	require.False(t, Entitled("garbage", "none", SkillVaultRun))

	require.Equal(t, "free", EffectiveTier("free", "lol"))
}

// BYOEntitlementPolicyTests.
func TestBYOEntitlementPolicy(t *testing.T) {
	billed := []Capability{Chat, Capture, MemoryQuery, MemoGenerator, MemoryCompile, HealthIngest}
	for _, c := range allCapabilities {
		for _, tier := range allTiers {
			require.Empty(t, BYOExemption(c, tier, false, false), "no signal, no exemption: %s %s", c, tier)
		}
		require.Empty(t, BYOExemption(c, "archived", true, false), "archived never exempt")
		require.Empty(t, BYOExemption(c, "archived", false, true), "archived never exempt")
	}
	for _, c := range billed {
		require.Equal(t, "ownHermes", BYOExemption(c, "lapsed", true, false), c)
		require.Equal(t, "ownKey", BYOExemption(c, "lapsed", false, true), c)
	}
	require.Equal(t, "ownHermes", BYOExemption(Chat, "lapsed", true, true), "hermes wins")
	for _, c := range ultimateOnly {
		require.True(t, c.RequiresUltimate())
		for _, tier := range allTiers {
			require.Empty(t, BYOExemption(c, tier, true, false))
			require.Empty(t, BYOExemption(c, tier, false, true))
		}
	}
	for _, tier := range []string{"trial", "pro", "ultimate"} {
		require.Equal(t, "ownHermes", BYOExemption(Chat, tier, true, false), "paying tiers exempt too")
	}
}
