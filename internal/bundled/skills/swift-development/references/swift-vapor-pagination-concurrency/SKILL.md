---
name: swift-vapor-pagination-concurrency
description: >
  Cursor-based pagination across Swift/Vapor backend + iOS SwiftUI client,
  plus Swift protocol default-argument workarounds, Vapor middleware init fixes,
  IdempotencyMiddleware Redis handling, Xcode SPM cache recovery, and SwiftUI
  async closure bridging patterns.
---

## When to use

Use this skill when:
- Implementing cursor-based pagination across a Swift/Vapor backend + iOS SwiftUI client stack
- Fixing Swift protocol errors ("Default argument not permitted", "Immutable value never mutated")
- Resolving Vapor middleware init failures (`ResponseCompressionMiddleware(override:)`)
- Recovering from Xcode SPM cache corruption (constant extraction errors, PIF invalidated)
- Bridging async service calls into SwiftUI synchronous closure contexts
- Setting up idempotency middleware with Redis in Vapor

## Prerequisites

- Vapor 4 backend with Swift 5.9+
- iOS client using SwiftData/SwiftUI
- Redis available for idempotency/rate-limit
- PostgreSQL for persistent storage

## Approach

### 1 — Backend: Cursor-based pagination pattern

**Goal**: Replace offset pagination with keyset pagination using ISO8601 `createdAt` cursor.

**Steps**:
1. In `*Controller.swift`:
   - Add `cursor: String?` query param to `*ListQuery` struct
   - Parse cursor to `Date` using `ISO8601DateFormatter`
   - Query with `where` clause: `createdAt < cursorDate` for descending order
   - After fetching, check if more pages exist: `if let last = items.last, items.count == limit`
   - If more exists, set `nextCursor = last.createdAt` (ISO8601 string)
   - Return via response header: `response.headers.add(name: "X-Next-Cursor", value: nextCursor)`

2. In `*Service.swift`:
   - Change return type from `[T]` to `(items: [T], nextCursor: String?)`
   - Destructure repository result and return tuple

3. In routes (`routes.swift`):
   - Ensure route uses `protected` group if auth required (no change needed for pagination)

**Key files modified** (examples):
- `Stocks/StockController.swift`
- `Expenses/ExpensesController.swift`
- `News/NewsController.swift`

**Gotchas**:
- `createdAt` must be non-optional in response DTO; unwrap before encoding
- Date parsing: `ISO8601DateFormatter()` does not throw — remove `try` from guard/let blocks
- Header name: `X-Next-Cursor` (not `X-Next-Cursor-Value` or similar)

### 2 — iOS: Protocol default arguments → overload extensions

**Goal**: Remove default argument values from protocol method signatures (Swift forbids defaults in protocols).

**Steps**:
1. In `*Service.swift` protocol:
   - Remove all default parameter values from method declaration
   - Keep all parameters as plain optional/required

2. Add protocol extension:
```swift
extension ExpensesServicing {
    func getExpenses() async throws -> ([Expense], String?) {
        try await getExpenses(from: nil, to: nil, cursor: nil, limit: nil)
    }
    func getExpenses(from: String?, to: String?) async throws -> ([Expense], String?) {
        try await getExpenses(from: from, to: to, cursor: nil, limit: nil)
    }
}
```

3. In implementation (`*ServiceImpl.swift`):
   - Match protocol signature exactly (no defaults)
   - Forward all params to HTTP client

4. Update call sites (ViewModels, SyncManagers):
   - Adapt to new return type `(items, nextCursor)`
   - Store `nextCursor` as `String?`; append on `loadMoreIfAvailable()`

**Files**:
- `Features/Stocks/StockService.swift`
- `Features/Expenses/ExpensesService.swift`
- `Features/News/NewsService.swift` (if exists)

### 3 — iOS: HTTP client header access + return type change

**Goal**: Expose response headers (for pagination cursor) from HTTP client.

**Steps**:
1. In `*HTTPClient.swift`:
   - Add helper:
```swift
private func callWithHeaders(_ method: HTTPMethod, path: String, body: Encodable? = nil) async throws -> (data: Data, response: HTTPURLResponse) {
    var request = URLRequest(url: baseURL.appendingPathComponent(normalizedPath))
    request.httpMethod = method.rawValue
    request.setValue("application/json", forHTTPHeaderField: "Content-Type")
    if let body = body {
        request.httpBody = try JSONEncoder().encode(body)
    }
    let (data, urlResponse) = try await session.data(for: request)
    guard let httpResponse = urlResponse as? HTTPURLResponse else {
        throw MyError.invalidResponse
    }
    return (data, httpResponse)
}
```
   - Change `perform()` to return `(Data, HTTPURLResponse)` instead of just `Data`
   - Update `call()` to use `callWithHeaders` and discard headers

2. In `get*()` methods:
   - Call `let (data, response) = try await callWithHeaders(...)`
   - Extract `X-Next-Cursor` from `response.allHeaderFields` or `response.value(forHTTPHeaderField:)`
   - Return tuple `(items, nextCursor)`

**Access control fix**: Replace all file-scoped `private` declarations with `fileprivate`. Top-level `private` at file scope is illegal in Swift.

### 4 — Backend middleware initializer changes (Vapor breaking change)

**Problem**: `ResponseCompressionMiddleware()` fails to compile after Vapor update.

**Fix**:
```swift
// Old (fails)
app.middleware.use(ResponseCompressionMiddleware())

// New
app.middleware.use(ResponseCompressionMiddleware(override: .useDefault))
```

Also ensure server config set:
```swift
app.http.server.configuration.responseCompression = .enabled(initialByteBufferCapacity: 1024)
```

**Files**: `configure.swift`

### 5 — IdempotencyMiddleware Redis key + response reconstruction

**Problem**: `RedisKey.string` not available; `Response.Body.init(string:)` signature changed.

**Fix**:
- Use `RedisKey("\(prefix):\(key)")` directly; pass `RedisKey` to redis get/set
- Rebuild response:
```swift
var response = Response(status: HTTPStatus(statusCode: status))
response.headers = HTTPHeaders(headers)
response.body = Response.Body(string: body)
```
- Status code: convert `UInt` → `Int` (`Int(resp.status.code)`)

**Files**: `Shared/IdempotencyMiddleware.swift`

### 6 — Xcode SPM cache corruption recovery

**Symptoms**:
- `error: accessing build database ... disk I/O error`
- `Couldn’t check out revision ... fatal: unable to read tree`
- `PIF object has been invalidated`

**Nuclear clean**:
```bash
# Remove entire DerivedData for project
rm -rf ~/Library/Developer/Xcode/DerivedData/<projectname>-*
# Remove SPM checkouts/repositories
rm -rf ~/Library/Developer/XideDerivedData/<hash>/SourcePackages/{checkouts,repositories,artifacts}
# Remove project's Package.resolved
rm -f <project>/Package.resolved
# Clean project build folder
xcodebuild clean -project <proj>.xcodeproj -scheme <scheme>
```

Then resolve:
```bash
xcodebuild -resolvePackageDependencies -project <proj>.xcodeproj -scheme <scheme>
xcodebuild -project <proj>.xcodeproj -scheme <scheme> build
```

### 7 — SwiftUI: Async closure in synchronous context

**Problem**: `onLoadMore: { await viewModel.loadMoreIfAvailable() }` fails: "invalid conversion from 'async' function to synchronous function"

**Fix**:
```swift
onLoadMore: { Task { await viewModel.loadMoreIfAvailable() } }
```

**Files**: `PortfolioScreen.swift` (and any other SwiftUI closure passing async fn)

### 8 — Database provisioning for Vapor dev

**Problem**: Backend logs `role "stockplan_user" does not exist`.

**Fix**:
```bash
psql -h localhost -p 5432 postgres
CREATE USER stockplan_user WITH PASSWORD 'stockppassword';
CREATE DATABASE stockplan_dev OWNER stockplan_user;
GRANT ALL PRIVILEGES ON DATABASE stockplan_dev TO stockplan_user;
```

Then run migrations:
```bash
swift run StockPlanBackend migrate --yes
```

### 9 — Rate limiting + Idempotency verification

**Test idempotency**:
```python
import requests
key = "test-key"
r1 = requests.post("http://localhost:8080/v1/auth/register", json=payload, headers={"Idempotency-Key": key})
r2 = requests.post("http://localhost:8080/v1/auth/register", json=payload, headers={"Idempotency-Key": key})
assert r1.json()["userId"] == r2.json()["userId"]
```

**Test rate limit**:
```python
for i in range(6):
    r = requests.post(url, json=payload, headers={"Idempotency-Key": f"key-{i}"})
    print(r.status_code)  # 5th or 6th should be 429
```

## Pitfalls

- `private` at top-level (file scope) → compile error. Use `fileprivate` for file-restricted helpers.
- Protocol methods cannot have default arguments → use overloads in protocol extensions.
- Vapor middleware signatures change across versions; check `init` requirements if build fails.
- Redis key type: `RedisKey` has no `.string`; use string interpolation directly or `key.description`.
- `Response.Body.init(string:)` → use `Response.Body(string: body)` (lowercase `Body`).
- `HTTPStatus(statusCode:)` requires label; use `HTTPStatus(statusCode: status)`.
- Date formatters in Swift don’t throw — remove `try` from `guard let date = formatter.date(from: ...)`.
- Pagination stops when `X-Next-Cursor` header absent; `nextCursor = nil` terminates loop.
- SPM caches can become corrupt; full DerivedData wipe often needed (not just clean).

## Verification

- Backend `swift build` succeeds
- `swift run StockPlanBackend` → `GET /health` returns `{"status":"ok"}`
- POST with same `Idempotency-Key` returns identical body and same resource ID
- Rate-limited endpoint returns 429 after threshold
- `X-Request-ID` present on all responses
- Pagination: first page returns `X-Next-Cursor`; subsequent page with `?cursor=` returns next cursor and new items without duplicates
