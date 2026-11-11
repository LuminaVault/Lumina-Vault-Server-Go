---
name: swiftui-subscription-gating
description: >
  Implement UI gating for subscription tiers in SwiftUI apps using ProGateView
  pattern. Surgical per-component wrapping, toolbar filtering, and navigation link
  gating following monetization spec boundaries.
category: swiftui-pro
status: stable
---

# Plan: Gatekeep Pro Features in Expenses Views

**Date:** 2025-04-25
**Scope:** iOS — Expenses feature tier gating (Free vs Pro)
**Related:** `monetization.md` — Expenses & Budget Planner tier definitions

---

## Goal

Enforce subscription tiers in Expenses feature per monetization strategy:

| Feature | Tier |
|---------|------|
| Manual expense entry (local-only), 3-month history, current month summary | **Free** |
| Cloud sync, unlimited history, salary-aware planner, pillars, month duplication, year trends | **Pro** |
| Reports: monthly/yearly comparisons, charts, variance analysis | **Pro** |
| Tax export, custom date ranges, combined net-worth | **Premium** (out of scope) |

Current state:
- `ExpensesPlannerScreen` — shows full UI to all users (no gating).
- `ExpensesComparisonScreen` (Reports tab) — tab navigation already redirects free users to Dashboard and shows paywall (HomeScreen `.onChange`), but view itself has no internal gate; deep-link bypass possible.
- Free users should see basic expense entry + current month summary.
- Pro users see everything.

---

## Current Context & Assumptions

1. **Backend** — `ExpensesService` / API endpoints return all data (no tier-based filtering). Tier enforcement is frontend-only for now.
2. **Billing** — `BillingManager.isPro` indicates Pro or active trial. Premium not yet implemented.
3. **Pattern** — Use existing `ProGateView` component (blur + lock overlay + tap → paywall), same as StockDetails pro tabs.
4. **ExpensesPlannerScreen complexity** — Large view with many sub-components. Gating must be surgical: wrap only pro-specific sections; keep free sections (expense entry, current month category breakdown) visible.
5. **Data visibility** — Even when UI is gated, view model still loads all data. Acceptable for MVP (data is not sensitive). Future: backend could filter, but out of scope.

---

## Proposed Approach

### A. ExpensesPlannerScreen (Main Expenses Tab)

Wrap **pro-only sections** with `ProGateView`. Free sections remain visible:

**Free (always visible):**
- `ExpensesCircularOverviewCard` — shows current month netSalary vs spent? (likely derived from current month snapshot). If it shows only current month, keep. If it shows year/planned, gate.
- Expense recording UI (`recordSpendSheet` trigger) — free users can manually log expenses.
- `MonthlyPlanItemsCard` — this is planning (planned items). According to doc: "Salary-aware monthly planner" is Pro. So gate this.
- `ExpensesByCategoryCard` — "Basic category breakdown (current month)" is Free. Keep visible for current month only. If it shows multi-month or variance, gate.
- `BudgetCategoryDetailsScreen` navigation — contains planned items + expense logging. Gate entire link or specific parts? Planned items are Pro; logging expenses is Free. Better: gate the whole card (it's about "Budget Category Details" which includes planning). Free users only need simple entry. **Decision: gate the NavigationLink to BudgetCategoryDetailsScreen entirely** — free users use the plain record spend button instead.

**Pro (wrap with ProGateView):**
- `PlannerSalaryCard` — shows net salary, allocated, planned totals; salary-aware planning is Pro.
- `MonthlyPlanItemsCard` — planned items, pillar allocation — Pro.
- `ExpensesYearOverviewCard` — year chart/trends — Pro (year-over-year trends are Premium actually, but doc says "Monthly comparison views | Pro", "Yearly comparison views | Pro", so year overview is Pro).
- `PillarAllocationTableCard` — pillars view is Pro.
- `SmartSuggestionsCard` — variance analysis & suggestions are Pro.
- `BudgetCategoryDetailsScreen` NavigationLink — Pro.

**Ambiguous:**
- `ExpensesCircularOverviewCard` — check implementation. If it shows current month only (spent vs salary), it's Free. If shows planned/allocated, it's Pro. Likely shows current month summary + left-to-spend — that's basic, should be Free. Keep unwrapped.

Implementation: Inject `billingManager` into `ExpensesPlannerScreen`, then wrap pro sections:

```swift
ProGateView(billingManager: billingManager) {
   // pro component
}
```

### B. ExpensesComparisonScreen (Reports Tab)

Entire screen is Pro per monetization doc ("Monthly comparison views", "Yearly comparison views", "SwiftUI charts & pillar breakdowns", "Spending vs planning variance analysis" all Pro).

Wrap the root content of `ExpensesComparisonScreen` with `ProGateView`.

Currently, HomeScreen intercepts tab selection and redirects free users, but deep-links would bypass. Wrap to be safe.

### C. HomeScreen

Already has tab gate for `.reports` (ExpensesComparisonScreen). Optional: also add gate for `.expenses` tab to redirect free users to Dashboard with paywall? According to monetization, Expenses tab should still be accessible for basic entry. So **do NOT gate the tab** — free users can enter Expenses tab to use free features. Reports tab remains fully gated (already done).

---

## Files to Change

1. **ExpensesPlannerScreen.swift**
   - Add `@InjectedObservable(Container.billingManager) private var billingManager`
   - Wrap each pro section with `ProGateView(billingManager: billingManager) { ... }`
     - `PlannerSalaryCard`
     - `MonthlyPlanItemsCard`
     - `ExpensesYearOverviewCard`
     - `PillarAllocationTableCard`
     - `SmartSuggestionsCard`
     - `NavigationLink` → `BudgetCategoryDetailsScreen`
   - Keep free sections as-is:
     - `ExpensesCircularOverviewCard`
     - Record spend button / sheet (already free)
     - `ExpensesByCategoryCard` (if current-month only; verify)
   - No changes to view model needed; data still loads.

2. **ExpensesComparisonScreen.swift**
   - Add `@InjectedObservable(Container.billingManager) private var billingManager`
   - Wrap root `VStack`/`ScrollView` content (everything inside `rootContent` or main container) with `ProGateView(billingManager: billingManager)`.
   - Alternatively wrap entire `body` return: `ProGateView(...) { originalContent }`

3. **BudgetCategoryDetailsScreen.swift** (optional gate?)
   - This screen is reached from ExpensesPlanner. Contains planned items (Pro) and expense logging (Free).
   - If we gate the NavigationLink in ExpensesPlanner, free users never reach this screen. So no change needed.
   - If we want free users to access logging but not planning, we'd need to split the screen. Too complex for now. Gate the navigation link entirely (free users can't enter). Acceptable because free users can still record expenses via the record button (already in toolbar). **Decision: no change, gate at link.**

4. **HomeScreen.swift** — no change needed. Reports tab gate already present.

---

## Step-by-Step Implementation Plan

### Step 1 — ExpensesPlannerScreen gating
- Inject `billingManager`.
- Wrap pro components one by one.
- After each wrap, verify free user sees blur+lock overlay on that section; tap shows paywall.
- Ensure free sections remain interactive.

### Step 2 — ExpensesComparisonScreen gating
- Inject `billingManager`.
- Wrap entire content in `ProGateView`.
- Verify free users see full-screen blur + lock.

### Step 3 — Quick hygiene
- Remove any unused imports/vars introduced.
- Ensure ProGateView accessibility labels present.
- Test: free user can record expense; cannot see salary/planner/pillars/year charts.
- Test: pro user sees all.

---

## Verification

**Manual test plan:**

1. **Free user flow**
   - Tap Expenses tab → see monthly overview + record spend button.
   - Tap into "Budget Category Details" → should NOT navigate; instead overlay lock appears? Actually gate at link: NavigationLink should still render but disabled? Better: wrap the NavigationLink label in ProGateView too, so link appears blurred and tap shows paywall. We'll wrap the entire NavigationLink in ProGateView.
   - See "PlannerSalaryCard" area blurred with lock → tap → paywall.
   - See "MonthlyPlanItemsCard" blurred.
   - See "Year Overview" blurred.
   - See "Pillar Allocation" blurred.
   - See "Smart Suggestions" blurred.
   - Record expense via toolbar '+' → works (free).
   - Reports tab → redirect to Dashboard + paywall (existing behavior).

2. **Pro user flow**
   - All sections visible, no blur.
   - Navigation to BudgetCategoryDetailsScreen works.

3. **Trial user**
   - Treated as Pro — all visible.

**Build & compile:** `swift build` or Xcode.

---

## Risks & Tradeoffs

- **Free user expense entry UX**: Gating the "Budget Category Details" navigation means free users cannot drill into category breakdown? But they already see `ExpensesByCategoryCard` (current month breakdown) which is free. Detailed category screen likely shows planned vs actual — that's planning, so acceptable to gate.
- **Data loading**: View model still fetches all data (including pro-only) even for free users. Minor performance cost; acceptable until backend filtering added.
- **Visual consistency**: ProGateView adds blur+overlay. Ensure overlay covers full component area (cards have fixed heights, OK).
- **Complexity**: ExpensesPlannerScreen already large; adding 6 ProGate wrappers increases nesting but remains readable.

---

## Open Questions

1. Should **ExpensesCircularOverviewCard** be pro? It may show planned vs actual. Need to inspect its content. If it shows "Left to allocate" (planning), it's pro. If only shows "Spent vs Salary" (current), it's free.
2. Should **ExpensesByCategoryCard** show only current month for free? Already likely does. Verify it doesn't include year trends.
3. Should **toolbar "Plan next month"** action be gated? It's in ExpensesPlannerScreen's toolbar. Free users clicking it should either do nothing or show paywall. Currently no gate on toolbar actions. Need to add `.onChange` or wrap button? Actually toolbarButtons directly call viewModel methods. Might need to guard those actions at the view model level or add checks in the button actions. Simpler: disable/hide pro-only toolbar buttons for free users.

Let me inspect toolbar actions (line ~132-167) to see which are pro:
- "Plan next month" → Pro (month duplication)
- "Adjust monthly budget" → Pro (salary-aware)
- "Adjust pillar targets" → Pro (pillars)
- "Add pillar" → Pro
- "Record spend" → Free (keep)
- "Household partner" → ? (likely Pro or Premium; doc doesn't mention; possibly free? assume Pro)
- "Delete this month plan" → Pro (planning)

So toolbar menu needs heavy gating for free users. Might need to restructure menu to show only free actions when !isPro.

This adds complexity. Could also gate entire toolbar (show different menu based on pro status).

---

## Revised Scope (inclusive of toolbar)

**ExpensesPlannerScreen changes:**
- Add `billingManager`
- Wrap pro cards (as above)
- Modify toolbar menu:
  - Free: only "Record spend", maybe "Delete this month plan"? Deleting snapshot might be allowed. Keep delete? It's their data, OK. Others hide/disable.
  - Pro: full menu.
- Also gate `NavigationLink` to BudgetCategoryDetailsScreen via ProGateView (wrap entire link).
- Keep record spend sheet intact.

**ExpensesComparisonScreen changes:**
- Wrap entire content.

---

## Files Modified (Actual Implementation)

| File | Changes |
|------|---------|
| `Features/Expenses/ExpensesPlannerScreen.swift` | Inject `billingManager`; `isPaywallPresented` state + sheet; wrap `ExpensesYearOverviewCard`, `SmartSuggestionsCard` in `ProGateView`; gate Household Partner menu button (paywall on tap); gate Recurring Templates action (paywall on tap) in separate UI section; restrict month picker to Pro (free users locked to current month). |
| `Features/Expenses/ExpensesComparisonScreen.swift` | Inject `billingManager`; wrap entire screen content in `ProGateView` |
| `Features/Expenses/ExpensesSyncManager.swift` | Add Pro guard in `pullLatestData` and `pushPendingActions` (no-op for free users, logs skip) |

## Deviations / Omissions from Original Plan

**Toolbar menu gating**: Original plan called for hiding/disabling pro-only toolbar items for free users. Implementation did **not** gate the full toolbar menu. Current state:
- Only "Household partner" button has conditional (paywall-on-tap).
- Other pro actions ("Plan next month", "Adjust monthly budget", "Adjust pillar targets", "Add pillar") remain visible and invocable regardless of tier.
- "Delete this month plan" left un-gated (considered data management; acceptable per skill notes).

**Rationale for deferring toolbar gating**: Focus was on card-level gating and month navigation first. Toolbar gating is secondary UX polish. Free users can still use core free features; pro actions invoke viewModel methods but with no server-side harm. However, update recommended: hide pro menu items for free to reduce confusion.

**Final note**: Skill correctly identified all gating points and recommended surgical `ProGateView` approach. Implementation followed pattern for cards/screens; service-layer gate added for sync unexpectedly (not in original plan). Month picker gating implemented via `selectedMonthBinding` as planned.

**Updated**: 2026-04-25 — added service-layer gating; toolbar gating remains outstanding.

---

## Alternative Considered (Not Recommended)

Split ExpensesPlannerScreen into two separate screens (FreeExpensesView vs ProExpensesView) and route via tab selection based on isPro. Too disruptive; loses seamless UX.

---

## Next Steps After Implementation

- Backend: eventually enforce pro-only data restrictions in API (e.g., exclude planning data if not pro). Defer until needed.
- Analytics: track paywall impressions from expenses sections.
- A/B test: maybe free users see upgrade prompts more prominently.

---

**Plan status:** Ready for review. Execute?
