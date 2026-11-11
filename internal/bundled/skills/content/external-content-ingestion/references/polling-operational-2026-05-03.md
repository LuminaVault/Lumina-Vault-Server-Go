# Poller Operational Findings — 2026-05-03

## Script Discovery

| Location | Purpose |
|----------|---------|
| `/opt/data/scripts/multi_link_poller.py` | Demo/template — processes only `--urls`, no platform polling |
| `/opt/data/home/.hermes/scripts/x_link_poller_v2.py` | **Production** — polls Discord/Telegram/Slack, classifies, saves, compiles |

**Rule:** Always call the v2 script. The demo script exists only as a reference scaffold.

## One-Cycle Debug Run

```bash
timeout 90s python3 /opt/data/home/.hermes/scripts/x_link_poller_v2.py 2>&1
```

Observed timestamps:
```
2026-05-03 20:08:23 — startup
2026-05-03 20:08:23 — Discord 403 (missing Read Message History in YOUR_DISCORD_CHANNEL_ID)
2026-05-03 20:08:27 — Telegram 409 (webhook active on bot 8708552435)
2026-05-03 20:08:28 — Slack OK (no X URLs found)
2026-05-03 20:08:28 — "Sleeping 300s before next poll cycle…" (timed out before sleep elapsed)
```

**Exit code:** `124` (SIGTERM from `timeout`). State cursors updated correctly despite timeout.

## Platform Error Signatures

### Discord
```
HTTP 403 fetching https://discord.com/api/v10/channels/<channel_id>/messages?limit=50: Forbidden
Discord 403 Forbidden on channel <channel_id>. Bot needs 'Read Message History' permission in this channel.
Current: can SEND but cannot READ. Fix: add bot role → channel → Permissions → 'Read Message History'.
```
- **Cause:** Bot role lacks `Read Message History`. Often present at server level but overridden by channel permission deny.
- **Fix:** In Discord server settings → Roles → bot role → enable **Read Message History**; check channel-specific permission overrides to ensure not denied.

### Telegram
```
HTTP 409 fetching https://api.telegram.org/bot<token>/getUpdates?limit=100&timeout=5: Conflict
Telegram 409 Conflict: Webhook is active. Long-poll and webhook are mutually exclusive.
```
- **Cause:** Webhook endpoint registered for this bot; `getUpdates` long-poll blocked.
- **Fix:**
  ```bash
  curl -X POST "https://api.telegram.org/bot<YOUR_TOKEN>/deleteWebhook"
  # Also unset TELEGRAM_WEBHOOK_URL in /opt/data/.env
  ```
- Verify bot has been added to the target chat; otherwise `getUpdates` returns empty.

### Slack
- Working. No URLs detected in monitored channel `C0B0BDGEJTT` during this run. Consider monitoring additional channels if expected X URLs are elsewhere.

## Daemon Behavior

- After each full poll cycle (all 3 platforms), script calls `time.sleep(300)` (5 minutes).
- State cursors are saved **after each platform's batch** (Discord/Slack) or per-update (Telegram) so process can be killed and resumed safely.
- No CLI flags to force one-shot runs; use `timeout` or patch `while True` for cron.

## Compile Script Discovery

The poller tries, in order:
1. `VAULT_ROOT/scripts/compile_wiki.py` (`/opt/data/obsidian-vault/Vault/scripts/compile_wiki.py`)
2. `~/.hermes/scripts/compile_wiki.py` (`/opt/data/home/.hermes/scripts/compile_wiki.py`)

On this system, only the second path exists — poller would find it at runtime. Skill docs previously assumed only vault-location; updated in SKILL.md to reflect fallback.

## State Location

Fixed path, not `Path.home()` relative:
```
/opt/data/home/.hermes/state/x_link_poller_state.json
```

Ensures consistent state across users/cron contexts.

## Channel Configuration

Relevant env vars in `/opt/data/.env`:
```bash
DISCORD_MONITOR_CHANNEL=YOUR_DISCORD_CHANNEL_ID   # channel to poll (differs from default YOUR_DISCORD_CHANNEL_ID)
DISCORD_HOME_CHANNEL=YOUR_DISCORD_CHANNEL_ID      # for bot responses (unused by poller)
TELEGRAM_HOME_CHANNEL=476978568
SLACK_HOME_CHANNEL=C0B0BDGEJTT
```

The poller uses `DISCORD_MONITOR_CHANNEL`, not the `_HOME_` variant.
