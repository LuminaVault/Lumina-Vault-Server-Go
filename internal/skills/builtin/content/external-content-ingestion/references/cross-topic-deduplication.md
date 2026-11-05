# Cross-Topic Duplicate Prevention for LLM-Classified Ingestion

**Skill:** `external-content-ingestion`  
**Discovered:** 2026-05-02 — Poller v2 run observed duplicate file in `AI/` and `Dev/Swift/` for the same X URL  
**Severity:** Medium — vault clutter, fragmented article threads

---

## Problem

When using an LLM to classify incoming content into topic folders, the **same URL** may be classified into **multiple topics** across:
- Different poll cycles (LLM prediction variance)
- Different URL representations (x.com vs fixupx.com vs twitter.com/i/status)
- Platform message variants (same article posted in multiple Discord/Telegram/Slack channels)

**Observed case:** `https://x.com/edwardsanchez/status/2049907157341180299` appeared in both:
- `Raw/AI/2026-05-02 — X - Edward Sanchez - 20499071.md` (class: AI)
- `Raw/Dev/Swift/2026-05-02 — X - Edward Sanchez - 20499071.md` (class: Dev/Swift)

State file only prevented re-processing of the exact URL. `fixupx.com` variant was in state; `x.com` variant was not, so LLM re-classified and saved to a second topic.

---

## Detection

**Symptom:** Identical or near-identical filenames (same date, similar title) appearing in multiple topic directories after a poll cycle.

**Check:** Search for files with matching tweet IDs or URL query parameters across `Raw/*/`:
```bash
grep -r "status/2049907157341180299" /opt/data/obsidian-vault/Vault/Raw/
```

**State audit:** Cross-reference `processed_urls` entries by URL hash:
- Compute SHA256 of all known URL variants
- If same content ID appears under multiple hashes → cross-topic duplication risk

---

## Solution Pattern — Canonical URL Normalization

**Before** storing a URL in state or passing to classifier, **normalize** to a canonical form:

```python
import re
from urllib.parse import urlparse, parse_qs

def canonicalize_x_url(url: str) -> str:
    """Normalize any X/fixupx/twitter URL to x.com/<user>/status/<id>."""
    parsed = urlparse(url)
    host = parsed.netloc.lower()
    path = parsed.path

    # Extract tweet ID from various patterns
    # x.com/user/status/1234567890
    # fixupx.com/user/status/1234567890
    # twitter.com/i/status/1234567890
    # x.com/user/status/123?some=param

    # Strip trailing query string; ID is last numeric path segment
    status_match = re.search(r'/status/(\d+)', path)
    if not status_match:
        return url  # Not a status URL; return as-is

    tweet_id = status_match.group(1)
    # Extract username (precedes /status)
    user_match = re.match(r'^/([^/]+)/status/', path)
    username = user_match.group(1) if user_match else 'unknown'

    return f"https://x.com/{username}/status/{tweet_id}"

# Usage at ingestion entry point:
normalized_url = canonicalize_x_url(raw_url)
url_hash = hashlib.sha256(normalized_url.encode()).hexdigest()[:16]
# Use normalized_url for state key, classification, and saved frontmatter
```

**Result:** Both `fixupx.com/edwardsanchez/status/2049907157341180299` and `x.com/edwardsanchez/status/2049907157341180299` normalize to the same canonical URL → same hash → deduplicated in state → single save pass → only one topic assignment (the first classification wins, or re-classify deterministically).

---

## Alternative: Cross-Topic Existence Check

If canonicalization is undesirable (you want to preserve original URL in frontmatter), check **all topic directories** for an existing file with the same canonical ID:

```python
def already_saved(vault_root: Path, topic: str, canonical_url: str) -> bool:
    """Check if ANY Raw/ topic folder already contains this URL."""
    tweet_id = extract_tweet_id(canonical_url)
    for topic_dir in vault_root.glob('*'):
        if not topic_dir.is_dir() or topic_dir.name == 'scripts':
            continue
        for md in topic_dir.glob('*.md'):
            try:
                fm = parse_frontmatter(md.read_text())
                if canonicalize_x_url(fm.get('url', '')) == canonical_url:
                    return True
            except:
                continue
    return False

# During ingestion:
if url_hash in processed:
    continue
if already_saved(VAULT_ROOT, topic, canonical_url):
    log.info(f"  Duplicate across topics — skipping save")
    continue
```

**Trade-off:** Adds filesystem I/O per URL; slower but preserves original topic classification on first-save.

---

## Recommended Setup

Apply **both** strategies in order:

1. **Canonicalize** before state lookup → prevents variant URLs from entering state separately
2. If canonical URL not in state but found across any topic folder → skip (race-safe since single poller instance)

This ensures:
- No duplicate files regardless of LLM classification variance
- No duplicate work (fetch+LLM call) for known URLs
- Original frontmatter still shows the platform-specific URL (store `original_url` field separately if preservation is required)

---

## Configuration

Add to SKILL.md `### Configuration` section:

| Variable | Default | Description |
|----------|---------|-------------|
| `NORMALIZE_X_URLS` | `true` | Canonicalize all X/fixupx/twitter URLs to x.com format before state deduplication |
| `ALLOW_CROSS_TOPIC_DUPLICATES` | `false` | If `false`, check all Raw/*/ folders before saving (slower, safer) |

---

## Implementation Checklist

- [ ] Add `canonicalize_x_url()` helper to ingestion script
- [ ] Apply canonicalization at: state key generation, `processed_urls` lookup, and `already_saved()` check
- [ ] Update `save_to_vault()` to record both `url` (canonical) and `original_url` (as-received) in frontmatter
- [ ] Maintain backward compatibility: state entries with old-style URLs continue to work (canonicalize on load)
- [ ] Add unit tests for: `x.com`, `fixupx.com`, `twitter.com/i/status`, URL-with-query variants
- [ ] Patch `external-content-ingestion` SKILL.md with canonicalization pattern

---

## Related References

- `external-content-ingestion/references/cross-topic-deduplication.md` (this file)
- `external-content-ingestion/references/url-normalization-edge-cases.md` — query params, mobile URLs, t.co wrappers
- `external-content-ingestion/references/duplicate-audit-script.md` — script to find and merge cross-topic duplicates
