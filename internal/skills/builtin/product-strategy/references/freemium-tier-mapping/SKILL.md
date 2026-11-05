---
name: freemium-tier-mapping
description: Map product features to subscription tiers (Free/Pro/Premium) and update monetization documentation based on roadmap and strategic principles.
trigger: When asked to assign features to subscription tiers, update a monetization strategy, or map an MVP roadmap to a freemium model.
---

# Freemium Tier Mapping

## Goal
Take a product's MVP roadmap or feature list and produce a clear, defensible feature-to-tier mapping that maximizes conversion while keeping the free tier useful. Update the monetization document with structured tables and rationale.

## Steps

1. **Read source documents**
   - Product roadmap / MVP feature list
   - Existing monetization strategy (if any)
   - Competitor context or README for market positioning

2. **Establish tier principles**
   Define the philosophy before mapping features:
   - **Free**: Table stakes, habit-building, viral/organic acquisition. Never gate row counts, manual work, or CSV import.
   - **Pro**: Automation, time savings, cloud sync, data costs. The "easy yes" tier.
   - **Premium**: Tax/reporting pain killers, advanced analytics, power-user scale. Justify with direct cost savings (CPA hour, etc.).

3. **Map every feature by domain**
   Group features by product domain (e.g., Portfolio, Research, Expenses, Reports, Imports).
   For each feature assign a tier and write a one-sentence rationale.

   Key rules:
   - Manual work / friction stays **Free**
   - Data provider costs justify **Pro**
   - Tax/export/accountant features go to **Premium**
   - Share/export that drives acquisition stays **Free** or **Pro** (PNG/CSV)

4. **Update the monetization document**
   Insert the mapping as a new section before tier definitions.
   Update the tier bullet lists to reflect the newly mapped features.
   Ensure the document is internally consistent (no feature listed in two tiers).

5. **Add pricing rationale**
   Keep Pro at ~$5.99/mo (easy yes) and Premium at ~$11.99/mo (cheaper than one CPA hour).
   Explain why the price is lower than consumer entertainment subscriptions.

## Pitfalls

- **Do not gate row counts** (holdings, watchlist symbols, notes). Free competitors offer unlimited.
- **Do not charge for manual CSV import**. Users still do the work; it's friction, not value.
- **Avoid gating basic P&L** or current prices. These are table stakes.
- **Don't put tax features in Pro**. Tax is a premium pain killer; keep it as an upsell anchor.
- **Keep the free tier genuinely useful**. A crippled free tier kills word-of-mouth.

## Verification

- Check that every MVP roadmap feature appears in the mapping.
- Check that no feature is assigned to two tiers.
- Check that the tier summaries at the bottom of the doc include all mapped features.
- Confirm the rationale for broker auto-sync, tax tools, and cloud sync is explicitly stated.

## Example Output Structure

```
## MVP Feature Monetization Map

### Domain Name
| Feature | Tier | Rationale |
|---------|------|-----------|
| ...     | ...  | ...       |

## Tier Definitions
### Free / Pro / Premium
[Updated bullet lists]
```
