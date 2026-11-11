# GitHub Repository Ingestion Pattern

**Skill:** `external-content-ingestion`  
**Source type:** `github_repo`  
**Deployed:** 2026-05-03 — used by `multi_link_poller.py`

## Purpose

Fetch full repository metadata + README from GitHub and save as a structured Obsidian note in `Raw/Dev/...`.

## Canonical Implementation

```python
import urllib.request, json, base64, hashlib
from datetime import datetime
from pathlib import Path

def fetch_github_repo(url: str) -> dict | None:
    """Fetch repo metadata and README from GitHub API.
    Returns dict with keys: title, description, stars, forks, language,
    license, created, updated, readme (full text, truncated to 10k).
    """
    try:
        m = re.search(r'github\.com/([^/]+)/([^/]+)', url)
        if not m:
            return None
        owner, repo_name = m.group(1), m.group(2)
        api_base = f"https://api.github.com/repos/{owner}/{repo_name}"
        headers = {"User-Agent": "Hermes-Agent/1.0"}

        # Repo metadata
        with urllib.request.urlopen(urllib.request.Request(api_base, headers=headers), timeout=15) as r:
            repo = json.loads(r.read().decode())

        # README (may be base64)
        readme_url = f"{api_base}/readme"
        with urllib.request.urlopen(urllib.request.Request(readme_url, headers={**headers, "Accept": "application/vnd.github.v3+json"}), timeout=15) as r2:
            readme_data = json.loads(r2.read().decode())

        if readme_data.get("encoding") == "base64":
            readme = base64.b64decode(readme_data["content"]).decode("utf-8", errors="replace")
        else:
            readme = readme_data.get("content", "") or ""

        return {
            "title": repo.get("name", repo_name),
            "description": repo.get("description") or "",
            "stars": repo.get("stargazers_count", 0),
            "forks": repo.get("forks_count", 0),
            "language": repo.get("language") or "",
            "license": (repo.get("license") or {}).get("name") if repo.get("license") else None,
            "created": (repo.get("created_at") or "")[:10],
            "updated": (repo.get("updated_at") or "")[:10],
            "readme": readme[:10000],  # truncate for vault size
        }
    except Exception as e:
        print(f"[warn] GitHub fetch failed: {url} — {e}", file=sys.stderr)
        return None
```

## Classification Heuristics

Primary signal: `repo['language']` field
- `Python` → `Dev/Python`
- `Swift` → `Dev/Swift`
- `Go` → `Dev/Go`
- `JavaScript` / `TypeScript` → `Dev/JS`
- `Jupyter Notebook` → `AI`
- Unknown / None → fall back to README + description keywords → `Dev`, `Tech`, or `XFeed`

Secondary: scan `description + README[:2000]` for keywords:
- `"scrap", "crawler", "spider", "selenium", "playwright", "httpx", "beautifulsoup", "scrapy"` → `Dev/Python`
- `"framework", "library", "package"` → `Tech`

## Frontmatter Template

```yaml
---
source: GitHub
url: <canonical_url>
date: YYYY-MM-DD
tags: [Dev/Python]
classification: keyword
url_hash: <sha256[:16]>
source_type: github_repo
---
```

## Pitfalls

| Pitfall | Symptom | Fix |
|---------|---------|-----|
| Unauthenticated rate limit (60/hr) | GitHub API returns `403` with `X-RateLimit-Remaining: 0` after ~60 repo fetches | Set `GITHUB_TOKEN` environment variable (personal access token). Increases limit to 5,000/hr. Add header `Authorization: token $GITHUB_TOKEN` |
| README is `null` | Repo has no README → `readme_data.get('content')` returns `None` | Guard: `readme = readme_data.get('content') or ""` before base64 check |
| Base64 padding issues | `binascii.Error: Incorrect padding` on decode | Add padding: `content += '=' * (-len(content) % 4)` before `b64decode` |
| Large README bloat | Vault fills with 100 KB notes | Truncate README to 10,000 chars (configurable); keep link to full repo in frontmatter |
| Private repos | API returns `404` even with token | Skip private repos — not meant for public vault ingestion |

## Environment

- No additional Python deps (uses stdlib `urllib`, `json`, `base64`)
- Optional: `requests` not needed for GitHub fetch (uses `urllib` for consistency)
- Token: `GITHUB_TOKEN` in environment or `/opt/data/.env`

## Example Output File

`Raw/Dev/Python/2026-05-03 — Scrapling.md`:
```markdown
---
source: GitHub
url: https://github.com/D4Vinci/Scrapling
date: 2026-05-03
tags: [Dev/Python]
classification: keyword
url_hash: a1b2c3d4e5f6a7b8
source_type: github_repo
---

# Scrapling

**Description:** 🕷️ An adaptive Web Scraping framework that handles everything from a single request to a full-scale crawl!

**Metadata**
- Stars: 42,312
- Forks: 3,840
- Language: Python
- License: MIT
- Created: 2023-05-14
- Updated: 2026-05-02

## README

<full or truncated README body>
```