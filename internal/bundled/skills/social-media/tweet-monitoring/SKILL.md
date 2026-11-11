---
name: tweet-monitoring
title: Tweet Monitoring — Hermes Tweet Monitor Skill
description: Monitors X/Twitter timelines and keyword searches for relevant content and generates daily digests.
triggers:
  - Cron job execution (typically daily)
  - Manual invocation when user wants to check recent tweets
  - When troubleshooting social media ingestion issues
scope: >
  Covers the Hermes Tweet Monitor script (`~/.hermes/scripts/tweet_monitor.py`) — configuration, operation, troubleshooting, and maintenance.
  Includes both API mode and Nitter RSS fallback behavior.
  Addresses credential requirements, rate limiting, and output generation.
---

# Tweet Monitoring — Hermes Tweet Monitor Skill

**Skill description:** Monitors X/Twitter timelines and keyword searches for relevant content and generates daily digests.

**Trigger conditions:**
- Cron job execution (typically daily)
- Manual invocation when user wants to check recent tweets
- When troubleshooting social media ingestion issues

**Purpose:** 
Automatically scan @fatc88's timeline and following list, plus keyword searches for stocks, NBA, SL Benfica, tech, AI, Swift, iOS, and Go. Generate daily Markdown digests of relevant tweets and deliver them to configured channels (Slack by default).

## Configuration & Requirements

### Required Credentials
The script requires Twitter API v2 credentials:
- **BEARER_TOKEN**: Application-only auth token
- **ACCESS_TOKEN**: User context auth token (optional but recommended)

Credentials can be provided in two ways:
1. **JSON file** at `/opt/data/home/.hermes/scripts/.twitter_creds`:
```json
{
  "BEARER_TOKEN": "your_bearer_token_here",
  "ACCESS_TOKEN": "your_access_token_here"
}
```

2. **Environment variables**:
- `TWITTER_BEARER_TOKEN`
- `TWITTER_ACCESS_TOKEN`

### Optional Configuration
Via environment variables:
- `TWITTER_FETCH_FOLLOWING` (default: `true`) - Set to `false` to conserve API credits
- `TWITTER_FOLLOWING_SAMPLE_SIZE` (default: `30`) - Number of followed accounts to sample

### Script Location
- Main script: `~/.hermes/scripts/tweet_monitor.py` (typical location)
- In some installations, it may be found at `$HERMES_HOME/.hermes/scripts/tweet_monitor.py` or under the user's home directory (e.g., `/root/.hermes/home/.hermes/scripts/tweet_monitor.py`)

To locate the script, search for `tweet_monitor.py` in the `.hermes` directory:
```bash
find ~/.hermes -name "tweet_monitor.py"
```

## Operation Flow

### Normal API Mode (Preferred)
1. Authenticate with X API v2 using provided credentials
2. Fetch target user ID for `@fatc88`
3. Retrieve recent tweets from target user (last 2 days)
4. Fetch following list and sample tweets from followed accounts
5. Perform keyword searches across recent tweets
6. Filter tweets matching configured topics
7. Rank by engagement + recency
8. Generate Markdown digest
9. Print preview to stdout and save full digest to output file

### Nitter RSS Fallback Mode
If API authentication fails (401/403), the script switches to Nitter RSS mirrors:
- Scrapes Nitter instances for RSS feeds
- Extracts tweets from target user, following accounts, and keyword searches
- Provides basic monitoring when API credentials are invalid or rate limited

**Note:** The fallback mode is triggered by authentication failures, not just missing credentials. If credentials are completely absent, the script may exit early. To ensure fallback mode works, provide some credentials (even if expired) so the script attempts API calls and falls back gracefully.

## Common Issues & Troubleshooting

### Missing Credentials
**Error:** `ERROR: TWITTER_BEARER_TOKEN not set in script or environment`

**Fix:** Provide Twitter API credentials via JSON file or environment variables.

### API Rate Limits
The script includes rate limit handling (1-second delays between requests). If rate limited, consider:
- Setting `FETCH_FOLLOWING=false` to reduce API calls
- Using Nitter fallback mode (though less reliable)
- Rotating API credentials

### Nitter Instances Unreachable
Fallback mode tries multiple Nitter instances. If all fail:
- Check internet connectivity
- Verify Nitter instances are online
- Consider obtaining proper Twitter API credentials

## Setup Instructions

1. Obtain Twitter API v2 credentials (Developer account required)
2. Add credentials to `.twitter_creds` JSON file or environment variables
3. Ensure script is executable: `chmod +x ~/.hermes/scripts/tweet_monitor.py`
4. Verify cron job is configured (usually daily at 08:00 UTC)
5. Check output in `~/.hermes/output/` for daily digests

## Verification

To test the script manually:
```bash
python3 ~/.hermes/scripts/tweet_monitor.py
```

Successful execution returns exit code 0 and prints digest preview to stdout.

## Maintenance Notes

- The script is sensitive to X API v2 changes
- Nitter instances may change or go offline; update fallback list as needed
- Consider adding error handling to allow fallback mode even when credentials are missing

## Related Skills
- `social-media-xurl` - X/Twitter posting and interaction via xurl CLI
- `x-link-poller` - Monitoring X/Twitter for URLs and content ingestion
- `media` - General media monitoring and content ingestion