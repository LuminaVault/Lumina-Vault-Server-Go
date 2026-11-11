---
name: vapor-production-hardening
description: Systematic pre-launch production hardening for Vapor/Swift backends. Ensures response compression, request body limits, rate limiting on quota-sensitive endpoints, global idempotency for mutations, and correct Redis API usage. Derived from StockPlan production audit.
---

## Problem

Production Vapor backends need:
- Response compression (60–80% size reduction)
- Request body size limits
- Rate limiting to protect third-party API quotas
- Idempotency on mutation endpoints to prevent duplicates
- Correct Redis middleware usage

### Common gaps: middleware exists but isn't wired; Redis API chaining doesn't compile; unrelated build errors obscure changes; **iOS client must adapt to tuple returns** `(items, nextCursor)` after pagination changes; health endpoint may return empty if route registration order/middleware blocks it.

## Resolution Stack

### 1. Audit configure.swift

Verify present:
```swift
app.routes.defaultMaxBodySize = "10mb"           // body limit
// CORS configured via CORSMiddleware
// Redis configured from env
```

Add missing items.

### 2. Wire Idempotency Globally

IdempotencyMiddleware is often stored but not applied:

```swift
// During configure:
app.idempotencyMiddleware = IdempotencyMiddleware(ttl: 86_400) // 24h
// Apply globally (after error middleware, before routes):
app.middleware.use(app.idempotencyMiddleware)
```

Automatically deduplicates POST/PUT/DELETE/PATCH when clients send `Idempotency-Key` header. No per-route changes.

### 3. Rate Limit Sensitive Collections

Identify endpoints that need protection (auth, market data, webhooks). Apply per-collection:

```swift
// routes.swift
let marketRateLimit = RateLimitMiddleware(limit: 100, interval: 60, keyPrefix: "ratelimit:market")
try api.grouped(marketRateLimit).register(collection: MarketDataController())
```

Uses existing `RateLimitMiddleware` (Redis-backed, per-IP). Adjust numbers to match provider quotas.

### 4. Fix Redis Chaining Bug

Redis middleware methods return `EventLoopFuture<Void>`. Cannot chain `expire()` after `set()`.

Wrong:
```swift
try await request.redis.set(key, to: value).expire(key, after: .seconds(60)).get()
```

Right:
```swift
try await request.redis.set(key, to: value).get()
try await request.redis.expire(key, after: .seconds(60)).get()
```

Applies to idempotency and rate-limit code.

### 5. Build & Validate

```bash
cd StockPlanBackend
swift package clean
swift build 2>&1 | tail -30
```

If unrelated pre-existing errors block build, note them but don't conflate with hardening changes.

Runtime checks:
- Redis connected in logs
- Rate-limited endpoint → 429 after threshold
- Idempotency duplicate → cached response, `idempotency.hit` log
- JSON responses >1KB → `Content-Encoding: gzip`

### 6. Compression Configuration (Vapor 4.115.0+)

Vapor 4.115.0+ uses `ResponseCompressionMiddleware` separately; server configuration just enables it:

```swift
// Enable compression engine
app.http.server.configuration.responseCompression = .init()  // or .enabled(initialByteBufferCapacity: 1024)

// Add middleware with threshold (bytes)
app.middleware.use(ResponseCompressionMiddleware(threshold: 1024))
```

Older tutorials conflate server config and middleware. Both pieces needed: config enables compressor; middleware applies it with threshold.

### 7. IdempotencyMiddleware Implementation Corrections (Vapor 4 Redis API)

Actual compile fixes applied:

- **RedisKey usage:** `RedisKey` is a struct; don't access nonexistent `.string` member. Use string interpolation directly:
  ```swift
  let redisKey = self.redisKey(for: idemKey)           // RedisKey
  let cacheKey = RedisKey("\(redisKey):response")      // ✅ correct
  // NOT: RedisKey("\(redisKey.string):response")
  ```

- **HTTPStatus init:** Use `HTTPStatus(statusCode: Int)`:
  ```swift
  var response = Response(status: HTTPStatus(statusCode: Int(status)))
  ```

- **Response body init:** `Response.Body` has `init(string: String)`:
  ```swift
  response.body = .init(string: body)
  ```

- **Redis set value type:** No `RedisData` wrapper needed for `String`:
  ```swift
  try await request.redis.set(cacheKey, to: cached).get()
  // NOT: RedisData(cached)
  ```

- **Status code conversion:** `resp.status.code` is `UInt16`/`UInt`; cast to `Int`:
  ```swift
  let statusCode = Int(resp.status.code)
  ```

Full corrected IdempotencyMiddleware snippet:
```swift
if let cachedString = try? await request.redis.get(cacheKey, as: String.self).get(),
   let (status, headers, body) = Self.parseCached(cachedString) {
  request.logger.debug("idempotency.hit key=\(idemKey.prefix(8))...")
  var response = Response(status: HTTPStatus(statusCode: Int(status)))
  response.headers = HTTPHeaders(headers)
  response.body = .init(string: body)
  return response
}
// ...
let statusCode = Int(resp.status.code)
if (200...399).contains(statusCode), let body = resp.body.string {
  let headerPairs = resp.headers.map { ($0.name, $0.value) }
  let cached = Self.encodeCachedResponse(status: statusCode, headers: headerPairs, body: body)
  try? await request.redis.set(cacheKey, to: cached).get()
  try? await request.redis.expire(cacheKey, after: .seconds(Int64(ttl))).get()
}
```

### 8. ETag Caching Headers

When DTOs lack a modification timestamp, use content-based hashing for weak ETags. Use Vapor's type-safe header keys (`.ifNoneMatch`, `.ifModifiedSince`, `.lastModified`, `.eTag`). Example for a profile endpoint without `lastUpdated`:

```swift
var hasher = Hasher()
hasher.combine(profile.ticker ?? "")
hasher.combine(profile.name ?? "")
hasher.combine(profile.marketCapitalization ?? 0)
// ... combine other relevant fields
let hash = hasher.finalize()
let etag = "W/\"\(symbol)-\(hash)\""

if let ifNoneMatch = req.headers[.ifNoneMatch].first, ifNoneMatch == etag {
    return Response(status: .notModified)
}
...
response.headers.add(name: .eTag, value: etag)
```

For history/quote endpoints with timestamps, compute ETag directly from numeric timestamp.

## Pitfalls

- **Idempotency not firing:** Middleware must be added to `app.middleware`; storing in `app.idempotencyMiddleware` alone does nothing.
- **Redis unconfigured:** Both rate limiting and idempotency require `REDIS_URL`; they fail closed in production (503).
- **Pre-existing build errors:** Unrelated errors can mask hardening changes. Clean build to verify.
- **Vary: Origin:** Dynamic CORS origins need `Vary: Origin` for CDN correctness. Vapor's CORSMiddleware may not add it automatically; verify with `curl -I`. If missing, add custom `VaryHeaderMiddleware` after CORS.
- **Key granularity:** Default rate limiter uses client IP (`remoteAddress`). For authenticated endpoints, user-based keys may be more appropriate.
- **ETag header syntax:** Vapor exposes headers via type-safe `HTTPHeaders.Name` properties (`.ifNoneMatch`, `.ifModifiedSince`, `.eTag`, `.lastModified`). Do NOT use bare identifiers; access via `req.headers[.ifNoneMatch]`.
- **Shared DTO changes:** `StockPlanShared` is a separate Swift package within the repo. Edit DTOs in `StockPlanShared/Sources/...`, not in `.build/checkouts`. Run clean build to regenerate.
- **ETag/304 cleanup:** When removing a timestamp field from a DTO (e.g., `lastUpdated`), also remove all ETag/conditional GET logic in controller routes — including `ifNoneMatch`/`ifModifiedSince` header checks and `formatHTTPDate`/`parseHTTPDate` calls. Those helpers are not Vapor built-ins; they are custom and often removed entirely.
- **Compression API version:** Vapor 4.115.0+ requires both `app.http.server.configuration.responseCompression = .init()` and `app.middleware.use(ResponseCompressionMiddleware(threshold: N))`. Older versions use `.responseCompression.threshold` property directly. Check your Vapor version.

## Compile Error Reference

| Symptom | Fix |
|---|---|
| `cannot find 'ifNoneMatch' in scope` | Use Vapor's type-safe header keys: `req.headers[.ifNoneMatch]` (import `Vapor`). Or strip all ETag logic if no timestamp field exists. |
| `value of type 'X' has no member 'lastUpdated'` | DTO missing timestamp. Remove ETag/304 branches and `formatHTTPDate` calls from route. Use content-based hash for weak ETag instead. |
| `extra argument 'createdAt' in call` | `StockResponse` now includes `createdAt: String` (ISO8601). Ensure you pass it, or if signature changed, update all initializers. |
| `incorrect argument label in call (have 'code:', expected 'from:')` | `HTTPStatus` initializer is `HTTPStatus(statusCode:)`, not `HTTPStatus(code:)`. |
| `Response.Body(string:)` not found | Use `Response.Body(string: body)`. Verify import `Vapor`. |
| Redis `decode` vs `get` confusion | Use `request.redis.get(key, as: String.self).get()` for plain string cache. |
| Middleware order matters | Typical: CORS → Vary → Compression → Idempotency → Tracing → RequestLogging. Adjust per needs. |
| Redis chaining error | `set` and `expire` must be separate calls — they return `EventLoopFuture<Void>` which cannot be chained directly. |
| `RedisKey` has no `.string` | Remove `.string`; use `RedisKey("\(keyPrefix):\(key)")` directly. |
| `RedisData` not found | Pass raw `String`/`Data` to `redis.set(to:)`; no wrapper. |
| `HTTPStatus` from `UInt` | Cast: `HTTPStatus(statusCode: Int(statusCode))`. |

## Lessons from Iterative Debugging

- **Protocol default arguments (Swift limitation):** Protocol methods cannot have default parameters. When you see `error: extra argument 'X' in call` on a protocol conformance, remove defaults from the protocol signature and add an extension overload supplying default values:
  ```swift
  // Protocol — no defaults
  protocol ExpensesService {
    func getExpenses(limit: Int?, cursor: Date?, on db: any Database) async throws -> [ExpenseResponse]
  }

  // Extension — overload with defaults
  extension ExpensesService {
    func getExpenses(limit: Int? = nil, cursor: Date? = nil, on db: any Database) async throws -> [ExpenseResponse] {
      try await getExpenses(limit: limit, cursor: cursor, on: db)
    }
  }

  // Implementation matches protocol exactly
  struct ExpensesHTTPService: ExpensesService {
    func getExpenses(limit: Int?, cursor: Date?, on db: any Database) async throws -> [ExpenseResponse] { ... }
  }
  ```
  This preserves call-site convenience while satisfying Swift's type system. Related error: `value of optional type 'Date?' must be unwrapped` — if model field is optional, unwrap safely (`model.createdAt ?? Date()` or guard).

- **File-scope access control:** Top-level `private` functions in a `.swift` file are illegal. Compiler error appears as "`private` modifier cannot be used in top-level code". Change to `fileprivate` to restrict to file scope while allowing cross-extension access.
  ```swift
  // ❌
  private func makeRequest() async throws -> Response { ... }

  // ✅
  fileprivate func makeRequest() async throws -> Response { ... }
  ```

- **iOS client pagination adaptation:** After changing service methods to return `(items: [T], nextCursor: String?)`, ViewModels must destructure correctly. Two forms:
  ```swift
  // Form A — explicit tuple named properties
  let result = try await service.getStocks(...)
  self.items = result.items
  self.nextCursor = result.nextCursor

  // Form B — direct destructuring
  (items, nextCursor) = try await service.getStocks(...)
  ```
  Common pitfall: `async let` parallel tasks returning nested tuples require fully-parenthesized destructuring:
  ```swift
  let ((stocks, stockCursor), (expenses, expenseCursor)) = try await (
      stockService.getStocks(limit: 20, cursor: nil),
      expenseService.getExpenses(limit: 20, cursor: nil)
  )
  ```
  Split into sequential lines if nesting is confusing. HTTP client must store `X-Next-Cursor` header and pass it back as `?cursor=` query param.

- **Start minimal:** Get idempotency working with plain string cache before adding complex wrapper structs.
- **Redis API shape:** `get(key, as: String.self)` returns `EventLoopFuture<String?>`. Use `.get()` to block in simple contexts or `flatMap` for async flow.
- **Response body extraction:** `response.body.string` is optional; guard nil before caching to avoid storing empty bodies.
- **HTTPStatus initializer:** Vapor 4 uses `HTTPStatus(statusCode:)`.
- **ETag logic cleanup:** When removing `lastUpdated` from a DTO, also remove `ifNoneMatch` / `ifModifiedSince` helpers and all 304-branch handlers from routes.
- **Method overload disambiguation:** If Swift complains about overload mismatch, pass all parameters explicitly (e.g., `cursor: nil`) to select intended overload.
- **Middleware registration:** Storing a middleware in an app property does NOT add it to the pipeline; must also call `app.middleware.use(...)`.
- **Compression config API changed:** Vapor 4.115.0+ uses `ResponseCompressionMiddleware` separately; server config only enables the compressor (`.init()` or `.enabled(initialByteBufferCapacity:)`).
- **Build error triage:** When `swift build` shows errors across many files, focus on the files you changed. Some errors may be stale; run a clean build after each batch of fixes to confirm progress.
- **Parallel async destructuring:** When using `async let` with tasks returning tuples, destructuring must match the arity exactly. If a callee's return type changes from `[T]` to `([T], String?)`, update the parent's destructuring accordingly: `let ((items, cursor), other) = try await (task1, task2)` or split into two lines to avoid nested tuple confusion.

## Pagination Audit & Verification

Many list endpoints start with offset/limit and hit performance walls at scale. Verify all collection endpoints use cursor-based pagination.

### Pattern to check

Service layer:
```swift
func list(..., limit: Int, cursor: Date?, on db: any Database) async throws -> [Stock] {
  var query = Model.query(on: db).filter(...).sort(\.$createdAt, .ascending)
  if let cursor { query.filter(\.$createdAt > cursor) }
  query.limit(limit + 1)
  return try await query.all()
}
```

Service wrapper (returns tuple):
```swift
func list(...) async throws -> (items: [StockResponse], nextCursor: String?) {
  let fetchLimit = (limit ?? 50) + 1
  let records = try await repository.list(..., limit: fetchLimit, cursor: cursorDate, on: db)
  let items = Array(records.prefix(limit ?? 50))
  let nextCursor = records.count > (limit ?? 50)
    ? formatISO8601(records[records.count - 1].createdAt)
    : nil
  return (items, nextCursor)
}
```

Controller layer:
- Accept `?cursor=ISO8601&limit=N` query params
- Parse cursor via `ISO8601DateFormatter` with fractional seconds
- Return `X-Next-Cursor` header when `nextCursor != nil`

### Common gaps

- **Missing pagination entirely** — endpoint returns full table (`query.all()`). Add `limit + 1` pattern + `X-Next-Cursor`.
- **Cursor field missing** — model has no `createdAt` or no index on it. Add timestamp column + DB index.
- **Using offset instead of cursor** — `OFFSET :n` becomes O(n) at scale. Replace with keyset pagination.
- **Header absent** — controller forgets to add `X-Next-Cursor`. Client can't advance pages.
- **Composite cursor needed** — if two items share same `createdAt`, page may skip/duplicate. Tie-breaker with `id`: `WHERE (createdAt < cursor) OR (createdAt = cursor AND id < lastId)`.
- **Non-standard param name** — client sends `?after=` or `?page=`. Standardize to `cursor`.
- **Returning cursor in JSON** — some APIs return `{ items, nextCursor }` in body rather than header. Either is fine; be consistent.

### Endpoints to verify

| Endpoint | Cursor param | Response header | Notes |
|---|---|---|---|
| `GET /v1/stocks` | `?cursor=` | `X-Next-Cursor` | Uses `StockResponse.createdAt` (String) |
| `GET /v1/expenses` | `?cursor=` | `X-Next-Cursor` | Service returns `(items, nextCursor)` tuple |
| `GET /v1/news` | `?cursor=` | `X-Next-Cursor` | `NewsItem.createdAt` → ISO8601 |

If any list endpoint lacks pagination, implement the three-layer pattern above.

## Validation Checklist

- [ ] `swift build` clean, no new errors/warnings from changes
- [ ] Compression: `curl -H "Accept-Encoding: gzip" -I /v1/...` shows `Content-Encoding: gzip`
- [ ] Max body: posting >10MB returns 413 (Payload Too Large)
- [ ] Rate limit: >100 market requests/min returns 429
- [ ] Idempotency: repeat request with same `Idempotency-Key` returns cached 200, no duplicate DB writes
- [ ] Redis connected: logs show redis configuration without errors
- [ ] Pagination: All `GET /collection` endpoints accept `cursor`, return `X-Next-Cursor`, use `limit+1` fetch pattern

## Notes

- karpathy-guidelines: surgical changes only; match existing code style
- Compression and body-size were already present in StockPlan — only verified
- IdempotencyMiddleware existed (24h TTL, Redis-backed) but was never registered globally
- RateLimitMiddleware existed (auth-only) — extended to market data
- Redis chaining bug in IdempotencyMiddleware required splitting `set` and `expire` calls; also fixed `RedisKey` string interpolation, `HTTPStatus(statusCode:)`, and removed incorrect `RedisData` wrapper
- StockResponse.createdAt field added to shared DTO to support pagination cursors
- Compression config uses `ResponseCompressionMiddleware(threshold: 1024)` with `app.http.server.configuration.responseCompression = .init()` on Vapor 4.115.0+
- Build error triage: when `swift build` shows errors across many files, focus on files you changed; some errors may be stale from previous state — run clean build after each batch of fixes to confirm progress
- ETag caching headers are now implemented on quote/history/profile routes
- pagination cursor-based swift ios vapor
