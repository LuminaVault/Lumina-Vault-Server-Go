---
name: media
description: Comprehensive media monitoring and content ingestion system for tracking TV shows, movies, YouTube videos, and social media.
license: MIT
version: 1.0.0
author: Hermes Agent
metadata:
  capability: medium
  tags: [media, trakt, youtube, monitoring, content-ingestion]
  related_skills: [trakt-watcher, youtube-content, media]
---

# Media Monitoring System

This umbrella skill consolidates media tracking capabilities: monitoring TV show progress, movie releases, YouTube content, and social media. It covers both automated monitoring (Trakt) and content ingestion (YouTube transcripts, social links).

## Trakt.tv Monitoring

Trakt is a platform for tracking TV shows, movies, and what you're watching. This section covers automated monitoring of Trakt watching status and notifications.

### Configuration

The skill requires Trakt API credentials:

```yaml
params:
  - name: trakt_client_id
    description: Trakt API client ID
    default: dc22389fbad6bb350eb7b3714d21a71d0a16c35cba461c375728d99f55c3de59
  - name: trakt_access_token
    description: Trakt OAuth access token
    default: (will be set dynamically)
  - name: trakt_refresh_token
    description: Trakt refresh token
    default: (will be set dynamically)
  - name: trakt_client_secret
    description: Trakt API client secret
    default: \"aa64d05b2812c0ad9e1a3430b199b933ea5bd0cb4671c45b65026c744fa827a3\"
```

**State Management:**
- `last_watching`: Stores the last known watching status
- `last_check`: Timestamp of the last check

### How It Works

1. **OAuth Token Refresh**: Uses client ID and secret to refresh access tokens automatically when expired.
2. **Watching Status Check**: Polls Trakt API to get current watching status (TV show episode, movie).
3. **State Comparison**: Compares current status with previous state to detect changes.
4. **Notification**: When a new episode or movie starts, posts a notification to configured channels.
5. **Silent Operation**: When no changes detected, outputs nothing (cron job SILENT mode).

### Key Features

- **Token Storage**: Access and refresh tokens are stored in `~/.hermes/tokens/trakt.json` with proper permissions (chmod 600).
- **State Persistence**: Watching state saved to `~/.hermes/trakt-watcher-state.json` for reliability across restarts.
- **Rate Limit Handling**: Trakt API enforces rate limits; the script will not retry automatically on 429 responses.
- **Short Timeouts**: Uses 5-second request timeout to prevent hangs.
- **Absolute Path**: Uses absolute path to script for reliable cron execution.

### Error Handling and Pitfalls

- **Client Secret Required**: Without `trakt_client_secret`, the skill cannot refresh expired tokens and will fail silently.
- **Import Dependencies**: Requires `requests` library. Ensure it is installed (`pip install requests`).
- **Token Storage Security**: Ensure `~/.hermes/tokens/trakt.json` has appropriate permissions (chmod 600).
- **Silent Failures**: When the access token is expired and the client secret is missing, or when rate limits are encountered (429), the skill will fail silently without printing any output. Check logs for errors.
- **Rate Limit Handling**: If a 429 response is received, the script will not retry automatically. Manual intervention or increasing the cron interval may be required.
- **Cron Job SILENT Mode**: When running as a scheduled cron job with no new watching status to report, the script will output nothing (empty string). This is the intended behavior to suppress delivery. Do not combine with error messages.

### Setup and Installation

1. **Install Dependencies**:
   ```bash
   pip install requests
   ```

2. **Configure Credentials**: Set the required parameters (client ID, secret, access token, refresh token) via the Hermes configuration system or environment variables.

3. **Set Up Cron Job**: Schedule the skill to run periodically (e.g., every 15 minutes) to check for new watching status.

### Usage

The skill runs as a cron job that:
- Fetches current watching status from Trakt API
- Compares with previous state
- If changed (new episode/movie started), generates a notification
- Posts to configured messaging channels (Discord, Telegram, Slack)
- Updates state for next run

### Integration with Other Skills

- **Messaging Skills**: Works with `discord-integration`, `telegram`, `slack` for notifications.
- **Content Ingestion**: Can be combined with `external-content-ingestion` for automatic note-taking.
- **Monitoring**: Fits into broader monitoring setups using `ops-sentry` patterns.

## YouTube Content Ingestion

YouTube monitoring involves extracting transcripts from videos and converting them into useful formats: summaries, chapters, threads, blog posts, or quotes.

### Setup

The user has `youtube-transcript-api` installed in a virtual environment at `/opt/hermes/.venv/`. Before running any commands, check for and activate this virtual environment:

```bash
if [ -f \"/opt/hermes/.venv/bin/activate\" ]; then
    source \"/opt/hermes/.venv/bin/activate\"
fi
```

If the module is not found in the virtual environment, install it:

```bash
pip install youtube-transcript-api
```

This ensures you're using the pre-installed dependencies when available.

### Helper Script

The script accepts any standard YouTube URL format, short links (youtu.be), shorts, embeds, live links, or a raw 11-character video ID.

```bash
# JSON output with metadata
python3 SKILL_DIR/scripts/fetch_transcript.py \"https://youtube.com/watch?v=VIDEO_ID\"

# Plain text (good for piping into further processing)
python3 SKILL_DIR/scripts/fetch_transcript.py \"URL\" --text-only

# With timestamps
python3 SKILL_DIR/scripts/fetch_transcript.py \"URL\" --timestamps

# Specific language with fallback chain
python3 SKILL_DIR/scripts/fetch_transcript.py \"URL\" --language tr,en
```

### Output Formats

After fetching the transcript, format it based on what the user asks for:

- **Chapters**: Group by topic shifts, output timestamped chapter list
- **Summary**: Concise 5-10 sentence overview of the entire video
- **Chapter summaries**: Chapters with a short paragraph summary for each
- **Thread**: Twitter/X thread format — numbered posts, each under 280 chars
- **Blog post**: Full article with title, sections, and key takeaways
- **Quotes**: Notable quotes with timestamps

### Workflow

1. **Fetch** the transcript using the helper script with `--text-only --timestamps`.
2. **Validate**: confirm the output is non-empty and in the expected language. If empty, retry without `--language` to get any available transcript. If still empty, tell the user the video likely has transcripts disabled.
3. **Chunk if needed**: if the transcript exceeds ~50K characters, split into overlapping chunks (~40K with 2K overlap) and summarize each chunk before merging.
4. **Transform** into the requested output format. If the user did not specify a format, default to a summary.
5. **Verify**: re-read the transformed output to check for coherence, correct timestamps, and completeness before presenting.

### Error Handling

- **Transcript disabled**: tell the user; suggest they check if subtitles are available on the video page.
- **Private/unavailable video**: relay the error and ask the user to verify the URL.
- **No matching language**: retry without `--language` to fetch any available transcript, then note the actual language to the user.
- **Dependency missing**: 
  - First, check if the virtual environment exists and contains the `youtube-transcript-api` package.
  - If the package is missing, attempt to install it: `pip install youtube-transcript-api`.
  - If installation fails or the environment is not set up, provide alternative methods (see below).
  - Print a clear error message with troubleshooting steps.

### Alternative Methods When Primary Dependency Fails

If the primary `youtube-transcript-api` dependency is not available, follow these steps:

1. **Check virtual environment**: Verify that the virtual environment exists and contains the `youtube-transcript-api` package. Check multiple common locations:

   ```bash
   # Common virtual environment locations (in order of priority)
   VENV_PATHS=\"/opt/hermes/.venv /opt/data/.venv /opt/data/home/.venv /opt/data/scoreboard-venv\"
   
   for VENV in $VENV_PATHS; do
       if [ -f \"$VENV/bin/activate\" ]; then
           source \"$VENV/bin/activate\"
           pip list | grep youtube-transcript-api >/dev/null
           if [ $? -eq 0 ]; then
               echo \"youtube-transcript-api found in $VENV\"
               break
           fi
       fi
   done
   ```

2. **Install if missing**: If the package is not installed, install it using the available Python environment. Try multiple methods:

   ```bash
   # Try using uv if available
   if command -v uv >/dev/null; then
       uv pip install youtube-transcript-api
   # Try using pip in the current environment
   elif command -v pip >/dev/null; then
       pip install youtube-transcript-api
   # Try using the scoreboard-venv's pip
   elif [ -f \"/opt/data/scoreboard-venv/bin/pip\" ]; then
       /opt/data/scoreboard-venv/bin/pip install youtube-transcript-api
   fi
   ```

3. **Fallback options** (if installation fails):
   - **yt-dlp method**: If `yt-dlp` is installed, use it to fetch the transcript:
     ```bash
     yt-dlp --get-transcript \"URL\"
     ```
   - **API method**: Use a third-party transcript API (note: cloud IPs may be blocked):
     ```bash
     curl -s \"https://api.youtubetranscript.com/transcript/VIDEO_ID\"
     ```
   - **Browser method**: If vision tools are available, navigate to the video page and extract the transcript from the HTML.
   - **Alternative APIs**: Try different services that may have better cloud IP access:
     ```bash
     curl -s \"https://api.yntrk.ml/transcript/VIDEO_ID\"
     curl -s \"https://youtubetranscript.com/api/transcript/VIDEO_ID\"
     ```

4. **Provide clear guidance**: When all methods fail, inform the user with specific instructions based on the likely cause:
   - **IP blocking**: \"YouTube is blocking requests from your IP (common in cloud environments). Try using proxies with youtube-transcript-api or a different network.\"
   - **Missing dependencies**: \"Please ensure youtube-transcript-api or yt-dlp is installed.\"
   - **Browser issues**: \"Chrome not found. Install Chrome or use an alternative method.\"

### Cloud IP Blocking Workarounds

When using youtube-transcript-api from a cloud environment, you may encounter IP blocking. Here are effective workarounds:

#### 1. Use Proxies
```bash
# Set up proxies in the environment
export HTTP_PROXY=http://your-proxy:port
export HTTPS_PROXY=http://your-proxy:port

# Or configure youtube-transcript-api to use proxies
```

#### 2. Use Browser-Based Extraction
If vision tools are available, use the browser to navigate to the video page and extract the transcript from the embedded JSON state. This bypasses API blocks.

#### 3. Use Alternative Services
Some third-party APIs may have better access from cloud IPs. Test multiple services to find one that works.

#### 4. Use Cookies (Not Recommended)
Authenticating with YouTube cookies can provide temporary access, but may result in permanent account bans. Only use this if you don't mind your account being banned.

## Related Skills

- **trakt-watcher**: Automated Trakt.tv watching status monitoring
- **youtube-content**: YouTube transcript extraction and content transformation
- **ops-sentry**: Intelligent monitoring and alerting systems
- **external-content-ingestion**: Capturing external content into knowledge bases
- **social-media-content-aggregation**: Multi-platform social media monitoring

## References

- `references/trakt-oauth-flow.md` - Trakt OAuth authentication details
- `references/youtube-transcript-api.md` - API documentation and usage tips
- `scripts/trakt-watcher.py` - Main implementation script
- `scripts/fetch_transcript.py` - YouTube transcript fetching script
- `scripts/dependency_check.sh` - Dependency detection and installation