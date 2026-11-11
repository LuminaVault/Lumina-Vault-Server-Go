# Trakt auth blocking and refresh failure notes

Session learnings from a failed cron run:

- `~/.hermes/tokens/trakt.json` existed in the runtime home and contained access/refresh tokens.
- The watcher still failed during refresh with:
  `HTTP 401: {'error': 'invalid_client', 'error_description': 'Client authentication failed due to unknown client, no client authentication included, or unsupported authentication method.'}`
- This is not an empty-history signal. Treat it as an auth/provisioning failure.
- Likely causes: wrong Trakt client secret, revoked/mismatched app pairing, or Trakt rejecting the client identity upstream.
- If refresh fails with `invalid_client`, do not keep retrying the watcher loop. Verify the client/app pairing and token provenance first.
- If the token file is present but unusable, refreshing the token file alone will not fix the issue unless the app credentials are corrected.

Useful checks:

```bash
python3 - <<'PY'
from pathlib import Path
p = Path('~/.hermes/tokens/trakt.json').expanduser()
print(p, p.exists())
if p.exists():
    print(p.stat().st_mode & 0o777)
PY
```
