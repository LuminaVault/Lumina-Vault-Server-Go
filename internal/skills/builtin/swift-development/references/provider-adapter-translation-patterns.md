# Provider Adapter Translation Patterns

When adding new LLM provider adapters to a routing infrastructure (ProviderAdapter protocol), follow this pattern from the GeminiContentsAdapter implementation.

## Core structure

Every adapter implements `ProviderAdapter`:

```swift
struct SomeAdapter: ProviderAdapter {
    let kind: ProviderKind = .someProvider
    private let apiKey: String
    private let session: URLSession
    private let logger: Logger

    func chatCompletionsWithMetadata(payload: Data, profileUsername: String) async throws -> HermesChatTransportMetadata
}
```

## Request translation checklist

1. **Parse inbound JSON** — `JSONSerialization.jsonObject(with:)` as `[String: Any]`
2. **Extract model name** — from `payload["model"]`, resolve to provider-specific name
3. **Map roles:**
   - `user` → native equivalent
   - `assistant` → native equivalent
   - `system` → provider's system field (e.g., `system_instruction`) or fold into first message
4. **Map tools:**
   - `tools[].function` → provider's function declaration format
   - `tool_calls[]` → provider's function call parts
   - `role: "tool"` messages → provider's function response parts
5. **Map generation config:** `temperature`, `max_tokens`, `top_p`, `stop` → provider fields
6. **Build and POST** to `{base}/{model}:generate?key={apiKey}`

## Response translation checklist

1. **Parse JSON** — handle parse failure gracefully
2. **Map content:** text parts → `choices[].message.content`
3. **Map tool calls:** function calls → `choices[].message.tool_calls[]`
4. **Map usage:** provider's token counts → `usage.prompt_tokens/completion_tokens/total_tokens`
5. **Map finish_reason:**
   - STOP → "stop"
   - MAX_TOKENS → "length"  
   - SAFETY/BLOCKLIST → "content_filter"
6. **Wrap in OpenAI shape:** `{"id", "object": "chat.completion", "created", "model", "choices", "usage"}`

## Error classification

Follow the ProviderError pattern:
- 429 → `.transient` (retryable, rate limit)
- 5xx → `.transient` (retryable, upstream error)
- 4xx (except 429) → `.permanent` (not retryable, bad payload)
- Network → `.network` (DNS, TLS, connection reset)

```swift
if status == 429 || (500 ..< 600).contains(status) {
    throw ProviderError.transient(provider: kind, status: status, body: preview)
}
throw ProviderError.permanent(provider: kind, status: status, body: preview)
```

## Pitfalls

- **system_instruction + tools compatibility:** Some providers restrict combining system fields with tool calls. If you get errors here, fall back to folding system into the first user message.
- **Model name resolution:** Build a mapping layer to resolve user-facing aliases (`gemini-pro` → `gemini-2.5-pro`) to API-expected names.
- **Response encoding:** Response must be valid OpenAI JSON — the dispatcher and downstream services expect this shape regardless of which provider was used.
- **No internal retry:** The dispatcher owns retry/failover. Don't retry inside the adapter.
