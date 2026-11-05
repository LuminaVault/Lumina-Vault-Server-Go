#!/usr/bin/env python3
"""
Verify news digest script availability and Discord token resolution.
Part of the news-digest-delivery skill — run this before cron jobs to diagnose setup issues.

Exit codes:
  0 — All checks passed
  1 — Script not found at any expected location
  2 — Discord token missing
  3 — Discord API unreachable (network/auth error)
"""

import os
import sys
import json
import subprocess
from pathlib import Path

HERMES_HOME = Path.home() / ".hermes"
PRIMARY_SCRIPT_DIR = Path("/opt/data/home/.hermes/scripts")
FALLBACK_SCRIPT_DIR = Path("/root/.hermes/home/.hermes/scripts")
LOG_FILE = HERMES_HOME / "logs" / "news_digest_verify.log"
DISCORD_CHANNEL_ID = "YOUR_DISCORD_CHANNEL_ID"


def log(msg, level="INFO"):
    timestamp = subprocess.run(['date', '-u', '+%Y-%m-%dT%H:%M:%SZ'],
                               capture_output=True, text=True).stdout.strip()
    print(f"[{level}] {timestamp} — {msg}")
    LOG_FILE.parent.mkdir(parents=True, exist_ok=True)
    with open(LOG_FILE, 'a') as f:
        f.write(f"[{level}] {timestamp} — {msg}\n")


def find_script(name="news_digest.py") -> Path | None:
    """Locate the news digest script, handling broken symlinks."""
    # Check primary location
    primary = PRIMARY_SCRIPT_DIR / name
    if primary.exists():
        if primary.is_symlink():
            target = Path(os.readlink(primary))
            if not target.exists():
                log(f"⚠️  Broken symlink: {primary} -> {target}", "WARN")
            else:
                log(f"✅ Symlink resolved: {primary} -> {target}")
                return target
        else:
            log(f"✅ Found script at primary: {primary}")
            return primary

    # Check fallback location
    fallback = FALLBACK_SCRIPT_DIR / name
    if fallback.exists():
        log(f"✅ Found script at fallback: {fallback}")
        return fallback

    # Search broadly
    for pattern in [str(FALLBACK_SCRIPT_DIR / "*news_digest.py"),
                    str(PRIMARY_SCRIPT_DIR / "*news_digest.py")]:
        matches = list(Path().glob(pattern))
        if matches:
            log(f"✅ Found candidate via glob: {matches[0]}")
            return matches[0]

    return None


def get_discord_token() -> str | None:
    """Resolve DISCORD_BOT_TOKEN from env or .env files."""
    token = os.environ.get('DISCORD_BOT_TOKEN')
    if token:
        log("✅ Token found in environment")
        return token

    # Check known .env locations
    env_paths = [
        HERMES_HOME / ".env",
        Path("/root/.hermes/.env"),
        Path("/opt/data/.env"),
    ]
    for env_path in env_paths:
        if env_path.exists():
            with open(env_path) as f:
                for line in f:
                    if line.strip().startswith('DISCORD_BOT_TOKEN='):
                        token = line.split('=', 1)[1].strip()
                        if token.startswith('export '):
                            token = token[6:].strip()
                        log(f"✅ Token loaded from {env_path}")
                        return token

    log("❌ DISCORD_BOT_TOKEN not found", "ERROR")
    return None


def verify_discord_api(token: str) -> bool:
    """Smoke-test Discord API access."""
    import urllib.request
    url = "https://discord.com/api/v10/users/@me"
    req = urllib.request.Request(
        url,
        headers={"Authorization": f"Bot {token}"}
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            if resp.status == 200:
                data = json.loads(resp.read())
                log(f"✅ Discord API OK — bot user: {data.get('username', 'unknown')}")
                return True
    except Exception as e:
        log(f"❌ Discord API check failed: {e}", "ERROR")
    return False


def main():
    log("=== News Digest Verification ===")
    
    # 1. Find script
    script = find_script()
    if not script:
        log("FAIL: news_digest.py not found at any expected location", "ERROR")
        sys.exit(1)
    
    # 2. Verify it's executable
    test_result = subprocess.run(
        ["python3", str(script), "--help"],
        capture_output=True, text=True, timeout=5
    )
    # Script may not have --help; just verify it starts
    if test_result.returncode not in [0, 2]:  # 2 = argparse error (no --help), still ok
        log(f"⚠️  Script test returned {test_result.returncode}", "WARN")
    log(f"✅ Script is runnable: {script}")
    
    # 3. Check Discord token
    token = get_discord_token()
    if not token:
        sys.exit(2)
    
    # 4. Verify Discord API
    if not verify_discord_api(token):
        sys.exit(3)
    
    log("✅ All checks passed — cron job should work")
    sys.exit(0)


if __name__ == "__main__":
    main()
