# r.jina.ai Article Extraction

## Overview

Jina AI's Reader API (`https://r.jina.ai/http://<url>`) extracts clean markdown from web pages without authentication. Perfect for X/Twitter article ingestion when the X API is unavailable or rate-limited.

## Usage

```python
import requests
fetch_url = f"https://r.jina.ai/http://{url.split('://',1)[1]}"
resp = requests.get(fetch_url, timeout=30)
text = resp.text
```

**Important:** The `http://` prefix inside the r.jina.ai path is required. Do NOT URL-encode the inner URL; place it directly after `/http://`.

Examples:
- Input: `https://x.com/jeremynguyenphd/status/2050377302207524867`
- Fetch: `https://r.jina.ai/http://x.com/jeremynguyenphd/status/2050377302207524867`

## Response Format

Jina returns plain text (UTF-8). Typical structure:

```
# Article Title Here

First paragraph of the article body...

Second paragraph...
```

- Title: first line, often starting with `# ` (H1 markdown). Strip `#` prefix.
- Body: everything after the first blank line (`\n\n`). Split on the *first* double-newline.

Parsing code:
```python
lines = text.split("\n\n", 1)
title = lines[0].strip()
body = lines[1].strip() if len(lines) > 1 else ""
```

## HTTP Status Handling

| Code | Meaning | Action |
|------|---------|--------|
| `200` | Success | Parse title + body |
| `403` | Forbidden / Protected | Article requires login (X Premium, private account). Save placeholder: title `"X Article (Protected)"`, body `"This article requires login."` |
| `404` | Not found | Deleted or inaccessible. Save `"X Article (404)"` |
| `429` | Rate limited | Back off per `Retry-After` header. Retry after delay. |
| `500` / `502` / `503` | Server error | Retry with backoff (3×); if still failing, save `"Fetch Error"` |

## Rate Limits & Etiquette

- Jina AI is free but throttles aggressive usage. A cron job polling 3 platforms every 15 minutes is well within polite limits (~12 requests/hour max).
- Set a custom `User-Agent` header if desired (e.g., `"Hermes-X-Poller/1.0"`). Not required.
- Do NOT hammer with concurrent requests; serialize per-URL.

## Edge Cases

- **Protected articles** (`403`): Jina cannot bypass login walls. Save as protected placeholder. LLM classifier may still infer topic from URL/title pattern.
- **Redirects:** Jina follows redirects automatically. `twitter.com` → `x.com` handled transparently.
- **Multimedia:** Images, videos, polls are ignored; body text only. Media URLs may appear as plain links in extracted text.
- **Long threads:** Jina typically extracts the full thread text (concatenated). Body may be lengthy; truncate to 5000 chars if storage is a concern.
- **Formatting:** Jina preserves basic markdown (links, bold, code). Complex layouts (tables, embedded tweets) may degrade.
- **Date metadata:** Jina output sometimes includes a `Published Time:` line before the body. If needed, parse it:
  ```python
  import email.utils
  if text.startswith("Published Time:"):
      date_line, body = text.split("\n\n", 1)
      published = email.utils.parsedate_to_datetime(date_line.split(":",1)[1].strip())
  ```

## Alternatives (if r.jina.ai fails)

1. **Direct HTML fetch + BeautifulSoup** — requires User-Agent rotation, may be blocked.
2. **Nitter instances** — `https://nitter.net/<user>/status/<id>` returns static HTML; fragile, rate-limited, often down.
3. **X API v2** — requires bearer token, paid tier for recent search. Use only for guaranteed completeness.

## References

- Jina AI Reader API: https://github.com/jina-ai/reader
- Rate limits: undocumented; conservative <60 req/min recommended
