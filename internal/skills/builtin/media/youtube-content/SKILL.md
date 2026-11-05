---
name: youtube-content
description: "YouTube transcripts to summaries, threads, blogs."
---

# YouTube Content Tool

## When to use

Use when the user shares a YouTube URL or video link, asks to summarize a video, requests a transcript, or wants to extract and reformat content from any YouTube video. Transforms transcripts into structured content (chapters, summaries, threads, blog posts).

Extract transcripts from YouTube videos and convert them into useful formats.

## Setup

The user has youtube-transcript-api installed in a virtual environment at `/opt/hermes/.venv/`. Before running any commands, check for and activate this virtual environment:

```bash
if [ -f "/opt/hermes/.venv/bin/activate" ]; then
    source /opt/hermes/.venv/bin/activate
fi
```

If the module is not found in the virtual environment, install it:

```bash
pip install youtube-transcript-api
```

This ensures you're using the pre-installed dependencies when available.

## Helper Script

`SKILL_DIR` is the directory containing this SKILL.md file. The script accepts any standard YouTube URL format, short links (youtu.be), shorts, embeds, live links, or a raw 11-character video ID.

```bash
# JSON output with metadata
python3 SKILL_DIR/scripts/fetch_transcript.py "https://youtube.com/watch?v=VIDEO_ID"

# Plain text (good for piping into further processing)
python3 SKILL_DIR/scripts/fetch_transcript.py "URL" --text-only

# With timestamps
python3 SKILL_DIR/scripts/fetch_transcript.py "URL" --timestamps

# Specific language with fallback chain
python3 SKILL_DIR/scripts/fetch_transcript.py "URL" --language tr,en
```

## Output Formats

After fetching the transcript, format it based on what the user asks for:

- **Chapters**: Group by topic shifts, output timestamped chapter list
- **Summary**: Concise 5-10 sentence overview of the entire video
- **Chapter summaries**: Chapters with a short paragraph summary for each
- **Thread**: Twitter/X thread format — numbered posts, each under 280 chars
- **Blog post**: Full article with title, sections, and key takeaways
- **Quotes**: Notable quotes with timestamps

### Example — Chapters Output

```
00:00 Introduction — host opens with the problem statement
03:45 Background — prior work and why existing solutions fall short
12:20 Core method — walkthrough of the proposed approach
24:10 Results — benchmark comparisons and key takeaways
31:55 Q&A — audience questions on scalability and next steps
```

## Workflow

1. **Fetch** the transcript using the helper script with `--text-only --timestamps`.
2. **Validate**: confirm the output is non-empty and in the expected language. If empty, retry without `--language` to get any available transcript. If still empty, tell the user the video likely has transcripts disabled.
3. **Chunk if needed**: if the transcript exceeds ~50K characters, split into overlapping chunks (~40K with 2K overlap) and summarize each chunk before merging.
4. **Transform** into the requested output format. If the user did not specify a format, default to a summary.
5. **Verify**: re-read the transformed output to check for coherence, correct timestamps, and completeness before presenting.

## Error Handling

- **Transcript disabled**: tell the user; suggest they check if subtitles are available on the video page.
- **Private/unavailable video**: relay the error and ask the user to verify the URL.
- **No matching language**: retry without `--language` to fetch any available transcript, then note the actual language to the user.
- **Dependency missing**: 
  - First, check if the virtual environment exists and contains the `youtube-transcript-api` package.
  - If the package is missing, attempt to install it: `pip install youtube-transcript-api`.
  - If installation fails or the environment is not set up, provide alternative methods (see below).
  - Print a clear error message with troubleshooting steps.
- **Alternative methods when primary dependency fails**:
  - Use `yt-dlp` if available: `yt-dlp --get-transcript "URL"`
  - Use a browser-based extraction via vision tools (if available)
  - Use a third-party API: `curl -s "https://api.youtubetranscript.com/transcript/VIDEO_ID"`
  - Note: Some methods may have rate limits or require authentication.

## Handling Missing Dependencies

If the primary `youtube-transcript-api` dependency is not available, follow these steps:

1. **Check virtual environment**: Verify that the virtual environment exists and contains the `youtube-transcript-api` package. Check multiple common locations:
   ```bash
   # Common virtual environment locations (in order of priority)
   VENV_PATHS="/opt/hermes/.venv /opt/data/.venv /opt/data/home/.venv /opt/data/scoreboard-venv"
   
   for VENV in $VENV_PATHS; do
       if [ -f "$VENV/bin/activate" ]; then
           source "$VENV/bin/activate"
           pip list | grep youtube-transcript-api >/dev/null
           if [ $? -eq 0 ]; then
               echo "youtube-transcript-api found in $VENV"
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
   elif [ -f "/opt/data/scoreboard-venv/bin/pip" ]; then
       /opt/data/scoreboard-venv/bin/pip install youtube-transcript-api
   fi
   ```

3. **Fallback options** (if installation fails):
   - **yt-dlp method**: If `yt-dlp` is installed, use it to fetch the transcript:
     ```bash
     yt-dlp --get-transcript "URL"
     ```
   - **API method**: Use a third-party transcript API (note: cloud IPs may be blocked):
     ```bash
     curl -s "https://api.youtubetranscript.com/transcript/VIDEO_ID"
     ```
   - **Browser method**: If vision tools are available, navigate to the video page and extract the transcript from the HTML.
   - **Alternative APIs**: Try different services that may have better cloud IP access:
     ```bash
     curl -s "https://api.yntrk.ml/transcript/VIDEO_ID"
     curl -s "https://youtubetranscript.com/api/transcript/VIDEO_ID"
     ```

4. **Provide clear guidance**: When all methods fail, inform the user with specific instructions based on the likely cause:
   - **IP blocking**: "YouTube is blocking requests from your IP (common in cloud environments). Try using proxies with youtube-transcript-api or a different network."
   - **Missing dependencies**: "Please ensure youtube-transcript-api or yt-dlp is installed."
   - **Browser issues**: "Chrome not found. Install Chrome or use an alternative method."

## Cloud IP Blocking Workarounds

When using youtube-transcript-api from a cloud environment, you may encounter IP blocking. Here are effective workarounds:

### 1. Use Proxies
```bash
# Set up proxies in the environment
export HTTP_PROXY=http://your-proxy-server:port
export HTTPS_PROXY=http://your-proxy-server:port

# Or configure youtube-transcript-api to use proxies
```

### 2. Use Browser-Based Extraction
If vision tools are available, use the browser to navigate to the video page and extract the transcript from the embedded JSON state. This bypasses API blocks.

### 3. Use Alternative Services
Some third-party APIs may have better access from cloud IPs. Test multiple services to find one that works.

### 4. Use Cookies (Not Recommended)
Authenticating with YouTube cookies can provide temporary access, but may result in permanent account bans. Only use this if you don't mind your account being banned.

## Detailed Troubleshooting

For comprehensive troubleshooting procedures, error handling workflows, and environment-specific recommendations, refer to the **[troubleshooting.md](troubleshooting.md)** reference file.

## Dependency Management Script

Use the `scripts/dependency_check.sh` script to automate dependency detection and installation:

```bash
# Check available dependencies
scripts/dependency_check.sh check

# Install youtube-transcript-api
scripts/dependency_check.sh install

# Install yt-dlp
scripts/dependency_check.sh install-yt-dlp

# Detect best method
scripts/dependency_check.sh detect-method
```

## Browser-Based Extraction (Vision Tools)

When using vision tools to extract transcripts:

1. Navigate to the YouTube video page: `https://www.youtube.com/watch?v=VIDEO_ID`
2. Wait for the page to load and the transcript to become available
3. Extract the transcript from the HTML using vision analysis
4. Parse the timestamped text blocks

This method works even when API access is blocked, but requires Chrome to be installed and accessible.

## Error Messages and Troubleshooting

### YouTube IP Blocked
**Error**: "Could not retrieve a transcript for the video... YouTube is blocking requests from your IP."

**Solution**: 
- Use proxies with youtube-transcript-api
- Switch to browser-based extraction if vision tools are available
- Try alternative transcript APIs
- Use a non-cloud network if possible

### Missing Dependencies
**Error**: "youtube-transcript-api not installed" or "yt-dlp not found"

**Solution**: Install the required package using the available method in your environment.

### Browser Not Found
**Error**: "Chrome not found. Checked: agent-browser cache, system Chrome installations"

**Solution**: Install Chrome or use an alternative browser. Ensure the browser executable is in the system PATH.

## Environment Detection

The skill automatically detects the appropriate Python environment and tools. Common virtual environment locations are checked in order:
1. `/opt/hermes/.venv` (primary)
2. `/opt/data/.venv`
3. `/opt/data/home/.venv`
4. `/opt/data/scoreboard-venv`

If none of these contain youtube-transcript-api, the skill will attempt to install it using the first available method.

1. **Check virtual environment**: Verify that the virtual environment exists and contains the `youtube-transcript-api` package. Check multiple common locations:
   ```bash
   # Common virtual environment locations (in order of priority)
   VENV_PATHS="/opt/hermes/.venv /opt/data/.venv /opt/data/home/.venv /opt/data/scoreboard-venv"
   
   for VENV in $VENV_PATHS; do
       if [ -f "$VENV/bin/activate" ]; then
           source "$VENV/bin/activate"
           pip list | grep youtube-transcript-api >/dev/null
           if [ $? -eq 0 ]; then
               echo "youtube-transcript-api found in $VENV"
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
   elif [ -f "/opt/data/scoreboard-venv/bin/pip" ]; then
       /opt/data/scoreboard-venv/bin/pip install youtube-transcript-api
   fi
   ```

3. **Fallback options** (if installation fails):
   - **yt-dlp method**: If `yt-dlp` is installed, use it to fetch the transcript:
     ```bash
     yt-dlp --get-transcript "URL"
     ```
   - **API method**: Use a third-party transcript API (note: cloud IPs may be blocked):
     ```bash
     curl -s "https://api.youtubetranscript.com/transcript/VIDEO_ID"
     ```
   - **Browser method**: If vision tools are available, navigate to the video page and extract the transcript from the HTML.
   - **Alternative APIs**: Try different services that may have better cloud IP access:
     ```bash
     curl -s "https://api.yntrk.ml/transcript/VIDEO_ID"
     curl -s "https://youtubetranscript.com/api/transcript/VIDEO_ID"
     ```

4. **Provide clear guidance**: When all methods fail, inform the user with specific instructions based on the likely cause:
   - **IP blocking**: "YouTube is blocking requests from your IP (common in cloud environments). Try using proxies with youtube-transcript-api or a different network."
   - **Missing dependencies**: "Please ensure youtube-transcript-api or yt-dlp is installed."
   - **Browser issues**: "Chrome not found. Install Chrome or use an alternative method."

## Cloud IP Blocking Workarounds

When using youtube-transcript-api from a cloud environment, you may encounter IP blocking. Here are effective workarounds:

### 1. Use Proxies
```bash
# Set up proxies in the environment
export HTTP_PROXY=http://your-proxy:port
export HTTPS_PROXY=http://your-proxy:port

# Or configure youtube-transcript-api to use proxies
```

### 2. Use Browser-Based Extraction
If vision tools are available, use the browser to navigate to the video page and extract the transcript from the embedded JSON state. This bypasses API blocks.

### 3. Use Alternative Services
Some third-party APIs may have better access from cloud IPs. Test multiple services to find one that works.

### 4. Use Cookies (Not Recommended)
Authenticating with YouTube cookies can provide temporary access, but may result in permanent account bans. Only use this if you don't mind your account being banned.

## Browser-Based Extraction (Vision Tools)

When using vision tools to extract transcripts:

1. Navigate to the YouTube video page: `https://www.youtube.com/watch?v=VIDEO_ID`
2. Wait for the page to load and the transcript to become available
3. Extract the transcript from the HTML using vision analysis
4. Parse the timestamped text blocks

This method works even when API access is blocked, but requires Chrome to be installed and accessible.

## Error Messages and Troubleshooting

### YouTube IP Blocked
**Error**: "Could not retrieve a transcript for the video... YouTube is blocking requests from your IP."

**Solution**: 
- Use proxies with youtube-transcript-api
- Switch to browser-based extraction if vision tools are available
- Try alternative transcript APIs
- Use a non-cloud network if possible

### Missing Dependencies
**Error**: "youtube-transcript-api not installed" or "yt-dlp not found"

**Solution**: Install the required package using the available method in your environment.

### Browser Not Found
**Error**: "Chrome not found. Checked: agent-browser cache, system Chrome installations"

**Solution**: Install Chrome or use an alternative browser. Ensure the browser executable is in the system PATH.

## Environment Detection

The skill automatically detects the appropriate Python environment and tools. Common virtual environment locations are checked in order:
1. `/opt/hermes/.venv` (primary)
2. `/opt/data/.venv`
3. `/opt/data/home/.venv`
4. `/opt/data/scoreboard-venv`

If none of these contain youtube-transcript-api, the skill will attempt to install it using the first available method.

1. **Check virtual environment**: Verify that `/opt/hermes/.venv/` exists and contains the `youtube-transcript-api` package.
   ```bash
   if [ -f "/opt/hermes/.venv/bin/activate" ]; then
       source /opt/hermes/.venv/bin/activate
       pip list | grep youtube-transcript-api
   fi
   ```

2. **Install if missing**: If the package is not installed, install it:
   ```bash
   pip install youtube-transcript-api
   ```

3. **Fallback options** (if installation fails):
   - **yt-dlp method**: If `yt-dlp` is installed, use it to fetch the transcript:
     ```bash
     yt-dlp --get-transcript "URL"
     ```
   - **API method**: Use a third-party transcript API:
     ```bash
     curl -s "https://api.youtubetranscript.com/transcript/VIDEO_ID"
     ```
   - **Browser method**: If vision tools are available, navigate to the video page and extract the transcript from the HTML.

4. **Provide clear guidance**: When all methods fail, inform the user with specific instructions:
   > "Unable to fetch YouTube transcript. Please ensure the following dependencies are installed:
   > - youtube-transcript-api (recommended: `pip install youtube-transcript-api`)
   > - OR yt-dlp (recommended: `pip install yt-dlp`)
   > - OR grant browser access for vision-based extraction.
   > 
   > You can also try: `curl -s \"https://api.youtubetranscript.com/transcript/VIDEO_ID\"`"

This ensures the skill remains functional even in minimal environments and provides clear troubleshooting paths.
