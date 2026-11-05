# Session Error: Missing Twitter API Credentials

**Date:** 2026-05-08  
**Context:** Scheduled cron job execution of Hermes Tweet Monitor  
**Error:** `ERROR: TWITTER_BEARER_TOKEN not set in script or environment`  
**Exit Code:** 1

## Problem Description
The Hermes Tweet Monitor script (`~/.hermes/scripts/tweet_monitor.py`) failed to execute because required Twitter API v2 credentials were not configured. The script expects either:
- A JSON credentials file at `/opt/data/home/.hermes/scripts/.twitter_creds`
- Environment variables `TWITTER_BEARER_TOKEN` and `TWITTER_ACCESS_TOKEN`

Neither was present in the environment.

## Root Cause
The cron job is configured to run automatically, but the Twitter API credentials were never set up in this Hermes instance. This is a common setup oversight when deploying the tweet monitoring functionality.

## Resolution Steps
1. Obtain Twitter API v2 credentials (requires Twitter Developer account)
2. Create credentials file:
```bash
echo '{"BEARER_TOKEN": "your_bearer_token_here", "ACCESS_TOKEN": "your_access_token_here"}' > /opt/data/home/.hermes/scripts/.twitter_creds
```
3. Or set environment variables:
```bash
export TWITTER_BEARER_TOKEN="your_bearer_token_here"
export TWITTER_ACCESS_TOKEN="your_access_token_here"
```
4. Test the script manually:
```bash
python3 ~/.hermes/scripts/tweet_monitor.py
```
5. Verify output in `~/.hermes/output/tweet_digest_YYYY-MM-DD.md`

## Important Notes
- The script includes a Nitter RSS fallback mode, but it won't activate unless the credential check is bypassed or modified
- Consider adding error handling to allow fallback mode even when credentials are missing
- The cron job will continue to fail until credentials are properly configured
- This error will be delivered to the configured Slack channel (default: #general or configured channel)

## Prevention
- Add credential validation to the cron job setup process
- Implement automatic fallback to Nitter RSS when API credentials are missing
- Create a setup script that guides users through credential configuration

## Related Documentation
- Main script: `~/.hermes/scripts/tweet_monitor.py`
- Output location: `~/.hermes/output/`
- Skill documentation: `tweet-monitoring` skill