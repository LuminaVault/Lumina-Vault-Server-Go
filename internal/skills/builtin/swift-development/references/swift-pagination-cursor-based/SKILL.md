---
name: swift-pagination-cursor-based
description: Add cursor-based pagination (limit + next_cursor) to Swift/Vapor list endpoints using tuple return type, fetch-limit+1 pattern, and X-Next-Cursor header. Prevents OOM from unbounded arrays, backward compatible, minimal boilerplate.
---

## When to Use

Adding pagination to any unbounded `GET` list endpoint in a Swift/Vapor backend where:
- Full array returns cause memory/performance issues
- Need cursor-based navigation (not offset-based)
- Must preserve backward compatibility (default limit applies)
- Existing repository methods already accept `limit` and `cursor` parameters

Also applies to **iOS client migration** from page-based to cursor-based infinite scroll when backend already has cursor pagination.

**Do NOT use** for: finite small datasets (< 100 rows), one-off endpoints, or when repo doesn't support cursor filtering.

## Backend Approach

### 1. Service Layer — Change Return Type

**Before:**
```swift
protocol StockService {
  func list(application: Application, limit: Int?, cursor: String?) async throws -> [StockResponse]
}
```

**After:**
```swift
protocol StockService {
  func list(application: Application, limit: Int?, cursor: String?) async throws -> (items: [StockResponse], nextCursor: String?)
}
```

Implementation pattern:
```swift
func list(application: Application, limit: Int?, cursor: String?) async throws -> (items: [StockResponse], nextCursor: String?) {
  let fetchLimit = (limit ?? 50) + 1  // fetch 1 extra to detect next page
  let results = try await repository.list(application: application, limit: fetchLimit, cursor: cursor)

  let items = Array(results.prefix(limit ?? 50))
  let nextCursor = results.count > (limit ?? 50)
    ? formatISODate(results[results.count - 1].createdAt)
    : nil

  return (items, nextCursor)
}
```

**Key decisions:**
- Tuple `(items, nextCursor)` over wrapper struct → minimal boilerplate, direct destructuring
- Fetch `limit + 1` → standard "lookahead" pattern; no extra query needed
- Cursor = ISO8601 timestamp of last returned item's `createdAt` → lexicographically sortable, safe for embedded SQL `ORDER BY createdAt ASC`
- Use `formatISODate` (existing utility) → ensures consistent string format across all endpoints

### 2. Controller — Consume Tuple + Set Header

```swift
func listStocks(req: Request) async throws -> Response {
  let limit = clampedLimit(req.query[Int.self, at: "limit"])
  let cursor = req.query[String.self, at: "cursor"]

  let (items, nextCursor) = try await service.list(
    userId: session.userId,
    portfolioListId: portfolioListId,
    limit: limit,
    cursor: cursorDate,
    on: req.db
  )

  let response = try Response(status: .ok)
  try response.content.encode(items)
  if let cursor = nextCursor {
    response.headers.add(name: .xNextCursor, value: cursor)
  }
  return response
}
```

**Notes:**
- Use Vapor's type-safe header: `.xNextCursor` (or `"X-Next-Cursor"` string literal)
- Header only added when `nextCursor != nil` → last page omits cursor
- Response body always encodes `items` array → backward compatible for clients ignoring cursor

### 3. Repository — Keyset Pagination

```swift
func list(userId: UUID, portfolioListId: UUID?, limit: Int, cursor: Date?, on db: any Database) async throws -> [Stock] {
  var query = Stock.query(on: db)
    .filter(\.$userId == userId)
    .sort(\.$createdAt, .ascending)
    .limit(limit)

  if let cursor {
    query.filter(\.$createdAt > cursor)
  }

  return try await query.all()
}
```

**Critical:** Order must be deterministic. If `createdAt` can collide, add tie-breaker: `.filter(\.$id > lastId)`.

## iOS Client Migration

When backend already uses cursor pagination (`X-Next-Cursor` header + `?cursor=` param), migrate client from page-based or unbounded requests.

### 1. Endpoint — Add Cursor & Limit Params

**Before:**
```swift
struct GetStocksEndpoint: Endpoint {
  typealias Response = [StockResponse]
  let portfolioListId: String?
  var method: HTTPMethod { .get }
  var path: String { "/v1/stocks" }
}
```

**After:**
```swift
struct GetStocksEndpoint: Endpoint {
  typealias Response = [StockResponse]
  let portfolioListId: String?
  let cursor: String?
  let limit: Int?

  var method: HTTPMethod { .get }
  var path: String { "/v1/stocks" }

  var queryItems: [URLQueryItem] {
    var items: [URLQueryItem] = []
    if let portfolioListId { items.append(.init(name: "portfolioListId", value: portfolioListId)) }
    if let cursor { items.append(.init(name: "cursor", value: cursor)) }
    if let limit { items.append(.init(name: "limit", value: "\(limit)")) }
    return items
  }
}
```

### 2. HTTP Client — Expose Response Headers

Add `callWithHeaders` to return both decoded body and `HTTPURLResponse`:

```swift
func callWithHeaders<E: Endpoint>(_ endpoint: E) async throws -> (response: E.Response, headers: HTTPURLResponse) where E.Response: Codable {
  let (data, urlResponse) = try await perform(endpoint)
  guard let httpResponse = urlResponse as? HTTPURLResponse else {
    throw SomeError.invalidResponse
  }
  let decoded = try endpoint.decode(data)
  return (decoded, httpResponse)
}

private func perform<E: Endpoint>(_ endpoint: E) async throws -> (Data, URLResponse) {
  let request = try makeURLRequest(for: endpoint)
  logRequest(request, endpoint: endpoint)
  let (data, response) = try await URLSession.shared.data(for: request)
  logResponse(response, data: data)
  return (data, response)
}
```

Update existing `call()` to use `perform` and decode only.

### 3. Service Protocol — Return Tuple

**Before:**
```swift
protocol StockServicing {
  func fetchPortfolio(portfolioListId: String?) async throws -> [StockResponse]
}
```

**After:**
```swift
protocol StockServicing {
  func fetchPortfolio(portfolioListId: String? = nil, cursor: String? = nil, limit: Int? = nil) async throws -> (items: [StockResponse], nextCursor: String?)
}
```

### 4. Service Implementation — Extract Cursor Header

```swift
func fetchPortfolio(portfolioListId: String? = nil, cursor: String? = nil, limit: Int? = nil) async throws -> (items: [StockResponse], nextCursor: String?) {
  try await performAuthenticated { client in
    let (result, response) = try await client.callWithHeaders(
      GetStocksEndpoint(portfolioListId: portfolioListId, cursor: cursor, limit: limit)
    )
    let nextCursor = response.headers[.xNextCursor].first
    return (result, nextCursor)
  }
}
```

**Header name:** Use `"X-Next-Cursor"` string literal — `HTTPURLResponse` doesn't have type-safe constants.

### 5. View Model — Add Pagination State & Load More

Add state:
```swift
@Published private(set) var nextCursor: String? = nil
@Published private(set) var isLoadingMore = false
```

Update initial load:
```swift
func load(force: Bool = false) async {
  guard !isLoading, !isLoadingMore else { return }
  isLoading = true
  defer { isLoading = false }

  do {
    nextCursor = nil
    let (items, fetchedCursor) = try await service.fetchPortfolio(limit: 50)
    // update published items via reconciliation/merge
    nextCursor = fetchedCursor
    hasLoadedOnce = true
  } catch { ... }
}
```

Add `loadMoreIfAvailable()`:
```swift
func loadMoreIfAvailable() async {
  guard !isLoadingMore, let cursor = nextCursor else { return }
  isLoadingMore = true
  defer { isLoadingMore = false }
  do {
    let (items, newCursor) = try await service.fetchPortfolio(cursor: cursor, limit: 50)
    // append items to data source
    nextCursor = newCursor
  } catch { ... }
}
```

### 6. UI — Infinite Scroll Trigger

In SwiftUI view, trigger on last row appearance:
```swift
ForEach(items) { item in
  RowView(item: item)
    .onAppear {
      if items.last?.id == item.id {
        Task { await viewModel.loadMoreIfAvailable() }
      }
    }
}
```

Alternative: use `ScrollViewProxy` and `onDidAppear` for precise control.

## Pitfalls (Client)

- **Overlapping items:** Ensure backend `createdAt` is strictly increasing for all inserts; otherwise cursor may skip or duplicate. Fix by adding tie-breaker `id` to cursor (composite cursor) if necessary.
- **Stale cursor:** If backend data changes between pages (new items inserted before current cursor), client may see duplicates or gaps. Acceptable for most feeds; for strict consistency, require stable sort order and immutable `createdAt`.
- **Multiple parallel loads:** Guard `isLoadingMore` to prevent double fetch when `onAppear` fires rapidly.
- **Pull-to-refresh:** Reset `nextCursor = nil` and reload from scratch. Don't append.
- **Error handling:** Log errors but don't clear `nextCursor` on failure — allows retry. Optionally provide retry UI.
- **Empty page:** If server returns 0 items with no cursor, treat as last page. If it returns 0 items but cursor exists, continue loading (rare edge case).
- **Header name case:** `HTTPURLResponse` header keys are case-insensitive but canonicalized. Access via `allHeaderFields["X-Next-Cursor"]` or Swift's `HTTPURLResponse` typed access (if available). Safe to use `.first` on array returned from ` headers[.xNextCursor]` in Vapor; on iOS use `response.allHeaderFields["X-Next-Cursor"] as? String`.
- **Client data merge:** When appending, don't deduplicate by ID unless you're certain no duplicates exist across pages. Cursor pagination guarantees non-overlap if ordering is strict.

## Compile Error Reference (Backend)

| Symptom | Fix |
|---|---|
| `extra argument 'createdAt' in call` | `StockResponse` now includes `createdAt: String` (ISO8601). Ensure initializer sets it. |
| `value of optional type 'Date?' must be unwrapped` | Use `if let`/`guard let` or provide default (`??`). Backend `createdAt` should be non-optional in response DTO. |
| `cannot find 'CompressionMiddleware' in scope` | Use `app.http.server.configuration.responseCompression = .enabled(initialByteBufferCapacity: 1024)` + `app.middleware.use(ResponseCompressionMiddleware(override: .useDefault))`. |
| `value of type 'RedisKey' has no member 'string'` | `RedisKey` is already a string wrapper. Remove `.string`, use `"\(redisKey)"` directly. |
| `missing argument label 'from:' in call` | `HTTPStatus` initializer is `HTTPStatus(statusCode: Int)`, not `HTTPStatus(code:)`. |
| `cannot infer contextual base in reference to member 'init'` | For `Response.Body`, use `Response.Body(string: body)` with explicit `Response` type or full qualification. |
| `cannot convert value of type 'UInt' to expected argument type 'Int'` | Cast status code: `Int(statusCode)`. |
| `cannot find 'RedisData' in scope` | Redis `set` accepts any `Codable`. Pass raw `String` directly: `request.redis.set(key, to: cached)`. No `RedisData` wrapper needed. |

## Lessons from Iterative Debugging

- **HTTP client refactor:** Changing `perform()` to return `(Data, URLResponse)` requires updating all callers (`call`, `callWithHeaders`). Do it in one atomic patch.
- **Parallel destructuring:** When using `async let` with multiple tasks, each task result matches its position in the tuple. If one task's return type changes (e.g., `[T]` → `([T], String?)`), update the destructuring accordingly: `let (resultA, resultB) = try await (taskA, taskB)` where `resultA` is now a tuple itself → `let ((items, cursor), summary) = ...` or split into two steps.
- **Tuple vs single value:** If service method returns `(items, cursor)` but caller expects just `[T]`, compiler error is clear. Update all callers together (ViewModel, stub, sync manager).
- **Header access:** Vapor backend uses type-safe `req.headers[.ifNoneMatch]`; iOS `HTTPURLResponse` uses `allHeaderFields` dictionary. Don't confuse the two.
- **Cursor parameter naming:** Keep consistent: backend uses `cursor` query param; iOS endpoint must include it in `queryItems`.
- **Build error triage:** When `swift build` reports errors across many files after a change, focus on files you touched. Some errors may be pre-existing; run clean build after each batch.

## Validation Checklist

- [ ] Backend `swift build` clean; service protocol matches impl; controller sets `X-Next-Cursor`
- [ ] iOS client `xcodebuild` or `swift build` clean; all updated endpoints compile
- [ ] Pagination flow: first page returns `nextCursor`; second page with cursor returns next chunk; last page `nextCursor` nil
- [ ] Infinite scroll: last row triggers `loadMoreIfAvailable`; `isLoadingMore` prevents parallel loads
- [ ] Pull-to-refresh resets cursor and reloads
- [ ] No duplicate items across pages
- [ ] Network trace: verify `?cursor=` query param sent; verify `X-Next-Cursor` header received

## Files Modified (This Session)

Backend:
- `Sources/StockPlanBackend/Stocks/StockService.swift`
- `Sources/StockPlanBackend/Stocks/StockController.swift`
- `Sources/StockPlanBackend/Expenses/ExpensesService.swift`
- `Sources/StockPlanBackend/Expenses/ExpensesController.swift`
- `Sources/StockPlanShared/Stocks/StockDTOs.swift` (added `createdAt: String`)

iOS:
- `API/Stocks/StockEnpoints.swift`
- `API/Stocks/StockHTTPClient.swift`
- `Features/Stocks/StockService.swift`
- `API/Expenses/ExpensesEndpoints.swift`
- `API/Expenses/ExpensesHTTPClient.swift`
- `Features/Expenses/ExpensesService.swift`
- `Features/Expenses/BudgetPlannerViewModel.swift`
- `Features/Expenses/ExpensesSyncManager.swift`
- `Features/Portfolio/PortfolioViewModel.swift`
- `Features/Portfolio/PortfolioScreen.swift`

---
