# Cron Job Setup & Platform Troubleshooting (2026-05-03)

## Overview

The Hermes multi-platform X link poller is implemented as a daemon (`x_link_poller_v2.py`) but cron jobs require a one-shot execution. This reference documents the correct setup, common error patterns, and fixes.

## Script Locations (IMPORTANT)

**DO NOT use** `/opt/data/scripts/multi_link_poller.py` — that is a DEMO/TEMPLATE that only handles `--urls` arguments and prints "platform polling not yet implemented".

**ACTUAL WORKING IMPLEMENTATION:**
```
/opt/data/home/.hermes/scripts/x_link_poller_v2.py
```

State file: `/opt/data/home/.hermes/state/x_link_poller_state.json`  
Vault Raw: `/opt/data/obsidian-vault/Vault/Raw/`

## One-Shot Cron Wrapper Pattern

Because the poller runs as an infinite loop daemon, create a one-shot wrapper for cron:

```bash
#!/bin/bash
# /opt/data/scripts/run_x_link_poller_cron.sh

LOCK_FILE="/tmp/x_link_poller_cron.lock"
SCRIPT="/opt/data/home/.hermes/scripts/x_link_poller_v2.py"

# Prevent overlapping runs
if [ -f "$LOCK_FILE" ]; then
    OLD_PID=$(cat "$LOCK_FILE" 2>/dev/null)
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
        echo "$(date): Already running (PID $OLD_PID). Exiting." >> /tmp/x_link_poller_cron.log
        exit 1
    else
        rm -f "$LOCK_FILE"
    fi
fi

echo $$ > "$LOCK_FILE"
echo "$(date): Starting one-shot poll" >> /tmp/x_link_poller_cron.log

# Load Hermes environment
if [ -f /opt/data/.env ]; then
    source /opt/data/.env
fi

# Run poller ONCE by temporarily modifying its __main__ behavior
# Method: Use Python one-shot runner
python3 -c "
import sys, os, importlib.util
from pathlib import Path

# Load env (redundant but safe)
env_path = Path('/opt/data/.env')
if env_path.exists():
    for line in env_path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith('#'): continue
        if line.startswith('export '): line = line[7:]
        if '=' not in line: continue
        key, _, val = line.partition('=')
        os.environ.setdefault(key.strip(), val.strip().strip('\"\"'''))

# Import poller module
spec = importlib.util.spec_from_file_location('poller', '/opt/data/home/.hermes/scripts/x_link_poller_v2.py')
mod = importlib.util.module_from_spec(spec)
sys.modules['poller'] = mod
spec.loader.exec_module(mod)

# Call main() once (bypasses while True loop because __name__ != '__main__' when imported)
mod.main()
" 2>&1 | tee -a /tmp/x_link_poller_cron.log

EXIT_CODE=${PIPESTATUS[0]}
rm -f "$LOCK_FILE"
echo "$(date): Poll finished (exit $EXIT_CODE)" >> /tmp/x_link_poller_cron.log
exit $EXIT_CODE
```

**Crontab entry (every 15 minutes):**
```cron
*/15 * * * * /opt/data/scripts/run_x_link_poller_cron.sh
```

## Platform Error 403 Forbidden
**Observed on:** Discord  
**Symptoms:**
```
[ERROR] HTTP 403 fetching https://discord.com/api/v10/channels/YOUR_DISCORD_CHANNEL_ID/messages?limit=50: Forbidden
[ERROR] Discord 403 Forbidden on channel YOUR_DISCORD_CHANNEL_ID. Bot needs 'Read Message History' permission in this channel.
```

**Cause:** Discord bot token is valid but the bot lacks **Read Message History** permission in the target channel.

**Fix:**
1. Go to Discord Server Settings → Roles → Find the bot's role
2. Ensure the role has **Read Message History** permission enabled
3. If the role already has it, check channel-specific overrides: right-click the channel → Edit Channel → Permissions → ensure the bot's role isn't explicitly denied "Read Message History"
4. The bot must also be **above** the @everyone role in the role hierarchy for channel permission overrides to work

**Verification:** After fixing, run the poller once — it should log `Polling discord...` followed by either "No messages on discord" or actual message count.

## Platform Error 409 Conflict (Telegram)
**Observed on:** Telegram  
**Symptoms:**
```
[ERROR] HTTP 409 fetching https://api.telegram.org/bot<token>/getUpdates?limit=100&timeout=5: Conflict
[ERROR] Telegram 409 Conflict: Webhook is active. Long-poll and webhook cannot coexist.
```

**Cause:** The Telegram bot has an active webhook set (probably via another Hermes integration or manual setup). Telegram bots can use **either** webhooks **or** `getUpdates` long-poll, not both simultaneously.

**Fix:**
```bash
# Option 1: Delete webhook (switch to polling)
curl -X POST "https://api.telegram.org/bot<YOUR_TOKEN>/deleteWebhook"

# Option 2: If webhook is desired, remove TELEGRAM_BOT_TOKEN from poller env
# and disable cron job. Use webhook-based ingestion instead.
```

**Verification:** After deleting webhook, re-run poller. Telegram should return either messages or an empty list (not 409).

## State File Growth
The `processed_urls` dictionary grows unbounded. Current state file size is ~5 KB with ~60 entries. Consider periodic pruning if running long-term.

No action needed for cron job — the state is small and JSON-based.

## Compilation Trigger
The poller automatically runs `kb-compile` only when `new_count > 0`. If no new X links were found in the poll cycle, compilation is skipped (saves CPU). This is correct behavior.

Manual compile (if needed):
```bash
python3 /opt/data/obsidian-vault/Vault/scripts/compile_wiki.py --root /opt/data/obsidian-vault/Vault
```

## Environment Variables Required
All tokens must be present in `/opt/data/.env` and sourced by the wrapper:
- `DISCORD_BOT_TOKEN` — Discord bot token
- `TELEGRAM_BOT_TOKEN` — Telegram bot token
- `SLACK_BOT_TOKEN` — Slack bot token (xoxb-...)
- `DISCORD_MONITOR_CHANNEL` — Channel ID to poll (default: `YOUR_DISCORD_CHANNEL_ID`)
- `TELEGRAM_HOME_CHANNEL` — Chat ID (default: `476978568`)
- `SLACK_HOME_CHANNEL` — Channel/IM ID (default: `C0B0BDGEJTT`)
- `OPENROUTER_API_KEY` — Optional; enables LLM classification (Claude Haiku)

**Note on newlines:** `.env` files may store values with trailing newlines. The wrapper above strips them. If setting tokens manually in crontab, ensure no newlines.

## Current Channel Status (2026-05-03)
| Platform | Channel ID | Status |
|----------|-----------|--------|
| Discord | `YOUR_DISCORD_CHANNEL_ID` (monitor) | 403 — Read Message History permission missing |
| Telegram | `476978568` (home) | 409 — Webhook active; must delete before polling |
| Slack | `C0B0BDGEJTT` (home) | OK — but no X links found in last scan |
