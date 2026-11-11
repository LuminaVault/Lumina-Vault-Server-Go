---
name: news-digest-delivery
category: scheduled-reports
description: Run news digest scripts and deliver combined output to Discord
triggers:
  - news digest execution
  - combined content delivery
  - discord posting via script fallback
---
## Purpose

Automate the execution of news digest scripts (e.g., Go and Swift) and deliver the combined output to Discord. This skill covers the full workflow: script discovery, execution, output aggregation, Discord posting via the `script-based-discord-delivery` method, and logging.

## Prerequisites

- Python 3 installed
- News digest scripts available (e.g., `go_news_digest.py`, `swift_news_digest.py`)
- Discord bot token (DISCORD_BOT_TOKEN) in environment or `.env` file
- Target Discord channel ID known

## Procedure

### 1. Discover Available Scripts

If the expected script path (`/opt/data/home/.hermes/scripts/news_digest.py`) does not exist, search for alternative scripts. Common patterns:

```python
import glob
import os

script_dir = "/opt/data/home/.hermes/scripts"
scripts = glob.glob(os.path.join(script_dir, "*news_digest.py"))
# scripts might contain: go_news_digest.py, swift_news_digest.py
```

#### Pitfall: Broken symlinks in the scripts directory

The `/opt/data/home/.hermes/scripts/` directory often contains symlinks pointing to `/root/.hermes/home/.hermes/scripts/`. The symlink target may be missing or stale, causing `FileNotFoundError` when you try to execute the script. Always resolve the real path before executing.

**Robust discovery with fallback search:**

```python
import os
import subprocess

def find_news_digest_script(preferred_name="news_digest.py"):
    """Find the news digest script, handling broken symlinks and alternate roots."""
    # Primary location (cron standard)
    primary = f"/opt/data/home/.hermes/scripts/{preferred_name}"
    if os.path.exists(primary) and os.path.isfile(primary):
        return primary

    # If it's a symlink, check if target exists
    if os.path.islink(primary):
        target = os.readlink(primary)
        if not os.path.exists(target):
            print(f"  ⚠️  Symlink broken: {primary} -> {target}")
        else:
            return target  # target exists, use it

    # Fallback: search the actual Hermes home directory
    hermes_home = "/root/.hermes/home/.hermes/scripts"
    fallback = os.path.join(hermes_home, preferred_name)
    if os.path.exists(fallback) and os.path.isfile(fallback):
        print(f"  ✅ Found at fallback location: {fallback}")
        return fallback

    # Last resort: find any news digest variant
    import glob
    candidates = glob.glob(os.path.join(hermes_home, "*news_digest.py"))
    if candidates:
        return candidates[0]

    raise FileNotFoundError(f"news_digest.py not found in expected locations")
```

**Typical path mapping for this installation:**

| Expected path | Actual location |
|---|---|
| `/opt/data/home/.hermes/scripts/news_digest.py` | `/root/.hermes/home/.hermes/scripts/news_digest.py` |
| `/opt/data/home/.hermes/scripts/go_news_digest.py` | `/root/.hermes/home/.hermes/scripts/go_news_digest.py` |
| `/opt/data/home/.hermes/scripts/swift_news_digest.py` | `/root/.hermes/home/.hermes/scripts/swift_news_digest.py` |

### 2. Execute Scripts and Capture Output

Run each discovered script and capture stdout:

```python
import subprocess

def run_script(script_path):
    result = subprocess.run(
        ["python3", script_path],
        capture_output=True,
        text=True,
        cwd=script_dir
    )
    return result.stdout
```

### 3. Combine Output

Merge outputs from all scripts into a single message. Add a header to indicate it's a combined digest.

### 4. Post to Discord

Use the `script-based-discord-delivery` approach (curl-based) to post the message. This bypasses TLS fingerprinting issues and works reliably in cron jobs.

**Important:** Check for the Discord bot token before attempting to post. If missing, log an error and skip posting.

### 5. Log the Run

Append a timestamped entry to the news digest log file (`/opt/data/home/.hermes/logs/news_digest.log`).

## Error Handling

- **Script not found:** Search for alternative scripts. If none found, raise an error.
- **Missing Discord token:** Log a clear error message and skip Discord posting. Do not fail the entire job; the digest scripts still ran successfully.
- **Discord API errors:** Implement retry with exponential backoff for transient errors (5xx, 429). For permanent errors (401, 403, 404), log and skip.

## Example Usage

```python
# Discover and run scripts
scripts = find_news_digest_scripts()
outputs = [run_script(s) for s in scripts]

# Combine
combined = combine_outputs(outputs)

# Post to Discord
try:
    post_to_discord(channel_id, combined)
except ValueError as e:
    if "token" in str(e).lower():
        logger.error(f"Discord posting skipped: {e}")
    else:
        raise

# Log
log_run()
```

## Related Skills

- `script-based-discord-delivery` — for the underlying Discord posting mechanism
- `cron-script-deployment` — for cron job setup and troubleshooting

## Quick Verification

Before relying on the cron job, run the bundled verification script to check script path and token resolution:

```bash
python3 /root/.hermes/skills/scheduled-reports/news-digest-delivery/scripts/verify_digest_setup.py
```

Exit code 0 means all systems go.

## References

- `references/script-discovery.md` — Detailed pattern for finding alternative script paths
- `references/combined-output-format.md` — Recommended formatting for combined digests
- `references/hermes-install-path-mapping.md` — Session 2026-05-12: broken symlink diagnosis, real script location, and verified token resolution path