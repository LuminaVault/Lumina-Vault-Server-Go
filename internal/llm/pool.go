package llm

import "slices"

// PoolInput is AvailableModelPoolBuilder.Input.
type PoolInput struct {
	Mode                       CredentialMode
	Policy                     RoutingPolicy
	ProfileRoutes              []WireRoute
	AllowedProviders           []ProviderID
	BlockedProviders           []ProviderID
	CredentialedProviders      map[ProviderID]bool
	DeploymentEnabledProviders map[ProviderID]bool
	MinTier                    RouterModelTier
}

// BuildModelPool is AvailableModelPoolBuilder.build: the candidates Auto may
// choose from, scoped to what the user or deployment can actually call. Empty
// means no usable model under this policy.
func BuildModelPool(in PoolInput) []WireRoute {
	var candidates []WireRoute
	seen := map[string]bool{}
	add := func(route WireRoute) {
		if seen[route.ID()] || slices.Contains(in.BlockedProviders, route.Provider) {
			return
		}
		if len(in.AllowedProviders) > 0 && !slices.Contains(in.AllowedProviders, route.Provider) {
			return
		}
		seen[route.ID()] = true
		candidates = append(candidates, route)
	}

	usable := map[ProviderID]bool{}
	switch {
	case in.Mode != ModeBYOK && in.Policy == PolicyAutoSmart:
		// Managed Auto rides the shared gateway's system OpenRouter key, so no
		// registry credential is needed.
		usable[ManagedProvider] = true
	case in.Mode != ModeBYOK:
		// Deployment keys, plus any key the tenant also added.
		for id := range in.DeploymentEnabledProviders {
			usable[id] = true
		}
		for id := range in.CredentialedProviders {
			usable[id] = true
		}
	case in.Policy == PolicyAutoSmart:
		// BYOK Auto needs the tenant's own OpenRouter key. Callers downgrade the
		// policy first when it is absent.
		if in.CredentialedProviders[ManagedProvider] {
			usable[ManagedProvider] = true
		}
	default:
		for id := range in.CredentialedProviders {
			usable[id] = true
		}
	}

	// Profile routes first. BYOK keeps only routes with a key. Managed keeps them
	// all, since the gateway cascade covers a missing key, except under Auto.
	for _, route := range in.ProfileRoutes {
		if (in.Mode == ModeBYOK || in.Policy == PolicyAutoSmart) && !usable[route.Provider] {
			continue
		}
		add(route)
	}
	for _, e := range routerCatalogEntries {
		if usable[e.Provider] {
			inRate, outRate := e.InputPerMillionUsdMicros, e.OutputPerMillionUsdMicros
			add(WireRoute{e.Provider, e.Model, &inRate, &outRate})
		}
	}
	// The client-facing catalogue has a richer cheap tier. Swift walks a Set here.
	for _, id := range allProviderIDs {
		if usable[id] {
			for _, m := range ModelsFor(id) {
				add(WireRoute{Provider: id, Model: m.ID})
			}
		}
	}

	var tiered []WireRoute
	for _, route := range candidates {
		if RouteTier(route).rank() >= in.MinTier.rank() {
			tiered = append(tiered, route)
		}
	}
	if len(tiered) > 0 {
		return tiered
	}
	// Nothing clears the floor: the strongest available beats an empty pool.
	slices.SortStableFunc(candidates, func(a, b WireRoute) int { return RouteTier(b).rank() - RouteTier(a).rank() })
	return candidates
}

// RouteTier is AvailableModelPoolBuilder.tier(for:).
func RouteTier(route WireRoute) RouterModelTier {
	if e, ok := CatalogEntry(route.Provider, route.Model); ok {
		return e.Tier
	}
	return modelCatalogTier(route.Provider, route.Model)
}

// RoutingReason is AvailableModelPoolBuilder.reason. Clients show it.
func RoutingReason(policy RoutingPolicy, complexity RouterComplexity, task RouterTaskType, selected WireRoute, deferred bool) string {
	if deferred {
		return "Routing managed by your Hermes"
	}
	tier := string(RouteTier(selected))
	switch policy {
	case PolicyLocked:
		return "Locked to primary model"
	case PolicyFastCheap:
		return "Fast & Cheap · " + tier + " tier"
	case PolicyBalanced:
		return "Balanced · " + string(task) + " · " + tier
	case PolicyMaxQuality:
		return "Max Quality · " + tier + " tier"
	}
	switch complexity {
	case ComplexityLow:
		return "Auto · simple " + string(task) + " → " + tier
	case ComplexityHigh:
		return "Auto · complex " + string(task) + " → " + tier
	}
	return "Auto · moderate " + string(task) + " → " + tier
}
