---
name: swift-development
description: Comprehensive Swift expertise umbrella covering UI, data, concurrency, testing, architecture, and production hardening across the Apple ecosystem.
license: MIT
metadata:
  capability: medium
---
# Swift Development

This umbrella skill consolidates a full spectrum of Swift development capabilities:

- **Core Data & SwiftData** — persistent storage, migrations, CloudKit sync, concurrency
- **SwiftUI** — component design, subscription gating, UI testing, best-practice review
- **Concurrency & networking** — Sendable fixes, HTTP client, pagination patterns, Vapor backend
- **Testing & Quality** — Swift Testing, property-wrapper debugging, protocol issues
- **Architecture** — multi-repo analysis, environment switching, production hardening (Vapor)

## When to use

Load `swift-development` for any Apple platform (iOS/macOS) or server-side Swift work requiring deep expertise across the stack.

## Subskills

### swift-concurrency-pro

**Description:** Reviews Swift code for concurrency correctness, modern API usage, and common async/await pitfalls. Use when reading, writing, or reviewing Swift concurrency code.

**Resources:** 
- Full documentation: `references/swift-concurrency-pro/SKILL.md`
- Scripts: `scripts/swift-concurrency-pro/` (if any)

### swift-testing-pro

**Description:** Writes, reviews, and improves Swift Testing code using modern APIs and best practices. Use when reading, writing, or reviewing projects that use Swift Testing.

**Resources:** 
- Full documentation: `references/swift-testing-pro/SKILL.md`
- Scripts: `scripts/swift-testing-pro/` (if any)

### swiftdata-pro

**Description:** Writes, reviews, and improves SwiftData code using modern APIs and best practices. Use when reading, writing, or reviewing projects that use SwiftData.

**Resources:** 
- Full documentation: `references/swiftdata-pro/SKILL.md`
- Scripts: `scripts/swiftdata-pro/` (if any)

### swiftui-pro

**Description:** Comprehensively reviews SwiftUI code for best practices on modern APIs, maintainability, and performance. Use when reading, writing, or reviewing SwiftUI projects.

**Resources:** 
- Full documentation: `references/swiftui-pro/SKILL.md`
- Scripts: `scripts/swiftui-pro/` (if any)

### core-data-expert

**Description:** 'Expert Core Data guidance (iOS/macOS): stack setup, fetch requests & NSFetchedResultsController, saving/merge conflicts, threading & Swift Concurrency, batch operations & persistent history, migrations, performance, and NSPersistentCloudKitContainer/CloudKit sync.'

**Resources:** 
- Full documentation: `references/core-data-expert/SKILL.md`
- Scripts: `scripts/core-data-expert/` (if any)

### swift-http-client-sendable-fixes

**Description:** Fix Sendable conformance and protocol errors in BaseHTTPClient-based modules

**Resources:** 
- Full documentation: `references/swift-http-client-sendable-fixes/SKILL.md`
- Scripts: `scripts/swift-http-client-sendable-fixes/` (if any)

### swift-pagination-cursor-based

**Description:** Add cursor-based pagination (limit + next_cursor) to Swift/Vapor list endpoints using tuple return type, fetch-limit+1 pattern, and X-Next-Cursor header. Prevents OOM from unbounded arrays, backward compatible, minimal boilerplate.

**Resources:** 
- Full documentation: `references/swift-pagination-cursor-based/SKILL.md`
- Scripts: `scripts/swift-pagination-cursor-based/` (if any)

### swift-property-wrapper-keypath-debugging

**Description:** Diagnose and fix Swift property wrapper keypath errors (e.g., @InjectedObservable, @Injected) using grep-based codebase scanning and surgical corrections.

**Resources:** 
- Full documentation: `references/swift-property-wrapper-keypath-debugging/SKILL.md`
- Scripts: `scripts/swift-property-wrapper-keypath-debugging/` (if any)

### swift-protocol-default-args-resolution

**Description:** Resolve "Default argument not permitted in a protocol method" errors in Swift protocols by removing default values from protocol method signatures and adding convenience overloads in protocol extensions. Also covers file-level access control corrections (`private` → `fileprivate`) and optional date unwrapping for formatter calls.

**Resources:** 
- Full documentation: `references/swift-protocol-default-args-resolution/SKILL.md`
- Scripts: `scripts/swift-protocol-default-args-resolution/` (if any)

### swift-vapor-pagination-concurrency

**Description:** Cursor-based pagination across Swift/Vapor backend + iOS SwiftUI client,   plus Swift protocol default-argument workarounds, Vapor middleware init fixes,   IdempotencyMiddleware Redis handling, Xcode SPM cache recovery, and SwiftUI   async closure bridging patterns. ---

**Resources:** 
- Full documentation: `references/swift-vapor-pagination-concurrency/SKILL.md`
- Scripts: `scripts/swift-vapor-pagination-concurrency/` (if any)

### swiftui-subscription-gating

**Description:** Implement UI gating for subscription tiers in SwiftUI apps using ProGateView   pattern. Surgical per-component wrapping, toolbar filtering, and navigation link   gating following monetization spec boundaries. category: swiftui-pro status: stable ---

**Resources:** 
- Full documentation: `references/swiftui-subscription-gating/SKILL.md`
- Scripts: `scripts/swiftui-subscription-gating/` (if any)

### swiftui-ui-testing-setup

**Description:** End-to-end workflow for adding XCTest UI tests to a SwiftUI iOS project. Covers accessibility identifier injection, reusable component patching, language-resilient test helpers, and smoke-test authoring.

**Resources:** 
- Full documentation: `references/swiftui-ui-testing-setup/SKILL.md`
- Scripts: `scripts/swiftui-ui-testing-setup/` (if any)

### provider-adapter-translation

**Description:** Translate OpenAI chat-completions JSON ↔ provider-specific formats (Gemini, Anthropic, etc.) for `ProviderAdapter` implementations. Covers message role mapping (`user`/`assistant`/`system` → provider equivalents), tool/function declaration mapping, response shape normalization, and ProviderError classification (transient/permanent/network).

**Resources:** 
- Full documentation: `references/provider-adapter-translation.md`

### wire-format-dto-patterns

**Description:** Codable wire-format DTO design for Swift, including cross-package shared-DTO migration patterns. Covers: `AnyJSONValue` enum pattern for arbitrary JSON parameters, `AnyEncodable` anti-pattern for round-trip fields, `NSNull` does-not-conform-to-Encodable trap, Swift module-scope type deduplication, `CodingKeys` wire-format mapping, `Sendable` conformance for DTOs crossing actor boundaries, and the cross-package shared-DTO workflow for server↔client type sharing (see pitfalls below).

**Resources:** 
- Full documentation: `references/wire-format-dto-patterns.md`

