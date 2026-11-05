---
name: trakt-watcher
version: 1.0
description: Monitors Trakt.tv watching status and posts notifications to Hermes platforms
author: Hermes Agent
depends:
  - superpowers
  - terminal
  - file
  - messaging
---

# Configuration
params:
  - name: trakt_client_id
    description: Trakt API client ID
    default: dc22389fbad6bb350eb7b3714d21a71d0a16c35cba461c375728d99f55c3de59
  - name: trakt_access_token
    description: Trakt OAuth access token
    default: ""  # loaded from file or env
  - name: trakt_refresh_token
    description: Trakt refresh token
    default: ""  # loaded from file or env
  - name: trakt_client_secret
    description: Trakt API client secret
    default: ""  # required for token refresh
  - name: trakt_vault_root
    description: Obsidian Raw root used for note output
    default: /opt/data/home/obsidian-vault/Vault/Raw

# State
state:
  last_sync_at: null
  seen_history_ids: []

# Main Execution
exec: python3 /opt/data/home/trakt_watcher.py

# Supported Modes
modes:
  - auth-pin: Exchange a Trakt PIN for access/refresh tokens and persist them to ~/.hermes/tokens/trakt.json.
  - backfill: Ignore any existing seen-state and write the full available Trakt history into the vault Raw tree.
  - sync: Poll Trakt watch history and write one markdown note per new watched item into the vault Raw tree.
  - dry-run: Verify behavior without writing files or mutating sync state.

# Vault Output Contract
vault_output:
  tv_episode_path: /opt/data/home/obsidian-vault/Vault/Raw/tvshows/<show-slug>/<date> - SxxEyy - <episode title> - trakt-<id>.md
  movie_path: /opt/data/home/obsidian-vault/Vault/Raw/movies/<movie-slug>/<date> - <title> - trakt-<id>.md
  note_fields: [source, type, watched_at, history_id, show/title, season/episode/year, trakt_ids]

# Pitfalls
pitfalls:
  - **PIN Is Not The End State**: A Trakt PIN only exists to exchange for access + refresh tokens. If the user only has a PIN, run the auth-pin flow first, and ensure the PIN is exchanged against the exact client ID/secret pair that created it.
  - **Token Storage**: Access and refresh tokens are stored in `~/.hermes/tokens/trakt.json`. Ensure this file has appropriate permissions (chmod 600).
  - **Import Dependencies**: The actual implementation uses only the Python standard library for HTTP, so it can run in cron without extra packages.
  - **Dry-Run Must Be Side-Effect Free**: `--dry-run` should preview writes only. It must not advance `last_sync_at`, mark history IDs as seen, or seed the state file.
  - **Backfill vs Incremental**: Use `--backfill` for the initial vault population or a full re-sync. Normal `sync` runs must be incremental and dedupe by Trakt history ID.
  - **Credential Mismatch**: A 401 during refresh usually means revoked token, wrong client secret, or tokens minted against a different Trakt app.
  - **`invalid_client` Is Not a Retry Problem**: If refresh returns `401` with `invalid_client`, treat it as app/client credential drift or upstream rejection. Do not hammer refresh. Verify the Trakt client secret, token provenance, and app pairing first. See `references/session-2026-05-08-invalid-client.md`.
  - **429 Means Cooldown, Not More Retries**: If refresh returns `HTTP 429` / `REFRESH_TOKEN_API_GET_LIMIT`, persist the wait window, exit 0 with `[SILENT]`, and stop retrying until the cooldown expires. Do not try alternate body encodings or keep probing the same token pair. See `references/session-2026-05-08-refresh-rate-limit.md`.
  - **Anti-bot / Header Weirdness**: A saved token can still fail with `403`/Cloudflare `error code: 1010` or `401` depending on request headers and Trakt's upstream blocking. If this happens, verify the token pair, account/app pairing, and user-agent behavior before assuming the history is empty.

- **Source of Truth**: For this class of task, prefer Trakt watch history as the source of truth rather than trying to capture playback directly from Stremio.

# References
references:
  - actual-implementation.md - Current working behavior, file locations, and runtime contract.
  - credential-availability-session-notes.md - Notes about env/token availability and provisioning failures.
  - references/session-2026-05-07-auth-blocking.md - Cron-session record of 403/401 auth blocking and refresh 429 behavior.
  - references/auth-blocking-and-refresh.md - Current-session notes on `invalid_client` refresh failures despite an existing token file.
  - trakt_watcher.py - Standalone implementation script.
  - debug-output.md - Troubleshooting and request/response logging guidance.
  - rate-limiting-fix.md - Retry-After cooldown behavior and rate-limit handling.
  - references/session-2026-05-08-refresh-rate-limit.md - Manual probe showing Trakt refresh can return `429 REFRESH_TOKEN_API_GET_LIMIT`; respect cooldown and do not spam retries.

