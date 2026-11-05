---
name: swift-http-client-sendable-fixes
description: Fix Sendable conformance and protocol errors in BaseHTTPClient-based modules
license: MIT
metadata:
  author: Hermes Agent
  version: "1.0"
---

## When to use

Multi-module iOS/macOS codebase using a generic `BaseHTTPClient<ErrorType>` where
`ErrorType: LocalizedError & Equatable & Sendable & HTTPClientError`.
Build fails with errors like:

- **"Main actor-isolated conformance of '<Client>.Error' to 'Equatable' cannot satisfy conformance requirement for a 'Sendable' type parameter 'ErrorType'"**
- **"Method does not override any method from its superclass"** (after moving Error enum)
- "Type 'Error' does not conform to protocol 'Sendable'"
- "Return type 'Error' of overridden method...must be the concrete type..."
- "Type 'HTTPClient<...>.Error' is not 'Sendable'"
- "Instance member 'X' cannot be used on type 'BaseHTTPClient'; did you mean to use 'self'?"

**Root cause:** nested `Error` enums inside `@MainActor` (or implicit-main-actor) classes inherit actor isolation,
making Equatable conformance evaluated on a main-actor isolated type, which fails `Sendable` requirements.

## Approach

### Phase 1 — Fix error enum isolation

1. **Scan all HTTP client subclasses** — identify every file inheriting `BaseHTTPClient`.
2. **For each client**, move the nested `Error` enum from the class/extension to top-level (file/module scope).
   - Place it before the client class definition.
3. **Annotate the top-level enum** with `@preconcurrency` on its declaration to relax Sendable checking:
   ```swift
   @preconcurrency
   enum ActivityError: LocalizedError, Equatable, @unchecked Sendable, HTTPClientError {
       case invalidStatusCode(Int)
       case decodingError(Error)
       case urlError(URLError)
       // ...
   }
   ```
4. **Update the client class**:
   - Change inheritance to `BaseHTTPClient<ActivityError>` (use the concrete top-level error type).
   - Add `typealias Error = ActivityError` inside the class to preserve existing API surface.
   - Remove the old extension block that contained the nested enum.
5. **Fix override methods** — ensure `makeInvalidStatusError` and any other overrides return `Error` (the concrete type):
   ```swift
   override func makeInvalidStatusError(_ code: Int) -> Error {
       .invalidStatusCode(code)
   }
   ```
6. **Update internal references** — replace all `Error.someCase` qualifiers with `ActivityError.someCase`.

### Phase 2 — Fix implicit self in BaseHTTPClient

In `BaseHTTPClient.swift`, prefix all instance property accesses inside closure bodies with `self.`:
- `logger.debug` / `logger.error` autoclosure arguments referencing `endpoint.path`, `method`, `attempt`, `maxRetries`.
- `reportError` closure bodies that capture any `self` property.
Every instance member used within a closure must be explicitly `self.` to satisfy strict concurrency.

### Phase 3 — Verify

Build all schemes cleanly:
```bash
xcodebuild build -project financeplan.xcodeproj -scheme "Norviqa TestFlight Dev" -destination 'platform=iOS Simulator,name=iPhone 17,OS=latest'
```

## Pattern

Reference implementation: `AuthHTTPClient.swift` already using the correct pattern.

**Top-level error enum:**
```swift
@preconcurrency
enum AuthError: LocalizedError, Equatable, @unchecked Sendable, HTTPClientError {
    case invalidCredentials
    case invalidStatus(Int)
    case decodingError(Error)
    case urlError(URLError)

    var errorDescription: String? {
        switch self {
        case .invalidCredentials: "Invalid credentials"
        case .invalidStatus(let code): "Invalid status code: \(code)"
        case .decodingError(let error): "Decoding error: \(error)"
        case .urlError(let error): "URL error: \(error)"
        }
    }

    var statusCode: Int? {
        if case .invalidStatus(let code) = self { return code }
        return nil
    }
}
```

**Client class:**
```swift
final class AuthHTTPClient: BaseHTTPClient<AuthError> {
    typealias Error = AuthError  // preserves existing API

    override func makeInvalidStatusError(_ code: Int) -> Error {
        .invalidStatus(code)
    }

    // ... rest of implementation
}
```

### Fixing BaseHTTPClient implicit self captures

Inside `BaseHTTPClient.swift`, any closure that references `self` properties must use explicit `self.`:

```swift
// BEFORE (strict concurrency error)
logger.debug(
    "Request to \(self.endpoint.path) failed",
    logger: self.logger,
    error: error,
    attempts: attempt,
    max: maxRetries
)

// AFTER (all properties explicitly self.)
logger.debug(
    "Request to \(self.endpoint.path) failed",
    logger: self.logger,
    error: error,
    attempts: self.attempt,
    max: self.maxRetries
)
```

Check also in `reportError` calls and any inline closures. This is required even though the closure captures `self` implicitly; Swift 6 strict concurrency wants explicit `self.` for each property access.

## Pitfalls

- `@preconcurrency` on the enum silences the Sendable/Equatable conformance warning that arises from actor isolation. Keep `@unchecked Sendable` as well (the enum contains `Error` associated values which are genuinely not thread-safe to share, but we only use these values locally).
- The `typealias Error = <Module>Error` is critical — existing call sites use `Client.Error` as a type. Removing it breaks compilation.
- `HTTPClientError` protocol requires `statusCode: Int?`. Most enums return `nil` for all cases except `.invalidStatus(code)`. Keep that pattern.
- Do NOT combine the session protocol with another module's protocol — Swift module privacy requires the protocol live in the same module as the client type that references it via associated type.
- After moving the enum, verify all `Error` references within the file now point to the top-level type, not the old nested one. Xcode refactor tools can help.
- If `makeInvalidStatusError` override still fails, check that the base class signature matches exactly: `override func makeInvalidStatusError(_ code: Int) -> Error`.

## Workflow across multiple modules

When the same pattern appears in many submodules, execute in this order:

1. **Read one correct reference** (`AuthHTTPClient.swift`) to confirm the target pattern.
2. **Read one broken client** (e.g., `ActivityHTTPClient.swift`) to identify differences.
3. **Patch the broken client** completely using the pattern.
4. **Build & inspect log** — verify those specific errors are gone. Capture remaining error lines.
5. **List all target modules** — glob for `*HTTPClient.swift` files (typically under `API/<Module>/`).
6. **Apply patches systematically** — either:
   - Use `patch` tool to apply V4A multi-file patches, or
   - Delegate to subagents for parallel editing of independent modules.
7. **Fix base class issues in parallel** — while child modules compile, address `BaseHTTPClient.swift` implicit-self errors.
8. **Rebuild clean** — final `xcodebuild` to confirm zero errors.
9. **Run tests** — validate runtime behavior unchanged.

## Affected modules in financeplan project

These modules were identified with nested `Error` enums requiring the same fix:
- Activity, Auth (already correct), Badges, Billing, Brokers, Crypto, Dashboard, Expenses, Goals, MarketData, News, Notifications, Stocks, UserProfile.

Standardize them all to the pattern shown in the 'Pattern' section.

## Verification

After applying to all clients:

- `swift build` reports zero Sendable or override signature errors.
- No duplicate `URLSessionProtocol` definitions remain.
- Each client module compiles cleanly in isolation.

## References

- Swift Concurrency: actor isolation and `Sendable`
- `BaseHTTPClient` generic constraints
- `HTTPClientError` protocol (requires `statusCode: Int?`)