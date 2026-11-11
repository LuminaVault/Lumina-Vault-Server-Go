# Polling Operational Reality (2026-05-04)

**Skill:** `external-content-ingestion`  
**Related scripts:** `multi_link_poller.py` (demo/template), `x_link_poller_v2.py` (production)  
**Scope:** Platform polling infrastructure state, vault path resolution, daemon management, and compile trigger gaps observed on 2026-05-04.

---

## Scripts: Demo vs Production

- **`multi_link_poller.py`** (located `/opt/data/scripts/` and `~/.hermes/scripts/`)
  - Intended as unified X + GitHub ingestion script.
  - Currently **does not implement platform polling** — it only processes URLs passed via `--urls` argument. Without `--urls` it prints:
    ```
    [info] No URLs provided — platform polling not yet implemented in this demo.
    [info] Use --urls <url1> <url2> ... or integrate with Discord/Telegram/Slack APIs.
    ```
  - Suitable for ad-hoc batch ingest or testing.
  - State file: `~/.hermes/state/multi_link_poller_state.json`
  - **Use case:** manual runs, debugging, one-off URL lists.

- **`x_link_poller_v2.py`** (located `/opt/data/home/.hermes/scripts/`)
  - Production multi-platform poller (Discord, Telegram, Slack) with LLM classification, state persistence, and auto-compile.
  - Runs as a self-daemonizing process (`while True:` + `sleep(300)`) unless killed.
  - State file: `~/.hermes/state/x_link_poller_state.json`
  - **Use case:** scheduled automated polling (via Hermes daemon or cron wrapper).

**Pitfall:** Do not confuse the two. Cron jobs or manual invocations that accidentally call the demo script will produce no work and report "platform polling not yet implemented".

## Vault Path Resolution Duality

Agent environment sets `$HOME=/opt/data/home`. Scripts resolve the vault differently:

| Script | VAULT_ROOT resolution | Target vault |
|--------|----------------------|--------------|
| `multi_link_poller.py` | `Path.home() / "obsidian-vault" / "Vault"` | `/opt/data/home/obsidian-vault/Vault/` |
| `x_link_poller_v2.py` | Hardcoded `VAULT_ROOT = Path("/opt/data/obsidian-vault/Vault")` | `/opt/data/obsidian-vault/Vault/` |
| `compile_wiki.py` (canonical) | Called with `--root /opt/data/obsidian-vault/Vault` | `/opt/data/obsidian-vault/Vault/` |

**Impact:** Content ingested by `multi_link_poller.py` lands in the `$HOME`-relative vault and is invisible to the canonical pipeline until manually synced or re-saved. When files appear "missing", check both locations:

```bash
ls /opt/data/home/obsidian-vault/Vault/Raw/
ls /opt/data/obsidian-vault/Vault/Raw/
```

**Best practice:** Always run scripts with explicit `--root /opt/data/obsidian-vault/Vault` when available; patch scripts that rely on `Path.home()` for consistent behavior.

## Daemon Instance Management

`x_link_poller_v2.py` includes an infinite loop with 300s sleep. If launched repeatedly (e.g., via cron every 15 min), overlapping instances pile up. On 2026-05-03 we observed 4 concurrent sleepers.

**Symptoms:** Multiple PIDs, duplicated state writes, potential race corruption.

**Detection:**
```bash
ps aux | grep x_link_poller_v2.py | grep -v grep
```

**Prevention:**
- Use a process manager (Hermes daemon, systemd) with `Restart=on-failure` and a single service unit.
- If using cron, guard at script start:
  ```python
  import fcntl, sys
  lockfile = open('/tmp/x_link_poller.lock', 'w')
  try:
      fcntl.flock(lockfile, fcntl.LOCK_EX | fcntl.LOCK_NB)
  except BlockingIOError:
      print("Another instance is running — exiting")
      sys.exit(0)
  ```
- Or wrap with `timeout 280` in cron to force termination before next schedule.

**Recovery:** `pkill -f x_link_poller_v2.py` then restart a single instance.

## Compile Script Trigger Gap

After successful saves, `multi_link_poller.py` attempts to run:

```python
compile_script = VAULT_ROOT / "scripts" / "compile_wiki.py"
# Fallback chain:
# 1) VAULT_ROOT/scripts/compile_wiki.py
# 2) HOME/.hermes/scripts/compile_wiki.py
# else: skip
```

On this system the canonical compile script lives at `/opt/data/obsidian-vault/Vault/scripts/compile_wiki.py`. Since VAULT_ROOT for `multi_link_poller.py` resolves to `/opt/data/home/obsidian-vault/Vault`, the first candidate would be `/opt/data/home/obsidian-vault/Vault/scripts/compile_wiki.py` — which does not exist. The second candidate `~/.hermes/scripts/compile_wiki.py` also did not exist. The script therefore prints "compile_wiki.py not found" even though the canonical compile script exists at a third, unlisted path.

**Fix options:**

1. **Patch `multi_link_poller.py`** to include a third candidate:
   ```python
   for cand in [
       VAULT_ROOT / "scripts" / "compile_wiki.py",
       HOME / ".hermes" / "scripts" / "compile_wiki.py",
       Path("/opt/data/obsidian-vault/Vault/scripts/compile_wiki.py"),  # ← add
   ]:
   ```

2. **Manual compile** after poller runs:
   ```bash
   python3 /opt/data/obsidian-vault/Vault/scripts/compile_wiki.py --root /opt/data/obsidian-vault/Vault
   ```

3. **Rely on scheduled pipeline** (the nightly/daily compile job) which already calls the canonical script.

**Recommendation:** Option 1 (patch) for consistency; option 3 for simplicity if the pipeline runs frequently enough.

## State File Divergence

Each poller script maintains its own state:
- `x_link_poller_v2.json` → `~/.hermes/state/x_link_poller_state.json`
- `multi_link_poller.json` → `~/.hermes/state/multi_link_poller_state.json`

They do not share `processed_urls` history. Migrating from one to the other risks duplicate ingestion of the same URLs. If consolidating, copy the `processed_urls` dict from one state file into the other before starting the new poller.

## Platform Error Signatures (Current as of 2026-05-03)

| Platform | Error | Typical cause | Immediate fix |
|----------|-------|---------------|--------------|
| Discord | HTTP 403 Forbidden | Bot lacks **Read Message History** on `DISCORD_MONITOR_CHANNEL` or channel ID mismatch | Grant permission; verify channel ID; re-invite bot with correct scopes |
| Telegram | HTTP 409 Conflict | Webhook is active, blocking `getUpdates` long-poll | `curl -X POST "https://api.telegram.org/bot$TELEGRAM_BOT_TOKEN/deleteWebhook"`; ensure `TELEGRAM_WEBHOOK_URL` unset |
| Telegram | Empty updates forever | Bot not added to target chat | Add bot to chat; ensure `TELEGRAM_HOME_CHANNEL` matches |
| GitHub API | HTTP 403 rate limit | Unauthenticated 60/hr exceeded | Set `GITHUB_TOKEN`; respect `X-RateLimit-Reset` |

These are already covered in skill Pitfalls but repeated here for quick reference.

---

*Last observed: 2026-05-04 (Hermes Agent cron run)*
