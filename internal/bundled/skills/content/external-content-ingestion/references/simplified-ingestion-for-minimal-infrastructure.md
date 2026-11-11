# Simplified Content Ingestion for Minimal Infrastructure Environments

When the full knowledge base infrastructure (kb-config.json, manifest.json, .kb directory) is not available, use this direct approach to get content into the vault.

## Workflow

1. **Fetch content using Jina AI's reader API** (no authentication required):
   ```python
   fetch_url = f"https://r.jina.ai/http://{canonical_url.split('://',1)[1]}"
   response = requests.get(fetch_url, timeout=30)
   ```
   This returns clean article text in the format: `Title\n\nBody...` (split on first `\n\n`).

2. **Extract title and body** from the response.

3. **Generate a filename slug** from the URL:
   - Take the URL path, strip leading/trailing slashes
   - Replace `/`, `.`, `?`, `=`, `&`, `#` with `-`
   - Lowercase, max 60 characters
   - Example: `https://x.com/jacobtechtavern/status/2051594004932759794?s=20` → `jacobtechtavern-status-2051594004932759794-s-20`

4. **Save directly to the vault's Raw directory** with proper frontmatter:
   ```markdown
   ---
   source: <original URL>
   ingested_at: <current UTC ISO timestamp>
   type: web
   status: uncompiled
   ---
   
   # <Extracted title>
   
   <Cleaned body content>
   ```
   Save to: `{VAULT_ROOT}/Raw/{Topic}/{slug}.md` (create directory if missing).

5. **Manually trigger compilation** later with `/kb-compile` when ready.

**This approach is reliable, requires no special dependencies, and aligns with the user's preference for straightforward execution.**

## Dependency Verification

Before attempting to use any external module (e.g., `youtube-transcript-api`, `yt-dlp`), **verify its availability**:

```python
import subprocess
result = subprocess.run([sys.executable, "-c", "import youtube_transcript_api"], 
                       capture_output=True, text=True)
if result.returncode != 0:
    # Fallback to alternative method (e.g., jina.ai extraction)
    # or notify user to install the dependency
```

**Common pitfalls:**
- `youtube-transcript-api` may be listed as installed but actually missing from virtual environments
- System Python may have an "externally managed environment" that blocks pip
- Virtual environments may not include pip by default

Always test import before use and have a fallback plan (e.g., use `yt-dlp` or manual extraction).

## User Preference Embedding

The user has explicitly stated: **"kb-ingest is working properly. Just add the links to the vault."** This indicates a strong preference for:

- **Direct action over complex automation** when the full infrastructure isn't available
- **Simplicity and reliability** over feature completeness
- **Getting content into the vault** as the primary goal, with compilation handled separately

When encountering infrastructure gaps, default to the simplified workflow rather than attempting to set up the full knowledge base system.

## Examples

### X/Twitter Link Example

**URL:** `https://x.com/jacobtechtavern/status/2051594004932759794?s=20`

**Jina AI extraction:**
```bash
curl -s "https://r.jina.ai/http://x.com/jacobtechtavern/status/2051594004932759794?s=20"
```

**Resulting file:** `Raw/web/jacobtechtavern-status-2051594004932759794-s-20.md`

### YouTube Video Example

**URL:** `https://www.youtube.com/watch?v=4T_2FthG0UE`

**Jina AI extraction:**
```bash
curl -s "https://r.jina.ai/http://www.youtube.com/watch?v=4T_2FthG0UE"
```

**Resulting file:** `Raw/web/youtube-4T_2FthG0UE.md`

## When to Use This Approach

- The knowledge base infrastructure files are missing (`kb-config.json`, `manifest.json`, `.kb` directory)
- You need a quick, reliable way to save content without setting up the full pipeline
- The user explicitly requests a straightforward "just add the links" approach
- You're working in an environment with limited dependencies

This simplified method has been successfully tested in the current session and produces vault-ready markdown files that can be compiled later with `/kb-compile`.