# Cross-Package Shared DTO Migration

## When to do it
Migrate API contract types (requests, responses, DTOs) between a Hummingbird server and an iOS client into a shared Swift package so both platforms consume a single source of truth.

## Split-conformance pattern
`ResponseEncodable` is a Hummingbird marker protocol (no fields, no behavior). The cleanest approach:

```swift
// Shared package (no Hummingbird dependency):
public struct AuthResponse: Codable {
    public let userId: UUID
    // ... fields only ...
}

// Server-side extension (one file):
import Hummingbird
import MySharedPackage

extension AuthResponse: ResponseEncodable {}
extension MeResponse: ResponseEncodable {}
// ... all response types in one file ...
```

This pattern is already used in the codebase for WebAuthn response types (line 41-43 in WebAuthnService.swift).

## Pitfalls

### 1. Cross-module `init` in extensions
You **cannot** add a struct `init` that assigns `let` properties directly in an extension when the struct is defined in a different module:

```swift
// ❌ ERROR: 'let' property may not be initialized directly
extension SpaceDTO {
    init(_ space: Space) throws {
        id = try space.requireID()    // ERROR in Swift 6
        name = space.name             // ERROR in Swift 6
    }
}

// ✅ Use the memberwise init or a static factory:
extension SpaceDTO {
    static func fromSpace(_ space: Space) throws -> SpaceDTO {
        SpaceDTO(id: try space.requireID(), name: space.name, ...)
    }
}
```

The Swift 6 compiler requires `self.init(...)` or `self = ...` instead of direct `let` property assignment in extensions.

### 2. `Sendable` conformance chain
Adding `Sendable` to one DTO cascades to all nested types:

```swift
// If ChatRequest: Sendable, then:
// - ChatMessage must be Sendable
// - ChatToolCall must be Sendable  
// - ChatToolCallFunction must be Sendable
// - AnyJSONValue must be Sendable (if used in fields)
```

Audit the entire tree when adding `Sendable`. A single non-Sendable nested type blocks the whole chain.

### 3. `ResponseEncodable` import errors
After moving structs to the shared package, all server files referencing them need `import LuminaVaultShared`. Use this terminal command to find files missing the import:

```bash
grep -rl "AuthResponse|MeResponse|ChatResponse" Sources/App --include="*.swift" | \
  while read f; do grep -q "import LuminaVaultShared" "$f" || echo "$f"; done
```

### 4. Server-only `init(_ model:)` convenience constructors
Types like `MemoryDTO` have `init(_ memory: Memory)` constructors that depend on the Fluent model — these **must stay server-side** as extensions. The shared package only contains the pure DTO struct. Use factory methods rather than initializers:

```swift
extension MemoryDTO {
    static func fromMemory(_ memory: Memory) throws -> MemoryDTO {
        MemoryDTO(id: try memory.requireID(), ...)
    }
}
// Then replace MemoryDTO.init with MemoryDTO.fromMemory everywhere
```

### 5. Nested DTOs in response types
If a response type contains nested structs (like `AchievementsListResponse.SubDTO`), the client needs access to those nested types. Either:
- Flatten them to top-level types in the shared package
- Keep them nested but make them `public`
- Create server-side typealiases if the client doesn't need them

### 6. Package dependency syntax
When using a local path dependency, the product reference must match the package name:

```swift
.package(path: "LocalMySharedPackage"),  // in dependencies
.product(name: "MySharedPackage", package: "LocalMySharedPackage"),  // in target
```

Mismatched package names cause "unknown package" errors in swift package resolve.

## Migration checklist

1. ✅ Create the shared package with all DTO structs (pure Codable, no platform imports)
2. ✅ Add explicit `init` for structs that need cross-module instantiation
3. ✅ Add `Sendable` to entire conformance chain
4. ✅ Server: strip DTO struct definitions, keep ResponseEncodable extensions
5. ✅ Server: add `import LuminaVaultShared` to all referencing files
6. ✅ Server: replace `init(_ model:)` with `static func fromModel()` extensions
7. ✅ Server: update usage sites to use factory methods
8. ✅ Build and test both packages
9. ✅ Push and tag shared package
10. ✅ Update iOS SPM dependency to new tag
