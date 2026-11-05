package llm

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FreeLanePolicyTests.
func TestFreeLanePolicy(t *testing.T) {
	input := func(tier string, mode CredentialMode, hasKey, platform, enabled bool) FreeLaneInput {
		return FreeLaneInput{EffectiveTier: tier, RequestedMode: mode, HasUsableUserCredential: hasKey, PlatformPaidManagedAvailable: platform, FreeLaneEnabled: enabled}
	}
	managed := func(tier string) FreeLaneInput { return input(tier, ModeManaged, false, true, true) }
	byok := func(tier string, hasKey bool) FreeLaneInput { return input(tier, ModeBYOK, hasKey, true, true) }
	forced := &FreeLaneVerdict{Trigger: TriggerNotEntitled, Forced: true}
	outage := &FreeLaneVerdict{Trigger: TriggerPlatformUnavailable, Forced: true}
	entitled := []string{"pro", "ultimate", "trial"}
	every := []string{"pro", "ultimate", "trial", "lapsed", "free", "archived"}

	t.Run("lapsed managed user is forced onto the free lane", func(t *testing.T) {
		assert.Equal(t, forced, EvaluateFreeLane(managed("lapsed")))
	})
	t.Run("free managed user is forced onto the free lane", func(t *testing.T) {
		assert.Equal(t, forced, EvaluateFreeLane(managed("free")))
	})
	t.Run("free user with a real key keeps their own models", func(t *testing.T) {
		assert.Nil(t, EvaluateFreeLane(byok("free", true)))
		assert.True(t, HonoursUserChoice(byok("free", true)))
	})
	t.Run("free byok user with no key gets the lane, not a dead end", func(t *testing.T) {
		assert.Equal(t, forced, EvaluateFreeLane(byok("free", false)))
	})
	t.Run("lapsed byok user with no key gets the free lane, not a dead end", func(t *testing.T) {
		assert.Equal(t, forced, EvaluateFreeLane(byok("lapsed", false)))
	})
	t.Run("byok with no key anywhere gets the free lane on every entitled tier", func(t *testing.T) {
		for _, tier := range []string{"pro", "ultimate", "trial", "lapsed"} {
			assert.Equal(t, forced, EvaluateFreeLane(byok(tier, false)), tier)
		}
	})
	t.Run("archived byok user with no key is still left alone", func(t *testing.T) {
		assert.Nil(t, EvaluateFreeLane(byok("archived", false)))
	})
	t.Run("byok with a key is still honoured and never diverted", func(t *testing.T) {
		for _, tier := range entitled {
			assert.Nil(t, EvaluateFreeLane(byok(tier, true)), tier)
		}
	})
	t.Run("paying and trial tiers keep their own routing", func(t *testing.T) {
		for _, tier := range entitled {
			assert.Nil(t, EvaluateFreeLane(managed(tier)), tier)
			assert.True(t, HonoursUserChoice(managed(tier)), tier)
		}
	})
	t.Run("byok with a real key is honoured on every tier", func(t *testing.T) {
		for _, tier := range every {
			assert.Nil(t, EvaluateFreeLane(byok(tier, true)), tier)
		}
	})
	t.Run("archived never gets a manufactured route", func(t *testing.T) {
		assert.Nil(t, EvaluateFreeLane(managed("archived")))
		assert.Nil(t, EvaluateFreeLane(input("archived", ModeManaged, false, false, true)))
	})
	t.Run("platform outage degrades even a paying user", func(t *testing.T) {
		for _, tier := range entitled {
			assert.Equal(t, outage, EvaluateFreeLane(input(tier, ModeManaged, false, false, true)), tier)
		}
	})
	t.Run("platform outage does not touch a byok user with a key", func(t *testing.T) {
		assert.Nil(t, EvaluateFreeLane(input("pro", ModeBYOK, true, false, true)))
	})
	t.Run("kill switch disables the lane for every input", func(t *testing.T) {
		for _, tier := range every {
			for _, mode := range []CredentialMode{ModeManaged, ModeBYOK} {
				for _, hasKey := range []bool{true, false} {
					for _, platform := range []bool{true, false} {
						assert.Nil(t, EvaluateFreeLane(input(tier, mode, hasKey, platform, false)))
					}
				}
			}
		}
	})
}

func defaultFreeLaneRoutes(preferring FreeLaneLeg) []FreeLaneRoute {
	return FreeLaneRoutes(FreeLaneOpenRouterModel, FreeLaneOpenRouterSecondaryModel, FreeLaneNvidiaModel, preferring)
}

// FreeLaneCatalogTests.
func TestFreeLaneCatalog(t *testing.T) {
	legsOf := func(routes []FreeLaneRoute) []FreeLaneLeg {
		out := make([]FreeLaneLeg, 0, len(routes))
		for _, r := range routes {
			out = append(out, r.Leg)
		}
		return out
	}
	t.Run("three routes: two OpenRouter slugs, then the NIM reserve", func(t *testing.T) {
		routes := defaultFreeLaneRoutes("")
		assert.Equal(t, []FreeLaneLeg{LegOpenRouterFree, LegOpenRouterFree, LegNvidiaDirect}, legsOf(routes))
		assert.Equal(t, []ProviderID{ProviderID(OpenRouter), ProviderID(OpenRouter), ProviderID(Nvidia)}, []ProviderID{routes[0].Provider, routes[1].Provider, routes[2].Provider})
		assert.Equal(t, FreeLaneOpenRouterModel, routes[0].Model)
		assert.Equal(t, FreeLaneOpenRouterSecondaryModel, routes[1].Model)
	})
	t.Run("default free slugs are the Nemotron ultra, then the Nemotron super", func(t *testing.T) {
		assert.Equal(t, "nvidia/nemotron-3-ultra-550b-a55b:free", FreeLaneOpenRouterModel)
		assert.Equal(t, "nvidia/nemotron-3-super-120b-a12b:free", FreeLaneOpenRouterSecondaryModel)
		assert.Equal(t, "nvidia/nemotron-3-super-120b-a12b", FreeLaneNvidiaModel)
		for _, r := range defaultFreeLaneRoutes("") {
			assert.False(t, strings.HasPrefix(r.Model, "z-ai/glm"))
		}
	})
	t.Run("both OpenRouter slugs meter as a single leg", func(t *testing.T) {
		routes := defaultFreeLaneRoutes("")
		assert.NotEqual(t, routes[0].Model, routes[1].Model, "the two slugs must be distinct models")
		assert.Equal(t, routes[0].Leg, routes[1].Leg)
		assert.ElementsMatch(t, freeLaneLegs, []FreeLaneLeg{routes[0].Leg, routes[2].Leg})
	})
	t.Run("preferring the OpenRouter leg keeps both its slugs, in order", func(t *testing.T) {
		routes := defaultFreeLaneRoutes(LegOpenRouterFree)
		assert.Equal(t, []FreeLaneLeg{LegOpenRouterFree, LegOpenRouterFree, LegNvidiaDirect}, legsOf(routes))
		assert.Equal(t, FreeLaneOpenRouterModel, routes[0].Model)
		assert.Equal(t, FreeLaneOpenRouterSecondaryModel, routes[1].Model)
	})
	t.Run("preferring a leg promotes it without dropping the others", func(t *testing.T) {
		assert.Equal(t, []FreeLaneLeg{LegNvidiaDirect, LegOpenRouterFree, LegOpenRouterFree}, legsOf(defaultFreeLaneRoutes(LegNvidiaDirect)))
	})
	t.Run("every free-lane model clears the Hermes 64K context floor", func(t *testing.T) {
		for _, route := range defaultFreeLaneRoutes("") {
			var window int
			for _, m := range ModelsFor(route.Provider) {
				if m.ID == route.Model {
					window = m.ContextWindow
				}
			}
			assert.GreaterOrEqual(t, window, HermesMinimumContextWindow, route.Model)
		}
	})
	t.Run("the NVIDIA NIM leg never carries a :free suffix", func(t *testing.T) {
		for _, route := range defaultFreeLaneRoutes("") {
			if route.Leg == LegNvidiaDirect {
				assert.False(t, strings.HasSuffix(route.Model, ":free"))
			}
		}
	})
	t.Run("no :free slug leaks into the paying-user Auto pool", func(t *testing.T) {
		for _, e := range RouterCatalogEntries() {
			assert.False(t, strings.HasSuffix(e.Model, ":free"), e.Model)
		}
	})
	t.Run("no zero-cost entry in the paying-user Auto pool", func(t *testing.T) {
		for _, e := range RouterCatalogEntries() {
			assert.False(t, e.InputPerMillionUsdMicros == 0 && e.OutputPerMillionUsdMicros == 0, e.Model)
		}
	})
	t.Run("dead NVIDIA llama entries are gone", func(t *testing.T) {
		for _, model := range []string{"meta/llama-3.1-8b-instruct", "meta/llama-3.1-70b-instruct", "nvidia/nemotron-3-ultra"} {
			_, ok := CatalogEntry(ProviderID(Nvidia), model)
			assert.False(t, ok, model)
		}
	})
	t.Run("the OpenRouter leg is zero-rated by slug; the NIM leg is credit-funded", func(t *testing.T) {
		for _, route := range defaultFreeLaneRoutes("") {
			entry, ok := CatalogEntry(route.Provider, route.Model)
			if route.Leg == LegOpenRouterFree {
				assert.True(t, strings.HasSuffix(route.Model, ":free"), route.Model)
				assert.False(t, ok, "never catalogued")
				continue
			}
			require.True(t, ok, "the NIM leg must stay a catalogued, priced model")
			assert.Positive(t, entry.InputPerMillionUsdMicros)
		}
	})
}

// FreeLaneRecoveryActionsTests.
func TestFreeLaneRecoveryActions(t *testing.T) {
	t.Run("unpaid tiers are offered an upgrade or a key", func(t *testing.T) {
		for _, tier := range []string{"free", "lapsed"} {
			assert.Equal(t, []string{"upgrade", "add_key"}, FreeLaneRecoveryActions(tier, ModeManaged, TriggerNotEntitled), tier)
		}
	})
	t.Run("entitled byok without a key is offered managed, never an upgrade", func(t *testing.T) {
		for _, tier := range []string{"trial", "pro", "ultimate"} {
			assert.Equal(t, []string{"add_key", "switch_to_managed"}, FreeLaneRecoveryActions(tier, ModeBYOK, TriggerNotEntitled), tier)
		}
	})
	t.Run("a platform outage offers only a key", func(t *testing.T) {
		for _, tier := range []string{"trial", "pro", "ultimate"} {
			assert.Equal(t, []string{"add_key"}, FreeLaneRecoveryActions(tier, ModeManaged, TriggerPlatformUnavailable), tier)
			assert.Equal(t, []string{"add_key"}, FreeLaneRecoveryActions(tier, ModeBYOK, TriggerPlatformUnavailable), tier)
		}
	})
}

// FreeLaneStartupWarningTests.
func TestFreeLaneStartupWarning(t *testing.T) {
	t.Run("an enabled lane with no platform key warns", func(t *testing.T) {
		assert.NotEmpty(t, FreeLaneStartupWarning(true, false, false))
	})
	t.Run("one loaded leg is enough to stay quiet", func(t *testing.T) {
		assert.Empty(t, FreeLaneStartupWarning(true, true, false))
		assert.Empty(t, FreeLaneStartupWarning(true, false, true))
	})
	t.Run("a disabled lane never warns", func(t *testing.T) {
		assert.Empty(t, FreeLaneStartupWarning(false, false, false))
	})
}

// FreeLaneGateTests. The leg buckets are platform-wide, so every case clears them.
func TestFreeLaneGate(t *testing.T) {
	ctx, pool, _ := llmDB(t)
	bothLegs := []FreeLaneLeg{LegOpenRouterFree, LegNvidiaDirect}
	gate := func(t *testing.T, perUser, openRouter, nvidia int64) *FreeLaneGate {
		t.Helper()
		_, err := pool.Exec(ctx, `DELETE FROM workflow_spend_buckets WHERE scope_key LIKE 'freelane:leg:%'`)
		require.NoError(t, err)
		return NewFreeLaneGate(pool, FreeLaneLimits{PerUserDaily: perUser, PerLegDaily: map[FreeLaneLeg]int64{LegOpenRouterFree: openRouter, LegNvidiaDirect: nvidia}})
	}
	t.Run("per-user daily grace exhausts after the configured number of requests", func(t *testing.T) {
		g := gate(t, 3, 45, 900)
		tenant := uuid.New()
		for range 3 {
			assert.Equal(t, LegOpenRouterFree, g.Claim(ctx, tenant, bothLegs).Granted)
		}
		outcome := g.Claim(ctx, tenant, bothLegs)
		assert.Empty(t, outcome.Granted)
		assert.Positive(t, outcome.RetryAfterSeconds)
		assert.LessOrEqual(t, outcome.RetryAfterSeconds, 86400.0)
	})
	t.Run("a full leg falls over to the next leg rather than failing", func(t *testing.T) {
		g := gate(t, 10, 1, 900)
		tenant := uuid.New()
		assert.Equal(t, LegOpenRouterFree, g.Claim(ctx, tenant, bothLegs).Granted)
		assert.Equal(t, LegNvidiaDirect, g.Claim(ctx, tenant, bothLegs).Granted)
	})
	t.Run("both legs full is exhaustion", func(t *testing.T) {
		g := gate(t, 10, 1, 1)
		tenant := uuid.New()
		g.Claim(ctx, tenant, bothLegs)
		g.Claim(ctx, tenant, bothLegs)
		assert.Empty(t, g.Claim(ctx, tenant, bothLegs).Granted)
	})
	t.Run("tenants do not share the per-user grace", func(t *testing.T) {
		g := gate(t, 1, 45, 900)
		first, second := uuid.New(), uuid.New()
		assert.Equal(t, LegOpenRouterFree, g.Claim(ctx, first, bothLegs).Granted)
		assert.Empty(t, g.Claim(ctx, first, bothLegs).Granted)
		assert.Equal(t, LegOpenRouterFree, g.Claim(ctx, second, bothLegs).Granted)
	})
	t.Run("concurrent claims never exceed the ceiling", func(t *testing.T) {
		g := gate(t, 10, 1000, 900)
		tenant := uuid.New()
		var granted atomic.Int64
		var wg sync.WaitGroup
		for range 50 {
			wg.Go(func() {
				if g.Claim(ctx, tenant, bothLegs).Granted != "" {
					granted.Add(1)
				}
			})
		}
		wg.Wait()
		assert.Equal(t, int64(10), granted.Load())
	})
	t.Run("remaining reflects what has been spent", func(t *testing.T) {
		g := gate(t, 5, 45, 900)
		tenant := uuid.New()
		assert.Equal(t, int64(5), g.RemainingToday(ctx, tenant))
		g.Claim(ctx, tenant, bothLegs)
		g.Claim(ctx, tenant, bothLegs)
		assert.Equal(t, int64(3), g.RemainingToday(ctx, tenant))
	})
	t.Run("a zero limit never grants", func(t *testing.T) {
		assert.Empty(t, gate(t, 0, 45, 900).Claim(ctx, uuid.New(), bothLegs).Granted)
	})
	t.Run("scope keys use Swift's uppercase uuidString", func(t *testing.T) {
		g := gate(t, 5, 45, 900)
		tenant := uuid.New()
		g.Claim(ctx, tenant, bothLegs)
		var rows int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM workflow_spend_buckets WHERE scope_key LIKE '%' || $1 || '%'`, strings.ToUpper(tenant.String())).Scan(&rows))
		assert.Equal(t, 1, rows)
	})
}
