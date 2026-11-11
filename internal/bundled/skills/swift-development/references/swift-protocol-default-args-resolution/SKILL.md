---
name: swift-protocol-default-args-resolution
description: Resolve "Default argument not permitted in a protocol method" errors in Swift protocols by removing default values from protocol method signatures and adding convenience overloads in protocol extensions. Also covers file-level access control corrections (`private` → `fileprivate`) and optional date unwrapping for formatter calls.
---
## When to Use

Call this skill when encountering:
- `Default argument not permitted in a protocol method` compiler error
- `Attribute 'private' can only be used in a non-local scope` at file-level Swift declarations
- `value of optional type 'Date?' must be unwrapped` when calling `ISO8601DateFormatter().string(from:)`
- Need to implement cursor-based pagination across backend (Vapor) + iOS client with tuple returns `(items: [T], nextCursor: String?)`

## Approach

**1. Protocol default arguments**
- Protocol methods cannot have default parameter values.
- Remove all `= default` from protocol method signature.
- Add extension on the protocol with overload method that supplies `nil` defaults for optional parameters.
- Keep implementation method unchanged — it already accepts all parameters.

**2. File-level access control**
- Top-level declarations (functions, variables) marked `private` at file scope are invalid.
- Change `private` → `fileprivate` for file-scoped declarations.
- `internal` (default) is fine if module-wide visibility is acceptable.

**3. Optional date unwrapping for formatters**
- `ISO8601DateFormatter().string(from: Date?)` fails — requires non-optional `Date`.
- Use nil-coalescing with fallback: `model.createdAt ?? Date()` or force-unwrap if nil is impossible.
- Prefer nil-coalescing with sensible default (e.g., `Date()` for "now" placeholder).

**4. Cursor-based pagination pattern**
- Backend: list endpoint accepts `?cursor=<ISO8601>&limit=<Int>`; returns items + `X-Next-Cursor` header when more data exists.
- iOS: HTTP client exposes `callWithHeaders()` returning `(Data, HTTPURLResponse)`; service extracts header and returns tuple `(items, nextCursor)`.
- Protocol signature: `func fetchItems(cursor: String? = nil, limit: Int? = nil) async throws -> (items: [ItemResponse], nextCursor: String?)`
- Guard against using non-existent properties (e.g., `last.createdAt` must exist on response type).

**5. Middleware/import fixes**
- Ensure middleware imports: `import Vapor` + `import VaporCompression` if using `CompressionMiddleware`.
- For response compression: configure via `app.http.server.configuration.responseCompression = .init()` rather than middleware if module missing.

## Common Pitfalls

- Forgetting to update all call-sites after protocol signature change — extension overload preserves backward compatibility.
- Using `private` at top-level — Swift only allows `fileprivate` or `internal` outside types.
- Optional `Date?` passed directly to formatter — unwrap first.
- Assuming last item property exists for pagination header — verify response DTO includes cursor field.
- Redis types (RedisKey, RedisData) may differ by library version — check actual API (`.redisKey.string` may be `String(redisKey)` or `redisKey.description`).

## Verification

- `swift build` succeeds with no errors.
- Backend: `GET /health` returns `{"status":"ok"}`.
- iOS: project builds; pagination calls return both items and next cursor correctly.
- Idempotency middleware compiles with correct Redis API usage.

## Example Fixes

Error: `Default argument not permitted in a protocol method`
```swift
// Before (protocol)
func fetchPortfolio(portfolioListId: String? = nil, cursor: String? = nil, limit: Int? = nil) async throws -> (items: [StockResponse], nextCursor: String?)

// After (protocol)
func fetchPortfolio(portfolioListId: String? = nil, cursor: String? = nil, limit: Int? = nil) async throws -> (items: [StockResponse], nextCursor: String?)

// After (extension)
extension StockServicing {
  func fetchPortfolio(portfolioListId: String? = nil, cursor: String? = nil, limit: Int? = nil) async throws -> (items: [StockResponse], nextCursor: String?) {
    try await fetchPortfolio(portfolioListId: portfolioListId, cursor: cursor, limit: limit)
  }
}
```

Error: `Attribute 'private' can only be used in a non-local scope`
```swift
// Before (top-level in file)
private let logger = Logger(label: "HTTPClient")

// After
fileprivate let logger = Logger(label: "HTTPClient")
```

Error: `value of optional type 'Date?' must be unwrapped`
```swift
// Before
createdAt: Self.formatISO8601(model.createdAt)

// After
createdAt: Self.formatISO8601(model.createdAt ?? Date())
```