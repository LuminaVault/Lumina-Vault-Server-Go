# Telegram Bot Setup & Long-Poll Pattern

## Bot Creation

1. Message `@BotFather` on Telegram
2. `/newbot` → choose name, username
3. Copy the **HTTP API Token** (format: `1234567890:ABCdefGHIjklMNOpqrsTUVwxyz...`)
4. Store in environment: `TELEGRAM_BOT_TOKEN=<token>`

## Adding Bot to Chats

The bot must be a participant in any chat you want to monitor.

- **Private chat:** Open bot in Telegram, click **Start** (sends `/start`). Bot now has a chat with you.
- **Group/channel:** Add bot as member (admin privileges optional). Ensure **"Allow group messages?"** is enabled via BotFather if needed.

## getUpdates Long-Poll

Telegram Bot API is pull-based. The bot polls for updates:

```
GET https://api.telegram.org/bot<token>/getUpdates?limit=100&timeout=30&offset=<N>
```

- `limit` — max updates to return (1–100). Use 100 for efficiency.
- `timeout` — long-poll wait time in seconds (max 30). Cron interval should exceed this to avoid overlapping polls.
- `offset` — return updates with `update_id >= offset`. Use `last_update_id + 1` to fetch only new updates.

**Important:** If `offset` is not set, every poll returns *all* stored updates (up to 100 most recent). Setting `offset` to `last_update_id + 1` ensures only new updates are returned.

## Update Object Structure

Each update has an `update_id` (monotonically increasing integer). The message may be in:
- `update.message` — private message to bot
- `update.channel_post` — message in a channel where bot is admin
- `update.edited_message` / `update.edited_channel_post` — edits (typically ignore for ingestion)

Extract content:
```python
msg = update.get("message") or update.get("channel_post")
if msg:
    text = msg.get("text", "")
    # For media with caption: msg.get("caption", "")
```

## Common Pitfalls

**No messages returned even though bot is in chat:**
- Bot may not have received any messages *since last offset*. Check that your state's `telegram_last_update_id` is current. If state is stale (e.g., bot offline), you may skip new messages. Solution: if `getUpdates` returns empty and `last_update_id` is old, consider resetting offset with `offset=-1` to clear backlog (use cautiously — may re-fetch old messages).
- Bot is in chat but hasn't been **started** (private chat requires user to send `/start` first). Without `/start`, no `message` updates are generated.

**"URL can't contain control characters" error:**
- Cause: `TELEGRAM_BOT_TOKEN` read from `.env` file includes a trailing newline (`\n`). When constructing the URL `f"/bot{token}/getUpdates..."`, the newline becomes part of the URL string.
- Fix: Always `.strip()` token value. When loading from `.env`, use `value = line.split('=',1)[1].strip()`. Hermes daemon automatically strips; standalone scripts must do it manually.

**Rate limits:**
- Telegram allows ~30 requests/sec per bot. 15-min polling with 100-update fetch is well within limits.
- If you hit `429 Too Many Requests`, response includes `retry_after` seconds. Back off accordingly.

**Missing messages across multiple chats:**
- `getUpdates` aggregates updates from **all** chats the bot participates in. You'll receive messages from every group/private chat unless you filter.
- Use `msg.get("chat", {}).get("id")` to identify which chat a message came from. Compare against your configured `TELEGRAM_HOME_CHANNEL` (numeric chat ID). Discard messages from other chats.

## Getting Chat ID

Your home channel ID (`TELEGRAM_HOME_CHANNEL`) is a numeric ID (e.g., `476978568`). To discover it:

1. Send any message to your bot in the target chat.
2. Poll `getUpdates` manually:
   ```bash
   curl "https://api.telegram.org/bot<token>/getUpdates"
   ```
3. Find the `message` object; look for `"chat": {"id": 476978568, ...}`.

For groups with negative IDs (supergroups), the ID is still numeric.

## State Recovery After Downtime

If the poller was offline and missed updates:
- Telegram retains updates for up to 24 hours (configurable via BotFather's `mode` settings; default retains ~100 updates).
- If you restart with old `last_update_id`, Telegram will return updates from `offset=last_update_id+1`. If those updates were already deleted (exceeded retention), Telegram returns empty and you've lost messages. To recover, reset offset to `-1` (fetch most recent 100) and re-process, but you'll re-fetch already-processed messages (deduplication via URL hash handles that).
- **Recommended:** Keep poller running continuously (cron every 15 min). If you must restart, consider running once with `offset=-1` to catch any missed updates.

## References

- Telegram Bot API docs: https://core.telegram.org/bots/api
- getUpdates method: https://core.telegram.org/bots/api#getupdates
