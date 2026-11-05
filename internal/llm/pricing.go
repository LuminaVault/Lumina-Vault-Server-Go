package llm

type modelRate struct {
	provider ProviderID
	model    string
	input    int64 // micro-USD per million input tokens
	output   int64 // micro-USD per million output tokens
}

// modelRates is the price half of RouterModelCatalog: entries in order, then
// pricingOnlyEntries. Routing estimates, reviewed and shipped in code.
var modelRates = []modelRate{
	{"anthropic", "claude-3-5-haiku-20241022", 800_000, 4_000_000},
	{"gemini", "gemini-2.5-flash", 150_000, 600_000},
	{"gemini", "gemini-2.5-flash-lite", 75_000, 300_000},
	{"openai", "gpt-4o-mini", 150_000, 600_000},
	{"openRouter", "deepseek/deepseek-v4-flash", 90_000, 180_000},
	{"xai", "grok-3-mini", 300_000, 500_000},
	{"nvidia", "nvidia/nemotron-3-nano-30b-a3b", 50_000, 200_000},
	{"openRouter", "qwen/qwen-2.5-72b-instruct", 350_000, 400_000},
	{"anthropic", "claude-sonnet-4-6", 3_000_000, 15_000_000},
	{"openai", "gpt-4o", 2_500_000, 10_000_000},
	{"gemini", "gemini-2.5-pro", 1_250_000, 10_000_000},
	{"xai", "grok-4", 3_000_000, 15_000_000},
	{"nvidia", "meta/llama-3.3-70b-instruct", 600_000, 600_000},
	{"nvidia", "nvidia/nemotron-3-super-120b-a12b", 85_000, 400_000},
	{"openRouter", "anthropic/claude-sonnet-4.6", 3_000_000, 15_000_000},
	{"openRouter", "x-ai/grok-4.5", 2_000_000, 6_000_000},
	{"openRouter", "openai/gpt-5", 1_250_000, 10_000_000},
	{"openRouter", "anthropic/claude-opus-4.1", 15_000_000, 75_000_000},
	{"openai", "gpt-5", 1_250_000, 10_000_000},
	{"anthropic", "claude-opus-4-1", 15_000_000, 75_000_000},
	{"openai", "o3", 10_000_000, 40_000_000},
	{"nvidia", "nvidia/nemotron-3-ultra-550b-a55b", 600_000, 3_600_000},
	{"anthropic", "claude-haiku-5-5", 100_000, 500_000},
}

func catalogPricing(provider ProviderID, model string) (modelRate, bool) {
	for _, r := range modelRates {
		if r.provider == provider && r.model == model {
			return r, true
		}
	}
	return modelRate{}, false
}

// CatalogCost is RoutedLLMTransport.catalogCost: micro-USD for a call. The rate
// quoted on the decision's route wins, then the catalogue. A model in neither
// costs 0, because a guessed price is worse than no row in the ledger.
func CatalogCost(provider ProviderID, model string, tokensIn, tokensOut int, cerberus *CerberusMetadata) int64 {
	var inputRate, outputRate int64
	catalog, hasCatalog := catalogPricing(provider, model)
	if hasCatalog {
		inputRate, outputRate = catalog.input, catalog.output
	}
	if cerberus != nil {
		for _, route := range cerberus.Routes {
			if route.Provider != provider || route.Model != model {
				continue
			}
			if route.InputPerMillionUsdMicros != nil {
				inputRate = *route.InputPerMillionUsdMicros
			}
			if route.OutputPerMillionUsdMicros != nil {
				outputRate = *route.OutputPerMillionUsdMicros
			}
			break
		}
	}
	return int64(max(0, tokensIn))*inputRate/1_000_000 + int64(max(0, tokensOut))*outputRate/1_000_000
}

// LedgerCharge is RoutedLLMTransport.ledgerCharge: what to write to cost_ledger
// for a completed call. Nothing for BYOK (the tenant pays the provider), for a
// provider with no ProviderID, or for a model with no known rate.
func LedgerCharge(mode CredentialMode, provider ProviderKind, model string, tokensIn, tokensOut int, cerberus *CerberusMetadata) (ProviderID, int64, bool) {
	if mode == ModeBYOK {
		return "", 0, false
	}
	id, ok := provider.ToShared()
	if !ok {
		return "", 0, false
	}
	usd := CatalogCost(id, model, tokensIn, tokensOut, cerberus)
	if usd <= 0 {
		return "", 0, false
	}
	return id, usd, true
}
