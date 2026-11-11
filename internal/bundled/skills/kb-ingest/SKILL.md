---
name: kb-ingest
description: Ingest a URL, PDF, image path, or plain text note into your personal knowledge base Raw directory. This skill adapts to the actual Hermes knowledge base structure.
trigger: /kb-ingest
metadata:
  capability: medium
---

# KB Ingest

Stage content into the knowledge base `raw` directory. Does not compile or modify the wiki.

## Important: Vault Structure

The Hermes knowledge base at `/opt/data/obsidian-vault/Vault` uses a different structure than described in older documentation:

- **Raw directory:** `raw/` (lowercase) not `Raw/`
- **Subdirectories:** `AI`, `Books`, `Business`, `Careers`, `Health`, `NBA`, `News`, `Stocks`, `Swift`, `TV and Movies`, `Tech`, `WWE`
- **Wiki output:** Compiled pages go to `wiki/` (lowercase) not `Wiki/`

**Note on Content Organization:** Unlike the type-based subdirectories (`raw/web/`, `raw/pdfs/`, etc.) mentioned in some steps, the actual vault organizes content by topic (AI, Tech, Business, etc.). When ingesting content, choose the subdirectory that best matches the content's topic. For mixed or general content, use `raw/AI` or `raw/Tech` as defaults.

**For detailed content organization guidelines, see:** `references/content-organization.md`

## Steps

### 1. Read Config (if available)

```bash
cat kb-config.json
```

Extract `kb_path`. Expand `~` to the actual home directory path (run `echo ~` if needed).
Set this as `KB_PATH` for all subsequent steps.

If the file doesn't exist, default to `/opt/data/obsidian-vault/Vault`.

**Note:** In some environments, the default vault may not be writable. If you encounter permission errors, try the home vault path: `/opt/data/home/obsidian-vault/Vault`. This path is often owned by the hermes user and is writable. To test writability, you can create a test file:

```bash
touch /opt/data/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
```

If both vaults fail, the skill will use a temporary directory and provide copy instructions.

### 2. Detect Input Type

The argument passed after `/kb-ingest` is the source. Classify it:

| Condition | Type |
|---|---|
| Starts with `http://` or `https://` | `web` |
| Ends with `.pdf` (case-insensitive) | `pdf` |
| Ends with `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.svg` (case-insensitive) | `image` |
| Anything else | `note` |

### 3. Ingest by Type

Record the current UTC time in ISO 8601 format (e.g. `2026-04-05T10:00:00Z`) as `INGESTED_AT`.

#### Web (Bulk and Multiple URLs)

For market intelligence gathering or bulk ingestion of multiple URLs, adapt the following approach:

**Data Source Strategy:** When fetching from APIs (arXiv, GitHub, Hugging Face, etc.):
1. **Save to file first** - Avoid piping directly to interpreters to prevent security risks
2. **Process safely** - Read the saved file and parse with appropriate libraries
3. **Handle errors gracefully** - If an API fails, log the error and continue with other sources

**Safe Data Processing Pattern:**
```bash
# Fetch and save
curl -s "API_URL" > /tmp/response.json 2>&1 && echo "Fetch completed"

# Process safely
python3 -c "import json; data = json.load(open('/tmp/response.json')); # process data"
```

**Topic-Based Organization:** For web content, save to the appropriate subdirectory based on topic:
- AI/ML content → `raw/AI/`
- Programming/technical content → `raw/Tech/`
- Business/finance → `raw/Business/`
- General news → `raw/News/`

**Recommended Content Extraction Methods**

When standard web fetching fails (e.g., JavaScript rendering, anti-bot protections), use these methods in order of preference:

**1. jina.ai (Primary Recommendation)**  
For most websites, especially YouTube, X/Twitter, and news sites, use jina.ai for robust content extraction:
```bash
curl -s "https://r.jina.ai/http://{URL}"
```
This service extracts clean article content, handles paywalls, and works well with JavaScript-heavy sites. For best results with special characters, properly URL-encode:
```bash
curl -s "https://r.jina.ai/http://$(python3 -c "import sys, urllib; print(urllib.parse.quote(sys.argv[1]))" "{URL}")"
```

**Source-Specific Considerations**

- **GitHub Trending Pages**: These are heavily JavaScript-rendered and cannot be scraped directly with simple curl commands. Always use jina.ai (`https://r.jina.ai/http://github.com/trending/swift`) for reliable extraction. Direct HTML scraping will fail.

**Source-Specific Considerations**\n\n- **GitHub Trending Pages**: These are heavily JavaScript-rendered and cannot be scraped directly with simple curl commands. Always use jina.ai (`https://r.jina.ai/http://github.com/trending/swift`) for reliable extraction. Direct HTML scraping will fail.\n\n- **YouTube Videos**: When using jina.ai to extract YouTube video content, be aware that the output will contain HTML artifacts, image placeholders, and JavaScript code rather than clean markdown. The extraction captures the full page HTML including thumbnails, video player embeds, and tracking elements. You will need to manually clean up the extracted content to create a readable markdown note. Focus on extracting:\n  - The video title and basic metadata
  - The description text
  - Key comments (especially top comments with high engagement)
  - Main topics and timestamps if available
  - Video links and references
  
  See `references/youtube-cleanup-example.md` for a cleaned-up example of a YouTube video ingestion.

**2. Twitter/X oEmbed Endpoint**  
For Twitter/X posts when jina.ai fails:
```bash
curl -s "https://publish.twitter.com/oembed?url={URL}"
```
Extract the `html` field from the JSON response.

**3. Substack RSS Feed**  
For Substack blogs:
```bash
curl -s "{URL}/feed"
```
Parse the XML to find the <content:encoded> or <description> tags.

**4. Textise dot iitty for Cached Content**  
For Cloudflare-blocked sites:
```bash
curl -s "https://r.jina.ai/http://cc.bingj.com/cache.aspx?d=504-..."
```
Construct from browser cache URL.

**5. Browser Automation**  
Use `browser_navigate` when available for JavaScript-heavy sites.

If content cannot be fetched immediately, create a placeholder entry with frontmatter and empty content, to be updated later.

#### PDF

1. Read the PDF file using the Read tool (pass the full file path).
2. Extract all text content, preserving section headings and paragraph breaks.
3. Generate a `slug` from the filename: strip `.pdf`, lowercase, replace spaces with `-`.
4. Write extracted text to `{KB_PATH}/raw/PDFs/{slug}.md` with this exact format:

```markdown
---
source: {original file path}
ingested_at: {INGESTED_AT}
type: pdf
status: uncompiled
---
{extracted text}
```

5. Copy the original PDF file:
```bash
cp \\\"{original path}\\\" \\\"{KB_PATH}/raw/PDFs/{slug}.pdf\\\"\n```

Set `RAW_KEY` = `Raw/PDFs/{slug}.md`

#### Image

1. Read the image file using the Read tool (Claude will display it visually).
2. Write a detailed description of the image: what it shows, any text visible, diagrams, charts, or figures explained in words.
3. Generate a `slug` from the filename: strip extension, lowercase, replace spaces with `-`.
4. Get the original file extension (e.g. `png`).
5. Write description to `{KB_PATH}/raw/Images/{slug}.md` with this exact format:

```markdown
---
source: {original file path}
ingested_at: {INGESTED_AT}
type: image
status: uncompiled
image_file: {slug}.{ext}
---
{detailed description}
```

6. Copy the image:
```bash
cp \\\"{original path}\\\" \\\"{KB_PATH}/raw/Images/{slug}.{ext}\\\"\n```

Set `RAW_KEY` = `Raw/Images/{slug}.md`

#### Note

1. The argument is the note content directly (not a file path).
   - Exception: if the argument is a path to an existing file, read that file's contents instead.
2. Generate a `slug` from the first 6 words of the content: lowercase, join with `-`.
   - Example: "attention mechanism in transformer models" → `attention-mechanism-in-transformer-models`
3. Write to `{KB_PATH}/raw/Notes/{slug}.md` with this exact format:

```markdown
---
source: manual
ingested_at: {INGESTED_AT}
type: note
status: uncompiled
---
{content}
```

Set `RAW_KEY` = `Raw/Notes/{slug}.md`

### 4. Confirm

Print a single confirmation line:
```markdown
Ingested: {RAW_KEY}
```

### Troubleshooting

#### Vault Structure Mismatch - Enhanced Adaptation Guide

The skill assumes a `.kb/` directory and specific `raw/web/`, `raw/pdfs/`, etc. subdirectories. However, the actual vault at `/opt/data/obsidian-vault/Vault` uses:

- `raw()` as the root raw directory (lowercase, not `Raw()`)
- Subdirectories: `AI()`, `Books()`, `Business()`, `Careers()`, `Health()`, `NBA()`, `News()`, `Stocks()`, `Swift()`, `TV and Movies()`, `Tech()`, `WWE()`
- No `.kb()` directory or `manifest.json`

**Adaptation Strategy:** When the expected structure doesn't match, adapt by:
1. Using the actual `raw()` directory as the root
2. Choosing the most appropriate topic subdirectory based on content:
   - Crypto/finance → `raw/AI()` or `raw/Tech()`
   - Programming/technical → `raw/Tech()`
   - Business/finance → `raw/Business()`
   - General news → `raw/News()`
3. Following the same frontmatter format and compilation workflow

The compilation script (`compile_wiki.py`) reads from `raw()` and writes to `Wiki()` directly, so this topic-based organization is compatible.

#### Permission Denied Errors - Enhanced Workaround

If you encounter permission errors when writing to the vault, follow this systematic approach:

**Step 1: Test Vault Writability**
```bash
# Test main vault
touch /opt/data/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
# Test home vault (if main fails)
touch /opt/data/home/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
```

**Step 2: If Both Vaults Fail, Use Temporary Directory**
When neither vault is writable, create a temporary directory and save files there:

```python
import tempfile, os

# Create temporary directory
temp_dir = tempfile.mkdtemp()
raw_path = f"{temp_dir}/kb_ingest_output"
os.makedirs(raw_path, exist_ok=True)

# Save files to raw_path instead of vault
# Example: 
#   with open(f"{raw_path}/slug.md", "w") as f:
#       f.write(content)

print(f"Permission denied on all vaults. Using temporary directory: {raw_path}")
```

**Step 3: Provide User with Copy Instructions**
After ingestion, inform the user to copy the files manually:

```markdown
Files saved to: {RAW_PATH}
Copy these files to your Obsidian vault raw directory:
  cp {RAW_PATH}/* /opt/data/obsidian-vault/Vault/raw/Tech/
```

**Step 4: Fix Permissions (Permanent Solution)**
For a permanent fix, change the ownership of the vault directory:

```bash
cd /opt/data && make fix-permissions
# This executes: docker compose exec -u root hermes chown -R hermes:hermes /opt/data
```

**Step 5: Alternative Vault**
If you frequently encounter permission issues, consider using the home vault at `/opt/data/home/obsidian-vault/Vault`. Create it and configure kb-ingest via `kb-config.json`.

**For more detailed permission handling patterns, see:** `references/permission-handling.md`

#### Example: Crypto Market Intelligence Gathering

When gathering crypto market intelligence from multiple sources, follow this pattern:

**1. Source Selection:** Target key crypto news outlets and analytics providers:
- CoinDesk, Cointelegraph, The Block (news)
- Glassnode, Santiment (on-chain analytics)
- DeFi protocol blogs (Uniswap, Aave, etc.)
- Regulatory news sources (SEC filings, global regulations)

**2. Content Extraction:** Use jina.ai for robust content extraction, especially for JavaScript-heavy sites:
```bash
curl -s "https://r.jina.ai/http://URL"
```

**3. Topic Organization:** Save crypto-related content to `raw/AI()` as the default topic category.

**4. Content Types:** Include:
- Market-moving news (price action, geopolitical events)
- Institutional adoption reports (fund inflows, ETF activity)
- Regulatory developments (legislation, policy changes)
- Sector analysis (DeFi, NFTs, on-chain metrics)
- Educational content (analytics guides, research explainers)

**5. Frontmatter Consistency:** Maintain uniform frontmatter across all entries for proper compilation.

This example demonstrates how to adapt the kb-ingest skill for specialized intelligence gathering while maintaining the knowledge base structure and compilation workflow.

#### API Limitations and Authentication Issues

Many external APIs (especially Twitter/X, some news sites) require authentication or have rate limits. When encountering these issues:

**1. Use Alternative Extraction Methods:**\n- For Twitter/X: Use the oEmbed endpoint (`https://publish.twitter.com/oembed?url={URL}`)\n- For news sites: Use jina.ai (`https://r.jina.ai/http://{URL}`)\n- For JavaScript-heavy sites: Use browser automation if available\n\n**2. Implement Fallback Strategies:**\n- If primary API fails, try secondary endpoints\n- Cache responses for retry later\n- Create placeholder entries with frontmatter for manual updating\n\n**3. HTTPS Requirement:** Always use HTTPS endpoints for external APIs to avoid security issues:\n```python\n# WRONG - triggers security scan\ncurl -s \"http://export.arxiv.org/api/query?\"\n\n# CORRECT\ncurl -s \"https://export.arxiv.org/api/query?\"\n```\n\n**4. Error Handling Pattern:** When fetching from APIs, implement robust error handling:\n```python\nimport json\nimport os\nfrom datetime import datetime\n\ndef safe_api_fetch(url, output_file, max_retries=3):\n    '''Fetch data from API with retry logic and error handling.'''\n    for attempt in range(max_retries):\n        try:\n            cmd = f\"curl -s --max-time 30 \\\"{url}\\\" > {output_file} 2>&1\"\n            result = os.system(cmd)\n            if result == 0 and os.path.getsize(output_file) > 0:\n                return True\n        except Exception as e:\n            print(f\"Exception: {e}\")\n        if attempt < max_retries - 1:\n            import time; time.sleep(2)\n    return False\n```\n\n**5. Permission Testing:** Always test vault writability before bulk ingestion:\n```bash\ntouch /opt/data/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5\n```\n\nIf the main vault is not writable, try the home vault:\n```bash\ntouch /opt/data/home/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5\n```\n\nWhen both vaults fail, use a temporary directory and provide clear copy instructions to the user.

#### Cloudflare / Anti-Bot Protection

Many sites use Cloudflare. To extract content:

1. **Use jina.ai API:** `https://r.jina.ai/http://URL`
2. **Use textise dot iitty:** `https://r.jina.ai/http://cc.bingj.com/cache.aspx?d=504-...`
3. **Use browser automation** (when Chrome is available) with `browser_navigate`
4. **Set a realistic User-Agent** header when using curl

If content cannot be fetched immediately, create a placeholder entry with frontmatter and empty content, to be updated later.

#### Vault Not Found

If the default vault path does not exist, check for alternative vaults:

- `/opt/data/home/obsidian-vault/Vault` (common home vault)
- `~/obsidian-vault/Vault`

Use the first writable vault you find.

## References

- `references/twitter-oembed-example.md` — concrete example: successful Twitter/X post ingestion via oEmbed endpoint
- `references/github-trending-extraction.md` — detailed guide for extracting GitHub trending data
- `references/vault-sync.md` — instructions for syncing temporary files to vault
- `references/content-extraction-patterns.md` — comprehensive guide for web content extraction patterns, slug generation, error handling, and fallback strategies
- `references/content-extraction.md` — comprehensive guide for web content extraction
- `references/infrastructure-status.md` — current infrastructure status and known limitations
- `references/stack-exchange-api-safety.md` — safe API consumption patterns
- `references/permission-handling.md` — detailed permission handling patterns
- `references/youtube-x-extraction.md` — YouTube and X/Twitter extraction reference
- `references/permission-handling.md` — detailed permission handling patterns
- `references/youtube-x-extraction.md` — YouTube and X/Twitter extraction reference <parameter= tags. <description> tags.

**4. Textise dot iitty for Cached Content**  
For Cloudflare-blocked sites:
```bash
curl -s "https://r.jina.ai/http://cc.bingj.com/cache.aspx?d=504-..."
```

**5. Browser Automation**  
Use `browser_navigate` when available for JavaScript-heavy sites.

If content cannot be fetched, create a placeholder with frontmatter for later updating.
For Twitter/X posts, use the oEmbed endpoint to get clean HTML representation:
```bash
curl -s "https://publish.twitter.com/oembed?url=$URL}"
```
Extract the `html` field from the JSON response, which contains the tweet content in a blockquote format with proper attribution.

**2. Substack RSS Feed**
For Substack blogs (and other RSS-enabled sites), fetch the RSS feed and extract the article content:
```bash
curl -s "$URL/feed"
```
Parse the XML to find the <content:encoded> or <description> tags containing the full article.

**3. jina.ai with Proper Encoding**
If the standard jina.ai API fails, try proper URL encoding:
```bash
curl -s "https://r.jina.ai/http/$(echo -n \\\"$URL\\\" | jq -s -R -r @uri)"
```

**4. Textise dot iitty for Cached Content**
For sites blocked by Cloudflare, use the Bing cache via textise dot iitty:
```bash
curl -s "https://r.jina.ai/http://cc.bingj.com/cache.aspx?d=504-... (construct from browser)"
```

**5. Browser Automation (when available)**
Use the `browser_navigate` tool to load the page and extract content after JavaScript execution.

If content cannot be fetched immediately, create a placeholder entry with frontmatter and empty content, to be updated later.

#### PDF

1. Read the PDF file using the Read tool (pass the full file path).
2. Extract all text content, preserving section headings and paragraph breaks.
3. Generate a `slug` from the filename: strip `.pdf`, lowercase, replace spaces with `-`.
4. Write extracted text to `{KB_PATH}/raw/PDFs/{slug}.md` with this exact format:

```markdown
---
source: {original file path}
ingested_at: {INGESTED_AT}
type: pdf
status: uncompiled
---
{extracted text}
```

5. Copy the original PDF file:
```bash
cp \\\"{original path}\\\" \\\"{KB_PATH}/raw/PDFs/{slug}.pdf\\\"\n```

Set `RAW_KEY` = `Raw/PDFs/{slug}.md`

#### Image

1. Read the image file using the Read tool (Claude will display it visually).
2. Write a detailed description of the image: what it shows, any text visible, diagrams, charts, or figures explained in words.
3. Generate a `slug` from the filename: strip extension, lowercase, replace spaces with `-`.
4. Get the original file extension (e.g. `png`).
5. Write description to `{KB_PATH}/raw/Images/{slug}.md` with this exact format:

```markdown
---
source: {original file path}
ingested_at: {INGESTED_AT}
type: image
status: uncompiled
image_file: {slug}.{ext}
---
{detailed description}
```

6. Copy the image:
```bash
cp \\\"{original path}\\\" \\\"{KB_PATH}/raw/Images/{slug}.{ext}\\\"\n```

Set `RAW_KEY` = `Raw/Images/{slug}.md`

#### Note

1. The argument is the note content directly (not a file path).
   - Exception: if the argument is a path to an existing file, read that file's contents instead.
2. Generate a `slug` from the first 6 words of the content: lowercase, join with `-`.
   - Example: "attention mechanism in transformer models" → `attention-mechanism-in-transformer-models`
3. Write to `{KB_PATH}/raw/Notes/{slug}.md` with this exact format:

```markdown
---
source: manual
ingested_at: {INGESTED_AT}
type: note
status: uncompiled
---
{content}
```

Set `RAW_KEY` = `Raw/Notes/{slug}.md`

### 4. Confirm

Print a single confirmation line:
```markdown
Ingested: {RAW_KEY}
```

### Troubleshooting

#### Vault Structure Mismatch - Enhanced Adaptation Guide

The skill assumes a `.kb/` directory and specific `raw/web/`, `raw/pdfs/`, etc. subdirectories. However, the actual vault at `/opt/data/obsidian-vault/Vault` uses:

- `raw()` as the root raw directory (lowercase, not `Raw()`)
- Subdirectories: `AI()`, `Books()`, `Business()`, `Careers()`, `Health()`, `NBA()`, `News()`, `Stocks()`, `Swift()`, `TV and Movies()`, `Tech()`, `WWE()`
- No `.kb()` directory or `manifest.json`

**Adaptation Strategy:** When the expected structure doesn't match, adapt by:
1. Using the actual `raw()` directory as the root
2. Choosing the most appropriate topic subdirectory based on content:
   - Crypto/finance → `raw/AI()` or `raw/Tech()`
   - Programming/technical → `raw/Tech()`
   - Business/finance → `raw/Business()`
   - General news → `raw/News()`
3. Following the same frontmatter format and compilation workflow

The compilation script (`compile_wiki.py`) reads from `raw()` and writes to `Wiki()` directly, so this topic-based organization is compatible.

#### Permission Denied Errors - Enhanced Workaround

If you encounter permission errors when writing to the vault, follow this systematic approach:

**Step 1: Test Vault Writability**
```bash
# Test main vault
touch /opt/data/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
# Test home vault (if main fails)
touch /opt/data/home/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
```

**Step 2: If Both Vaults Fail, Use Temporary Directory**
When neither vault is writable, create a temporary directory and save files there:

```python
import tempfile, os

# Create temporary directory
temp_dir = tempfile.mkdtemp()
raw_path = f"{temp_dir}/kb_ingest_output"
os.makedirs(raw_path, exist_ok=True)

# Save files to raw_path instead of vault
# Example: 
#   with open(f"{raw_path}/slug.md", "w") as f:
#       f.write(content)

print(f"Permission denied on all vaults. Using temporary directory: {raw_path}")
```

**Step 3: Provide User with Copy Instructions**
After ingestion, inform the user to copy the files manually:

```markdown
Files saved to: {RAW_PATH}
Copy these files to your Obsidian vault raw directory:
  cp {RAW_PATH}/* /opt/data/obsidian-vault/Vault/raw/Tech/
```

**Step 4: Fix Permissions (Permanent Solution)**
For a permanent fix, change the ownership of the vault directory:

```bash
cd /opt/data && make fix-permissions
# This executes: docker compose exec -u root hermes chown -R hermes:hermes /opt/data
```

**Step 5: Alternative Vault**
If you frequently encounter permission issues, consider using the home vault at `/opt/data/home/obsidian-vault/Vault`. Create it and configure kb-ingest via `kb-config.json`.

**For more detailed permission handling patterns, see:** `references/permission-handling.md`

#### Example: Crypto Market Intelligence Gathering

When gathering crypto market intelligence from multiple sources, follow this pattern:

**1. Source Selection:** Target key crypto news outlets and analytics providers:
- CoinDesk, Cointelegraph, The Block (news)
- Glassnode, Santiment (on-chain analytics)
- DeFi protocol blogs (Uniswap, Aave, etc.)
- Regulatory news sources (SEC filings, global regulations)

**2. Content Extraction:** Use jina.ai for robust content extraction, especially for JavaScript-heavy sites:
```bash
curl -s "https://r.jina.ai/http://URL"
```

**3. Topic Organization:** Save crypto-related content to `raw/AI()` as the default topic category.

**4. Content Types:** Include:
- Market-moving news (price action, geopolitical events)
- Institutional adoption reports (fund inflows, ETF activity)
- Regulatory developments (legislation, policy changes)
- Sector analysis (DeFi, NFTs, on-chain metrics)
- Educational content (analytics guides, research explainers)

**5. Frontmatter Consistency:** Maintain uniform frontmatter across all entries for proper compilation.

This example demonstrates how to adapt the kb-ingest skill for specialized intelligence gathering while maintaining the knowledge base structure and compilation workflow.

#### Permission Denied Errors - Enhanced Workaround

If you encounter permission errors when writing to the vault, follow this systematic approach:

**Step 1: Test Vault Writability**
```bash
# Test main vault
touch /opt/data/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
# Test home vault (if main fails)
touch /opt/data/home/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
```

**Step 2: If Both Vaults Fail, Use Temporary Directory**
When neither vault is writable, create a temporary directory and save files there:

```python
import tempfile, os

# Create temporary directory
temp_dir = tempfile.mkdtemp()
raw_path = f"{temp_dir}/kb_ingest_output"
os.makedirs(raw_path, exist_ok=True)

# Save files to raw_path instead of vault
# Example: 
#   with open(f"{raw_path}/slug.md", "w") as f:
#       f.write(content)

print(f"Permission denied on all vaults. Using temporary directory: {raw_path}")
```

**Step 3: Provide User with Copy Instructions**
After ingestion, inform the user to copy the files manually:

```markdown
Files saved to: {RAW_PATH}
Copy these files to your Obsidian vault raw directory:
  cp {RAW_PATH}/* /opt/data/obsidian-vault/Vault/raw/Tech/
```

**Step 4: Fix Permissions (Permanent Solution)**
For a permanent fix, change the ownership of the vault directory:

```bash
cd /opt/data && make fix-permissions
# This executes: docker compose exec -u root hermes chown -R hermes:hermes /opt/data
```

**Step 5: Alternative Vault**
If you frequently encounter permission issues, consider using the home vault at `/opt/data/home/obsidian-vault/Vault`. Create it and configure kb-ingest via `kb-config.json`.

**For more detailed permission handling patterns, see:** `references/permission-handling.md`

## References

- `references/twitter-oembed-example.md` — concrete example: successful Twitter/X post ingestion via oEmbed endpoint

**6. Browser Automation for Dynamic Content Extraction**\n\nFor websites that block simple HTTP requests, require JavaScript execution, or when jina.ai fails to retrieve content, use browser automation tools (`browser_navigate`, `browser_click`, `browser_snapshot`) to extract content.\n\n### When to Use Browser Automation\n\n- Sites that require JavaScript to load content\n- Paywalled content that jina.ai cannot bypass\n- Sites with anti-bot protections that block API-based extraction\n- When jina.ai times out or returns incomplete content\n\n### Step-by-Step Workflow\n\n#### 1. Navigate to the Page\n```bash\nbrowser_navigate(url)\n```\n\n#### 2. Interact with the Page (if needed)\nUse `browser_click(ref=...)` to follow links or interact with elements.\n\n#### 3. Capture Content\nUse `browser_snapshot(full=true)` to get a complete text representation of the page.\n\n#### 4. Parse the Snapshot\nExtract clean article content from the snapshot using pattern matching. The snapshot is a text-based representation of the DOM, with elements shown as lines like `- heading "Title" [ref=e123]` or `- StaticText "Content text"`.\n\nLook for:\n- `- heading "Title"` lines for section headings\n- `- StaticText "Text content"` lines for paragraph text\n- `- listitem` lines for bullet points\n\nFilter out UI elements (banner, navigation, buttons) by skipping lines that start with these patterns.\n\n#### 5. Save to Knowledge Base\nProcess the extracted content as you would for any web source, using the appropriate subdirectory (e.g., `raw/AI/`).\n\n### Example: Extracting from a Blog Post\n\nWhen dealing with a blog post that has dynamic content:\n\n```python\n# After navigating to the article page and taking a snapshot\n snapshot = browser_snapshot(full=True)\n \n # Parse the snapshot to extract clean text\n content_lines = []\n in_content = False\n \n for line in snapshot.split('\\n'):\n     line = line.strip()\n     \n     # Start content extraction at first heading\n     if not in_content and '- heading "' in line and '[level=2' in line:\n         in_content = True\n         # Extract heading text\n         heading_match = re.search(r'- heading "([^"]+)"', line)\n         if heading_match:\n             content_lines.append(heading_match.group(1))\n         continue\n     \n     if in_content:\n         # Stop at new UI sections\n         if line.startswith('- banner') or line.startswith('- navigation') or line.startswith('- button'):\n             break\n         \n         # Extract StaticText content\n         if line.startswith('- StaticText "'):\n             text_match = re.search(r'- StaticText "([^"]*)"', line)\n             if text_match:\n                 text = text_match.group(1).replace('\\"', '"')\n                 if text and not text.isspace():\n                     content_lines.append(text)\n         \n         # Extract list items\n         if line.startswith('- listitem'):\n             marker_match = re.search(r'ListMarker "([^"]*)"', line)\n             text_match = re.search(r'- StaticText "([^"]*)"', line)\n             if marker_match and text_match:\n                 marker = marker_match.group(1)\n                 text = text_match.group(1)\n                 content_lines.append(f\"{marker} {text}\")\n ```\n\n#### 6. Save Extracted Content\nProcess the extracted content as a web source, saving to the appropriate topic subdirectory (e.g., `raw/AI/`).\n\n### Advantages of Browser Automation\n- Bypasses anti-bot protections by simulating a real browser\n- Handles JavaScript-heavy sites\n- Can interact with page elements (click links, fill forms)\n- Provides a complete snapshot of the rendered page\n\n### Limitations\n- Requires a browser environment\n- Slower than direct HTTP requests\n- May still be blocked by advanced anti-bot measures\n\n### When to Use This vs. jina.ai\n\n- Try jina.ai first for most websites (it's faster and simpler)\n- Use browser automation when jina.ai fails or the site requires JavaScript/interaction\n- For static sites without JavaScript, direct curl requests or jina.ai are sufficient\n\n### Example Use Cases\n\n- Extracting content from blogs that use JavaScript to load articles\n- Bypassing paywalls that jina.ai cannot circumvent\n- Scraping sites with complex anti-bot protections\n- Interacting with pages that require clicking (e.g., \"Load more\" buttons)\n\n### References\n- See `references/browser-snapshot-extraction.md` for detailed parsing patterns and examples

#### Vault Not Found

If the default vault path does not exist, check for alternative vaults:

- `/opt/data/home/obsidian-vault/Vault` (common home vault)
- `~/obsidian-vault/Vault`

Use the first writable vault you find.

## References

- For the actual implementation of compilation, see `~/.hermes/scripts/compile_wiki.py`
- For vault setup and permissions, see `~/.hermes/scripts/setup-obsidian-vault.sh`
- For detailed permission fixing instructions, see `references/fix-permissions.md`