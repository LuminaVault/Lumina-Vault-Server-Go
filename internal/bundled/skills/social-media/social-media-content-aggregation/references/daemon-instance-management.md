# Daemon Instance Management — Preventing Duplicate Poller Processes

## Problem

Long-running poller daemons (like `x_link_poller_v2.py`) can accumulate duplicate instances if:
- A previous run failed to exit cleanly (zombie/sleeping)
- Cron schedule overlaps with long runtime
- Manual starts without checking existing PIDs
- No lockfile or PID-file guard

**Symptoms:**
- Multiple PIDs visible in `ps aux | grep x_link_poller_v2.py`
- State file corruption or race conditions
- Duplicate article saves (same URL processed by multiple instances)
- Excessive API rate-limit consumption

## Detection

```bash
# Count running instances
pgrep -f x_link_poller_v2.py | wc -l

# List full command lines with start times
ps -eo pid,etime,cmd | grep x_link_poller_v2.py | grep -v grep
```

Expected for healthy deployment: **exactly 1**.

## Prevention Strategies

### Option A — Cron with Lockfile (simplest for cron-deployed daemons)

Wrap the script call in a shell guard:

```bash
# In crontab entry
*/5 * * * * flock -n /tmp/x_link_poller.lock -c "python3 -u /opt/data/home/.hermes/scripts/x_link_poller_v2.py" >> /var/log/x_link_poller.log 2>&1
```

- `flock -n` obtains an exclusive lock; exits immediately if lock held
- Lockfile path can be anywhere writable (`/tmp`, `~/.hermes/state/`)
- Cron will skip the run if previous instance still active

### Option B — File-Based PID Lock Inside Script

Add at the top of `main()`:

```python
import fcntl, sys

LOCKFILE = Path("/opt/data/home/.hermes/state/x_link_poller.lock")

def obtain_lock():
    try:
        lock_fd = open(LOCKFILE, 'w')
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        lock_fd.write(str(os.getpid()))
        lock_fd.flush()
        return lock_fd
    except (IOError, BlockingIOError):
        log.error("Another instance is already running (lock held). Exiting.")
        sys.exit(1)

# In main:
lock_fd = obtain_lock()
```

Remember to `lock_fd.close()` and remove `LOCKFILE` on graceful exit (or use `atexit`). Zombie processes will leave stale lock; add process-liveness check if needed.

### Option C — systemd Unit with `PIDFile` and `RestartSec`

For production deployments, use systemd:

```ini
[Unit]
Description=X Link Poller Daemon
After=network.target

[Service]
Type=simple
ExecStart=/opt/data/home/.hermes/venv/bin/python3 -u /opt/data/home/.hermes/scripts/x_link_poller_v2.py
Restart=on-failure
RestartSec=30
PIDFile=/run/x_link_poller.pid

[Install]
WantedBy=multi-user.target
```

systemd ensures only one active instance; `systemctl status` shows PID; `journalctl -u` for logs.

## Cleanup — Killing Stale Instances

When duplicates are detected:

```bash
# Step 1: List all PIDs
pids=$(pgrep -f x_link_poller_v2.py)

# Step 2: Kill all
kill $pids  # or kill -9 $pids for stubborn processes

# Step 3: Verify none remain
pgrep -f x_link_poller_v2.py || echo "All cleared"
```

**Safety:** Ensure you're targeting the correct process pattern. Use `ps -p <pid> -o cmd=` to verify before killing.

## State File Recovery After Duplicate Runtime

If duplicates ran concurrently, the state file may have been corrupted or have lost cursor positions. Use the reconciliation procedure:

1. Stop all daemon instances
2. Rebuild state from vault contents (see `references/state-vault-reconciliation.md`)
3. Restart single instance with lock guard enabled

## Monitoring & Alerting

Add a simple health check to detect multiple instances:

```bash
count=$(pgrep -f x_link_poller_v2.py | wc -l)
if [ "$count" -gt 1 ]; then
    echo "CRITICAL: $count x_link_poller instances running" >&2
    exit 2
fi
```

Schedule as a separate cron job every 10 minutes.
