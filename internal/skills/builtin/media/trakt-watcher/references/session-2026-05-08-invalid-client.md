# Trakt watcher session note: refresh failed with `invalid_client`

Date: 2026-05-08 UTC

Observed during cron execution:

- `python3 /opt/data/home/trakt_watcher.py` exited during token refresh.
- Runtime token file existed at `~/.hermes/tokens/trakt.json` and had `600` permissions.
- No `TRAKT_*` environment variables were present.
- Refresh failed with:
  `HTTP 401: {'error': 'invalid_client', 'error_description': 'Client authentication failed due to unknown client, no client authentication included, or unsupported authentication method.'}`

Operational takeaway:

- Treat `invalid_client` as an auth/provisioning failure, not as empty watch history.
- Do not keep retrying the watcher loop when refresh returns `invalid_client`.
- Verify client/app pairing, client secret, and token provenance before rerunning.
