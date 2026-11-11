# Twitter/X Post Ingestion via oEmbed Endpoint

**Date:** 2026-05-08  
**URL:** https://x.com/jackprescottx/status/2052837514260471860?s=46  
**Result:** Successful ingestion with clean metadata

## Problem Encountered

Initial attempt to fetch content using jina.ai API returned only the X login page instead of the actual tweet content:

```bash
curl -s "https://r.jina.ai/http://x.com/jackprescottx/status/2052837514260471860?s=46"
```

Output contained only generic X navigation and trending content, missing the tweet itself.

## Solution: Twitter oEmbed Endpoint

Used the official Twitter oEmbed API to get a clean, embeddable representation:

```bash
curl -s "https://publish.twitter.com/oembed?url=https://x.com/jackprescottx/status/2052837514260471860?s=46"
```

**Response:**
```json
{
  "url": "https://twitter.com/JackPrescottX/status/2052837514260471860",
  "author_name": "Jack Prescott",
  "author_url": "https://twitter.com/JackPrescottX",
  "html": "<blockquote class=\"twitter-tweet\"><p lang=\"zxx\" dir=\"ltr\"><a href=\"https://t.co/rnRV9hYIqM\">https://t.co/rnRV9hYIqM</a></p>&mdash; Jack Prescott (@JackPrescottX) <a href=\"https://twitter.com/JackPrescottX/status/2052837514260471860?ref_src=twsrc%5Etfw\">May 8, 2026</a></blockquote>\n<script async src=\"https://platform.twitter.com/widgets.js\" charset=\"utf-8\"></script>\n",
  "width": 550,
  "height": null,
  "type": "rich",
  "cache_age": "3153600000",
  "provider_name": "Twitter",
  "provider_url": "https://twitter.com",
  "version": "1.0"
}
```

## Extracted Information

- **Author:** Jack Prescott (@JackPrescottX)
- **Date:** May 8, 2026
- **Content:** Just a link to https://t.co/rnRV9hYIqM (no additional text)
- **Tweet URL:** https://x.com/jackprescottx/status/2052837514260471860?s=46

## Final Note Content Created

The ingested markdown file includes:
- Frontmatter with source, ingested_at timestamp, type: note, status: uncompiled
- Human-readable summary of the tweet
- Original URL and link

**File saved to:** `/opt/data/obsidian-vault/Vault/raw/Tech/https-x-com-jackprescottx-status-2052837514260471860-s-46.md`

## Key Takeaway

For Twitter/X posts, the oEmbed endpoint often provides cleaner results than jina.ai, especially when the tweet contains only a link or when jina.ai returns the login page. This approach is now the preferred method for X/Twitter ingestion in this environment.