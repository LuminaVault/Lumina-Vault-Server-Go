# Wrapper Fix Examples — Before → After

**Skill:** `monitoring-alert-orchestrator`  
**Context:** Stock alert system delivers to Slack, Telegram, Discord but orchestrator cannot detect delivery.

---

## Problem Recap

Wrappers run the producer script, send its output to a platform, and exit with the producer's exit code (always 0). Orchestrator relies on wrapper **exit code 1** or **non-empty stdout** to know an alert was sent. Neither condition occurs → orchestrator thinks no alert was generated.

---

## Fix Pattern: Pass-Through Output (Option 2)

After a wrapper successfully delivers an alert, it should **also print the alert message to its own stdout**. This allows the orchestrator to see non-empty stdout and detect the alert while keeping exit code 0 (cron-friendly).

### When to Apply
- Inside the branch where platform delivery succeeds
- After `urlopen`, `curl`, or `requests.post` returns success (200 OK or equivalent)
- Before exiting or returning from the wrapper

---

## File 1: `stock_alert_slack.py` (Python)

### Before
```python
if exit_code == 1 or output:
    token = os.environ.get("SLACK_BOT_TOKEN")
    # ...
    try:
        req = urllib.request.Request(url, ...)
        resp = urllib.request.urlopen(req, timeout=15)
        data = json.loads(resp.read())
        if not data.get("ok"):
            print(f"Slack error: {data.get('error')}", file=sys.stderr)
    except Exception as e:
        print(f"Slack send failed: {e}", file=sys.stderr)

sys.exit(exit_code)
```

### After
```python
if exit_code == 1 or output:
    token = os.environ.get("SLACK_BOT_TOKEN")
    channel_id = os.environ.get("SLACK_ALERT_CHANNEL") or os.environ.get("SLACK_HOME_CHANNEL", "C01ABCDEF")
    if not token:
        print("⚠️  SLACK_BOT_TOKEN not set", file=sys.stderr)
        sys.exit(exit_code)

    url = "https://slack.com/api/chat.postMessage"
    payload = {"channel": channel_id, "text": output, "mrkdwn": True}
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    try:
        req = urllib.request.Request(url, data=json.dumps(payload).encode(), headers=headers, method='POST')
        resp = urllib.request.urlopen(req, timeout=15)
        data = json.loads(resp.read())
        if not data.get("ok"):
            print(f"Slack error: {data.get('error')}", file=sys.stderr)
        else:
            print(output)  # ← Added: forward alert to stdout for orchestrator
    except Exception as e:
        print(f"Slack send failed: {e}", file=sys.stderr)

sys.exit(exit_code)
```

---

## File 2: `stock_alert_telegram.py` (Python)

### Before
```python
if exit_code == 1 or output:
    token = os.environ.get("TELEGRAM_BOT_TOKEN")
    chat_id = os.environ.get("TELEGRAM_HOME_CHANNEL") or os.environ.get("TELEGRAM_ALLOWED_USERS", "").split(",")[0]
    if not token or not chat_id:
        print("⚠️  TELEGRAM_BOT_TOKEN or TELEGRAM_HOME_CHANNEL not set", file=sys.stderr)
        sys.exit(exit_code)
    url = f"https://api.telegram.org/bot{token}/sendMessage"
    payload = {"chat_id": chat_id, "text": output[:4096], "parse_mode": "Markdown", "disable_web_page_preview": True}
    try:
        req = urllib.request.Request(url, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"})
        resp = urllib.request.urlopen(req, timeout=15)
        if resp.status != 200:
            print(f"Telegram API error: {resp.status}", file=sys.stderr)
    except Exception as e:
        print(f"Telegram send failed: {e}", file=sys.stderr)

sys.exit(exit_code)
```

### After
```python
if exit_code == 1 or output:
    token = os.environ.get("TELEGRAM_BOT_TOKEN")
    chat_id = os.environ.get("TELEGRAM_HOME_CHANNEL") or os.environ.get("TELEGRAM_ALLOWED_USERS", "").split(",")[0]
    if not token or not chat_id:
        print("⚠️  TELEGRAM_BOT_TOKEN or TELEGRAM_HOME_CHANNEL not set", file=sys.stderr)
        sys.exit(exit_code)
    url = f"https://api.telegram.org/bot{token}/sendMessage"
    payload = {"chat_id": chat_id, "text": output[:4096], "parse_mode": "Markdown", "disable_web_page_preview": True}
    try:
        req = urllib.request.Request(url, data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"})
        resp = urllib.request.urlopen(req, timeout=15)
        if resp.status != 200:
            print(f"Telegram API error: {resp.status}", file=sys.stderr)
        else:
            print(output)  # ← Added: forward alert to stdout
    except Exception as e:
        print(f"Telegram send failed: {e}", file=sys.stderr)

sys.exit(exit_code)
```

---

## File 3: `stock_alert_discord.sh` (Bash)

### Before
```bash
OUTPUT=$(python3 "$SCRIPT" 2>&1)
EXIT_CODE=$?

if [ $EXIT_CODE -eq 1 ] || [ -n "$OUTPUT" ]; then
    if [ -z "$DISCORD_BOT_TOKEN" ]; then
        echo "ERROR: DISCORD_BOT_TOKEN not set" >&2
        exit 1
    fi
    PAYLOAD=$(printf '%s' "$OUTPUT" | python3 -c "import sys, json; print(json.dumps({'content': sys.stdin.read()}))")
    RESPONSE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "https://discord.com/api/v10/channels/${CHANNEL_ID}/messages" \
        -H "Authorization: Bot ${DISCORD_BOT_TOKEN}" \
        -H "Content-Type: application/json" \
        -d "$PAYLOAD")
    if [ "$RESPONSE" != "200" ]; then
        echo "ERROR: Discord API returned HTTP $RESPONSE (channel $CHANNEL_ID)" >&2
        exit 1
    fi
fi

exit $EXIT_CODE
```

### After
```bash
OUTPUT=$(python3 "$SCRIPT" 2>&1)
EXIT_CODE=$?

if [ $EXIT_CODE -eq 1 ] || [ -n "$OUTPUT" ]; then
    if [ -z "$DISCORD_BOT_TOKEN" ]; then
        echo "ERROR: DISCORD_BOT_TOKEN not set" >&2
        exit 1
    fi
    PAYLOAD=$(printf '%s' "$OUTPUT" | python3 -c "import sys, json; print(json.dumps({'content': sys.stdin.read()}))")
    RESPONSE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "https://discord.com/api/v10/channels/${CHANNEL_ID}/messages" \
        -H "Authorization: Bot ${DISCORD_BOT_TOKEN}" \
        -H "Content-Type: application/json" \
        -d "$PAYLOAD")
    if [ "$RESPONSE" = "200" ]; then
        echo "$OUTPUT"  # ← Added: forward alert to stdout for orchestrator
        exit 0
    else
        echo "ERROR: Discord API returned HTTP $RESPONSE (channel $CHANNEL_ID)" >&2
        exit 1
    fi
fi

exit $EXIT_CODE
```

---

## Testing the Fix

### Before fix:
```bash
$ python3 /opt/data/scripts/stock_alert_triple.py
# stdout: empty
# stderr: DEBUG lines only
# exit code: 0
```

### After fix:
```bash
$ python3 /opt/data/scripts/stock_alert_triple.py
# stdout: (empty — wrappers print to their own stdout, not inherited)
# stderr: DEBUG lines

# Better test: capture each wrapper's stdout individually:
$ python3 -c "
import subprocess, sys
wrappers = [
    ('/opt/data/home/.hermes/scripts/stock_alert_slack.py', 'python3'),
    ('/opt/data/home/.hermes/scripts/stock_alert_telegram.py', 'python3'),
    ('/opt/data/home/.hermes/scripts/stock_alert_discord.sh', 'bash'),
]
for script, interp in wrappers:
    r = subprocess.run([interp, script], capture_output=True, text=True)
    print(f'{script}: rc={r.returncode}, stdout_len={len(r.stdout)}, has_output={bool(r.stdout.strip())}')
"
# Expected: each wrapper's stdout should contain the alert text (length > 0)
# Slack: stdout contains "🚨 **Stock Threshold Alert**..."
# Telegram: same
# Discord: same
```

---

## Alternative: Exit-Code Signaling (Option 1)

If you prefer non-zero exit on success:

**Python wrapper after send:**
```python
sys.exit(1)  # instead of print(output)
```

**Orchestrator already checks `result.returncode == 1` — will detect.**

**Downside:** Cron logs will show job exit 1 (may be flagged as "error" by Hermes cron). Acceptable if you treat exit 1 as "alert delivered" not failure.

---

## Template Snippet for Future Wrappers

```python
#!/usr/bin/env python3
import subprocess, sys, os, json, urllib.request

# 1. Load .env if needed
# 2. Run producer
result = subprocess.run([sys.executable, "producer.py"], capture_output=True, text=True)
output = result.stdout.strip()

# 3. If alert, deliver
if output:
    # ... platform-specific send logic ...
    if send_successful:
        print(output)  # ← forward to orchestrator
        # or: sys.exit(1)

sys.exit(result.returncode)  # or sys.exit(1 if delivered else 0)
```

---

**Apply this pattern to any new platform wrapper (PagerDuty, Opsgenie, Email-only, etc.).**
