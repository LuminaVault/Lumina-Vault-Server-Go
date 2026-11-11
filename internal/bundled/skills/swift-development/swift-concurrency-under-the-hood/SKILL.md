---
name: swift-concurrency-under-the-hood
category: swift-development
description: "Production-grade reference for Swift concurrency internals — cooperative thread pool, actor reentrancy, iOS vs Linux/Vapor differences, and battle-tested patterns for both platforms."
version: 1.0
created: 2026-05-12
last_updated: 2026-05-12
---

# Swift Concurrency Under the Hood

Load this skill when reviewing Swift/Vapor code for concurrency correctness, performance, or architecture decisions. It covers the actual runtime behavior — not just the syntax.

## Key Principles to Enforce

### 1. Never Block the Cooperative Pool
The thread pool has a FIXED number of threads (4-8 on iOS, max(cpuCount, 2) on Linux). Any synchronous operation that takes > few ms from an async context blocks a pool thread and starves other tasks.

- CPU-bound work → `Task.detached(priority:)`
- Sync DB/file I/O from async code → wrap in continuation or use async-compatible library
- Heavy JSON encoding → `Task.detached`

### 2. Actor Reentrancy Is By Design
When you `await` inside an actor method, isolation is released. Other tasks CAN enter the actor while suspended.

**Required pattern:** Always double-check state after an await inside actors:
```swift
actor Cache {
    func fetch(_ key: String) async throws -> Data {
        if let cached = items[key] { return cached }
        let data = try await fetchFromNetwork(key)
        if let cached = items[key] { return cached } // DOUBLE-CHECK
        items[key] = data
        return data
    }
}
```

### 3. Structured Concurrency > Detached
`async let` and `TaskGroup` are preferred. They handle cancellation propagation automatically. `Task.detached` should only be used for:
- CPU-bound work that would block the pool
- Work that must outlive the parent task scope
- When you explicitly need to break out of actor isolation

When using `Task.detached`, manually handle cancellation — it doesn't inherit task-local values or actor isolation.

## Platform-Specific Rules

### iOS
- Pool size: 4-8 threads. Saturating it stalls everything.
- `Task.detached` is dangerous — thread explosion risk
- MainActor handles thread hops automatically after `await`
- Use `.task` over `.onAppear { Task { } }` — lifecycle-aware, auto-cancels
- `async let` is the go-to for parallel prefetch

### Server (Vapor/Linux)
- Pool size: max(cpuCount, 2), configurable via `SWIFT_CONCURRENCY_WORKER_THREADS`
- I/O bottleneck is the DB connection pool, not thread count
- `Task.detached` is fine for CPU-heavy workloads
- `TaskGroup` with `taskCount >= maxConcurrent` guard for bounded concurrency
- epoll + NIO event loop handles actual socket I/O

## Pattern Checklist for Code Review

When reviewing async code, verify:

1. [ ] No blocking sync calls in async context
2. [ ] Actor methods double-check after await
3. [ ] TaskGroup has concurrency bounds (not unlimited spawning)
4. [ ] Cancellation is respected in long-running loops
5. [ ] `async let` used for independent parallel work
6. [ ] `.task` used instead of `.onAppear { Task { } }`
7. [ ] `Task.detached` only for CPU work or outliving parent scope
8. [ ] Continuations resume exactly once (use checked variant)

## Continuations Quick Reference

- `withCheckedThrowingContinuation` — wraps callback APIs, validates single resume at runtime, crashes if violated
- `withUnsafeThrowingContinuation` — same but skips the check, use only in performance-critical paths

## See Also

- `swift-concurrency-pro` — the code review skill with structured checklists for reentrancy, task groups, cancellability, and Swift 6 strict-concurrency diagnostics. Load it when doing actual line-by-line reviews.

## Async Result (SE-0530)

```swift
let result = await Result { try await fetchUser() }
```
Eliminates do/catch boilerplate when you need `Result<User, Error>`.
