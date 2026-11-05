---
name: social-media-content-aggregation
description: "Build and operate multi-platform social media monitors that poll Discord/Telegram/Slack for links, classify content, and persist to a knowledge base — full lifecycle: credentials, polling patterns, deduplication, LLM classification, vault integration, and scheduled compilation."
triggers:
  - "poll discord/telegram/slack for links"
  - "cross-platform content aggregation"
  - "social media monitoring daemon"
  - "fetch links from multiple platforms"
  - "persist social content to vault"
  - "content pipeline from chat to knowledge base"
version: 1.0.1
author: Hermes Agent
license: MIT
platforms: [linux, macos]
prerequisites:
  commands: [python3, curl]
  env_vars: [DISCORD_BOT_TOKEN, TELEGRAM_BOT_TOKEN, SLACK_BOT_TOKEN, OPENROUTER_API_KEY]
---

# Social Media Content Aggregation — Multi-Platform Link Harvesting

Use this skill when building a **persistent, scheduled daemon** that:
- Polls multiple chat platforms (Discord, Telegram, Slack) for X/twitter/fixupx URLs
- Extracts article content via a fetch service (e.g., r.jina.ai)
- Classifies topics using an LLM (OpenRouter/Claude Haiku) with fallback keyword matching
- Saves articles to an Obsidian vault under `Raw/{topic}/`
- Triggers vault compilation on new content
- Maintains persistent state to avoid re-processing

This pattern decouples **source monitoring** (social chat platforms) from **content ingestion** (vault/wiki). Common in personal knowledge management and intelligence gathering workflows.

---

## Architecture

```
cron / systemd
   ↓
x_link_poller_v2.py  (this daemon)
   ├─ Poll Discord (REST API, requires Read Message History)
   ├─ Poll Telegram (getUpdates long-poll; conflicts with webhook)
   ├─ Poll Slack (conversations.history)
   ├─ Extract X URLs (regex: x.com, twitter.com, fixupx.com)
   ├─ Fetch article text (r.jina.ai/http)
   ├─ Classify topic (OpenRouter LLM → fallback keywords)
   ├─ Save to Raw/{topic}/ (YYYY-MM-DD — title.md)
   └─ On new items → run compile_wiki.py
   ↓
Obsidian Vault (compiled)
```

**State persistence:** `~/.hermes/state/x_link_poller_state.json`
- `processed_urls`: `{url_hash → {url, title, saved_at}}`
- Platform cursors: `discord_last_msg_id`, `telegram_last_update_id`, `slack_last_ts`

---

## Setup Checklist

- [ ] Credentials present in environment or `.env`:
  - `DISCORD_BOT_TOKEN` + `DISCORD_MONITOR_CHANNEL` (channel ID to read)
  - `TELEGRAM_BOT_TOKEN` + `TELEGRAM_HOME_CHANNEL` (numeric chat ID)
  - `SLACK_BOT_TOKEN` + `SLACK_HOME_CHANNEL` (channel ID)
  - `OPENROUTER_API_KEY` (for classification)
- [ ] Discord bot has **Read Message History** permission in monitored channel *(see Discord pitfalls)*
- [ ] Telegram webhook **deleted** if previously set (long-poll mode only) *(see Telegram pitfalls)*
- [ ] Vault paths exist and are writable: `/opt/data/obsidian-vault/Vault/Raw/`
- [ ] Compile script exists: `/opt/data/obsidian-vault/Vault/scripts/compile_wiki.py`
- [ ] Test run produces log output and/or new article files

---

## Platform-Specific Gotchas

### Discord — "Read Message History" Required

Fetching channel messages (`GET /channels/{id}/messages`) requires the bot's role to have **Read Message History** enabled in that channel, **in addition to** View Channel and Send Messages.

**Error:** `HTTP 403 Forbidden` with hint:
> "Bot needs 'Read Message History' permission in this channel. Current: can SEND but cannot READ."

**Fix:**
1. Server Settings → Channels → select monitored channel → Edit Channel → Permissions
2. Find the bot's role (or @everyone if inherited)
3. Toggle **Read Message History** to ✔️ Allow
4. If a category-level override denies it, clear or override at channel level
5. Re-test with a direct API call or script run

**Why this is separate from View Channel:** View Channel allows seeing the channel in the UI; Read Message History allows reading past messages via API. Reading history is a stricter permission.


### Telegram — Webhook Conflicts with Long-Poll

Telegram bots support **either** webhook **or** `getUpdates` long-poll, not both simultaneously.

**Error:** `HTTP 409 Conflict` on `getUpdates`:
> "Webhook is active. Long-poll and webhook are mutually exclusive."

**Fix:** Delete the webhook once (permanent until re-set):
```bash
curl -X POST "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/deleteWebhook"
```

**Verification:**
```bash
curl "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getWebhookInfo"
# Should show empty "url" or has_custom_certificate false
```

**Note:** If a webhook URL is required elsewhere, you cannot run long-poll concurrently. Choose one delivery mechanism.

### Telegram — Channel vs Private Chat

The `TELEGRAM_HOME_CHANNEL` must be a **group or supergroup channel ID**, not a private user chat. A private chat yields `type: "private"` and has no broadcast content to monitor.

**Diagnostic:** Check chat type:
```bash
curl "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getChat?chat_id=${TELEGRAM_HOME_CHANNEL}"
```
Expected `type: "channel"` or `"group"` / `"supergroup"`. If `"private"` — obtain a channel ID from a group where the bot is a member.

**Channel ID format:** Negative numbers for groups/supergroups; positive for private chats.

### Slack — No Special Gotchas

Slack's `conversations.history` works with a standard bot token scoped to the channel. Ensure:
- Bot is invited to the channel (`/invite @bot`)
- Token has `channels:history` scope (usually automatic for classic bots)
- Channel ID correct (starts with `C` for public channels)

---

## Environment Loading Pattern

The poller loads environment from `/opt/data/.env` (Hermes-wide) at startup via `load_hermes_env()`:
- Supports `export VAR=value` and `VAR=value` lines
- Strips surrounding quotes
- Respects existing process environment (uses `os.environ.setdefault`)

**Tip:** Store tokens in that file, not in the user's shell profile, so cron jobs inherit them.

---

## Topic Classification Strategy

Primary: **OpenRouter LLM** (`anthropic/claude-3-haiku`) with structured prompt:
```
Classify this X/Twitter article into ONE of these topics:
AI, Dev/Swift, Stocks, Health, Tech, Business, News, XFeed

Article title: {title}
Article snippet: {content[:1000]}

Respond ONLY with the topic name. No punctuation.
```

**Fallback:** Keyword matching against `TOPIC_KEYWORDS` (per-topic word lists).

**Method attribution:** Save both the method (`llm` or `keywords`) in frontmatter for audit.

---

## Vault Integration

- **Save path:** `{VAULT_ROOT}/Raw/{topic}/{YYYY-MM-DD} — {sanitized-title}.md`
- **Frontmatter:** `source`, `url`, `date`, `tags`, `classification`, `classification_method`
- **Compilation trigger:** Only when `new_count > 0`
- **Compile command:** `python3 {VAULT_ROOT}/scripts/compile_wiki.py --root {VAULT_ROOT}`

---

## Operational Notes

- **Poll frequency:** 300 seconds (5 minutes) — adjust as needed
- **Deduplication:** SHA256(url)[:16] stored in state; survives restarts
- **Platform cursors:** 
  - Discord: `id` from message objects (string)
  - Telegram: `update_id` + 1 offset (long-poll only; webhook conflicts)
  - Slack: `ts` (timestamp string, sortable)
- **Error handling:** Platform fetch errors are logged but do not abort the cycle; other platforms continue
- **State file location:** Explicit absolute path (avoid `Path.home()` under cron)
- **Process hygiene:** Always ensure exactly one daemon instance is running. Multiple overlapping instances cause state corruption and duplicate work. Before starting, check `ps aux | grep x_link_poller_v2.py` and kill stale PIDs. If running under cron, implement file-based locking. *(see `references/daemon-instance-management.md` and `references/continuous-wrapper-script.md` for implementation patterns)*
- **Log rotation:** Rotate logs periodically to avoid filling disk space

---

## X Link Poller v2 — Proven Implementation Pattern

The canonical implementation lives at `/opt/data/home/.hermes/scripts/x_link_poller_v2.py`. It embodies this skill's architecture with the following operational characteristics:

**Versioned script path:** Explicit version suffix (`_v2.py`) enables safe in-place updates without breaking running instances.

**Environment loading:** Calls `load_hermes_env()` at module import to read `/opt/data/.env` (Hermes-wide credentials). Supports `export VAR=value` and `VAR=value` syntax.

**Graceful degradation:** If `OPENROUTER_API_KEY` is missing, falls back to `TOPIC_KEYWORDS` matching without LLM classification.

**Compile trigger guard:** Only invokes `compile_wiki.py` when `new_count > 0` to avoid unnecessary rebuilds.

**State cursors per platform:**
- `discord_last_msg_id` → string or `null`
- `telegram_last_update_id` → integer or `null`
- `slack_last_ts` → string or `null`

**Vault Raw topic directories:** `AI/`, `Dev/`, `Swift/`, `Stocks/`, `Health/`, `Tech/`, `Business/`, `News/`, `XFeed/`, plus domain-specific extensions (`Careers/`, `TV and Movies/`, etc.).

**Runtime supervision:** Designed for systemd/cron. If running under cron, add a lockfile (`.hermes/state/x_link_poller.lock`) to prevent overlapping runs.

---

## Diagnostics & State Reconciliation

When vault file count and `processed_urls` diverge, use this recovery sequence:

1. **Audit current state:** Examine `/opt/data/home/.hermes/state/x_link_poller_state.json` — check `processed_urls` count and timestamps.
2. **Inventory vault:** `find /opt/data/obsidian-vault/Vault/Raw -name "*.md" | wc -l` and cross-reference X URL presence.
3. **Identify cause:** Multiple stale daemon instances, corrupted state file, or manual vault edits.
4. **Reconcile:** If vault is authoritative and state is stale, rebuild state from vault files:
   ```bash
   python3 -c "
   import os, json, hashlib, re
   from pathlib import Path
   vault = Path('/opt/data/obsidian-vault/Vault/Raw')
   state = {'processed_urls': {}, 'discord_last_msg_id': None, 'telegram_last_update_id': None, 'slack_last_ts': None}
   for f in vault.rglob('*.md'):
       text = f.read_text()
       urls = re.findall(r'https?://(?:x\\.com|twitter\\.com|fixupx\\.com)/[\\w/]+', text)
       for url in urls:
           h = hashlib.sha256(url.encode()).hexdigest()[:16]
           state['processed_urls'][h] = {'url': url, 'saved_at': str(f.stat().st_mtime)}
   Path('/opt/data/home/.hermes/state/x_link_poller_state.json').write_text(json.dumps(state, indent=2))
   print(f'Rebuilt: {len(state[\"processed_urls\"])} URLs')"
   ```
5. **Restart single daemon instance** and verify next cycle logs.

---

## Platform Connectivity Verification (Pre-flight)

Run these checks to pre-empt blocking issues:

**Discord:**
```bash
curl -H "Authorization: Bot $DISCORD_BOT_TOKEN" \
  "https://discord.com/api/v10/channels/$DISCORD_MONITOR_CHANNEL/messages?limit=1"
```
Expected: `200 OK` with message array. `403` → bot lacks **Read Message History** permission.

**Telegram:**
```bash
curl "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/getChat?chat_id=$TELEGRAM_HOME_CHANNEL"
```
Expected: `200 OK` with chat object. If `type: "private"` → you configured a user chat, not a channel. Switch to a group/channel ID for broadcast monitoring.

**Slack:**
```bash
curl -H "Authorization: Bearer $SLACK_BOT_TOKEN" \
  "https://slack.com/api/conversations.info?channel=$SLACK_HOME_CHANNEL"
```
Expected: `{"ok": true, "channel": {...}}`. If `false` → check `is_member` and channel scope.

Perform these before relying on scheduled runs.

---

## Telemetry & Observability

**Log aggregation:** Both stdout and stderr are captured to the same log file when run via Hermes infrastructure. Rotate logs with `logrotate` or Hermes log rotation.

**Metrics (optional):** Increment a counter in `/opt/data/home/.hermes/metrics/x_link_poller_cycle.json` on each cycle to enable monitoring of missed schedules.

**Health endpoint:** Expose a simple check that confirms process is alive and state file fresh (< 10 min old):
```bash
test $(($(date +%s) - $(stat -c %Y /opt/data/home/.hermes/state/x_link_poller_state.json))) -lt 600 && echo OK
```

---

## Troubleshooting Decision Tree

```
No URLs processed in multiple cycles?
├─► Check log file for platform errors
│   ├─ Discord 403 → fix Read Message History permission
│   ├─ Discord 404 → channel ID wrong or bot not in server
│   ├─ Telegram 409 → webhook active; delete it: deleteWebhook
│   ├─ Telegram 0 updates → channel empty OR bot not admin/member
│   └─ Slack not_in_channel → invite bot to channel
└─► If all platforms OK → content has no X URLs → verify with direct message containing a known X link
```

---

## References

- `references/telegram-webhook-conflict.md` — 409 Conflict diagnosis and deletion procedure
- `references/discord-read-history-permission.md` — detailed Read Message History requirement and examples
- `references/r.jina.ai-fetch-pattern.md` — article extraction via Jina AI reader service
- `references/openrouter-classification-prompt.md` — exact prompt template and topic list
- `references/daemon-instance-management.md` — preventing and cleaning up duplicate daemon processes
- `references/state-vault-reconciliation.md` — rebuilding state from vault contents
- `references/platform-connectivity-checks.md` — pre-flight API verification per platform

---

## Related Skills

- `x-social-monitor` — X/Twitter monitoring (different domain, similar poll/compile pattern)
- `superpowers-executing-plans` — deploy multi-script automation fleets
