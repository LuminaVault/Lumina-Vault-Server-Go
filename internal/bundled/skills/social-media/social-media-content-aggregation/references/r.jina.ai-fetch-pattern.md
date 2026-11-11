# r.jina.ai — Article Extraction Pattern

## What It Is

`https://r.jina.ai/http://<url>` is a free, public HTTP service that:
1. Fetches the URL (follows redirects)
2. Strips HTML boilerplate (nav, ads, scripts)
3. Returns clean article text in **Markdown** format
4. Adds a `# Source: <original-url>` header line

Used in the X Link Poller to convert X post URLs into readable article bodies for LLM classification.

## Basic Usage

```bash
curl -sL "https://r.jina.ai/http://x.com/username/status/1234567890" | head -50
```

Output:
```
# Source: https://x.com/username/status/123456456

This is the extracted text of the X post...
```

## Python Integration

```python
import urllib.request
import urllib.error

def fetch_article(url: str) -> str:
    """Fetch article text via r.jina.ai reader service."""
    encoded = urllib.parse.quote(url, safe='')
    reader_url = f"https://r.jina.ai/http://{encoded}"
    req = urllib.request.Request(reader_url, headers={'User-Agent': 'Hermes/1.0'})
    try:
        resp = urllib.request.urlopen(req, timeout=15)
        return resp.read().decode('utf-8')
    except urllib.error.HTTPError as e:
        return f"[ERROR {e.code}: {e.reason}]"
```

## Rate Limits & Reliability

- **Rate limit:** Soft limit ~30–60 RPM per IP (no official docs, throttle if you hit `429`).
- **Uptime:** High but not guaranteed; add fallback to direct `requests` or mark article as `fetch_failed` if `5xx`.
- **Caching:** r.jina.ai results are cacheable (CDN). Polling the same URL twice within minutes often returns cached body.

## Fallback Strategy

When r.jina.ai fails:
1. Retry with exponential backoff (3 attempts, 1s → 2s → 4s delays)
2. If still failing, save article with `body: "[FETCH_FAILED]"` and still record URL in state to avoid re-fetch spam
3. Optionally, try a secondary fetcher like `https://r.jina.ai/http://cc.bingj.com/cache.aspx?d=...` or just store the raw URL

## Security Notes

- The service executes JavaScript in a sandbox; use HTTPS URLs only.
- Do **not** use for authentication-protected content (r.jina.ai is unauthenticated).
- Long articles (> 200 KB) may be truncated.

## Alternatives (if r.jina.ai is unavailable)

| Service | Pros | Cons |
|---|---|---|
| `https://r.jina.ai/http://URL` | Free, fast, Markdown | Rate-limited, public |
| `https://r.jina.ai/http://cc.bingj.com/cache.aspx?...` | Bing cache fallback | Complex, not always available |
| `https://r.jina.ai/http://r.jina.ai/http://URL` | (double-nesting) | No benefit, same service |
| Direct `requests.get(url)` + `readability-lxml` | Full control, no third-party | Needs `pip install readability-lxml`, slower |
| `https://r.jina.ai/http://r.jina.ai/http://URL` | mirrors | not reliable |

## References

- GitHub: https://github.com/jina-ai/reader (open-source, self-hostable)
- API is intentionally simple: prepend `https://r.jina.ai/http://` to any URL
- Use `https://r.jina.ai/http://` (not `https://`) — the service expects the target URL as path parameter after `http://`
