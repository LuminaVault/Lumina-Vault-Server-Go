# Polling Pattern for External Content Ingestion

When platform webhooks are unavailable or undesirable, use a cron-based polling script to detect and ingest external content.

## Architecture

```
cron (every 15 min)
    ↓
  poller.py
    ↓
Fetch recent messages from each platform (Discord/Telegram/Slack)
    ↓
Extract X/twitter/fixupx URLs via regex
    ↓
Deduplicate against state (SHA256(URL) → 16 char hex)
    ↓
Fetch article text (r.jina.ai/http://<url>)
    ↓
Classify topic (LLM first, keywords fallback)
    ↓
Save to Raw/<Topic>/YYYY-MM-DD — <title>.md
    ↓
Update state cursors & processed_urls
    ↓
If new articles: run kb-compile
```

## State File

Location: `~/.hermes/state/x_link_poller_state.json`

Schema:
```json
{
  "discord_last_msg_id": "string (Discord message ID, newest processed)",
  "telegram_last_update_id": 123456789,
  "slack_last_ts": "1746...",
  "processed_urls": {
    "a1b2c3d4e5f6g7h8": {
      "url": "https://x.com/...",
      "title": "...",
      "saved_at": "2026-05-02T10:43:32.549Z",
      "topic": "AI"
    }
  }
}
```

## Platform-Specific Notes

### Discord
- Endpoint: `GET /channels/{channel_id}/messages?limit=50`
- Headers: `Authorization: Bot <token>`, `Content-Type: application/json`
- Response: array of message objects, newest-first
- Process in reverse order (oldest → newest) to maintain monotonic `id` cursor
- Message text field: `content`
- Message ID field: `id` (snowflake string)

### Telegram
- Endpoint: `GET /bot<token>/getUpdates?limit=100&offset=<last_id+1>&timeout=5`
- Long-poll: `timeout` param up to 30s; cron interval must exceed poll timeout
- Response array elements are *updates*; extract `message` or `channel_post`
- Tracking: store highest `update_id` seen; pass `offset=last_id+1` on next fetch
- Caveat: Bot MUST be in the chat (added as member or admin). Empty updates → no messages.

### Slack
- Endpoint: `GET /api/conversations.history?channel=<channel>&limit=50`
- Headers: `Authorization: Bearer <xoxb-...>`
- Response: `{ "ok": true, "messages": [...] }`
- Message ID field: `ts` (string timestamp)
- Process newest-first → reverse to oldest-first for cursor tracking

## Deduplication

- URL → SHA256 → 16 hex chars (first 8 bytes) as key in `processed_urls`
- Prevents re-saving same article even if reposted across platforms
- State file grows indefinitely; implement periodic prune (keep last N entries or age cutoff) if needed

## Classification

Hybrid: LLM (OpenRouter) with 3-retry exponential backoff → keyword fallback.
Prompt: "Classify this X/Twitter article into ONE of: AI, Dev/Swift, Stocks, Health, Tech, Business, News, XFeed"
LLM response validated against allowed list; any mismatch triggers fallback.

## Error Handling

- Platform fetch errors: log warning, continue to next platform
- Rate limit responses (429): backoff per-platform, next cron will retry
- r.jina.ai 403: mark as `X Article (Protected)`; do not retry
- r.jina.ai 404: mark as `X Article (404)`; do not retry
- Network errors: log but do not retry within same poll (next cron will naturally retry)

## Deployment

Cron entry (via Hermes cron):
```
*/15 * * * *  name: x-link-poller, repeat: forever, prompt: "Run python3 -u /opt/data/home/.hermes/scripts/x_link_poller_v2.py"
```

Ensure environment (DISCORD_BOT_TOKEN, TELEGRAM_BOT_TOKEN, SLACK_BOT_TOKEN, OPENROUTER_API_KEY) is available. When testing from shell, source the Hermes env:
```bash
source /opt/data/.env  # or rely on daemon-inherited env
python3 -u /opt/data/home/.hermes/scripts/x_link_poller_v2.py
```

## Extension Points

- Add more platforms (Reddit, YouTube) by adding fetcher + URL regex
- Enrich classification with more granular topics
- Store article images (extract from tweet media fields) if needed
- Trigger notifications on specific keywords (e.g., "portfolio", "earnings") via separate cron job
