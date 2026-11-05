package llm

// CatalogCost is RoutedLLMTransport.catalogCost: micro-USD for a call. The rate
// quoted on the decision's route wins, then the catalogue. A model in neither
// costs 0, because a guessed price is worse than no row in the ledger.
func CatalogCost(provider ProviderID, model string, tokensIn, tokensOut int, cerberus *CerberusMetadata) int64 {
	var inputRate, outputRate int64
	if catalog, ok := catalogPricing(provider, model); ok {
		inputRate, outputRate = catalog.InputPerMillionUsdMicros, catalog.OutputPerMillionUsdMicros
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
