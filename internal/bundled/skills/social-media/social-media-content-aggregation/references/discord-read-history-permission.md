# Discord Read Message History Permission — Full Diagnosis

## Why 403 Happens Even When Bot is in the Channel

Discord's permission model is **resource-scoped**. A bot may have:
- `View Channel` — see the channel in the UI
- `Send Messages` — post messages
- **`Read Message History`** — fetch past messages via REST API (`GET /channels/{id}/messages`)

The third is **not granted by default** and is separate from "View Channel." Without it, any attempt to read message history returns:

```json
{
  "message": "Missing Access",
  "code": 50001,
  "httpStatus": 403
}
```

With the poller's enhanced error message:
```
HTTP 403 fetching .../messages?limit=50: Forbidden
Discord 403 Forbidden on channel <id>. Bot needs 'Read Message History' permission in this channel.
Current: can SEND but cannot READ.
```

## Step-by-Step Fix (Server UI)

1. Open Discord → Server Settings → Channels
2. Select the monitored channel (e.g., `#hermes`)
3. Click **Edit Channel** → **Permissions** tab
4. In "Roles / Members who can access this channel," find the bot's role
5. Under "Text Channel Permissions," locate **Read Message History**
6. Set to **✔️ Allow** (green check)
7. Click **Save Changes**

### Priority Overrides

If a **category-level** permission denies "Read Message History," it overrides the channel-level allow. To fix:
- Go to the category (folder) the channel belongs to
- Permissions → find the bot's role → either:
  - Set "Read Message History" to **Allow** at category level, OR
  - Set category override to **⛔ Neutral** (gray dash) so channel-level allow takes effect

## API Verification (curl)

After granting permission, confirm with:

```bash
curl -H "Authorization: Bot ${DISCORD_BOT_TOKEN}" \
  "https://discord.com/api/v10/channels/${DISCORD_MONITOR_CHANNEL}/messages?limit=1"
```

**Expected:** `200 OK` with a JSON array containing one message object.  
**403 persists:** bot role not saved, cached, or denied at category level — wait 1–2 minutes and re-check.

## Common Pitfalls

| Symptom | Likely cause |
|---|---|
| 403 after granting permission | Category-level deny override — adjust category permissions |
| 403 persists across restarts | Bot is in multiple servers; you granted permission in the wrong server's channel |
| 404 Not Found | Bot not in the server at all, or channel ID is incorrect |
| 401 Unauthorized | Token is malformed or bot token revoked |

## Related

- Discord Developer docs: [Channel Permissions](https://discord.com/developers/docs/topics/permissions)
