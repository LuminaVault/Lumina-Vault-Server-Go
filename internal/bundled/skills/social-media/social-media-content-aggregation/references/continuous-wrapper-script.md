# Continuous Wrapper Script — Running the Poller as a Daemon

## Overview

When the poller script (`x_link_poller_v2.py`) does **not** contain an infinite loop itself, use a **wrapper shell script** to run it continuously with proper daemonization, logging, and singleton protection.

This pattern is useful when:
- The Python script is designed as a single poll cycle
- You want to avoid modifying the script itself
- You need a simple, robust daemon without systemd or cron dependencies
- You want explicit control over poll intervals and error handling

## Implementation

### Wrapper Script Structure

```bash
#!/bin/bash
# Continuous X Link Poller v2 - Runs forever with error handling and delays

LOCK_FILE="/tmp/x_link_poller_v2_continuous.lock"
LOG_FILE="/opt/data/home/.hermes/logs/x_link_poller_v2.log"
SCRIPT="/opt/data/home/.hermes/scripts/x_link_poller_v2.py"
POLL_INTERVAL=60  # seconds between poll cycles

# Ensure directories exist
mkdir -p /opt/data/home/.hermes/logs
mkdir -p /opt/data/home/.hermes/state

# Check if already running
if [ -f "$LOCK_FILE" ]; then
    OLD_PID=$(cat "$LOCK_FILE")
    if kill -0 "$OLD_PID" 2>/dev/null; then
        echo "$(date): Already running (PID $OLD_PID). Exiting." >> "$LOG_FILE"
        exit 1
    else
        echo "$(date): Stale lock found. Cleaning up." >> "$LOG_FILE"
        rm -f "$LOCK_FILE"
    fi
fi

# Acquire lock
echo $$ > "$LOCK_FILE"
echo "$(date): Starting continuous poller (PID $$)" >> "$LOG_FILE"

# Load Hermes environment
if [ -f /opt/data/.env ]; then
    source /opt/data/.env
fi

# Main loop
while true; do
    echo "$(date): Starting poll cycle..." >> "$LOG_FILE"
    python3 -u "$SCRIPT" >> "$LOG_FILE" 2>&1
    EXIT_CODE=$?
    
    if [ $EXIT_CODE -ne 0 ]; then
        echo "$(date): Poll cycle failed with exit code $EXIT_CODE" >> "$LOG_FILE"
    else
        echo "$(date): Poll cycle completed successfully" >> "$LOG_FILE"
    fi
    
    # Wait before next cycle
    sleep $POLL_INTERVAL
done

# Cleanup on exit
rm -f "$LOCK_FILE"
echo "$(date): Poller stopped" >> "$LOG_FILE"
```

### Key Features

1. **Singleton protection via lockfile** — Prevents multiple wrapper instances from running simultaneously
2. **Centralized logging** — All output (stdout/stderr) goes to a dedicated log file
3. **Graceful error handling** — Script failures are logged but don't crash the wrapper
4. **Configurable interval** — Poll frequency easily adjustable via `POLL_INTERVAL`
5. **Environment loading** — Sources Hermes-wide `.env` file for credentials
6. **Automatic directory creation** — Ensures log and state directories exist

### Usage

1. Save the script (e.g., `/opt/data/home/.hermes/scripts/run_x_link_poller_continuous.sh`)
2. Make executable: `chmod +x /opt/data/home/.hermes/scripts/run_x_link_poller_continuous.sh`
3. Start in background: `/opt/data/home/.hermes/scripts/run_x_link_poller_continuous.sh &`
4. Verify it's running: `ps aux | grep x_link_poller_continuous`
5. Check logs: `tail -f /opt/data/home/.hermes/logs/x_link_poller_v2.log`

### Advantages Over Cron

- **State persistence** — The wrapper maintains a single process that can hold state if needed
- **Immediate restart** — On failure, the wrapper restarts the script after a configurable delay
- **Centralized logging** — All cycles logged to one file with timestamps
- **No cron scheduling** — Runs continuously, not at fixed times (better for real-time monitoring)

### Advantages Over Systemd

- **Simpler deployment** — No need for root privileges or systemd unit files
- **Portable** — Works across different Linux distributions and macOS
- **Easier debugging** — Log file is immediately accessible
- **Flexible** — Can be started/stopped by any user with appropriate permissions

### Monitoring

Add a simple health check to verify the wrapper is running:

```bash
# Check if lockfile exists and contains a running PID
if [ -f "/tmp/x_link_poller_v2_continuous.lock" ]; then
    PID=$(cat /tmp/x_link_poller_v2_continuous.lock)
    if kill -0 "$PID" 2>/dev/null; then
        echo "Poller is running (PID $PID)"
    else
        echo "Poller dead (stale PID $PID)"
    fi
else
    echo "Poller not running (no lockfile)"
fi
```

Schedule this check via cron to alert on failures.

## When to Use This Pattern

- The poller script is a single-cycle tool (no built-in loop)
- You need a lightweight daemon without systemd dependencies
- You want fine-grained control over poll intervals and error recovery
- You're running in an environment where systemd is unavailable or restricted

## When to Avoid

- The poller script already contains an infinite loop (use it directly)
- You need advanced process supervision (use systemd instead)
- You require precise timing (cron may be more appropriate)
- You need to run multiple instances for load balancing (use a process manager)

## Integration with Existing Patterns

This wrapper complements the existing `daemon-instance-management.md` reference. While that document covers protecting a single instance of the poller script itself, this wrapper adds an additional layer of protection at the process level and provides continuous operation.

**Recommendation:** Use this wrapper pattern **instead of** cron when running the poller as a long-lived service. For production deployments, consider systemd for better integration with system monitoring tools.