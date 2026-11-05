---
name: youtube-link-ingestor
description: "Autonomous cron job for extracting YouTube links from conversation history and saving to Obsidian vault with deduplication and fallback mechanism"
version: 1.0.0
author: Hermes Agent
license: MIT
metadata:
  hermes:
    tags: [automation, youtube, obsidian, cron, fallback]
---
# YouTube Link Ingestor Skill

## Overview
Autonomous cron job pattern for extracting YouTube links from conversation history and saving them to an Obsidian vault with proper formatting and deduplication. Includes a robust fallback mechanism when Hermes tools are unavailable.

## Trigger Conditions
- Runs every 30 minutes as a scheduled cron job
- Activated when YouTube links need to be harvested from conversation history
- Use when building automated content ingestion pipelines

## Prerequisites
- Obsidian vault structure with a `Raw/YouTube` directory
- Hermes Agent environment with access to conversation history
- Python 3.8+ with standard library

### Workflow

### 1. Primary Method (Hermes Tools Available)
- Use `hermes sessions list` tool to query recent conversations for YouTube links
- Extract video IDs using regex pattern
- Format as full youtu.be links

### 2. Fallback Method (Hermes Tools Unavailable)
- Scan markdown files in the Obsidian vault wiki directory
- Extract YouTube links directly from file content
- Robust regex matching for both youtube.com and youtu.be patterns

### 3. Deduplication & Storage
- Read existing YouTube-Links.md to build a set of known URLs
- Only save links that are not already present
- Append new links with format: `YYYY-MM-DD - [Link text](URL)`
- **Link text is fetched via YouTube's oEmbed API** when browser tools are unavailable, providing human-readable titles instead of just URLs

## Output & Delivery

This is a scheduled cron job, so its output is automatically delivered to the configured destination (typically via Hermes cron framework). The script should produce different outputs based on whether new links were found:

- **When new YouTube links are found:** Output the markdown-formatted list of links to stdout. Each link should be on its own line in the format: `YYYY-MM-DD - [Link text](URL)`. The cron framework will capture this output and deliver it to the configured channels (e.g., Discord, Telegram, or Hermes logs).

- **When no new YouTube links are found:** Output exactly `[SILENT]` to stdout. This signals to the cron framework that there is no content to deliver, and it will suppress notifications. This is the expected normal behavior for most runs.

**Important:** The script should never print debug logs, error messages, or status updates to stdout. Only the final digest (when links exist) or `[SILENT]` should go to stdout. All diagnostics should go to stderr or a log file.

## Python Script
The core implementation is in `scripts/save_youtube_links.py`. This script should be installed in the Hermes scripts directory and made executable.

## Configuration
- Vault path: `/opt/data/home/obsidian-vault/Vault`
- Output file: `Raw/YouTube-Links.md`
- Cron schedule: `*/30 * * * *`

## Pitfalls & Troubleshooting
- **Hermes tools missing**: The script automatically falls back to file scanning
- **Duplicate links**: Always check existing file before appending by comparing video IDs (the 11-character identifier after `v=`), not just full lines, to catch cases where the same video might be entered with different dates or slight variations
- **Post-append duplicates**: If after appending you discover a duplicate entry (e.g., from a previous run with a different date), verify by comparing video IDs and remove the newer duplicate to maintain a single source of truth
- **Link format**: Ensure consistent use of youtu.be short links for deduplication
- **Permissions**: Verify script has read access to vault and write access to Raw/YouTube directory
- **Incorrect command**: The `hermes session_search` command does not exist. Use `hermes sessions list` or `hermes sessions browse` instead. This is a common mistake from outdated documentation.
- **Incorrect command**: The `hermes session_search` command does not exist. Use `hermes sessions list` or `hermes sessions browse` instead.

## Verification When No New Links Found

When the ingestor runs and finds no new links, it's crucial to verify this conclusively before outputting "[SILENT]". False negatives can cause missed content and silent failures.

### Systematic Verification Process

1. **Check existing file state** - Determine last successful update date
2. **Search session history** - Use `hermes sessions list` for recent conversations containing YouTube links
3. **Direct file analysis** - When search tools are unavailable or for higher confidence, grep recent session files directly
4. **Cross-reference** - Compare extracted video IDs against existing file content
5. **Timestamp verification** - Ensure all relevant sessions have been considered

### When to Output "[SILENT]"

The cron job should output "[SILENT]" only after:
- Comprehensive verification shows no new YouTube links exist
- All detected links are already present in the file
- The file is confirmed to be up to date

This indicates successful completion with no new content, which is the expected normal behavior.

### Logging for Auditability

For auditability, log verification steps when outputting "[SILENT]":
```python
if no_new_links:
    print(f"[SILENT] - Verified no new YouTube links after {last_update_time}")
    # Optionally log detailed verification to a debug file
```

### Common Pitfalls

- **Incomplete verification** - Don't rely solely on `session_search`; when in doubt, perform direct file scanning
- **Time zone mismatches** - Ensure consistent time zones when comparing timestamps
- **Missing fallback coverage** - The fallback mechanism should also implement these verification steps
- **Silent failures** - A true "[SILENT]" output means the system is working correctly; no action needed

### Reference

For detailed verification procedures, see `references/verification-procedures.md`.

## Enhancements
Future improvements could include:
- Better link text extraction from conversation context
- More sophisticated conversation history detection
- Support for additional video platforms

**Current Implementation:** Video titles are fetched via YouTube's oEmbed API (`https://www.youtube.com/oembed`) as a reliable fallback when browser tools are unavailable. This provides human-readable titles and is documented in `references/oembed-api.md`. Alternatively, `https://noembed.com/embed` can be used as a simpler, no-auth alternative (see `references/alternative-title-fetching.md`).

## References
For examples of similar link polling patterns, see the `x_link_polestor` skills.

---

## Tool Selection for Verification

When verifying ingestion results, consider the strengths of different tools:

- **Terminal tools (grep, awk, sort, uniq)**: Excellent for quick processing of text files, especially the YouTube-Links.md format. Fast and memory-efficient.
- **Python scripts**: Better for complex logic, API integration, and structured data processing. May require careful regex tuning for specific file formats.
- **Hermes tools**: Provide the richest context and should be used when available.

For routine verification of the YouTube-Links.md file, terminal tools are often the most straightforward and reliable choice.

### Terminal-Based Verification Techniques

For practical terminal-based verification commands, see `references/terminal-verification.md`.
---