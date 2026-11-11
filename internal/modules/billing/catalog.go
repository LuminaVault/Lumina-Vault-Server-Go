package billing

import (
	"log/slog"
	"lumina/internal/swiftstr"
	"strings"
)

// Product is SubscriptionCatalog.Product.
type Product struct {
	ID       string
	Tier     string
	PriceUSD string
	Period   string // monthly, yearly
}

// Products is SubscriptionCatalog.products: Pro $14.99/mo, $149.99/yr;
// Ultimate $29.99/mo, $299.99/yr.
var Products = []Product{
	{ID: "pro_monthly_14_99", Tier: "pro", PriceUSD: "14.99", Period: "monthly"},
	{ID: "pro_yearly_149_99", Tier: "pro", PriceUSD: "149.99", Period: "yearly"},
	{ID: "ultimate_monthly_29_99", Tier: "ultimate", PriceUSD: "29.99", Period: "monthly"},
	{ID: "ultimate_yearly_299_99", Tier: "ultimate", PriceUSD: "299.99", Period: "yearly"},
}

// TierForProduct is SubscriptionCatalog.tier(forProductID:): an exact,
// case-sensitive match, or false for a product we don't sell.
func TierForProduct(id string) (string, bool) {
	for _, p := range Products {
		if p.ID == id {
			return p.Tier, true
		}
	}
	return "", false
}

// TierOverrides is TierOverrideAllowlist: lowercased email → "pro" or "ultimate".
type TierOverrides map[string]string

// ParseTierOverrides is TierOverrideAllowlist(parsing:): comma-separated
// `email=tier` entries; a bare email grants ultimate; an unknown tier or none
// is dropped with a warning (an allowlist grants, it never revokes).
func ParseTierOverrides(raw string, log *slog.Logger) TierOverrides {
	out := TierOverrides{}
	for _, entry := range strings.Split(raw, ",") {
		trimmed := swiftstr.TrimWhitespacesAndNewlines(entry)
		if trimmed == "" {
			continue
		}
		email, tierRaw, hasTier := strings.Cut(trimmed, "=")
		email = strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(email))
		if email == "" {
			if log != nil {
				log.Warn("tier override allowlist: entry without an email skipped", "entry", trimmed)
			}
			continue
		}
		tier := "ultimate"
		if hasTier {
			tier = strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(tierRaw))
		}
		if tier != "pro" && tier != "ultimate" {
			if log != nil {
				log.Warn("tier override allowlist: entry skipped, tier must be pro or ultimate", "email", email, "tier", tier)
			}
			continue
		}
		out[email] = tier
	}
	return out
}

// Override is override(forEmail:).
func (t TierOverrides) Override(email string) (string, bool) {
	v, ok := t[strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(email))]
	return v, ok
}
