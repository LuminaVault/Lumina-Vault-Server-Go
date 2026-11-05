---
name: url-ingestor-enhanced
description: |
  Autonomous system to capture ANY URL from any platform and automatically add to Obsidian vault
  with proper classification and formatting. Filenames follow <YYYY-MM-DD>-<author>-<short-title>.md
  under /opt/data/home/obsidian-vault/Vault/raw/<category>/. LLM steps (classify, summarize)
  route to provider `nous` model `arcee-ai/trinity-large-thinking` (free) with OpenRouter as fallback.
license: MIT
---

# URL Ingestor Enhanced

**Summary**: Autonomous system to capture ANY URL from any platform (Discord, Telegram, Slack, WhatsApp, chat) and automatically add to Obsidian vault with proper classification and formatting.

## Canonical conventions

- **Vault root**: `/opt/data/home/obsidian-vault/Vault/`
- **Raw inbox**: `<vault root>/raw/<category>/`  (lowercase `raw`, not `Raw`)
- **Filename**: `<YYYY-MM-DD>-<author>-<short-title>.md` (max 8 title words; collisions get `-2`, `-3`, ...)
- **Frontmatter**: `title`, `url`, `author`, `date`, `ingested_at`, `tags`, `topic`, `status: uncompiled`
- **LLM call routing**: classification + summarization → `nous/arcee-ai/trinity-large-thinking` (free, set `NOUS_API_KEY` in `~/.hermes/.env`). If Nous is unset or errors, fall back to OpenRouter Claude Haiku. All other agent reasoning stays on `kimi-k2.6`.

## Overview

This enhanced skill extends the existing link ingestion system to handle **ANY URL type** (not just X/Twitter and GitHub) and provides comprehensive automatic processing:

- **Universal URL capture**: All URL formats from any source
- **Smart content extraction**: Platform-specific extraction for different URL types
- **Automatic classification**: Topic-based organization using AI classification
- **Proper formatting**: Markdown with frontmatter and metadata
- **Auto-compilation**: Triggers knowledge base compilation when new content arrives

## Integration with Multi-Source Link Poller

When using an external poller script (like `multi_link_poller.py`) to fetch URLs from various platforms, it's recommended to call the URL ingestor skill's `process_urls` method rather than directly creating files. This ensures proper content extraction, classification, formatting, and compilation.

### Example Poller Integration

A typical poller script should:
1. Poll each platform for new messages containing URLs
2. Extract URLs and basic context (title, chat ID, sender)
3. Call the URL ingestor skill to process each URL:

```python
from hermes_tools import AIAgent, tool_discovery

# Initialize agent
agent = AIAgent(provider="openrouter", model="mise-kr-1.5", 
               enabled_toolsets=["skills"], disabled_toolsets=[])
agent.load_tools()

# Discover URL ingestor tool
url_ingestor = agent.get_tool("url_ingestor")

# Process a URL
result = url_ingestor.process_url(
    url="https://example.com", 
    title="Example Domain",
    source="whatsapp"
)
```

### WhatsApp Bridge Polling

For WhatsApp integration, the poller can use the Baileys WhatsApp bridge's GET `/messages` endpoint to retrieve new messages. The bridge runs as a separate Node.js process and exposes an HTTP API:

```python
import requests

def poll_whatsapp():
    """Poll WhatsApp bridge for new messages with URLs."""
    try:
        response = requests.get("http://localhost:3000/messages", timeout=10)
        response.raise_for_status()
        messages = response.json()
        
        new_urls = []
        for msg in messages:
            content = msg.get('body', '') or msg.get('caption', '')
            urls = re.findall(r'https?://\S+', content)
            for url in urls:
                new_urls.append({
                    'url': url,
                    'title': content[:100],
                    'chat_id': msg.get('chatId', ''),
                    'sender': msg.get('senderName', '') or msg.get('sender', '')
                })
        return new_urls()
        
    except requests.exceptions.RequestException as e:
        print(f"WhatsApp connection error: {e}")
        return []
```

### Manual Trigger

**Process individual URL**:
```bash
hermes skill_view name='url-ingestor-enhanced' action='ingest_url' url='https://example.com'
```

### Cron Job Setup

If not using an external poller, you can run the URL ingestor skill directly via cron:

```bash
hermes skill_view name='url-ingestor-enhanced' action='process_queue'
```

## Key Features

### 1. Universal URL Detection
- Handles all URL schemes: http, https, ftp, mailto, etc.
- Works across all platforms: Discord, Telegram, Slack, and direct chat
- Captures URLs in any message context

### 2. Smart Content Extraction
Based on URL type, uses appropriate extraction method:

- **Articles/Blog Posts**: Extract full content using Jina AI or similar
- **GitHub Repos**: Extract repo info, description, stars, language
- **YouTube Videos**: Extract title, description, transcript, channel info
- **Social Media**: Extract post content, author, engagement metrics
- **Documentation**: Extract title, structure, key information
- **Generic Web Pages**: Extract title, meta description, main content
- **PDFs/Docs**: Extract title, metadata, and text content

### 3. Automatic Classification

**AI-Powered Classification**:
```python
def classify_content(content, title, url):
    """Classify content by topic using AI"""
    classification_prompt = f"""
    Classify this content by topic. Choose from:
    1. AI/ML - Artificial Intelligence, Machine Learning, LLMs
    2. Development - Programming, Swift, iOS, Vapor, Web Dev
    3. Stocks/Investing - Stock market, trading, portfolio management
    4. Health/Fitness - Exercise, nutrition, wellness
    5. Technology - Gadgets, software, tech news
    6. Business/News - Business, finance, current events
    7. Entertainment - Movies, music, games, celebrities
    8. Science - Scientific discoveries, space, physics
    9. Other - Doesn't fit above categories

    Content:
    {content[:500] if len(content) > 500 else content}

    Title: {title}

    URL: {url}

    Classification:
    """

    # Use LLM for classification
    # Return primary topic
```

### 4. Proper Formatting

**Template**:
```markdown
---
classification: {topic}
source: {url}
captured_at: {timestamp}
original_content: true
---
# {title}

{content}

*Originally captured from {url} on {timestamp}*
```

### 5. Auto-Compilation

**Trigger compilation when new content arrives**:
```python
def trigger_compilation():
    """Trigger knowledge base compilation"""
    # Call kb-compile skill
    # Wait for completion
    # Update search indexes
```

## Configuration

### Environment Variables

**Required**:
- `OBSIDIAN_VAULT_PATH`: Path to your Obsidian vault
- `OBSIDIAN_VAULT_NAME`: Name of your primary vault

**Optional**:
- `MAX_CONTENT_LENGTH`: Maximum content to extract (default: 10000 chars)
- `CLASSIFICATION_MODEL`: Classification model to use
- `EXTRACTION_TIMEOUT`: Timeout for content extraction (default: 30 seconds)

### Vault Structure

**Expected Structure**:
```
vault/
├── raw/
│   ├── AI/
│   ├── Development/
│   ├── Stocks/
│   ├── Health/
│   ├── Technology/
│   ├── Business/
│   ├── Entertainment/
│   ├── Hermes/
│   ├── Sports/
│   ├── Science/
│   └── XFeed/
└── wiki/
```

## Quality Assurance

### Validation Checks
- ✅ URL format validation
- ✅ Content extraction success check
- ✅ Topic classification confidence threshold
- ✅ File creation verification
- ✅ Compilation trigger confirmation

### Error Handling
- **Network errors**: Retry with exponential backoff
- **Extraction failures**: Fallback to generic extraction
- **Classification uncertainty**: Default to "Other" category
- **File system errors**: Log and continue processing

### Performance
- **Concurrent processing**: Up to 5 URLs simultaneously
- **Rate limiting**: Respect source website rate limits
- **Memory management**: Stream large content processing
- **Timeout handling**: Individual URL timeout (60 seconds)

## Integration with Existing Systems

### 1. External Content Ingestion
Enhances `external-content-ingestion` with broader URL support.

### 2. Knowledge Base Compilation
Triggers `kb-compile` automatically when new content is added.

### 3. Portfolio Monitoring
Can capture stock-related content and automatically classify for portfolio research.

## Example Usage Scenarios

### Scenario 1: Article from TechCrunch
**Input**: "Check out this article: https://techcrunch.com/2026/05/05/ai-breakthrough/"

**Processing**:
- Extract full article content
- Classify as "Technology"
- Save to `raw/Technology/`
- Trigger compilation

### Scenario 2: GitHub Repository
**Input**: "This Swift library looks useful: https://github.com/apple/swift-algorithms"

**Processing**:
- Extract repo info (name, description, stars, language)
- Classify as "Development"
- Save to `raw/Development/`
- Trigger compilation

### Scenario 3: YouTube Video
**Input**: "Great tutorial on SwiftUI: https://www.youtube.com/watch?v=example"

**Processing**:
- Extract video title, description, channel
- Classify as "Development"
- Save to `raw/Development/`
- Trigger compilation

### Scenario 4: Generic Blog Post
**Input**: "Interesting post about investing: https://example.com/investing-tips"

**Processing**:
- Extract title and main content
- Classify as "Stocks/Investing"
- Save to `raw/Stocks/`
- Trigger compilation

## Maintenance & Monitoring

### Health Checks
- **URL processing rate**: URLs processed per minute
- **Extraction success rate**: Percentage of successful extractions
- **Classification confidence**: Average classification confidence score
- **Compilation success**: Knowledge base compilation success rate

### Logging
- **Processed URLs**: Log all successfully processed URLs
- **Failed URLs**: Log extraction/classification failures
- **Errors**: Comprehensive error logging
- **Performance**: Processing times and bottlenecks

### Alerts
- **High failure rate**: Alert if extraction success rate drops below threshold
- **Vault full**: Alert if storage space is running low
- **Compilation failures**: Alert if knowledge base compilation fails

## Future Enhancements

### 1. Advanced Content Processing
- **Multi-page articles**: Handle pagination and multi-part content
- **Embedded media**: Extract and process images, videos, audio
- **Code extraction**: Special handling for code snippets and repositories
- **Data tables**: Extract and format tabular data

### 2. Enhanced Classification
- **Multi-label classification**: Support for content that spans multiple topics
- **Sentiment analysis**: Detect positive/negative sentiment in content
- **Entity recognition**: Extract people, organizations, locations
- **Trend detection**: Identify trending topics and themes

### 3. Platform-Specific Optimizations
- **Twitter/X**: Enhanced handling for tweets and threads
- **Reddit**: Special handling for Reddit posts and comments
- **Stack Overflow**: Code-focused extraction for programming questions
- **News sites**: Optimized for various news article formats

### 4. Integration Features
- **Cross-referencing**: Link related content across different sources
- **Summary generation**: Create executive summaries for long content
- **Translation**: Automatic translation for non-English content
- **Voice readiness**: Format content for text-to-speech consumption

## Security Considerations

### 1. URL Safety
- **Malicious URL detection**: Basic checks for known malicious patterns
- **Content filtering**: Optional filtering for adult content
- **Rate limiting**: Prevent abuse of extraction services

### 2. Privacy Protection
- **Personal information**: Filter out PII from extracted content
- **Sensitive content**: Optional filtering for confidential information
- **Data retention**: Configurable retention policies

### 3. Access Control
- **Platform permissions**: Minimal required permissions for each platform
- **Vault security**: Proper file permissions for created content
- **API rate limits**: Respect service provider rate limits

## Conclusion

This enhanced URL ingestion skill provides comprehensive, automatic capture and processing of ANY URL from any platform, seamlessly integrating with your existing knowledge management workflow. The system is robust, scalable, and requires minimal maintenance while providing maximum value through automated content curation and organization.

**Key Benefits**:
- ✅ Universal URL support (all types from all sources)
- ✅ Automatic classification and organization
- ✅ Proper markdown formatting with metadata
- ✅ Seamless integration with Obsidian vault
- ✅ Auto-triggers knowledge base compilation
- ✅ Comprehensive error handling and monitoring
- ✅ Scalable architecture for future enhancements

**Trigger compilation when new content arrives**:
```python
def trigger_compilation():
    """Trigger knowledge base compilation"""
    # Call kb-compile skill
    # Wait for completion
    # Update search indexes
```

## References

- Multi-Link Poller Implementation: `scripts/multi_link_poller.py`
- WhatsApp Bridge API: GET `/messages` endpoint for polling new messages