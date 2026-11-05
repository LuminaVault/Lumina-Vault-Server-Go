# Hermes News Digest — Install Path Mapping

**Session:** 2026-05-12 — identified broken symlink pattern in `/opt/data/home/.hermes/scripts/`

## Observed Layout

The standard scripts directory at `/opt/data/home/.hermes/scripts/` contains **symlinks** that point to the real location:

```
/opt/data/home/.hermes/scripts/
├── news_digest.py -> /root/.hermes/home/.hermes/scripts/news_digest.py  (BROKEN)
├── go_news_digest.py -> /root/.hermes/home/.hermes/scripts/go_news_digest.py
├── swift_news_digest.py -> /root/.hermes/home/.hermes/scripts/swift_news_digest.py
└── ...
```

**The problem:** The `news_digest.py` symlink target does not exist at the expected location, causing `FileNotFoundError` when cron runs `python3 /opt/data/home/.hermes/scripts/news_digest.py`.

## Real Script Location

All live scripts are stored under:

```
/root/.hermes/home/.hermes/scripts/
```

This is the canonical source of truth. The `/opt/data/` path appears to be a stale mount or container artifact.

## Recommended Fix

**Option A — Repair the symlink (preferred):**
```bash
ln -sf /root/.hermes/home/.hermes/scripts/news_digest.py /opt/data/home/.hermes/scripts/news_digest.py
```

**Option B — Update cron to call the real path directly:**
Edit the cron entry to use `/root/.hermes/home/.hermes/scripts/news_digest.py` instead.

**Option C — Use the discovery wrapper (from the skill's Procedure):**
The `find_news_digest_script()` function in the `news-digest-delivery` skill handles this automatically by checking both locations and resolving/validating symlinks before execution.

## Discord Token Location

DISCORD_BOT_TOKEN is stored in `/root/.hermes/.env` (not in the environment when running under cron). The `script-based-discord-delivery` skill's .env fallback successfully resolved it.

```bash
# Token in .env
DISCORD_BOT_TOKEN=MTQ5OD...
```

## Verification

```bash
# Test script execution from real path
python3 /root/.hermes/home/.hermes/scripts/news_digest.py

# Test Discord posting (dry-run check)
curl -H "Authorization: Bot $(grep DISCORD_BOT_TOKEN /root/.hermes/.env | cut -d= -f2)" \
  https://discord.com/api/v10/users/@me
```
