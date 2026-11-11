# Content Extraction Notes

## Practical web extraction order
1. **jina.ai first** for most article URLs, especially Medium, news, YouTube, and long-form posts:
   ```bash
   curl -s "https://r.jina.ai/http://{URL}"
   ```
2. **Twitter/X oEmbed** for status URLs when jina output is thin or empty:
   ```bash
   curl -s "https://publish.twitter.com/oembed?url={URL}"
   ```
   Then strip HTML and keep the tweet text.
3. **Fallbacks**: RSS/feed endpoints, cached pages, browser automation.

## Gotchas observed
- Medium pages can fetch cleanly through jina.ai even when direct HTML is noisy.
- X status URLs often return better text through oEmbed than through raw page scraping.
- Do not create a placeholder if a real fetch later succeeds.
- If a fetch produced a zero-length or obviously broken placeholder, overwrite/remove it instead of leaving duplicate raw notes behind.

## Recommended ingest hygiene
- Use a deterministic slug derived from the source title or URL.
- Verify the saved file is non-empty before considering ingest complete.
- If a placeholder was created early, replace it with the final content file rather than keeping both.
