---
name: brand-legitimacy-investigation
description: |
  Systematic web-based investigation of a product or brand's legitimacy: multi-source verification, review aggregation, red flag detection, company background checks, and balanced recommendation synthesis.
version: 1.0.0
author: Hermes Agent
license: MIT
prerequisites:
  commands: [python3, curl]
  env_vars: []
---

# Brand Legitimacy Investigation

Investigate commercial products, brands, or services for legitimacy, quality, and scam risks using open-web research and multi-source evidence synthesis.

## When to Use

Use this skill when the user asks to vet a product/brand such as:
- "Is [brand/product] a scam?" / "Is [X] legitimate?"
- "Should I buy [product]?" / "Is [X] worth it?"
- "What do you know about [brand]?" (commercial context)
- "Reviews of [product]" — where a quality assessment is expected
- "Red flags for [product/brand]"

**Do NOT use** for:
- Academic/scientific literature reviews (use `research-paper-writing` or `arxiv`)
- News aggregation digests (use `rss-digest-generator`)
- Social media monitoring for brand mentions (use `x-social-monitor`)
- Competitor analysis or market research (traditional business research tools)
- Technical product comparisons (tech blogs, review sites)

## Investigation Phases

### Phase 1: Company & Founder Background
**Purpose:** Establish baseline legitimacy through business entity verification.

**Sources to query:**
- Company website (About Us, Press, Team pages)
- PRNewswire/BusinessWire for launch announcements
- SEC EDGAR (if publicly-traded parent company mentioned)
- LinkedIn company pages / founder profiles
- News articles (Google News, reputable outlets)
- Domain registration (WHOIS / ICANN lookup)

**Evidence markers (legitimate):**
- Real company name, EIN/registration visible
- Founder(s) with verifiable professional history
- Parent company is public or well-known
- Press coverage beyond self-published content
- Physical address / shipping address
- WHOIS dates consistent with company launch story

**Red flags:**
- Anonymous founders / pseudonyms
- Domain registered within last 6–12 months (unless known startup)
- No press coverage outside marketing channels
- Company claims contradicted by public records

---

### Phase 2: Product & Pricing Analysis
**Purpose:** Determine if product represents fair value vs marketing hype.

**Data to extract:**
- SKU names, pricing tiers, subscription terms
- Ingredient lists / tech specifications
- Dosages / concentrations / materials used
- Shipping costs, return policy, subscription cancellation terms

**Compare against:**
- Competitor offerings (similar products)
- Ingredient cost breakdown (if applicable)
- Market average for product category

**Evidence markers (reasonable):**
- Clear pricing with no hidden fees
- Ingredient/spec lists available up-front
- Money-back guarantee / return window stated
- Subscription model with transparent cancellation

**Red flags:**
- Hidden or auto-renew subscriptions without clear disclosure
- Pricing dramatically higher than alternatives with marginal differentiation
- Vague product claims without specifications

---

### Phase 3: Review Aggregation
**Purpose:** Extract unbiased user sentiment from multiple platforms.

**Primary review sources (in priority order):**
1. **Amazon** (if product sold there) — real customer reviews
2. **Trustpilot** — business review platform
3. **Sitejabber / Better Business Bureau (BBB)** — complaint tracking
4. **Reddit** — candid user discussions (product-specific subreddits, r/supplements, r/[category])
5. **YouTube** — unboxing/review videos (check comment sentiment)
6. **X/Twitter** — organic user mentions (non-promotional)

**For blocked/403 access:**
- Use jina.ai summarization bypass: `https://r.jina.ai/http://site.url`
- Use textise dot iitty / textise dot iitty variants
- Try `site:reddit.com [product]` via DuckDuckGo HTML search
- If all blocked → count as missing evidence, not negative evidence

**Record per source:**
- Rating (stars / score)
- Volume (approximate number of reviews)
- Common complaint themes
- Positive highlights

---

### Phase 4: Regulatory & Legal Compliance
**Purpose:** Check if product meets minimum legal standards (indirect legitimacy proxy).

**For supplements:**
- FDA disclaimer present on website (required by DSHEA)
- "Made in USA" claims verifiable (if claimed)
- No explicit disease-cure claims (illegal for supplements)

**For other products:**
- FTC disclosure of endorsements/affiliates
- Warranty/return terms compliant
- No suspicious "too good to be true" claims

**Red flags:**
- Promising to cure specific diseases (supplements) — FDA warning sign
- No Terms of Service / Privacy Policy
- Vague jurisdiction/non-US base when targeting US consumers

---

### Phase 5: Red Flag vs Green Signal Synthesis
**Purpose:** Weigh contradictory evidence and report uncertainty.

**Create a structured verdict table:**

| Category | Green lights ✅ | Red flags ⚠️ |
|----------|---------------|-------------|
| Company | Public parent, real CEO | Domain only 6mo old |
| Product | Transparent pricing, listed ingredients | Uses FB ads with extreme claims |
| Reviews | Amazon 4.5★ (500 reviews) | Trustpilot blocked, Sitejabber complaints |
| Legal | FDA disclaimer present | No return policy visible |

**Common red flag patterns:**
- New domain (<1 year) + blocked review sites + scam-warning indexed titles → high risk
- Celebrity endorsement but no behind-the-scenes coverage → careful
- Professional site + Amazon sales + mixed reviews → **usually legitimate but price-sensitive**
- All review platforms inaccessible → treat as unknown, recommend bypass (direct purchase only limited refund risk)

---

### Phase 6: Final Recommendation
Structure:
1. **Legitimacy Verdict** — scam / legit / uncertain / questionable
2. **Key Evidence** — bullet-summary of deciding factors
3. **Pros / Cons** — balanced view
4. **Caveats** — what could go wrong or is unknown
5. **Buy or Not?** — action-oriented advice with conditions

**Example conclusion patterns:**

**Legitimate ✓** — Company real, product delivers, reviews exist, but consider price/value.
**Questionable ⚠️** — Mixed signals; some legitimacy but serious concerns (billing, blocked reviews).
**Likely Scam ✗** — New domain + blocked reviews + scam-check warnings + no verifiable company.

## Common Pitfalls & Workarounds

| Pitfall | Why it happens | Fix |
|---------|----------------|-----|
| Trustpilot / Sitejabber blocked (403) | Anti-scraping / requires login | Use `r.jina.ai/http://URL` extraction or DuckDuckGo HTML search (`site:trustpilot.com [product]`) |
| Reddit thread requires JS client-side render | Search results snippet only | Use `r.jina.ai/http://reddit.com/r/...` to extract text |
| Domain age too new to judge | Brand is startup (<12 mo) | Cross-check parent company age and funding history instead |
| Only affiliate reviewers found | No organic user reviews | Search for "unboxing" + "review" + year on YouTube for independent footage |
| Conflicting review ratings | Amazon high, Trustpilot low | Report variance; each source has bias (Amazon may have incentivized reviews) |

## Output Format (Deliverable)

Deliver a **single markdown report** with these sections:

```markdown
# [Brand/Product] — Legitimacy Assessment

## 🏢 Company Background
[Founder, parent company, headquarters, launch date]

## 💰 Product & Pricing
[What it is, SKUs, price ranges, subscription model]

## ⭐ Review Summary
| Source | Rating | Notes |
|--------|--------|-------|
| Amazon | ~4.2★ | 500+ reviews |
| Trustpilot | 3.8★ | ~100 reviews |
| Sitejabber | 2.5★ | billing complaints |
| Reddit | Mixed | User experiences summarized |

## ✅ Green Signals
- [List of positive indicators]
- [Company signals, real shipping, professional site, etc.]

## ⚠️ Red Flags
- [List of concerns]
- [New domain, blocked reviews, scam warnings, suspicious billing]

## 📊 Verdict
[Legitimate / Questionable / Likely Scam] — reason in one sentence

## 📝 Recommendation
[Buy or not, with caveats about pricing/expected results]

## 🔍 Unknowns / Gaps
[What couldn't be verified due to access limitations]
```

## Related Skills

- `rss-digest-generator` — RSS feed aggregation for news digests (not investigation)
- `x-html-scrape` — Extract tweet data from X (single-source backup when API unavailable)
- `x-social-monitor` — Autonomous social media monitoring (tracking mentions over time)

## Notes

- This skill assumes **web-based open-source intelligence** (OSINT) only. Do NOT attempt to breach paywalls or terms-of-service; report when information is unavailable due to access barriers.
- Always include a **"Unknowns / Gaps"** section — transparency about missing evidence is critical for decision quality.
- For supplements specifically, FDA disclaimer presence is a **minimum legal compliance marker**, not a quality endorsement.
- A "legit" verdict ≠ "worth it." Many legitimate products are overpriced or provide minimal value; distinguish legitimacy from value.
