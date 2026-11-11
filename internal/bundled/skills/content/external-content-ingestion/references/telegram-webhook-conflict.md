# Telegram Long-Poll vs Webhook Conflict (HTTP 409)

## Symptom

X Link Poller v2 (or any Telegram long-poll bot) logs:

```
2026-05-03 03:44:34,417 [ERROR] HTTP 409 fetching https://api.telegram.org/bot<token>/getUpdates?limit=100&timeout=5: Conflict
2026-05-03 03:44:34,417 [ERROR] Telegram 409 Conflict: Webhook is active. Long-poll and webhook are mutually exclusive.
```

All other platforms work; OpenRouter classification succeeds. The poller receives **zero messages from Telegram** every cycle despite the bot being in the chat and having valid token.

## Root Cause

Telegram Bot API enforces **mutual exclusion**: a bot can either receive updates via **webhook** (push) OR **long-poll** (`getUpdates`), never both.

If a webhook URL is registered for the bot, any `getUpdates` request returns **HTTP 409 Conflict** with error message: `"Conflict: webhook is active"`.

This often occurs after:
- Bot was previously used with a webhook (e.g., in a different project or earlier deployment)
- BotFather's `/setwebhook` command was used
- External platform (Heroku, Render, AWS Lambda) auto-configured a webhook during setup
- The webhook endpoint was never deleted after migrating to polling

## Diagnosis

### 1. Check current webhook status

```bash
TOKEN=$(grep TELEGRAM_BOT_TOKEN /opt/data/.env | cut -d= -f2- | tr -d '"' | tr -d "'")
curl -s "https://api.telegram.org/bot${TOKEN}/getWebhookInfo" | jq .
```

Expected output if webhook active:
```json
{
  "url": "https://your-app.herokuapp.com/telegram",
  "has_custom_certificate": false,
  "pending_update_count": 0,
  "last_error_message": null,
  "last_error_date": 0,
  "max_connections": 40,
  "allowed_updates": null
}
```

If `"url"` is non-empty (and not the empty string `""`), the webhook is active.

### 2. Verify it's truly blocking long-poll

From the error log: `HTTP 409 ... Conflict` combined with `"Webhook is active"` in the error message confirms mutual exclusion.

## Remediation

### Option A — Delete webhook (switch to long-poll / polling mode)

**Immediate fix:** Call `deleteWebhook` to unblock `getUpdates`:

```bash
curl -X POST "https://api.telegram.org/bot${TOKEN}/deleteWebhook"
```

Expected response:
```json
{"ok":true,"result":true,"description":"Webhook was deleted"}
```

**After deletion**, re-run the poller. The next `getUpdates` call should succeed (HTTP 200) and return messages (if any).

### Option B — Switch to webhook delivery (if you prefer push)

If you want to use webhook instead of polling:
1. Deploy a publicly reachable HTTPS endpoint (with valid SSL cert)
2. Register it: `curl -X POST "https://api.telegram.org/bot${TOKEN}/setWebhook?url=https://your.domain/telegram/path"`
3. Modify the poller script to *not* call `getUpdates`; instead have your webhook endpoint write incoming updates to a queue/state file that the daemon reads.

**Note:** Webhook mode is out of scope for the X Link Poller v2 architecture, which is designed as a cron/polling daemon. Use **Option A** unless you are deliberately re-architecting.

## Prevention & Pre-Flight Check

Add a **pre-flight webhook check** at daemon startup:

```python
def ensure_longpoll_mode(token: str) -> bool:
    """Guarantee Telegram long-poll is usable; delete stray webhook if found."""
    url = f"https://api.telegram.org/bot{token}/getWebhookInfo"
    try:
        resp = urlopen(url, timeout=10)
        info = json.loads(resp.read())
        if info.get("url"):
            log.warning(f"Telegram webhook active on {info['url']} — deleting to enable long-poll")
            delete_url = f"https://api.telegram.org/bot{token}/deleteWebhook"
            urlopen(delete_url, timeout=10)
            log.info("Telegram webhook deleted; long-poll mode restored")
            return True
        return True  # No webhook, long-poll already available
    except Exception as e:
        log.error(f"Failed to check/clear Telegram webhook: {e}")
        return False
```

Call this at startup before the first poll cycle. If it fails, log clear guidance and either retry once or exit with non-zero status (letting cron retry next cycle).

## Common Gotchas

| Gotcha | Why it happens | Fix |
|--------|----------------|-----|
| **Webhook reappears after BotFather `/setwebhook`** | Someone (or another script) runs `setWebhook` again | Document ownership of bot configuration; add a periodic webhook check (every cycle) to auto-correct |
| **Heroku/Render/Glitch auto-deploys with webhook** | Platform sample code includes `setWebhook` on boot | Remove `setWebhook` from deployment scripts |
| **Bot has multiple projects** | One project uses webhook, another uses polling | Use separate bots per delivery mode, or ensure only one mode is active at a time |
| **deleteWebhook returns false** | Bot token invalid or network issue; webhook persists | Verify token validity via `/getMe`; retry deletion; if persistent, manually remove via BotFather `/deletewebhook` command |

## BotFather Commands Reference

Telegram provides built-in commands to manage webhooks:

- `/setwebhook <url>` — register webhook URL
- `/deletewebhook` — remove active webhook (same as API call)
- `/getwebhookinfo` — show current webhook status

If API calls fail, use BotFather as a fallback: message `@BotFather`, send `/deletewebhook`, confirm.

## Verification

After remediation:

```bash
# 1. Confirm webhook cleared
curl -s "https://api.telegram.org/bot${TOKEN}/getWebhookInfo" | jq -r .url
# Expected: "" (empty string)

# 2. Test long-poll manually (should return JSON array or empty array, not 409)
curl -s "https://api.telegram.org/bot${TOKEN}/getUpdates?limit=1&timeout=1" | jq .
# Expected: [] (no updates) or [ {update objects...} ], status 200

# 3. Check poller logs for successful Telegram fetch
tail -f /opt/data/home/.hermes/logs/x_link_poller_$(date +%Y%m%d).log | grep -i "telegram"
# Should show: "INFO Polling telegram..." then either messages found or "No messages on telegram"
# No more ERROR 409 lines
```

## Related

- **Skill**: `external-content-ingestion` — Telegram polling architecture
- **Reference**: `telegram-bot-setup.md` — long-poll fundamentals, offset handling, chat ID discovery
- **Telegram Bot API**: https://core.telegram.org/bots/api#getupdates
