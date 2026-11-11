# YouTube and X/Twitter Content Extraction

This reference documents the successful technique for extracting content from YouTube and X/Twitter URLs using jina.ai, developed on May 07, 2026.

## Problem

Direct HTTP requests with BeautifulSoup failed to extract content from:
- YouTube video pages (JavaScript-heavy, anti-bot protections)
- X/Twitter posts (dynamic content, rate limiting)

The standard `requests` + `BeautifulSoup` approach produced empty or minimal content.

## Solution: jina.ai Content Extraction

Use the jina.ai service which renders pages and extracts clean article content:

```
https://r.jina.ai/http://{URL}
```

### Implementation

**Python function:**
```python
import requests

def fetch_url_content_jina(url):
    """Fetch content using jina.ai for robust extraction."""
    try:
        jina_url = f"https://r.jina.ai/http://{url}"
        response = requests.get(jina_url, timeout=15)
        response.raise_for_status()
        return response.text
    except Exception as e:
        print(f"Error fetching {url} via jina: {e}")
        return None
```

### Why It Works

- jina.ai renders JavaScript and extracts the main content
- Handles paywalls and anti-bot protections
- Returns content in clean Markdown format
- Works well with YouTube, X/Twitter, and news sites

### Usage in kb-ingest

When ingesting URLs from platforms known to be problematic with direct scraping:
1. Detect platform from URL (YouTube, x.com, twitter.com)
2. Use jina.ai as the primary extraction method
3. Fall back to other methods if jina.ai fails

### Example Results

**YouTube video:** https://www.youtube.com/watch?v=harQYZVUdQo
- Retrieved full title, description, comments, and metadata
- File size: ~31KB (vs 254 bytes with direct method)

**X/Twitter post:** https://x.com/jacobtechtavern/status/2052318780521357646
- Retrieved full tweet thread with media
- File size: ~4KB

## Benefits

- **Reliable:** Works across multiple platforms
- **Rich content:** Extracts full articles, not just titles
- **Maintainable:** Simple API, no complex browser automation needed
- **Fast:** No need to render JavaScript locally

## When to Use

- YouTube videos
- X/Twitter posts
- News articles with paywalls
- Any site where direct scraping fails

## References

- jina.ai service: https://r.jina.ai
- Original implementation: May 07, 2026 kb-ingest session