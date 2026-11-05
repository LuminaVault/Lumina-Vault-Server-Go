# Swift Wire-Format DTO Patterns

When building Swift types that encode/decode JSON from external APIs (OpenAI, Gemini, etc.), avoid these pitfalls.

## Arbitrary JSON in Codable types

### The `AnyJSONValue` enum pattern (recommended)

```swift
enum AnyJSONValue: Codable, Equatable {
    case string(String)
    case number(Double)
    case bool(Bool)
    case null
    case object([String: AnyJSONValue])
    case array([AnyJSONValue])
}
```

This works for both encoding AND decoding. Use it for fields like tool parameters, arbitrary JSON Schema blobs, or `tool_choice` values.

### Anti-pattern: `AnyEncodable` for round-trip fields

`AnyEncodable` only conforms to `Encodable`, NOT `Decodable`. If a parent struct uses it as a stored property, the parent loses `Decodable` conformance silently (compile error, not runtime). Only use it for write-only fields.

### Anti-pattern: `NSNull` fallback

`NSNull` does not conform to `Encodable`. Using it in a decoder's else branch causes a compile error. Use `.null` from `AnyJSONValue` instead.

## Duplicate type definitions across files

Swift resolves type ambiguity at the module scope. If two files in the same module define `struct AnyEncodable`, any type referring to `AnyEncodable` gets an ambiguous lookup error. The compiler error says "found this candidate" and lists both files.

**Fix:** Keep shared utility types in a single file. Don't create them in multiple files "just in case" — Swift has no file-level privacy for type names.

When you accidentally create duplicates:
1. Pick the canonical file
2. Delete the other definition
3. The ambiguity error clears immediately

## Wire-format field naming

For OpenAPI/OpenAI-style wire formats, prefer `CodingKeys` with snake_case mapping over matching property names:

```swift
struct ChatRequest: Codable {
    let messages: [ChatMessage]
    let tool_choice: AnyJSONValue?   // snake_case property name

    enum CodingKeys: String, CodingKey {
        case messages, model, temperature, stream, tools
        case tool_choice             // maps to "tool_choice" on the wire
    }
}
```

Alternatively, use camelCase property with explicit CodingKey:

```swift
struct ChatRequest: Codable {
    let toolChoice: AnyJSONValue?

    enum CodingKeys: String, CodingKey {
        case toolChoice = "tool_choice"  // explicit wire mapping
    }
}
```

Both work — be consistent within a file.

## Sendable conformance for DTOs

All DTOs that cross actor boundaries (stored in `@Sendable` closures, passed to `Task.detached`) need `Sendable` conformance. Add it to every Codable struct that flows through your service layer:

```swift
struct ChatMessage: Codable, Sendable { ... }
struct ChatRequest: Codable, Sendable { ... }
```

If a struct contains only `Sendable` members (String, Int, arrays of Sendable, optionals of Sendable), `Sendable` conformance is automatic and needs no `@unchecked`.
