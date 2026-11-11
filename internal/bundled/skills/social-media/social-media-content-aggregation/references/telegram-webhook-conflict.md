# Telegram Webhook Conflict — 409 Error Resolution

## Symptom

`HTTP 409 Conflict` when calling `getUpdates`:
```json
{
  "ok": false,
  "error_code": 409,
  "description": "Conflict: webhook is active"
}
```

## Cause

Telegram bots accept **either** webhook push **or** long-poll `getUpdates`, never both. A webhook was previously registered for this bot and remains active.

## Resolution

### 1. Delete the webhook (permanent until re-set)

```bash
curl -s -X POST "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/deleteWebhook"
```

Expected response:
```json
{"ok":true,"result":true,"description":"Webhook was deleted"}
```

### 2. Verify webhook removal

```bash
curl -s "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getWebhookInfo" | jq
```

Expected:
```json
{
  "ok": true,
  "result": {
    "url": "",
    "has_custom_certificate": false,
    "pending_update_count": 0,
    ...
  }
}
```
`"url"` must be empty or `null`.

### 3. Restart the poller

After webhook deletion, restart the daemon to begin long-poll cycles immediately.

## Prevention

If you control both webhook and poll consumers:
- **Webhook mode:** Set `TELEGRAM_WEBHOOK_URL` env var; poller switches to webhook mode automatically (if implemented).
- **Poll mode:** Ensure no webhook is set anywhere in your infrastructure (CI/CD, other services).

Never mix both in the same bot token. If you need both push and pull consumers, create **two separate bot tokens** — one for webhook, one for polling.

## Related

- Telegram Bot API docs: [getUpdates](https://core.telegram.org/bots/api#getupdates) vs [setWebhook](https://core.telegram.org/bots/api#setwebhook)
- Error 409 is permanent until webhook is deleted; retrying `getUpdates` without deletion always fails.
