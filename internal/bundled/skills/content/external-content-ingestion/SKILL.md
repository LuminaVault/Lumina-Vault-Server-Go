---
name: external-content-ingestion
description: Capture external content (X/Twitter articles, web posts) and ingest into Obsidian vault Raw/ with automatic theme detection, summarization, and structured frontmatter. Handles X/fixupx links via multi-strategy extraction (direct fetch → r.jina.ai → nitter fallback).
license: MIT
---

# External Content Ingestion

Capture articles, threads, or posts from external sources and transform them into vault-ready markdown notes.

## Trigger

User shares a URL in any chat channel (Discord/Telegram/Slack), **OR** automated polling detects X links in recent messages.

**Modes:**
1. **Webhook / realtime** — User posts a URL; immediate ingestion triggered by platform webhook.
2. **Polling (cron)** — Periodic script polls recent messages from configured channels, extracts X URLs, and ingests them automatically. Preferred when webhook infrastructure is unavailable or when batch processing is acceptable.

## Simplified Workflow for Minimal Infrastructure Environments

**When the full knowledge base infrastructure is not available** (no `kb-config.json`, `manifest.json`, or `.kb` directory), use this direct approach:

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

### Dependency Verification

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

### User Preference Embedding

The user has explicitly stated: **"kb-ingest is working properly. Just add the links to the vault."** This indicates a strong preference for:

- **Direct action over complex automation** when the full infrastructure isn't available
- **Simplicity and reliability** over feature completeness
- **Getting content into the vault** as the primary goal, with compilation handled separately

When encountering infrastructure gaps, default to the simplified workflow rather than attempting to set up the full knowledge base system.

## Supported Sources (v2 — extended)

| Source Type | URL Pattern(s) | Fetch Strategy | Notes |
|-------------|----------------|----------------|-------|
| X / Twitter / fixupx | `x.com/*/status/*`, `twitter.com/*/status/*`, `fixupx.com/*/status/*` | `https://r.jina.ai/http://<url>` → plain text extract | Auth-wall detection (skip if login page boilerplate); title from first line |
| GitHub repositories | `github.com/<owner>/<repo>` | GitHub REST API v3 (`/repos/:owner/:repo` + `/readme`) | README may be base64-encoded; handle `encoding` field. Unauthenticated rate limit 60/hr — consider `GITHUB_TOKEN` for higher volume |
| YouTube videos | `youtube.com/watch?v=*`, `youtu.be/*` | YouTube Data API v3 or `yt-dlp` | Fetch title, description, channel, duration, upload date. Optionally fetch auto-generated captions/transcripts. |
| *(future)* Web blogs / articles | Any HTTP(S) URL | `r.jina.ai/http://<url>` reader or direct HTML scrape | Generic fallback |

## YouTube Video Links

**URL Patterns:**\
- `youtube.com/watch?v=<video_id>`\
- `youtu.be/<video_id>`\

**Fetch Strategies:**
1. **YouTube Data API v3** (recommended) — requires API key with `youtube.force-ssl` scope. Fetch video details (title, description, channel, duration, upload date) and optionally captions/transcripts.
2. **`yt-dlp`** (fallback) — command-line tool for downloading video metadata and transcripts. Use `yt-dlp --get-title --get-description --skip-download <url>`.
3. **Web scraping** (last resort) — parse HTML for title and description; may be blocked by anti-bot measures.

**Minimal Implementation (Link Saver):** When full content fetching is not feasible or user prefers simplicity, save only the YouTube link with minimal metadata:
- Extract video ID from URL
- Generate filename: `{date} — {video_title_or_id}.md`
- Frontmatter: `source: YouTube`, `url: <url>`, `date: YYYY-MM-DD`, `tags: [Video]`
- Body: empty or auto-generated summary if desired

This pattern is useful for tracking videos to watch later without storing full transcripts.

**Polling Pattern:** Deploy as a cron job running every 15-30 minutes. The script should:
1. Scan recent conversation history from Discord/Telegram/Slack channels
2. Extract YouTube URLs using regex: `r\\\"(https?://(?:www\\\\.)?(?:youtube\\\\.com/watch\\\\?v=|youtu\\\\.be/)[^\\\\s]+)\\\"`
3. Normalize URLs (convert `youtube.com` to `youtu.be` if needed)
4. Check against state to avoid duplicates
5. For each new URL, fetch video metadata (title, description) using preferred method
6. Save to vault at `Raw/YouTube/{date} — {safe_title}.md`
7. Trigger `kb-compile` after ingest (optional)

**Example Script:** See `references/youtube-link-saver.py` for a complete implementation using session_search and subprocess calls.

**Pitfalls:**
- YouTube API has quota limits (10,000 units/day). Estimating 2-5 units per video, this allows ~2000-5000 videos/day.
- `yt-dlp` may be slow or fail; implement timeouts and retries.
- Video titles can be very long; sanitize and truncate to 120 chars for filename.
- Some videos are age-restricted or private; handle errors gracefully.
- Avoid duplicate detection issues: same video may appear with different URL formats (with/without `www`, `youtube.com` vs `youtu.be`). Normalize before hashing.
- Transcripts may not be available for all videos (user disabled captions, auto-generated only, etc.).

**Related Scripts:**
- `scripts/youtube_link_poller.py` — full-featured poller with metadata fetching
- `scripts/save_youtube_links.py` — minimal link saver (just URLs)
- `scripts/fetch_youtube_transcript.py` — optional transcript fetching module

**Configuration Environment Variables:**
| Variable | Required | Description |
|----------|----------|-------------|
| `YOUTUBE_API_KEY` | For API fetching | Google Cloud API key with YouTube access |
| `YT_DLP_PATH` | For `yt-dlp` fallback | Path to `yt-dlp` executable |

**Note:** When user explicitly requests only links (no transcripts), use the minimal link saver approach to keep implementation simple and reliable.

### 1. Platform Message Fetching

**Discord:** `GET /channels/{channel_id}/messages?limit=50` with `Authorization: Bot <token>` header. Messages newest-first; reverse to oldest-first for ID-based deduplication. Track last processed `id` per channel.

**Telegram:** Use `getUpdates` with `timeout=5` (long-poll) and `offset=<last_update_id+1>`. Messages may be `message` (private chat) or `channel_post` (channel). Track `update_id` cursor in state for incremental fetches. If bot not in any chat, `getUpdates` returns empty — add bot to target chat.

**Slack:** `GET /api/conversations.history?channel=<channel>&limit=50` with `Authorization: Bearer <token>`. Messages newest-first; reverse; track last `ts`.

**Credentials:** Tokens read from environment (`DISCORD_BOT_TOKEN`, `TELEGRAM_BOT_TOKEN`, `SLACK_BOT_TOKEN`). **Important:** When reading tokens from a `.env` file manually (outside Hermes daemon), always `.strip()` values to remove trailing newlines. Hermes daemon strips automatically before export.

**State persistence (v2 schema):**
```json
{
  "discord_last_msg_id": "1498...",
  "telegram_last_update_id": 123456789,
  "slack_last_ts": "1746...",
  "processed_urls": {
    "<sha256(URL)[:16]>": {
      "url": "...",
      "title": "...",
      "saved_at": "...",
      "topic": "...",
      "method": "llm|keyword"
    }
  }
}
```
State file location: `~/.hermes/state/x_link_poller_state.json` (or skill-specific variant). Load at start, save after each run.

### 2. URL Detection

Regex: `r\"(https?://(?:www\\.)?(?:x\\.com|fixupx\\.com|twitter\\.com)/[\\w/]+)\"` applied to message text fields (`content`, `text`, `message` depending on platform). Extract all matching URLs per message. Deduplicate within-run via Python `set`, then globally via state URL-hash.

### 3. Content Fetching (r.jina.ai primary)

**Primary strategy:** Jina AI's reader API — no authentication required:
```python
fetch_url = f\"https://r.jina.ai/http://{original_url.split('://',1)[1]}\"
response = requests.get(fetch_url, timeout=30)
```
Jina returns format: `Title\\n\\nBody...` — split on first `\\n\\n`.

**GitHub repositories (added 2026-05-03):** Use GitHub REST API v3:
```python
import urllib.request, json, base64

api_base = f\"https://api.github.com/repos/{owner}/{repo_name}\"
# 1) Repo metadata
repo = json.loads(urllib.request.urlopen(urllib.request.Request(api_base, headers={\"User-Agent\":\"Hermes\"})).read())
# 2) README (may be base64)
readme_resp = json.loads(urllib.request.urlopen(urllib.request.Request(f\"{api_base}/readme\", headers={\"Accept\":\"application/vnd.github.v3+json\"})).read())
if readme_resp.get(\"encoding\") == \"base64\":
    readme = base64.b64decode(readme_resp[\"content\"]).decode(\"utf-8\", errors=\"replace\")
else:
    readme = readme_resp.get(\"content\", \"\")
```
Returns: title=`repo['name']`, description=`repo['description']`, stars/forks/language/license/dates + full README body.

**⚠️ Auth-wall detection (refined 2026-05-02):** When X blocks the article, r.jina.ai returns HTTP 200 but the body contains X login page boilerplate, not the article text. **Detect by scanning for ANY of these strings:**

- `\"Don't miss what's happening\"` (X header)
- `\"People on X are the first to know\"`
- `\"Sign in to X\"` / `\"Log in to your account\"`
- `\"Create account\"` / `\"Sign up now\"`
- `\"Terms of Service\"` + `\"Privacy Policy\"` + `\"Cookie Use\"` (footer trio)
- `\"Trending now\"` / `\"What's happening\"` / `\"Sports · Trending\"`
- `\"by signing up, you agree to\"` (legal boilerplate)
- `\"\\u00a9 2026 X Corp\"` (copyright footer)

**Action:** If ≥2 of these patterns appear in the extracted body, treat as **auth_wall_blocked**. Do NOT save placeholder file — skip entirely and log `X auth wall — skipped {url}`. The content is genuinely inaccessible without credentials. Exception: if the **title line** (first line after `Title:`) is readable and body length < 200 chars, you may save a minimal stub with title only (use `body: \"[Auth wall — full text blocked]\"`).

**Why skip placeholders:** Vault fill-up with useless `X Article (Protected)` files clutters compile. Only save if at least 100 meaningful, non-boilerplate characters extracted.

**Error handling & skip logic:**
- `HTTP 403` → Protected/gated article. Use placeholder title `\"X Article (Protected)\"` and body `\"This article requires login.\"`
- `HTTP 404` → Deleted. Use `\"X Article (404)\"`.
- Other HTTP errors → `\"Fetch Error\"` with status code.
- Network/other exceptions → `\"Fetch Failed\"` with error string.
- **Auth wall (200 + boilerplate)** → Skip saving entirely. The content is inaccessible. Log: \"X auth wall — skipping {url}\". Do NOT generate placeholder files.
- **Skip saving** if title is `\"Fetch Failed\"`, `\"Fetch Error\"`, `\"Untitled\"`, or body is empty after stripping AND contains X boilerplate patterns. This prevents polluting vault with unreadable junk.

**Protected article special case:** Even when blocking, r.jina.ai often returns the **title line intact** (first line after \"Title:\"). If title is readable but body is boilerplate, classify based on the title + URL path only. Save only if at least 100 meaningful chars extracted; otherwise skip.

### 2. URL Detection

Regex: `r\"(https?://(?:www\\.)?(?:x\\.com|fixupx\\.com|twitter\\.com)/[\\w/]+)\"` applied to message text fields (`content`, `text`, `message` depending on platform). Extract all matching URLs per message; deduplicate per-run via set then globally via state hash.

### 3. Content Fetching (r.jina.ai primary)

**Canonical URL normalization (required):** Before any state lookup or fetch, normalize all X-family URLs to prevent cross-topic duplicates:

```python
def canonicalize_x_url(url: str) -> str:
    parsed = urlparse(url)
    host = parsed.netloc.lower()
    path = parsed.path
    status_match = re.search(r'/status/(\\d+)', path)
    if not status_match:
        return url  # Not a status URL; leave as-is
    tweet_id = status_match.group(1)
    user_match = re.match(r'^/([^/]+)/status/', path)
    username = user_match.group(1) if user_match else 'unknown'
    return f\"https://x.com/{username}/status/{tweet_id}\"
```

Apply canonicalization **immediately after URL extraction** — before state lookup, fetch, classification, or saving. Store both `url` (canonical) and `original_url` (as-received) in frontmatter for traceability.

**Primary strategy:** Use Jina AI's reader API to extract clean article text without authentication:

```python
fetch_url = f\"https://r.jina.ai/http://{canonical_url.split('://',1)[1]}\"
response = requests.get(fetch_url, timeout=30)
```

Jina returns: `Title\\\\n\\\\nBody...` (double-newline separator). Split on first `\\\\n\\\\n`.

**Error handling:**
- `HTTP 403` → Article is protected/gated. Save placeholder title `\"X Article (Protected)\"` and body `\"This article requires login.\"` (classify as `XFeed` or let LLM decide).
- `HTTP 404` → Deleted article. Save `\"X Article (404)\"`.
- Other HTTP errors → `\"Fetch Error\"` with HTTP status code.
- Network exceptions → `\"Fetch Failed\"` with error string.

**Skip saving** if title is `\"Fetch Failed\"`, `\"Fetch Error\"`, `\"Untitled\"`, or body is empty after stripping AND contains X boilerplate patterns. This prevents polluting vault with unreadable junk.

**GitHub-specific notes:**
- Unauthenticated GitHub API rate limit: **60 requests/hour** from a single IP. If you hit `403` with `X-RateLimit-Remaining: 0`, either wait or provide `GITHUB_TOKEN` environment variable (adds 5,000/hr). Include `Authorization: token $GITHUB_TOKEN` header.
- README `content` field may be base64-encoded (check `encoding` key). Decode with `base64.b64decode()`; fallback to plain text if absent.
- Repo may have no README → `readme` field returns `null` or `404`. Treat as empty body and save with description only.
- Large READMEs (>10 KB) are truncated to 10,000 chars for vault size hygiene. Full text remains available via the `url` link.

### 4. Theme Detection & Classification

**Hybrid approach:** Try LLM first (if `OPENROUTER_API_KEY` available), fall back to keyword matching. Record classification method in frontmatter (`classification: llm|keyword`) for audit trail.

**LLM classification (OpenRouter):**
```python
prompt = f\"\"\"Classify this X/Twitter article into ONE of these topics:
AI, Dev/Swift, Stocks, Health, Tech, Business, News, XFeed

Article title: {title}
Article snippet: {content[:1000]}

Respond ONLY with the topic name. No punctuation.\"\"\"
POST https://openrouter.ai/api/v1/chat/completions
Headers: {
  Authorization: Bearer <OPENROUTER_API_KEY>,
  Content-Type: application/json,
  HTTP-Referer: https://hermes-agent.nousresearch.com,
  X-Title: Hermes X-Link Poller
}
Model: anthropic/claude-3-haiku (default; fast + cheap)
Body: {\"model\": \"...\", \"messages\": [{\"role\":\"user\",\"content\":prompt}], \"max_tokens\": 10}
Timeout: 20s
Retry: 3× with exponential backoff (1s, 2s)
```
Validate LLM response against allowed topic list (case-insensitive). If validation fails or LLM unavailable/fails, fall back to keywords.

**Keyword classification:** Lowercase title+body, scan ordered category keyword lists. First match wins. Category order matters: place more specific categories (AI, Dev/Swift, Stocks) before broader ones (Tech, Business, News). Unmatched → `XFeed`.

**Category keyword map (v2 — expanded):**

| Folder | Keywords |
|--------|----------|
| `AI` | ai, openai, claude, llm, gpt, anthropic, hermes, agent, openclaw, model, ml, deepseek, gemini, openrouter, llama, mistral, perplexity, cursor, windsurf, sora, dalle, midjourney, stable diffusion |
| `Dev/Swift` | swift, ios, xcode, apple, uikit, swiftui, vapor, hummingbird, appstore, ipa, macos, visionos, objective-c, wwdc, sf symbols, core data, swiftdata, realm, firebase |
| `Dev/Python` | python, pip, pypi, django, flask, fastapi, pytest, poetry, scrapy, requests, beautifulsoup, selenium, playwright, httpx, lxml, pandas, numpy, scikit-learn, tensorflow, pytorch, jupyter |
| `Dev/Go` | golang, go, gin, echo, fasthttp, standard library, modules |
| `Dev/JS` | javascript, typescript, node, npm, react, vue, next, nuxt, angular, svelte, vite |
| `Stocks` | stock, ticker, amd, googl, zeta, hims, rdw, smr, elf, oust, portfolio, earnings, buy, sell, market, invest, cathie wood, tesla, nvda, celh, msft, meta, amzn, apple, stock pick, watchlist, dow, s&p, nasdaq, fintech |
| `Health` | hims, weight-loss, glp-1, telehealth, biotech, eli lilly, novo, ozempic, wegovy, pfizer, moderna, abbvie, roche, novartis, j&j, fitness, nutrition |
| `Tech` | google, amazon, microsoft, startup, saas, tech, api, cloud, aws, azure, meta, nvidia, intel, qualcomm, chip, semiconductor, gpu, ai chip, hardware, software, github, gitlab, docker, kubernetes |
| `Business` | revenue, profit, startup, funding, acquihire, ipo, valuation, billion, acquires, merger, biz dev, partnership, investment round, series a, series b, vc |
| `News` | breaking, news, report, announcement, update, press release, official, confirmed, leaked |
| `XFeed` | default / fallback |

**GitHub repo classification heuristics (2026-05-03):**
- Primary signal: `repo['language']` (e.g., \"Python\" → `Dev/Python`, \"Swift\" → `Dev/Swift`, \"Go\" → `Dev/Go`, \"JavaScript\" → `Dev/JS`)
- Secondary: README + description keywords (matches above table)
- Fallback: `Dev` if language unknown or mixed
- Always include `source_type: github_repo` in frontmatter for traceability

**CLI override:** If `OPENROUTER_API_KEY` is unset, LLM step is skipped and keywords used directly. No additional dependencies required.

### 5. Markdown Generation

**Frontmatter:**
```yaml
---
source: X (<Platform>)   # Discord / Telegram / Slack
url: <original_url>
date: YYYY-MM-DD
tags: [<Topic>]
classification: llm|keyword
---
```

**Body:**
```markdown
# <Title>

<Body text (max ~5000 chars)>
```

**Filename:** `{date} — {sanitized_title}.md`
Sanitize: replace illegal filesystem chars (`<>:\"/\\\\|?*\\x00-\\x1F`) with `_`; trim to 120 chars.

**Save path:** `{VAULT_ROOT}/Raw/{Topic}/{filename}`
Create directory if missing: `mkdir -p`.

### 6. Post-Ingest Compile

Only if new articles were saved this run. Execute:
```bash
python3 {VAULT_ROOT}/scripts/compile_wiki.py --root {VAULT_ROOT}
```

**⚠️ Compilation Trigger Condition:** The poller **only** triggers compilation when `newly_saved > 0`. If no URLs are found or saved, the compilation step is skipped entirely to avoid unnecessary runs.

**compile_wiki.py Discovery Fallback (2026-05-05):** If the primary script is missing, the poller searches in this order:
1. `{VAULT_ROOT}/scripts/compile_wiki.py`
2. `~/.hermes/scripts/compile_wiki.py`
3. `/opt/data/home/.hermes/scripts/compile_wiki.py`

See `references/multi-link-poller-compilation-behavior.md` for detailed operational notes, including the specific fallback pattern and demo environment limitations observed during the 2026-05-05 session.

**Optional cleanup:** After compile, run the auth-wall cleaner to remove any placeholder files that slipped through (X often changes blocker patterns):
```bash
python3 {skill_dir}/scripts/clean_x_auth_wall.py --root {VAULT_ROOT} --dry-run  # Preview
python3 {skill_dir}/scripts/clean_x_auth_wall.py --root {VAULT_ROOT} --delete   # Remove
```
Compile script must be case-insensitive to `Raw/` vs `raw/` vs `RAW` directories.

## Configuration (Environment Variables)

| Variable | Required | Description |
|----------|----------|-------------|
| `DISCORD_BOT_TOKEN` | For Discord polling | Bot token (Hermes daemon provides) |
| `TELEGRAM_BOT_TOKEN` | For Telegram polling | Bot token |
| `SLACK_BOT_TOKEN` | For Slack polling | Bot token (xoxb-...) |
| `DISCORD_MONITOR_CHANNEL` | Discord channel ID to poll (default: `YOUR_DISCORD_CHANNEL_ID`) |
| `TELEGRAM_HOME_CHANNEL` | Telegram chat ID (default: `476978568`) |
| `SLACK_HOME_CHANNEL` | Slack channel/IM ID (default: `C0B0BDGEJTT`) |
| `OPENROUTER_API_KEY` | Optional | Enables LLM classification |
| `OPENROUTER_MODEL` | Optional | Model ID (default: `anthropic/claude-3-haiku`) |
| `GITHUB_TOKEN` | Optional (recommended for high volume) | GitHub personal access token for higher rate limits (5,000 req/hr vs 60/hr unauthenticated). Used as `Authorization: token $GITHUB_TOKEN` |

- **Duplicate-file race conditions (observed 2026-05-11):** When multiple code execution blocks independently write to the vault for the same post (e.g., fallback writes, retries, or parallel ingestion attempts), the `_unique_path` collision handler appends `-2`, `-3`, etc. instead of deduplicating — leaving orphaned copies. **Fix pattern:** After any multi-block ingestion, verify only one file per canonical URL exists in the target topic dir. If duplicates found, remove all but the most informative one. Future fix: have `_unique_path` return the *existing* file instead of creating a variant when the content is identical.

## Pitfalls

- **Demo script trap:** `/opt/data/scripts/multi_link_poller.py` is a DEMO/TEMPLATE that only processes `--urls` and prints \"platform polling not yet implemented\" when run without arguments. **Always use** `/opt/data/home/.hermes/scripts/x_link_poller_v2.py` for actual platform polling.
- **Infinite-loop daemon behavior:** `x_link_poller_v2.py` runs as a persistent process with `while True: … time.sleep(300)` after each poll cycle. It does **not** accept `--limit` or one-shot flags. For cron/debug, either kill after one cycle or wrap with `timeout`. To stop, `kill` the process; the state file persists cursors so next run resumes correctly.
- **Discord channel ID confusion:** There are two channel env vars: `DISCORD_HOME_CHANNEL` (for bot responses/notifications) and `DISCORD_MONITOR_CHANNEL` (the source channel to poll). These can be different. The poller uses `DISCORD_MONITOR_CHANNEL` (default `YOUR_DISCORD_CHANNEL_ID`). If the bot has Read Message History on a different channel but not the monitor channel, polling fails with `403 Forbidden` even though the token is valid. Verify the bot can actually read the monitor channel.
- **Telegram webhook conflict (409):** `getUpdates` long-poll fails if a webhook is active. Error: `Telegram 409 Conflict: Webhook is active. Long-poll and webhook cannot coexist.` Fix: `curl -X POST \"https://api.telegram.org/bot<TOKEN>/deleteWebhook\"`; ensure `TELEGRAM_WEBHOOK_URL` is unset in `.env`; verify bot is added to the target chat.
- **Environment variable newlines:** `.env` files may store tokens with trailing newlines. When reading `.env` directly in subprocesses, strip values. Hermes daemon automatically strips before export.
- **Discord permission error (403 Forbidden):** The bot token may be valid but the bot lacks **Read Message History** permission in the monitor channel. Error signature: `HTTP 403 on channel <id>: Bot needs 'Read Message History' permission`. Fix: grant the bot's role the permission at server and channel override levels; ensure bot role is above @everyone in role hierarchy.
- **Telegram webhook conflict (409 Conflict):** `getUpdates` long-poll fails if a webhook is active. Error: `Telegram 409 Conflict: Webhook is active. Long-poll and webhook cannot coexist.` Fix: delete webhook via `curl -X POST \"https://api.telegram.org/bot<TOKEN>/deleteWebhook\"` or unset `TELEGRAM_WEBHOOK_URL` and stop any webhook listener. Verify bot is added to the target chat.
- **Telegram bot not in chat:** If the bot hasn't been added to the chat, `getUpdates` returns empty indefinitely. Add bot to chat first.
- **X URL patterns:** Some links use `twitter.com/i/status/...` (redirect/intermediate). The regex captures them; r.jina.ai handles redirects.
- **X article auth wall (most common):** Many X articles (especially long-form \"Notes\") require authentication. All fetch methods (`r.jina.ai`, direct HTML, X API, GraphQL, nitter) will return the login page or empty `articleEntities`. Detect by: (a) HTTP 200 but body contains \"Sign in to X\", (b) `window.__INITIAL_STATE__` shows `\"articleEntities\":{\"entities\":{},\"errors\":{},\"fetchStatus\":{}}`, or (c) Jina returns the generic X login page text. **Action:** Skip saving entirely when ≥2 auth-wall boilerplate patterns detected; log \"X auth wall — skipping {url}\". Content is genuinely inaccessible without credentials.
- **Protected articles (403):** Jina AI returns HTTP 403 for gated content. Save as `\"X Article (Protected)\"` placeholder; LLM may still classify based on title/URL pattern.
- **Rate limits:** Platform APIs (Discord/Telegram/Slack) have rate limits. Polling every 15 min with `limit=50` is safe; do not reduce interval below 1 min.
- **LLM timeouts / 504s:** OpenRouter may timeout under load. Implement 3-retry with exponential backoff; fall back to keyword matching on final failure.
- **State file bloat:** `processed_urls` grows unbounded. Consider periodic pruning (e.g., keep last 1000 entries, or age-based >90 days).
- **Case-sensitive Raw/ detection:** The default `compile_wiki.py` expects lowercase `raw/`. Some vaults use `Raw/`. Patch `compile_wiki.py` to check `['raw', 'Raw', 'RAW']` or rename to lowercase. See `references/case-insensitive-raw-detection.md`.
- **Duplicate detection collision:** SHA256 hash truncated to 16 hex chars; extremely unlikely collision but possible across millions of URLs. If duplicate detection fails, same article may be saved twice.
- **Filename sanitization edge cases:** Long titles, non-ASCII characters, emoji. Current sanitizer replaces illegal filesystem chars and truncates to 120. Consider slugify for more readable ASCII-only filenames if desired.
- **Dependency availability:** Before using external modules (e.g., `youtube-transcript-api`, `yt-dlp`), verify they are actually installed in the current Python environment. Use `python3 -c "import module_name"` to test. If missing, fall back to alternative methods (Jina AI extraction, `yt-dlp` if available) or notify the user.
- **System Python restrictions:** Some systems have "externally managed environments" that block pip. Virtual environments may not include pip by default. Always check for pip in virtual environments and be prepared to use system pip or alternative installation methods.

## Related Skills

- `kb-compile` — compile vault `Raw/` into `wiki/` with index
- `x-html-scrape` — low-level X/Twitter HTML extraction (fallback if r.jina.ai fails)
- `knowledge-base` — unified KB framework including `kb-report`, `kb-healthcheck`
- `content-digests` — periodic digest generation and multi-platform delivery
- `multi-link-poller` **(new)** — unified script handling X/Twitter + GitHub ingestion, deployed as `*/15 * * * *` cron job

## References

- `references/multi-platform-polling.md` — full polling script architecture, state schema, platform-specific message parsing
- `references/telegram-bot-setup.md` — adding bot to chats, `getUpdates` long-poll, offset handling
- `references/telegram-webhook-conflict.md` — **diagnosing and fixing HTTP 409 Conflict when webhook blocks long-poll (2026-05-03)**
- `references/r.jina.ai-usage.md` — extraction limits, protected article handling, format parsing
- `references/openrouter-classification.md` — prompt engineering, model selection, retry logic, fallback strategy
- `references/case-insensitive-raw-detection.md` — patch for `compile_wiki.py` to support `Raw/` vs `raw/`
- `references/x-article-extraction-failure-patterns.md` — **X auth-wall detection patterns, failed extraction log, cleanup procedures (2026-05-02)**
- `references/polling-pattern.md` — state persistence schema and incremental polling loop
- `references/cross-topic-deduplication.md` — **LLM classification duplicate prevention: canonical URL normalization, cross-topic existence check (observed 2026-05-02)**
- `references/cron-job-setup-and-troubleshooting.md` — **one-shot cron wrapper, platform error 403/409 diagnosis and fixes, proper script path (2026-05-03)**
- `references/multi-link-poller-compilation-behavior.md` — **Compilation trigger condition, compile_wiki.py fallback search pattern, and demo environment limitations (2026-05-05)**
- `references/simplified-ingestion-for-minimal-infrastructure.md` — **Direct file saving using Jina AI extraction when full knowledge base infrastructure is missing (2026-05-05)**