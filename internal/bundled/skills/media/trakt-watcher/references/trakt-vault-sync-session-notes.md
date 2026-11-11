# Trakt watcher: vault sync session notes

Session outcome:
- The Trakt watcher was used to exchange a PIN for OAuth tokens, then sync watch history into the user's vault.
- The user wanted two modes:
  1. Full backfill of history
  2. Ongoing sync of only new watches from now on

Verified behavior:
- PIN exchange only succeeds when the Trakt client ID/secret match the app that generated the PIN.
- `--dry-run` should preview writes without mutating vault or watcher state.
- `--backfill` should ignore existing seen-state and process the full history feed.
- Normal runs should remain incremental and dedupe by Trakt history ID.

Vault layout used:
- TV episodes: `/opt/data/home/obsidian-vault/Vault/Raw/tvshows/<show-slug>/...`
- Movies: `/opt/data/home/obsidian-vault/Vault/Raw/movies/<movie-slug>/...`

Operational note:
- On first successful sync, a large backfill can be expected if the state file is empty or ignored.
