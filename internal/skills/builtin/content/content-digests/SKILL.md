---
name: content-digests
description: "Build and deliver periodic content digests (news, stock, entertainment) to multiple platforms: save to vault Raw/ and print full markdown to stdout for cron-based delivery."
version: 1.0.0
author: Hermes Agent
license: MIT
metadata:
  hermes:
    tags: [content, digests, news, aggregation, multi-platform]
---

# Content Digest Pipelines

This umbrella skill governs the construction and delivery of **periodic content digests** — news, stock updates, entertainment listings, and similar recurring data aggregations — to **multiple platforms** (Discord, Telegram, Slack) while **archiving to an Obsidian vault**.

## Core Principles

1. **Dual output** — Every digest script must:
   - Save full markdown to the vault's `Raw/<Category>/` directory for permanent archival
   - Print the complete markdown to stdout for cron-based platform delivery

2. **Platform-agnostic** — The same script drives all delivery channels. Cron configuration decides which platforms receive the stdout capture. No per-platform script branching.

3. **Chunking for size-limited platforms** — Telegram (4096 char limit) and Slack (~4000 chars) require splitting large digests into sequential chunks with part numbering. Discord handles larger messages (~8KB) but chunking still improves UX.

4. **Vault-first organization** — All content lives in `Raw/<Category>/<Date> <Digest Name>.md` inside the Obsidian vault. The filename convention is `YYYY-MM-DD <Digest Name>.md`.

## When to Use

Load `content-digests` when:
- Building a recurring news or data aggregation script
- Setting up periodic delivery to Discord/Telegram/Slack
- Ensuring content is both **viewable in-channel** and **persisted to knowledge base**
- Refactoring a script that only saves to file but doesn't print content

## Trigger Conditions

- User asks for daily/weekly digest of any domain (tech news, stocks, entertainment)
- Existing digest script only writes to file without stdout delivery
- Need to add a new platform channel (Telegram, Slack) to an existing digest
- Fixing "content not appearing in channel" complaints

## Implementation Recipe

### Step 1 — Vault paths

```python
from pathlib import Path
import os, datetime

VAULT = Path.home() / "obsidian-vault" / "Vault"  # adjust vault name
RAW_DIR = VAULT / "Raw" / "<Category>"          # e.g. Raw/Dev/Swift, Raw/Entertainment
RAW_DIR.mkdir(parents=True, exist_ok=True)
now = datetime.datetime.now()
outfile = RAW_DIR / f"{now.strftime('%Y-%m-%d')} <Digest Name>.md"
```

### Step 2 — Build markdown (`md` string)

Consistent header:
```python
md = f"""# <Digest Name> — {now.strftime('%Y-%m-%d %H:%M')}

> Summary line with counts per source
---
"""
md += "\n".join(entries)  # your collected items
md += f"\n\n✅ Saved to `{outfile}`"
```

### Step 3 — Save to vault (always)

```python
with open(outfile, 'w') as f:
    f.write(md)
print(f"✅ <Digest> saved ({len(entries)} items) → {outfile}")
```

### Step 4 — Print to stdout for cron delivery

**Critical**: This must be the LAST print statement, outside all loops, with no extra formatting before or after (except optional separator).

```python
# Separator helps platform delivery bots distinguish payload
print("\\n" + "="*60)
print(md)   # FULL markdown content
```

**Pitfall**: Double-printing (inside/outside loops) causes duplicate messages. Ensure only one `print(md)` call after saving.

### Step 5 — Chunking (for Telegram/Slack)

If the script is invoked per-platform with size limits, wrap the final delivery in a chunker:

```python
def chunk_send(content, limit=3900):
    """Yield chunks ending at newline boundaries."""
    lines = content.split('\n')
    chunk = []
    size = 0
    for line in lines:
        line_nl = line + '\n'
        if size + len(line_nl) > limit and chunk:
            yield ''.join(chunk)
            chunk = [line_nl]
            size = len(line_nl)
        else:
            chunk.append(line_nl)
            size += len(line_nl)
    if chunk:
        yield ''.join(chunk)

full_payload = "\\n" + "="*60 + "\\n" + md
for idx, part in enumerate(chunk_send(full_payload), 1):
    header = f"🔹 <Digest> (Part {idx}/{})" if total>1 else f"🔹 <Digest>"
    print(header + "\\n\\n" + part)
```

However, **prefer letting the cron delivery bot handle chunking** by delivering the full stdout to each configured platform. Only implement manual chunking in the script if the bot doesn't support automatic splitting.

## Digest-Specific Conventions

| Digest | Vault Category | Channel (Discord) | Frequency | Sources |
|--------|----------------|-------------------|-----------|---------|
| Swift News | `Raw/Dev/Swift/` | `#swift-news` (YOUR_DISCORD_CHANNEL_ID) | Daily 09:00 | Hacking with Swift, Swift Forums, HN, GitHub |
| Go News | `Raw/Dev/Go/` | `#go-news` (YOUR_DISCORD_CHANNEL_ID) | Daily 09:00 | Go Blog, Go Forum, HN, GitHub |
| Stock News | `Raw/Dev/Stock/` | `#stock-plan` (YOUR_DISCORD_CHANNEL_ID) | Daily 09:00 | Yahoo Finance, Motley Fool |
| Entertainment | `Raw/Entertainment/` | `#entertainment` (YOUR_DISCORD_CHANNEL_ID) | Daily/Weekly | TVmaze, MyAnimeList, IMDb |
| IMDb Weekly | `Raw/Movies/`, `Raw/TV/Seasonal/`, `Raw/Anime/` | `#entertainment` | Weekly (Fri) | IMDb datasets |

## Validation

```bash
# 1. Script runs without error
python3 digest_script.py

# 2. File saved to vault
ls -l /opt/data/obsidian-vault/Vault/Raw/Dev/Swift/$(date +%Y-%m-%d)\ *.md

# 3. Stdout contains full markdown (not just summary)
python3 digest_script.py | head -c 500   # should show content, not just "✅ saved"

# 4. Cron job configured with correct model (for LLM-backed digests)
hermes cron list | grep <digest-name>
# Model should be anthropic/claude-sonnet-4.6 (NOT openrouter/anthropic/claude-sonnet-4)
```

## Common Pitfalls

| Pitfall | Symptom | Fix |
|---------|---------|-----|
| `print(md)` inside a loop | Duplicate content in channel | Move `print(md)` to AFTER the loop, at root scope |
| Forgetting stdout entirely | Cron delivers only the summary line | Add `print(md)` after file save |
| Printing debug to stdout | Debug text appears in Telegram/Discord | Send diagnostics to `sys.stderr` or a log file |
| Platform 400s due to oversize | Telegram rejects message (>4096 chars) | Chunk output or reduce digest size |
| Wrong output directory | File saved but cron can't find it | Ensure stdout is captured, not just file I/O |
| Security restrictions on piped commands | Command fails with security warning | Use intermediate file: `script.py > /tmp/output && script2.py < /tmp/output` instead of `script.py | script2.py` |

## Cron Job Template

```bash
hermes cron create "0 9 * * *" \
  --name "daily-swift-news" \
  --prompt "Aggregate Swift news and deliver to Discord #swift-news" \
  --script "swift_news_digest.py" \
  --model "anthropic/claude-sonnet-4.6" \
  --deliver "discord:YOUR_DISCORD_CHANNEL_ID"
```

**Multi-platform variant** — Create separate jobs with same script but different `--deliver` targets (Discord, Telegram, Slack) to ensure all channels receive the digest:

```bash
# Discord
hermes cron create "0 9 * * *" --name "swift-news-discord" \
  --script "swift_news_digest.py" --model "anthropic/claude-sonnet-4.6" \
  --deliver "discord:YOUR_DISCORD_CHANNEL_ID"

# Telegram Home
hermes cron create "0 9 * * *" --name "swift-news-telegram" \
  --script "swift_news_digest.py" --model "anthropic/claude-sonnet-4.6" \
  --deliver "telegram:476978568"

# Slack Home
hermes cron create "0 9 * * *" --name "swift-news-slack" \
  --script "swift_news_digest.py" --model "anthropic/claude-sonnet-4.6" \
  --deliver "slack:C0B0BDGEJTT"
```

Alternatively, a single job with `--deliver origin` broadcasts to all configured platforms if the gateway is set up to mirror.

## References

- `references/multi-platform-delivery-pattern.md` — stdout-driven delivery and chunking strategies
- `references/vault-raw-convention.md` — `Raw/<Category>/` file organization
- `scripts/digest-template.py` — minimal working example (see `scripts/` directory)
