# X Article Extraction Failure Patterns

## Problem
X/Twitter blocks automated access. Even r.jina.ai (the primary fetch method) returns HTTP 200 with X's login page HTML instead of article content when:
- The tweet is from a protected/locked account
- The tweet is behind a login wall (most X articles require sign-in since 2023)
- Cloudflare/anti-bot challenges are triggered
- The content is a "Note" (long-form article) requiring JavaScript rendering

## Detection: Auth Wall vs Real Content

### Authentic article indicators (KEEP):
- First line after "Title:" contains the actual tweet/thread text
- Presence of media URLs (pbs.twimg.com, video links)
- Quote-tweet attribution blocks
- Author handle lines starting with `@`

### X auth wall boilerplate patterns (SKIP):
**Always present in login page returns:**
```
Don't miss what's happening
People on X are the first to know.
Log in
Sign up
# [](http://x.com/)
## Post
See new posts
# Conversation
```

**Footer boilerplate:**
```
## New to X?
Sign up now...
Terms of Service
Privacy Policy
Cookie Policy
© 2026 X Corp.
```

**Trending widgets:**
```
## Trending now
## What's happening
Sports · Trending
...
```

**Empty placeholder:**
```
## Post
## Conversation
[![Image](...profile image...)]
[@Username](...)
[Content]
```
...then immediately footer boilerplate with no actual text between.

### Detection function (Python)

```python
def is_x_auth_wall(content):
    """Returns True if content is X login page rather than article."""
    boilerplate_phrases = [
        "Don't miss what's happening",
        "People on X are the first to know",
        "Sign in to X",
        "Log in to your account",
        "Create account",
        "Terms of Service",
        "Privacy Policy",
        "What's happening",
        "Trending now",
        "See new posts",
        "## Post",
        "## Conversation",
    ]
    hit_count = sum(1 for phrase in boilerplate_phrases if phrase.lower() in content.lower())
    return hit_count >= 3  # 3+ boilerplate phrases = auth wall
```

## Fallback Chain

**Current fallback order:**
1. Direct HTML fetch → parse `window.__INITIAL_STATE__` JSON (requires non-JS page)
2. r.jina.ai extraction (primary; may return auth wall)
3. Nitter instances (all often down or blocked):
   - `https://nitter.net`
   - `https://nitter.1d4.us`
   - `https://nitter.kavin.rocks`
   - `https://nitter.it`
4. Give up — skip file, log URL

**When r.jina.ai returns auth wall:**
- Attempt to extract title from the first "Title:" line anyway (it's often preserved)
- If title is non-empty and body is 90%+ boilerplate, **skip saving entirely** to avoid polluting vault with unreadable placeholders
- If confidently classified by title alone (LLM/keyword), you may optionally save a tiny note (title only) for reference; default is skip.

## Cleanup After Failure

If placeholders were already saved before the auth-wall guard was added:

```bash
# Remove files containing only boilerplate
find Raw/ -name "*.md" -exec grep -l "People on X are the first to know" {} \; -delete
# Or more aggressively: delete all files < 200 bytes that match X footer patterns
find Raw/ -size -1k -exec grep -l "Terms of Service" {} \; -delete
```

## Known Failure Log (2026-05-02 session)

| URL | Author | Method | Result | Reason |
|---|---|---|---|---|
| `x.com/Teknium/20504733` | Teknium | r.jina.ai | Partial content (boilerplate + quote-tweet visible) | Auth wall but quote block leaked through |
| `x.com/edwardsanchez/20499071` | Edward Sanchez | r.jina.ai | Empty (auth wall) | Full block |
| `x.com/KSimback/20505514` | Kevin Simback | r.jina.ai | Partial content (boilerplate + main tweet) | Auth wall but main text present |
| `x.com/gittrend0x/20505721` | GitTrend | r.jina.ai | Full Chinese content | Success (no auth wall) |

**Observation:** Success rate ~25% even with r.jina.ai. Most tweets are behind auth as of 2026.

## References

- Skill: `external-content-ingestion` — main ingestion pipeline
- Related: `x-html-scrape` — low-level X HTML parsing (requires non-JS page, rarely usable)
- Alternative: `xurl` CLI with OAuth tokens (requires credentials; not available in Hermes agent)