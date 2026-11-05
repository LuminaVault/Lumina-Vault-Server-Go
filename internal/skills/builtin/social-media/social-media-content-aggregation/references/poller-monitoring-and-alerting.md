# Poller Monitoring & Alerting — Health Checks and Observability

## Overview

The X link poller is designed to run continuously and handle transient errors gracefully. However, silent failures (like permission issues or webhook conflicts) can cause the poller to stop harvesting content without crashing. This guide covers how to monitor the poller's health, analyze logs for warning signs, and set up alerts to ensure continuous operation.

## Expected Behavior

### Exit Codes
- **Exit code 0**: Normal completion (even if platform errors occurred). The script catches and logs errors but exits successfully unless there's an unhandled Python exception.
- **Exit code 1**: Unhandled exception (script crash). This is the only failure mode that causes a non-zero exit.

**Implication:** Do not rely on exit codes for health monitoring. A script that exits with code 0 may have failed to fetch any content due to configuration issues.

### Log Patterns
- **INFO logs**: Normal operation (polling started, no messages, poll completed, etc.)
- **ERROR logs**: Platform-specific failures (403, 404, 409, etc.)
- **WARNING logs**: Skipped URLs, fetch failures, LLM retries, etc.
- **INFO "New X link"**: Indicates successful discovery of a new article

## Health Checks

### 1. Process Running Check
Verify the poller process is alive:
```bash
pgrep -f "x_link_poller_v2.py" >/dev/null && echo "Poller running" || echo "Poller stopped"
```

For wrapper scripts:
```bash
pgrep -f "x_link_poller_loop.sh" >/dev/null && echo "Wrapper running" || echo "Wrapper stopped"
```

### 2. State File Freshness
The state file is updated on every cycle. If it hasn't been modified in > 10 minutes, the poller may be stuck or dead.
```bash
STATE_FILE="/opt/data/home/.hermes/state/x_link_poller_state.json"
if [ -f "$STATE_FILE" ]; then
    MODIFIED=$(stat -c %Y "$STATE_FILE")
    NOW=$(date +%s)
    AGE=$((NOW - MODIFIED))
    if [ $AGE -gt 600 ]; then
        echo "State file stale ($AGE seconds old)"
    else
        echo "State file fresh ($AGE seconds old)"
    fi
else
    echo "State file missing"
fi
```

### 3. Log Analysis
Tail the log and look for:
- **Error patterns**: Repeated 403/404/409 errors on a platform
- **No new articles**: If the poller runs for hours with no "New X link" messages, content may have dried up or there are configuration issues
- **High warning rate**: Frequent fetch failures or LLM retries may indicate network issues or rate limiting

### 4. Vault Output Check
Verify that articles are being saved:
```bash
find /opt/data/obsidian-vault/Vault/Raw -name "*.md" -mtime -1 | wc -l
```
This shows how many articles were saved in the last 24 hours. Compare with expected volume based on social media activity.

## Alert Triggers

Set up alerts for any of the following conditions:

### Critical Alerts
1. **Poller process not running** for > 2 minutes
2. **State file not modified** for > 15 minutes
3. **Repeated platform errors** (e.g., 5+ Discord 403 errors in 10 minutes)
4. **No new articles** for an extended period (configurable based on expected volume)

### Warning Alerts
1. **High warning rate** (e.g., > 10 warnings per hour)
2. **Compilation failures** (wiki compile errors in logs)
3. **State file missing** (indicates initialization failure)

## Sample Alert Scripts

### Alert if poller not running
```bash
#!/bin/bash
# check_poller_running.sh

POLER_PROCESS="x_link_poller_v2.py"
WRAPPER_PROCESS="x_link_poller_loop.sh"
LOG_FILE="/opt/data/home/.hermes/logs/poller_monitor.log"

if ! pgrep -f "$POLER_PROCESS" >/dev/null && ! pgrep -f "$WRAPPER_PROCESS" >/dev/null; then
    echo "$(date): Poller process not running!" >> "$LOG_FILE"
    # Send alert via your preferred channel (email, Slack, Telegram, etc.)
    # Example: send_telegram_alert "X Link Poller is down!"
fi
```

### Alert if state file is stale
```bash
#!/bin/bash
# check_state_stale.sh

STATE_FILE="/opt/data/home/.hermes/state/x_link_poller_state.json"
MAX_AGE=600  # 10 minutes

if [ -f "$STATE_FILE" ]; then
    MODIFIED=$(stat -c %Y "$STATE_FILE")
    NOW=$(date +%s)
    AGE=$((NOW - MODIFIED))
    if [ $AGE -gt $MAX_AGE ]; then
        echo "$(date): State file is stale ($AGE seconds old)" >> /opt/data/home/.hermes/logs/poller_monitor.log
        # Send alert
    fi
else
    echo "$(date): State file missing" >> /opt/data/home/.hermes/logs/poller_monitor.log
    # Send alert
fi
```

### Alert on repeated errors in logs
```bash
#!/bin/bash
# check_log_errors.sh

LOG_FILE="/opt/data/home/.hermes/logs/x_link_poller_v2.log"
ERROR_PATTERN="ERROR.*Discord 403|ERROR.*Telegram 409|ERROR.*Slack"
THRESHOLD=5  # number of errors in last hour

# Count errors in last hour
ERROR_COUNT=$(grep -c "$ERROR_PATTERN" <(find "$LOG_FILE" -mmin -60 -exec cat {} \;) || echo 0)

if [ "$ERROR_COUNT" -ge "$THRESHOLD" ]; then
    echo "$(date): High error rate detected ($ERROR_COUNT errors in last hour)" >> /opt/data/home/.hermes/logs/poller_monitor.log
    # Send alert with details
fi
```

## Integration with Monitoring Systems

### Prometheus (if available)
Expose metrics via a simple HTTP server:
- `poller_up{instance="host"} 1`
- `poller_state_age_seconds{instance="host"}`
- `poller_articles_last_24h{instance="host"}`
- `poller_errors_total{platform="discord",code="403"}`

### Hermes Cron Monitoring
If running under Hermes cron, use the built-in cron monitoring:
```bash
# Check last cron execution
hermes cron.list | grep x_link_poller
# Check output
cat ~/.hermes/output/x_link_poller_$(date +%Y-%m-%d).md
```

## Recommended Monitoring Setup

For production deployments, implement a layered monitoring approach:

1. **Process monitoring**: Basic check that the poller process is running (via cron or systemd)
2. **Log aggregation**: Centralize logs and set up alerts for error patterns
3. **Output validation**: Periodically check that new articles are being saved
4. **Synthetic testing**: Post a test X link in the monitored channels and verify it appears in the vault within one poll cycle

## Maintenance

- **Log rotation**: Ensure logs are rotated to avoid filling disk space
- **State file backup**: Consider backing up the state file periodically in case of corruption
- **Credential rotation**: Update tokens in `.env` and restart the poller

## Related Skills

- `social-media-content-aggregation` — main poller implementation
- `x-social-monitor-auth-troubleshooting` — X/Twitter auth issues

## See Also

- `references/state-vault-reconciliation.md` — rebuilding state from vault contents
- `references/daemon-instance-management.md` — preventing duplicate daemon processes
- `references/platform-connectivity-checks.md` — pre-flight API verification