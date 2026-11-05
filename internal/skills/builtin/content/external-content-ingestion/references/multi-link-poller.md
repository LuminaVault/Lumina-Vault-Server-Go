# Multi-Link Poller Deployment

**Skill:** `external-content-ingestion`  
**Script:** `~/.hermes/scripts/multi_link_poller.py` → `/opt/data/scripts/multi_link_poller.py`  
**Cron job:** `multi-link-poller` (`*/15 * * * *`, repeat forever, deliver=origin)  
**Deployed:** 2026-05-03

## Purpose

Unified polling script that extracts **both X/Twitter** and **GitHub repository** URLs from configured platform channels (Discord/Telegram/Slack), fetches content, classifies topic, and ingests into the Obsidian vault `Raw/`. Designed to replace the older `x-link-poller` with broader source coverage.

## Architecture

```
[Platform APIs] → recent messages → regex URL extraction
      ↓
   Deduplication (URL hash + state file)
      ↓
   Source-type dispatch:
     ├─ x.com / twitter.com / fixupx.com → r.jina.ai → auth-wall detection → classify
     ├─ github.com/<owner>/<repo>       → GitHub API + README (base64) → classify
      ↓
   Save to Raw/<Topic>/YYYY-MM-DD — <title>.md
      ↓
   Trigger kb-compile (if any new saves)
      ↓
   Update state file ~/.hermes/state/multi_link_poller_state.json
```

## State Schema

```json
{
  "last_discord_id": "1498123456789",
  "last_telegram_update_id": 123456789,
  "last_slack_ts": "1746123456.789",
  "processed_urls": {
    "a1b2c3d4e5f6a7b8": {
      "url": "https://x.com/user/status/123",
      "title": "Article title",
      "topic": "AI",
      "saved_at": "2026-05-03T12:00:00"
    }
  }
}
```

**`processed_urls`** keys are `sha256(url)[:16]` — prevents cross-topic duplicates (same URL posted to different platforms won't re-save).

## Usage

### Manual run (test)
```bash
~/.hermes/scripts/multi_link_poller.py --urls https://x.com/twostraws/status/123 https://github.com/user/repo
```

### Platform polling (not yet implemented)
Currently the script supports `--urls` explicit list. Platform integration (Discord/Telegram/Slack message fetch) is scaffolded but not active; add API fetch logic when tokens and channels are ready.

### Cron schedule
- **Frequency:** every 15 minutes (`*/15 * * * *`)
- **Job ID:** `7fc27b895e42` (`multi-link-poller`)
- **Workdir:** `/opt/data/scripts`
- **Script:** `multi_link_poller.py` (must be under `$HERMES_HOME/scripts/` for cron)
- **Deliver:** `origin` (stdout summary to all configured platforms)

## Dependencies

- `requests` — already installed in user site-packages
- No external binaries

## Extension Points

To add new source types (e.g., YouTube, RSS blogs):
1. Add URL pattern detection in `process_url()`: `elif "youtube.com" in url or "youtu.be" in url:`
2. Implement `fetch_youtube_video(url)` returning `{"title":..., "body":...}`
3. Call `save_note()` with appropriate `source_type` and classification
4. Update this reference doc with new source row

## Related

- Replaces older single-source `x-link-poller` (still runs in parallel)
- Shares vault layout and `kb-compile` pipeline with all `external-content-ingestion` flows
- Classification uses same keyword map as X ingestion