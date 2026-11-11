# Content Extraction Patterns for KB Ingest

This reference documents practical content extraction patterns discovered through real-world use of the kb-ingest skill.

## 1. Robust Slug Generation

When generating slugs from titles or filenames, use this regex-based approach to ensure valid filenames:

```python
import re

def generate_slug(text, max_length=50):
    """
    Generate a clean slug from text.
    
    Args:
        text: Input text (title or filename)
        max_length: Maximum slug length
    
    Returns:
        Clean slug with only lowercase alphanumeric and hyphens
    """
    if not text:
        return "untitled"
    
    # Remove special characters, keep only alphanumeric and spaces
    cleaned = re.sub(r'[^a-zA-Z0-9\s-]', '', text.lower())
    
    # Replace whitespace with hyphens
    slug = re.sub(r'\s+', '-', cleaned).strip('-')
    
    # Limit length
    if len(slug) > max_length:
        slug = slug[:max_length]
    
    return slug if slug else "untitled"
```

**Usage examples:**
```python
# From title
slug = generate_slug("Attention Mechanism in Transformer Models")
# Result: "attention-mechanism-in-transformer-models"

# From filename "my cool pdf.pdf"
slug = generate_slug("my cool pdf")
# Result: "my-cool-pdf"
```

## 2. HTTPS Requirement for APIs

Always use HTTPS endpoints for external APIs to avoid security issues:

```python
# WRONG - triggers security scan
curl -s "http://export.arxiv.org/api/query?..."

# CORRECT
curl -s "https://export.arxiv.org/api/query?..."
```

## 3. Error Handling for API Failures

When fetching from APIs, implement robust error handling:

```python
import json
import os
from datetime import datetime

def safe_api_fetch(url, output_file, max_retries=3):
    """
    Fetch data from API with retry logic and error handling.
    
    Args:
        url: API endpoint URL
        output_file: Local file to save response
        max_retries: Maximum number of retry attempts
    
    Returns:
        bool: True if successful, False otherwise
    """
    for attempt in range(max_retries):
        try:
            # Use curl with timeout
            cmd = f"curl -s --max-time 30 \"{url}\" > {output_file} 2>&1"
            result = os.system(cmd)
            
            if result == 0:
                # Check if file has content
                if os.path.getsize(output_file) > 0:
                    return True
                else:
                    print(f"Warning: Empty response from {url} (attempt {attempt + 1})")
            else:
                print(f"Failed to fetch {url} (attempt {attempt + 1})")
                
        except Exception as e:
            print(f"Exception fetching {url}: {e} (attempt {attempt + 1})")
        
        # Wait before retry
        if attempt < max_retries - 1:
            import time
            time.sleep(2)
    
    return False
```

## 4. Content Detection Patterns

Different websites require different content detection approaches. Here are proven patterns:

### Pattern A: Newsletter/Blog Format (The Batch, a16z)
```python
# Look for markdown-style headers
articles = []
current_article = None

for line in content.split('\n'):
    line = line.strip()
    # Detect article headers (markdown headers or short lines with periods)
    if line.startswith('## ') or line.startswith('# ') or ('.' in line and len(line) < 200):
        if current_article:
            articles.append(current_article)
        current_article = {'title': line.lstrip('# '), 'content': ''}
    elif current_article and line:
        current_article['content'] += line + ' '
```

### Pattern B: List Format (GitHub Trending, Medium)
```python
# Look for repository entries or list items
repos = []
current_repo = None

for line in content.split('\n'):
    line = line.strip()
    # Repository entries often start with stars or have slashes
    if line.startswith('## ') or ('/' in line and '.' not in line and len(line) < 100):
        if current_repo:
            repos.append(current_repo)
        current_repo = {'name': line, 'description': ''}
    elif current_repo and line and not line.startswith('##'):
        current_repo['description'] += line + ' '
```

### Pattern C: Article with Metadata (VentureBeat, TechCrunch)
```python
# Look for article titles and summaries
articles = []
in_article = False

for line in content.split('\n'):
    line = line.strip()
    # Article headers often have capitalization patterns
    if line.isupper() or (line[0].isupper() and len(line) > 20):
        if in_article:
            articles.append(current_article)
        current_article = {'title': line, 'content': ''}
        in_article = True
    elif in_article and line:
        current_article['content'] += line + ' '
```

## 5. Fallback Strategies

When primary extraction fails, use these fallback methods:

### Fallback 1: Use jina.ai with Proper Encoding
```python
# If standard jina.ai fails, try proper URL encoding
from urllib.parse import quote_plus
encoded_url = quote_plus(url)
curl -s "https://r.jina.ai/http://{{encoded_url}}"
```

### Fallback 2: Use RSS Feeds for Blogs
```python
# For Substack and RSS-enabled blogs
curl -s "{url}/feed" | xmlstarlet sel -t -v "//content:encoded" -n
```

### Fallback 3: Use oEmbed for Social Media
```python
# For Twitter/X posts
curl -s "https://publish.twitter.com/oembed?url={url}" | jq -r '.html'
```

### Fallback 4: Use Cached Content
```python
# For Cloudflare-blocked sites
curl -s "https://r.jina.ai/http://cc.bingj.com/cache.aspx?d=504-..." 
```

## 6. Topic-Based Organization

Map content types to appropriate raw subdirectories:

| Content Type | Target Directory | Examples |
|--------------|------------------|----------|
| AI/ML Research | `raw/AI/` | arXiv papers, AI blogs |
| Programming/Technical | `raw/Tech/` | GitHub trending, LangChain, LlamaIndex |
| Business/Finance | `raw/Business/` | Venture funding, market analysis |
| General News | `raw/News/` | Industry news, trends |
| Entertainment | `raw/TV and Movies/` | Reviews, analysis |
| Sports | `raw/NBA/` | Game analysis, news |

## 7. Validation Checks

Before saving files, perform these validation checks:

```python
def validate_content(content, source):
    """
    Validate extracted content before saving.
    
    Args:
        content: Extracted content string
        source: Source identifier
    
    Returns:
        tuple: (is_valid, error_message)
    """
    # Check for empty content
    if not content or len(content.strip()) < 10:
        return False, "Content too short"
    
    # Check for error messages in content
    error_keywords = ['error', 'failed', '404', '403', 'rate limit']
    content_lower = content.lower()
    for keyword in error_keywords:
        if keyword in content_lower:
            return False, f"Error keyword detected: {keyword}"
    
    # Check for successful extraction patterns
    success_patterns = [
        'published:', 'author:', 'summary:', 'description:', 
        'title:', 'abstract:', 'content:'
    ]
    if not any(pattern in content_lower for pattern in success_patterns):
        return False, "No content structure detected"
    
    return True, "Valid"
```

## 8. Temporary File Management

When vaults are not writable, use this pattern:

```python
import tempfile
import os
import shutil

def create_temp_ingest_dir():
    """
    Create a temporary directory for ingestion when vaults are not writable.
    
    Returns:
        tuple: (temp_dir_path, cleanup_function)
    """
    temp_dir = tempfile.mkdtemp(prefix="kb_ingest_")
    raw_path = os.path.join(temp_dir, "raw")
    os.makedirs(raw_path, exist_ok=True)
    
    def cleanup():
        if os.path.exists(temp_dir):
            shutil.rmtree(temp_dir)
    
    return temp_dir, cleanup
```