# Source-Specific Extraction Patterns

This document provides guidance on extracting content from various web sources when standard web fetching fails or is inefficient. These patterns are particularly useful for market intelligence gathering and bulk content ingestion.

## 1. GitHub Repository Content

When extracting content from GitHub repositories (like Swift Evolution proposals), use the GitHub API to fetch raw files directly.

### GitHub API Approach
```python
import requests, json, os

# Fetch directory listing
url = "https://api.github.com/repos/owner/repo/contents/path"
headers = {"Accept": "application/vnd.github.v3+json"}
response = requests.get(url, headers=headers)
files = json.loads(response.text)

# Sort by filename (for proposals, higher numbers are more recent)
files.sort(key=lambda x: int(x['name'].split('-')[0]), reverse=True)

# Download each file
for file in files[:N]:
    content = requests.get(file['download_url']).text
    # Save to raw directory with appropriate slug
```

**Advantages:**
- Bypasses JavaScript rendering
- Gets clean markdown/text content
- Works with authentication if needed
- Handles large files efficiently

**Use Cases:**
- Swift Evolution proposals
- GitHub repository documentation
- Code snippets and examples
- Release notes and changelogs

## 2. RSS/Atom Feeds

Many platforms (Stack Exchange, blogs, newsletters) offer RSS/Atom feeds that provide structured content.

### RSS Extraction Pattern
```python
import requests, feedparser

# Fetch RSS feed
feed_url = "https://stackoverflow.com/feeds/tag?tagnames=swift&sort=hot"
response = requests.get(feed_url)

# Parse with feedparser
feed = feedparser.parse(response.content)

# Extract entries
for entry in feed.entries[:5]:
    title = entry.title
    link = entry.link
    published = entry.published
    summary = entry.summary
    # Save with appropriate metadata
```

**Advantages:**
- Clean, structured data
- No JavaScript rendering needed
- Often includes full content
- Works with many platforms

**Use Cases:**
- Stack Overflow questions
- Blog posts and articles
- Newsletter content
- Forum discussions

## 3. oEmbed Endpoints

For social media platforms (Twitter/X, YouTube, etc.), use official oEmbed endpoints to get clean HTML representations.

### Twitter/X oEmbed
```bash
curl -s "https://publish.twitter.com/oembed?url={URL}"
```
Extract the `html` field from the JSON response, which contains the tweet content in a blockquote format with proper attribution.

### YouTube oEmbed
```bash
curl -s "https://www.youtube.com/oembed?url={URL}"
```
Extract the `html` field for an embeddable video player with title and description.

**Advantages:**
- Official, stable APIs
- Clean, embeddable content
- Handles authentication properly
- Good for social proof and references

**Use Cases:**
- Social media posts
- Videos and multimedia
- Embedded content in articles

## 4. Cached Content via Textise dot iitty

For sites protected by Cloudflare or other anti-bot measures, use cached versions via Bing or textise services.

### Bing Cache via Textise dot iitty
```bash
curl -s "https://r.jina.ai/http://cc.bingj.com/cache.aspx?d=504-..."
```
Construct the URL from the browser's cache: view-source of the cached page.

**Advantages:**
- Bypasses Cloudflare challenges
- Gets static HTML content
- Works for paywalled sites sometimes

**Use Cases:**
- News sites with paywalls
- Cloudflare-protected blogs
- Region-locked content

## 5. Browser Automation (Last Resort)

When all else fails and the content is heavily JavaScript-dependent, use browser automation tools.

### Browser Navigation Pattern
```python
# Using browser_navigate tool
result = browser_navigate(url="https://example.com")
# Extract content from the snapshot
```

**Advantages:**
- Renders JavaScript like a real browser
- Can handle complex interactions
- Gets the final DOM state

**Disadvantages:**
- Slower and more resource-intensive
- May trigger bot detection
- Requires browser infrastructure

**Use Cases:**
- JavaScript-heavy SPAs
- Sites with complex authentication
- Interactive content requiring clicks

## 6. jina.ai Universal Extractor

For most websites, especially news sites, blogs, and documentation, jina.ai provides robust content extraction.

### Basic Usage
```bash
curl -s "https://r.jina.ai/http://{URL}"
```

### With Proper URL Encoding
```bash
curl -s "https://r.jina.ai/http://$(python3 -c "import sys, urllib; print(urllib.parse.quote(sys.argv[1]))" "{URL}")"
```

**Advantages:**
- Handles paywalls and popups
- Extracts clean article text
- Works with many site types
- Free and easy to use

**Use Cases:**
- News articles
- Blog posts
- Documentation pages
- Any HTML content

## Decision Flow for Source Extraction

When faced with a new source type, follow this decision process:

1. **Check for RSS/Atom feed** - If available, use it for clean structured data
2. **Check for official API** - GitHub API, Stack Exchange API, etc. for programmatic access
3. **Use jina.ai** - For general web pages and articles
4. **Use oEmbed** - For social media and videos
5. **Use cached content** - For Cloudflare-protected sites
6. **Use browser automation** - As a last resort for JS-heavy sites

## Best Practices

- **Always save raw API responses** to files before processing to avoid security risks
- **Implement error handling and retries** for network requests
- **Test writability** of the vault before bulk ingestion
- **Use appropriate subdirectories** based on content topic (AI, Tech, Business, etc.)
- **Include proper frontmatter** with source, ingested_at, type, and status
- **Consider content licensing** and fair use when ingesting third-party content

## Example: Swift Evolution Proposals

For Swift Evolution proposals, the GitHub API approach is ideal:

```python
import requests, json, os, re

# Get list of proposals
url = "https://api.github.com/repos/swiftlang/swift-evolution/contents/proposals?ref=main"
headers = {"Accept": "application/vnd.github.v3+json"}
response = requests.get(url, headers=headers)
proposals = json.loads(response.text)

# Sort by number (highest first)
proposal_files = []
for prop in proposals:
    match = re.match(r'^(\d{4})-', prop['name'])
    if match:
        proposal_num = int(match.group(1))
    else:
        proposal_num = 0
    proposal_files.append((proposal_num, prop))

recent_proposals = sorted(proposal_files, key=lambda x: x[0], reverse=True)[:10]

# Download each proposal
for num, prop in recent_proposals:
    content = requests.get(prop['download_url']).text
    filename = prop['name']
    path = f"/opt/data/obsidian-vault/Vault/raw/Swift/{filename}"
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(content)
```

This pattern can be adapted for any GitHub-hosted content.