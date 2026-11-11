# URL Ingestor Enhanced - Comprehensive Documentation

## Overview

The URL Ingestor Enhanced skill automatically captures **ANY URL** from any platform (Discord, Telegram, Slack, chat) and adds it to your Obsidian vault with proper classification, formatting, and metadata.

## Key Features

### Universal URL Support
- **All URL types**: Articles, GitHub repos, YouTube videos, documentation, PDFs, and generic web pages
- **All platforms**: Discord, Telegram, Slack, and direct chat
- **All schemes**: http, https, ftp, mailto, etc.

### Smart Content Extraction
- **Articles**: Full content extraction using Jina AI
- **GitHub**: Repository metadata and README extraction
- **YouTube**: Video title, description, and metadata extraction
- **Generic pages**: Title and main content extraction

### Automatic Organization
- **AI-powered classification**: Automatically categorizes content into topics
- **Proper markdown**: Well-formatted files with frontmatter
- **Topic-based folders**: Content organized in `Raw/{Topic}/` directories
- **Auto-compilation**: Triggers knowledge base compilation when new content arrives

## Installation & Setup

### 1. Create the Skill
The skill is already created. To install it:

```bash
hermes skill_manage action='install' name='url-ingestor-enhanced'
```

### 2. Set Up Environment Variables

Create or update your `.env` file with:

```env
# Obsidian vault configuration
OBSIDIAN_VAULT_PATH=/opt/data/obsidian-vault/Vault
OBSIDIAN_VAULT_NAME=Vault

# Optional: GitHub token for higher API limits
# GITHUB_TOKEN=your_github_personal_access_token

# Optional: YouTube extraction tool
# YT_DLP_PATH=/usr/local/bin/yt-dlp
```

### 3. Verify Vault Structure

Ensure your vault has the following structure:

```
Vault/
├── Raw/
│   ├── AI/
│   ├── Development/
│   ├── Stocks/
│   ├── Health/
│   ├── Technology/
│   ├── Business/
│   ├── Entertainment/
│   └── Science/
└── Wiki/
```

If any topic folders are missing, create them:

```bash
cd /opt/data/obsidian-vault/Vault/Raw
mkdir -p AI Development Stocks Health Technology Business Entertainment Science Other
```

## Usage

### 1. Manual URL Processing

Process a single URL:

```bash
hermes skill_view name='url-ingestor-enhanced' action='process_url' url='https://example.com'
```

Process multiple URLs from a file:

```bash
# Create a file with one URL per line
echo "https://example.com/article1" > urls.txt
echo "https://github.com/user/repo" >> urls.txt
echo "https://youtube.com/watch?v=example" >> urls.txt

# Process the file
hermes skill_view name='url-ingestor-enhanced' action='process_urls' urls_file='urls.txt'
```

### 2. Test the System

Run the test script to verify everything works:

```bash
python3 /opt/data/skills/content/url-ingestor-enhanced/scripts/test_url_ingestor.py
```

### 3. Set Up Automated Cron Job

Schedule automatic URL processing every 15 minutes:

```bash
hermes cronjob action='create' 
  name='url-ingestor-enhanced-poller' 
  schedule='*/15 * * * *' 
  script='/opt/data/skills/content/url-ingestor-enhanced/scripts/url_ingestor.py' 
  workdir='/opt/data/skills/content/url-ingestor-enhanced' 
  skills='["url-ingestor-enhanced"]'
```

## Configuration Options

### Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `OBSIDIAN_VAULT_PATH` | Yes | Path to your Obsidian vault |
| `OBSIDIAN_VAULT_NAME` | Yes | Name of your primary vault |
| `MAX_CONTENT_LENGTH` | No | Maximum content to extract (default: 10000) |
| `EXTRACTION_TIMEOUT` | No | Timeout for content extraction (default: 30s) |
| `CLASSIFICATION_MODEL` | No | Classification model to use |
| `GITHUB_TOKEN` | No | GitHub personal access token for higher API limits |
| `YT_DLP_PATH` | No | Path to yt-dlp executable |

### Customization

#### Add New Classification Categories

Edit the `CLASSIFICATION_KEYWORDS` dictionary in `url_ingestor.py` to add new topics and keywords.

#### Modify Extraction Strategies

You can customize the extraction methods for different URL types by modifying the corresponding functions in `url_ingestor.py`.

#### Change Markdown Template

Modify the `create_markdown_file` method to change the frontmatter or file structure.

## Supported URL Types

### Articles & Blog Posts
- Any website with readable content
- Uses Jina AI for clean text extraction
- Example: `https://techcrunch.com/2026/05/05/ai-breakthrough/`

### GitHub Repositories
- Extracts repository metadata and README
- Example: `https://github.com/apple/swift-algorithms`

### YouTube Videos
- Extracts title, description, and metadata
- Example: `https://www.youtube.com/watch?v=example`

### Generic Web Pages
- Extracts title and main content
- Works with documentation, wikis, and most websites
- Example: `https://docs.swift.org/swift-book/LanguageGuide/TheBasics.html`

### PDFs & Documents
- Extracts text content from PDFs
- Example: `https://example.com/document.pdf`

## Platform Integration

### Discord
1. Create a Discord bot and get the token
2. Add the bot to your server with "Read Message History" permission
3. Set environment variable:
   ```env
   DISCORD_BOT_TOKEN=your_bot_token
   DISCORD_MONITOR_CHANNEL=channel_id_to_monitor
   ```
4. The bot will automatically process URLs in the monitored channel

### Telegram
1. Create a Telegram bot via @BotFather
2. Add the bot to your channel
3. Set environment variable:
   ```env
   TELEGRAM_BOT_TOKEN=your_bot_token
   TELEGRAM_HOME_CHANNEL=channel_id
   ```
4. The bot will process URLs in the configured channel

### Slack
1. Create a Slack app and get the bot token
2. Install the app to your workspace
3. Set environment variable:
   ```env
   SLACK_BOT_TOKEN=xoxb-your-token
   SLACK_HOME_CHANNEL=channel_id
   ```
4. The bot will process URLs in the configured channel

## Monitoring & Maintenance

### Check Processing Status

View the current state and processed URLs:

```bash
# View state file
cat ~/.hermes/state/url_ingestor_state.json

# View logs
tail -f /tmp/url_ingestor.log
```

### Health Checks

The system performs automatic health checks:

- **URL processing rate**: URLs processed per minute
- **Extraction success rate**: Percentage of successful extractions
- **Classification confidence**: Average classification confidence score
- **Compilation success**: Knowledge base compilation success rate

### Error Handling

The system includes comprehensive error handling:

- **Network errors**: Automatic retry with exponential backoff
- **Extraction failures**: Fallback to alternative extraction methods
- **Classification uncertainty**: Defaults to "Other" category
- **File system errors**: Detailed logging and error recovery

### Performance

- **Concurrent processing**: Up to 5 URLs simultaneously
- **Rate limiting**: Respects source website rate limits
- **Memory management**: Stream processing for large content
- **Timeout handling**: Individual URL timeout (60 seconds)

## Troubleshooting

### Common Issues

#### "Could not extract content from URL"
- The URL may be blocked by authentication (X auth wall)
- The website may have anti-scraping measures
- Try adding `?format=json` or other format parameters

#### "GitHub API rate limit exceeded"
- Unauthenticated GitHub API has 60 requests/hour limit
- Add a GitHub token to increase to 5000 requests/hour

#### "Compilation failed"
- Ensure `compile_wiki.py` exists in your vault scripts
- Check file permissions in the vault
- Verify Python dependencies are installed

#### "Bot not processing messages"
- Check bot permissions (Read Message History for Discord)
- Verify bot is added to the correct channel
- Check environment variables are set correctly

### Debugging

Enable debug logging:

```python
# In url_ingestor.py, change logging level to DEBUG
logging.basicConfig(level=logging.DEBUG, ...)
```

Run in test mode:

```bash
python3 /opt/data/skills/content/url-ingestor-enhanced/scripts/url_ingestor.py --test --url https://example.com
```

### Logs

Main log file: `/tmp/url_ingestor.log`

Check for errors and warnings to diagnose issues.

## Future Enhancements

### Planned Features
- [ ] Multi-label classification (content spanning multiple topics)
- [ ] Sentiment analysis for content
- [ ] Entity recognition (people, organizations, locations)
- [ ] Cross-referencing between related content
- [ ] Automatic summary generation
- [ ] Translation for non-English content

### Contributing

To enhance this skill:
1. Edit the SKILL.md file to update documentation
2. Modify the Python script for new features
3. Test thoroughly with the test script
4. Submit improvements via skill management

## Support

For help with this skill, check:
- The skill documentation (`references/` directory)
- Existing skills for similar functionality
- The Hermes Agent community

## License

MIT License - see LICENSE file for details.