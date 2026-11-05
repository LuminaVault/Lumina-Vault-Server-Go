# Detection Bug: Orchestrator Cannot See Wrapper Delivery

**Session:** 2026-05-03 — stock-alert-triple cron job investigation  
**Skill:** `monitoring-alert-orchestrator`  
**Severity:** Critical (logic bug masks delivered alerts)

## Problem Statement

The `stock_alert_triple.py` orchestrator calls three platform wrappers (Slack, Telegram, Discord). Each wrapper runs `stock_threshold_alert.py`, captures its output, sends it to the platform, and exits with the producer script's exit code. The producer **always exits 0** (even on alert), so wrappers always exit 0. Orchestrator checks:

```python
if result.returncode == 1 or result.stdout.strip():
    any_alert = True
```

Both conditions are false → `any_alert = False` → orchestrator exits 0 with empty stdout.

**Result:** Cron logs show "no output" and exit 0 even though alerts may have been sent to Slack and Telegram.

## Reproduction Scenario

1. `stock_threshold_alert.py` detects 6 tickers below threshold → prints alert message → exits 0
2. `stock_alert_slack.py` runs producer, gets output, POSTs to Slack API (200 OK), exits 0
3. `stock_alert_telegram.py` does same → exits 0
4. `stock_alert_discord.sh` fails with 403 (auth issue) → exits 1, but no output
5. Triple sees: Slack rc=0 stdout="", Telegram rc=0 stdout="", Discord rc=1 stdout="" → any_alert=False → exit 0

Only Discord's non-zero exit is counted but it had no output; Slack & Telegram delivered but are invisible.

## Affected Code

**Orchestrator:** `/opt/data/scripts/stock_alert_triple.py`

```python
for script, interpreter in scripts:
    result = subprocess.run([interpreter, script], capture_output=True, text=True)
    if result.returncode == 1 or result.stdout.strip():
        any_alert = True
```

**Wrappers (current buggy versions):**
- `/opt/data/home/.hermes/scripts/stock_alert_slack.py`
- `/opt/data/home/.hermes/scripts/stock_alert_telegram.py`
- `/opt/data/home/.hermes/scripts/stock_alert_discord.sh`

All consume producer output and exit with producer's code.

## Platform Status at Time of Discovery

| Platform | Auth | Delivery | Detection |
|----------|------|----------|-----------|
| Slack | ✅ Working | Would succeed | ❌ Undetected by orchestrator |
| Telegram | ✅ Working | Would succeed | ❌ Undetected by orchestrator |
| Discord | ❌ 403 Forbidden | Fails | ✅ Detected (non-zero exit) but not logged meaningfully |

## The Fix Pattern

### Option 1 — Wrapper exit code signaling (minimal change to orchestator)
Modify each wrapper to exit 1 after successful delivery:

**Python wrappers:**
```python
if output:
    send_to_platform(output)
    sys.exit(1)   # ← changed from sys.exit(exit_code)
sys.exit(0)
```

**Bash wrapper:**
```bash
if [ -n "$OUTPUT" ]; then
    curl ... && exit 1   # ← exit 1 on success
fi
exit 0
```

### Option 2 — Pass-through stdout (preserves exit 0)
Modify wrappers to **print the alert** after sending:

```python
if output:
    send_to_platform(output)
    print(output)   # ← forward to orchestrator stdout
sys.exit(exit_code)  # still 0
```

Orchestrator already checks `result.stdout.strip()` — this will now be truthy.

### Option 3 — Return JSON status (most explicit)
Have wrappers print a status line and exit 0:

```python
print(json.dumps({"delivered": true, "platform": "slack"}))
```

Orchestrator parses stdout instead of relying on exit codes. More robust but requires wrapper redesign.

## Recommended Solution for stock_alert_triple

Apply **Option 2** to all three wrappers:
- Keeps cron job exit 0 (no "error" marking in cron logs)
- Makes orchestrator aware of delivery via stdout
- Minimal code change (~1 line per wrapper)

Patch Slack wrapper after successful `urlopen`:
```python
print(output)  # Forward for orchestrator detection
```

Patch Telegram wrapper similarly inside the try block after status 200.

Patch Discord wrapper: `echo "$OUTPUT"` before `exit $EXIT_CODE` (already echoes stderr on error).

## Follow-up Tasks

1. [ ] Patch all three wrappers with pass-through output
2. [ ] Test: `hermes cron run 18186766daf4` — triple should now exit 1 (because stdout non-empty) or at least output should appear
3. [ ] Fix Discord token (403) — re-generate bot token or fix channel permissions
4. [ ] Clean up unused `stock-alert-orchestrator/` directory (not in Hermes cron)
5. [ ] Consider unifying all stock alert scripts into a single coherent package
