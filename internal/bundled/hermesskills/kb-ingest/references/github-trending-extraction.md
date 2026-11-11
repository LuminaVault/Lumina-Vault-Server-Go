# GitHub Trending Extraction via Browser Snapshots

When direct API access or raw content URLs fail (e.g., GitHub Trending pages require JavaScript rendering), use this browser automation + snapshot parsing technique.

## When to Use
- GitHub Trending pages (or any JavaScript-heavy site)
- Sites with anti-bot protections that block direct scraping
- When jina.ai or similar content extractors fail to retrieve structured data

## Workflow

### 1. Navigate to the Target Page
Use `browser_navigate` to load the page with proper headers and stealth settings.

```bash
browser_navigate(url="https://github.com/trending/swift")
```

### 2. Capture a Full Snapshot
Use `browser_snapshot` with `full=true` to capture the complete DOM, including dynamically loaded content.

```bash
browser_snapshot(full=true)
```

This returns a structured text representation of the page elements, including repository cards.

### 3. Parse the Snapshot
Extract repository information from the snapshot using pattern matching. Each repository appears as an `article` block with consistent structure:

**Key Elements to Extract:**
- Repository owner and name (from `heading` element)
- Description (from `StaticText` within `paragraph`)
- Star count (from `link "star X"`)
- Fork count (from `link "fork Y"`)
- Stars today (from `StaticText "Z stars today"`)

### 4. Sample Parsing Logic
```python
import re

# Parse snapshot lines
repos = []
current_repo = None

for line in snapshot_lines:
    line = line.strip()
    
    # Detect repository heading
    if 'heading "' in line and ' / ' in line and ' [level=2,' in line:
        match = re.search(r'heading "([^"]+) / ([^"]+)"', line)
        if match:
            owner, repo_name = match.groups()
            current_repo = {
                'owner': owner,
                'name': repo_name,
                'description': '',
                'stars': '',
                'forks': '',
                'stars_today': ''
            }
    
    # Extract description from paragraph block
    if current_repo and 'paragraph' in line:
        # Look ahead for the StaticText within this paragraph
        k = lines.index(line) + 1
        while k < len(lines) and 'paragraph' not in lines[k]:
            if 'StaticText "' in lines[k]:
                desc_match = re.search(r'StaticText "([^"]+)"', lines[k])
                if desc_match:
                    current_repo['description'] = desc_match.group(1)
                    break
            k += 1
    
    # Extract stars
    if current_repo and 'link "star' in line and not current_repo['stars']:
        star_match = re.search(r'link "star ([\d,]+)"', line)
        if star_match:
            current_repo['stars'] = star_match.group(1)
    
    # Extract forks
    if current_repo and 'link "fork' in line and not current_repo['forks']:
        fork_match = re.search(r'link "fork ([\d,]+)"', line)
        if fork_match:
            current_repo['forks'] = fork_match.group(1)
    
    # Extract stars today
    if current_repo and 'StaticText "([^"]+ stars today)"' in line and not current_repo['stars_today']:
        today_match = re.search(r'StaticText "([^"]+ stars today)"', line)
        if today_match:
            current_repo['stars_today'] = today_match.group(1)
    
    # End of article block
    if current_repo and 'heading "' in line and ' / ' in line and ' [level=2,' in line:
        repos.append(current_repo)
        current_repo = None
```

### 5. Save to Knowledge Base
Format extracted data with proper frontmatter and save to the appropriate raw subdirectory (e.g., `raw/Swift/` for Swift repositories).

## Advantages
- Bypasses JavaScript rendering requirements
- Works around API rate limits and authentication barriers
- Captures dynamically loaded content
- Provides structured data from otherwise inaccessible pages

## Limitations
- Requires browser automation tools (browser_navigate, browser_snapshot)
- Snapshot parsing can be fragile if page structure changes
- Slower than direct API access
- May trigger bot detection on some sites

## Best Practices
1. Always test with a small subset first
2. Handle edge cases where data might be missing
3. Add error handling for parsing failures
4. Consider using residential proxies for high-volume scraping
5. Cache snapshots for retry or debugging

## Example Use Cases
- GitHub Trending repositories by language
- Stack Overflow trending tags
- Hacker News front page analysis
- Any JavaScript-heavy site requiring content extraction

## Related References
- `references/content-extraction.md` - General content extraction strategies
- `references/github-api-access.md` - Direct GitHub API integration patterns
- `references/jina-ai-extraction.md` - jina.ai content extraction guide