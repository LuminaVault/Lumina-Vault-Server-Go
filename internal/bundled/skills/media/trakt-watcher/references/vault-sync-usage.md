# Trakt -> vault sync usage

This note captures the workflow used by the current standalone implementation.

## Primary use case
- Sync Trakt watch history into the Obsidian raw inbox
- Write one note per watched episode or movie
- Keep the operation idempotent by tracking seen Trakt history IDs

## Default output layout
- TV episodes:
  /opt/data/home/obsidian-vault/Vault/Raw/tvshows/<show-slug>/<YYYY-MM-DD> - SxxEyy - <episode title> - trakt-<history-id>.md
- Movies:
  /opt/data/home/obsidian-vault/Vault/Raw/movies/<movie-slug>/<YYYY-MM-DD> - <title> - trakt-<history-id>.md

## Auth flow
If the user only has a Trakt PIN:
1. Run the script with --auth-pin PIN
2. Save access_token and refresh_token to ~/.hermes/tokens/trakt.json
3. Run the sync mode afterward

## Useful commands
```bash
python3 /opt/data/home/trakt_watcher.py --auth-pin "$TRAKT_PIN"
python3 /opt/data/home/trakt_watcher.py
python3 /opt/data/home/trakt_watcher.py --dry-run
python3 /opt/data/home/trakt_watcher.py --no-movies
```

## Operational notes
- Prefer Trakt as the source of truth, not Stremio addon state capture.
- Keep note writes append-only and deduped by history ID.
- If the token file is missing, this is usually a provisioning problem, not an API bug.
- The script should be treated as a cron-friendly sync daemon with silent no-op output when nothing changed.
