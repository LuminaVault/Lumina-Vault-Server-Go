---
name: ai-cohort-scoreboard-marketing
category: content
description: Generate daily Reddit and X/Twitter marketing content from the AI Cohort scoreboard, with automatic fallback to the most recent available data when today's file is missing.
---
# AI Cohort Scoreboard Marketing Content Generation

**Purpose:** Generate daily Reddit and X/Twitter marketing content from the AI Cohort scoreboard, with automatic fallback to the most recent available data when today's file is missing.

**Trigger Conditions:**
- Daily cron job scheduled after 8:30am ET (Eastern Time)
- Manual invocation when marketing content needs update
- Error recovery: When `generate_marketing_content.py` fails with "Today's scoreboard not found"

**Workflow Steps:**

1. **Check for Today's Scoreboard**
   ```python
   eastern = tz.gettz('America/New_York')
   today = datetime.now(eastern).strftime('%Y-%m-%d')
   # VAULT_ROOT should be: '/opt/data/home/obsidian-vault/Vault'
   scoreboard_path = f"{VAULT_ROOT}/{today} — AI Scoreboard.md"
   ```
   - The scoreboard file is saved directly in the vault root by the scoreboard generator script
   - Ensure VAULT_ROOT matches the scoreboard script's VAULT_ROOT
   - If today's scoreboard exists → use it
   - If missing → find latest available scoreboard (by date descending)
   - Never skip generation; always produce output

3. **Generate Marketing Content**
   - Parse the scoreboard markdown into sections
   - Format Reddit post with: Quick Take, Winners Cohort, Live Scoreboard, Insider Buys, Top Signals, Source attribution
   - Format X/Twitter thread with 5 tweets covering: performance gap, scoreboard table, top signals, insider highlights, cohort attribution
   - Write files to `/opt/data/home/.hermes/output/marketing/`
     - `reddit_ai_scoreboard_{date}.md`
     - `x_thread_ai_scoreboard_{date}.md`

4. **Report Status**
   - Success with today's date when using current data
   - Success with fallback notice when using older data, including which date was used
   - Include error context if generation failed completely

**Pitfalls & Gotchas:**
- ⚠️ **Path Consistency**: All scripts must use the same VAULT_ROOT path. The correct path is `/opt/data/home/obsidian-vault/Vault` (without the `.hermes` prefix). Inconsistent paths cause "scoreboard not found" errors even when the file exists.
- ⚠️ Missing historical data: If NO scoreboard files exist, the task cannot proceed. Verify the cohort directory contains at least one scoreboard file.
- ⚠️ Date format consistency: Scoreboard files use `YYYY-MM-DD` format with en dash separator. Ensure path construction matches exactly.
- ⚠️ Output date: Always stamp output files with today's date, even when using fallback data. This maintains cron job consistency.

**Verification:**
```bash
# Check output files exist
ls -lh /opt/data/home/.hermes/output/marketing/ | grep "reddit_ai_scoreboard_$(date +%Y-%m-%d)"

# Validate content structure
head -20 /opt/data/home/.hermes/output/marketing/reddit_ai_scoreboard_$(date +%Y-%m-%d).md
```

**References:**
- `references/error-transcript-2026-05-06.md` - Full error context and resolution steps
- `references/scoreboard-directory-structure.md` - Expected vault organization

**Related Skills:**
- `content-digests` - Periodic content digest patterns
- `external-content-ingesting` - Content ingestion workflows
- `x-post-ingestor` - Social media content ingestion

**Last Updated:** 2026-05-06