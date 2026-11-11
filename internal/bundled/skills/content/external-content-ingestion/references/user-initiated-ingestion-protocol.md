## User-Initiated Ingestion Requests

**Important User Preference:** When the user explicitly asks to add a link to the vault (e.g., "Add the link to the vault", "Save this content", "Please ingest this"), they are giving a direct instruction to take immediate action. **Do not wait for automatic pipelines or polling cycles.**

### Action Protocol for User Requests:

1. **Prioritize the request** — treat it as a high-priority task that should be executed immediately
2. **Use the simplified manual approach** if automatic methods are unavailable or would cause delay:
   - Fetch content directly using Jina AI (`r.jina.ai`) or browser tools
   - Extract title and body
   - Save to vault with proper frontmatter
   - This bypasses the full knowledge base infrastructure but achieves the core goal: **getting content into the vault**
3. **If automatic ingestion would succeed quickly**, you can still use it, but only if it doesn't cause noticeable delay
4. **Always confirm completion** to the user so they know the request was fulfilled

**Why This Matters:** The user values reliability and speed for explicitly requested content. They've indicated a strong preference for direct action over complex automation when they've made a specific request. This aligns with their broader preference for straightforward execution and getting content into the vault as the primary goal.

### Auth Wall Handling:

When fetching X/Twitter content, be aware that many tweets are behind auth walls. If r.jina.ai returns boilerplate (detect with ≥2 auth-wall patterns), the content is inaccessible via automatic methods. In such cases:

- **For automatic ingestion:** skip saving entirely to avoid polluting vault with placeholders
- **For user requests:** optionally save a minimal stub with title only (use `body: "[Auth wall — full text blocked]"`) for reference; default is to skip

The key principle: **When the user explicitly asks, take direct action.**