# Trakt credential availability notes

Session findings from a failed refresh investigation:

- The watcher can load from either `~/.hermes/tokens/trakt.json` or `TRAKT_*` environment variables, depending on the runtime path.
- In this session, no `~/.hermes/tokens/trakt.json` file was present in the inspected home locations.
- No `TRAKT_ACCESS_TOKEN`, `TRAKT_REFRESH_TOKEN`, `TRAKT_CLIENT_ID`, or `TRAKT_CLIENT_SECRET` env vars were present in the inspected runtime environment.
- A 401 refresh failure with no tokens available is a provisioning problem, not a code-path bug.
- If the token file is absent, rehydrating env vars or restoring the token file is the first fix. Code changes alone will not make refresh work.

Useful checks:

```bash
python3 - <<'PY'
from pathlib import Path
for p in [Path('~/.hermes/tokens/trakt.json').expanduser()]:
    print(p, p.exists())
PY
env | grep '^TRAKT_'
```
