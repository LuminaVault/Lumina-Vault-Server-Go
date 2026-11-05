# Multi-Platform Polling Pattern

Full-featured polling script template for ingesting X/Twitter links from Discord, Telegram, and Slack.

## State Schema

```json
{
  "discord_last_msg_id": "string or null",
  "telegram_last_update_id": 0 or null,
  "slack_last_ts": "string or null",
  "processed_urls": {
    "<sha256(url)[:16]>": {
      "url": "original url",
      "title": "article title",
      "saved_at": "ISO datetime",
      "topic": "AI|Dev/Swift|...",
      "method": "llm|keyword"
    }
  }
}
```

State file: `~/.hermes/state/x_link_poller_state.json`. Load at startup, save after each run.

## Platform Fetch Functions

### Discord
```python
def fetch_discord_messages(token, channel_id, last_msg_id):
    url = f"https://discord.com/api/v10/channels/{channel_id}/messages?limit=50"
    headers = {
        "Authorization": f"Bot {token}",
        "Content-Type": "application/json",
        "User-Agent": "HermesAgent/1.0"
    }
    body = http_get(url, headers, timeout=15)
    messages = json.loads(body)  # newest-first
    return list(reversed(messages))  # oldest-first for sequential processing
```
Cursor: store `state["discord_last_msg_id"] = newest_message_id`. Skip messages where `msg_id <= last_msg_id`.

### Telegram
```python
def fetch_telegram_messages(token, last_update_id):
    url = f"https://api.telegram.org/bot{token}/getUpdates?limit=100&timeout=5"
    if last_update_id:
        url += f"&offset={last_update_id + 1}"
    data = json.loads(http_get(url, timeout=20))
    if not data.get("ok"):
        return []
    updates = data.get("result", [])
    messages = []
    max_id = last_update_id or 0
    for upd in updates:
        msg = upd.get("message") or upd.get("channel_post")
        if msg:
            messages.append(msg)
        max_id = max(max_id, upd.get("update_id", 0))
    if max_id:
        state["telegram_last_update_id"] = max_id
    return messages
```
**Important:** Telegram bot must be added to the target chat. If not, `getUpdates` returns empty indefinitely.

### Slack
```python
def fetch_slack_messages(token, channel_id, last_ts):
    url = f"https://slack.com/api/conversations.history?channel={channel_id}&limit=50"
    headers = {"Authorization": f"Bearer {token}"}
    data = json.loads(http_get(url, headers, timeout=15))
    if not data.get("ok"):
        return []
    messages = list(reversed(data.get("messages", [])))
    return messages
```
Cursor: `state["slack_last_ts"] = newest_message_ts`.

## Message Text Extraction

Platforms differ; normalize:
```python
text = msg.get("content", "") or msg.get("text", "") or msg.get("message", "") or ""
```

## URL Extraction Regex

```python
X_URL_RE = re.compile(r"(https?://(?:www\.)?(?:x\.com|fixupx\.com|twitter\.com)/[\w/]+)")
urls = [m.group(1) for m in X_URL_RE.finditer(text)]
```

## Deduplication

Per-URL hash for global state tracking:
```python
url_hash = hashlib.sha256(url.encode()).hexdigest()[:16]
if url_hash in processed_urls:
    continue
```

## Complete Loop Structure

```python
def main():
    state = load_state()
    processed = state.get("processed_urls", {})
    new_count = 0

    platforms = [
        ("discord",  fetch_discord,  state.get("discord_last_msg_id")),
        ("telegram", fetch_telegram, state.get("telegram_last_update_id")),
        ("slack",    fetch_slack,    state.get("slack_last_ts")),
    ]

    for platform, fetch_fn, last_id in platforms:
        messages = fetch_fn()
        newest_id = None
        for msg in messages:
            msg_id = extract_msg_id(msg)
            if last_id and msg_id <= last_id:
                continue
            if newest_id is None:
                newest_id = msg_id
            urls = extract_x_urls(msg_text(msg))
            for url in urls:
                if url_hash(url) in processed:
                    continue
                title, body = fetch_article(url)
                if should_skip(title, body):
                    continue
                topic, method = classify(title, body)
                save_to_vault(topic, title, body, url, platform, msg_id, method)
                processed[url_hash(url)] = { ... }
                new_count += 1
        save_platform_cursor(state, platform, newest_id)

    save_state(state)
    if new_count > 0:
        compile_wiki()
```

## Error Handling Strategy

- **Network/API failure:** Log error, continue to next platform. Do not crash.
- **Missing token:** `log.warning()` and skip platform.
- **Protected article (403):** Save placeholder. LLM may still classify from title/URL.
- **LLM 504/timeout:** Retry 3× with 1s/2s backoff; fall back to keywords on final failure.
- **State unreadable:** Start fresh (empty state) if JSON corrupt; log warning.
- **Cron environment:** If tokens appear missing under cron, ensure Hermes daemon exports them OR source `/opt/data/.env` in the crontab line.

## Token-Stripping Pitfall

**.env files store values with trailing newlines.** Manual parsing must strip:
```python
val = line.split('=', 1)[1].strip()
```
Hermes daemon already strips before export; standalone scripts must strip themselves. Without stripping, Discord/Telegram/Slack API calls fail with `401 Unauthorized` or `Invalid token`.
