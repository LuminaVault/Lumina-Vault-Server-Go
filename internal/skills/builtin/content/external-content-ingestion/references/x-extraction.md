# X / fixupx Content Extraction

Techniques for extracting article/tweet content from X (Twitter) URLs, including fallback strategies when direct access fails.

## Primary Method: r.jina.ai

Jina AI's `r.jina.ai/http://URL` returns a clean markdown extraction of any web page, bypassing JavaScript and paywalls (for publicly visible content).

**Usage:**
```bash
curl -s "https://r.jina.ai/http://x.com/username/status/1234567890" | head -n 50
```

**Output format:**
```
Title: <article title>
URL Source: http://x.com/...
Published Time: Sat, 02 May 2026 10:15:12 GMT

Markdown Content:
# <title>
<full article text...>
```

**Pros:**
- No authentication needed
- Handles JavaScript-rendered pages
- Returns clean markdown
- Preserves thread structure for X threads

**Cons:**
- Rate-limited (unknown thresholds)
- May truncate very long threads (>50 tweets)
- Doesn't fetch images/videos (only text + alt text)

## Secondary Method: Direct X.com HTML Scrape

If Jina AI fails or is throttled, fetch X directly and parse HTML.

**Curl with realistic user-agent:**
```bash
curl -s -L -A "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36" "https://x.com/username/status/12345" -o page.html
```

**Key HTML markers (subject to change):**
- Tweet text: `<div data-testid="tweetText">...</div>` or `<span data-testid="tweetText">`
- Article title: `<meta property="og:title" content="...">`
- Publication date: `<meta property="og:article:published_time" content="...">` or `"created_at":"..."`
- Author: `<meta property="og:description" content="@username ...">`

**Parsing approach:**
- Use regex or BeautifulSoup (Python) to extract `data-testid="tweetText"` blocks
- Strip HTML tags from extracted text
- Decode HTML entities (`&lt;` → `<`, `&amp;` → `&`)

**Challenges:**
- X frequently changes class names and data attributes
- Often requires JavaScript to render; static fetch may return minimal HTML
- May show login wall for aggressive scraping

## Tertiary Method: Nitter Instance

Nitter is a static X viewer. Use when both Jina and direct fail.

```bash
curl -s "https://nitter.net/username/status/12345" | grep -A 20 'class="tweet-text"'
```

**Caveats:**
- Public Nitter instances are rate-limited, often down
- Self-hosting Nitter requires Redis + PostgreSQL
- Not reliable for production automation

## Fallback Chain Summary

1. Try `r.jina.ai/http://<url>` → parse markdown
2. If fails (429/500), try direct X fetch + HTML scrape
3. If fails, try Nitter (`https://nitter.net/...`)
4. If all fail, return error with suggestion to retry later

## Error Handling

- **429 Too Many Requests**: Wait 60s, retry once, then skip
- **404 Not Found**: Tweet deleted or account protected — skip, log warning
- **500/502**: Service temporarily unavailable — skip, retry next cycle
- **Empty content**: Tweet may be media-only; record "No text content (image/video only)"

## References

- Jina AI Reader API: https://r.jina.ai/
- Nitter GitHub: https://github.com/zedeus/nitter
- X web app HTML structure changes frequently; inspect live DOM for current selectors
