---
name: swiftui-ui-testing-setup
description: End-to-end workflow for adding XCTest UI tests to a SwiftUI iOS project. Covers accessibility identifier injection, reusable component patching, language-resilient test helpers, and smoke-test authoring.
---

# SwiftUI UI Testing Setup

## Trigger
Adding UI tests to an iOS SwiftUI project, especially when existing views lack `accessibilityIdentifier` markers.

## Steps

### 1. Audit existing infrastructure
- Locate `*UITests.swift` target and read existing tests
- Note launch arguments pattern (auth tokens, skip flags, reset session)
- Identify how app state is seeded / mocked

### 2. Inspect product code for identifiers
- Search target views for `accessibilityIdentifier` or `accessibilityLabel`
- Note reusable components (`FormTextField`, `FormActionBar`, custom buttons) — these are high-leverage patching targets
- Check `HomeScreen` / `TabView` for tab item labels (often localized)

### 3. Patch reusable components first
If views use shared components that don't expose identifiers, patch the component to accept an optional `accessibilityIdentifier` parameter and forward it to the inner view.

Example: `FormTextField`, `FormActionBar`, `FormSheetHeader`.

This prevents editing every call site individually.

### 4. Add identifiers to product views
Add `.accessibilityIdentifier(...)` to:
- Tab bar root views (e.g. `"tab.expenses"`, `"tab.reports"`)
- Toolbar buttons (record spend, edit salary)
- Sheet trigger buttons
- Text fields inside sheets
- Save / action bar buttons
- Charts or scroll content in reports

### 5. Handle language sensitivity
Tab labels and buttons may be localized (e.g. English + Portuguese). Use a fallback chain in helpers:

```swift
func tapExpensesTab() {
  if app.tabBars.buttons["Expenses"].waitForExistence(timeout: 2) { tap(); return }
  if app.tabBars.buttons["Despesas"].waitForExistence(timeout: 2) { tap(); return }
  app.tabBars.buttons.element(boundBy: 2).tap() // last resort
}
```

### 6. Write helper extensions
Create `*UITestHelpers.swift` with:
- `launchAuthenticated*()` wrappers that set the same launch args as existing tests
- Tab tap helpers with language fallback
- `dismissKeyboardIfPresent()` that checks existence before tapping Done

### 7. Write focused smoke tests
One test per user journey. Structure:
- `setUpWithError`: `continueAfterFailure = false`
- Private helpers for repeated flows (set budget, add item, record expense)
- Unique `userID` per test to avoid data collision
- Handle empty-state vs populated-state UI differences (e.g. "Add First Item" vs "Add planned item")

### 8. Xcode synced-folder check
Before editing `project.pbxproj`, verify if project uses Xcode 16+ `PBXFileSystemSynchronizedRootGroup`:

```bash
grep -c "PBXFileSystemSynchronizedRootGroup" project.pbxproj
```

If > 0, new `.swift` files in synced directories compile without pbxproj edits.

### 9. Build verification
Run targeted build to confirm compilation:

```bash
xcodebuild -project App.xcodeproj -scheme App -destination 'platform=iOS Simulator,name=iPhone 17' -only-testing:AppUITests/SmokeTests test
```

## Pitfalls
- **Sheets**: SwiftUI `.sheet(item:)` may dismiss asynchronously. Tests should wait for the triggering button to reappear rather than asserting sheet non-existence immediately.
- **Keyboard overlap**: Text fields near bottom may be hidden by software keyboard. Always dismiss keyboard before tapping save.
- **Empty state buttons**: First-launch empty states often have different button labels/IDs than populated states. Check both.
- **Backend dependency**: UI tests that hit real backends need unique user IDs per test and a running dev server. Consider `-ui_test_mock_*` launch args if flakiness arises.
- **Tab `accessibilityIdentifier`**: Setting `.accessibilityIdentifier` on the screen view inside `.tabItem {}` works for tests to find the tab bar button indirectly via label fallback, but tab items themselves don't expose IDs easily.
