# Multi-Link Poller Compilation Behavior & Operational Details

**Session:** 2026-05-05 Multi-Source Link Poller Run  
**Script:** `/opt/data/scripts/multi_link_poller.py`  
**Findings:** Compilation trigger condition, fallback search patterns, demo limitations

## Compilation Trigger Condition

The poller script **only triggers `kb-compile` when new files are saved** (`if newly_saved > 0`). This is a critical operational detail:

```python
if newly_saved > 0:
    print("⏳ Triggering vault compile …")
    # ... execute compile_wiki.py ...
```

**Implication:** If no URLs are found (0 newly saved), the compilation step is **skipped entirely**. This prevents unnecessary compilation runs but means the knowledge base won't be updated automatically when there's no new content.

## compile_wiki.py Fallback Search Pattern

When triggering compilation, the script searches for `compile_wiki.py` in this specific order:

1. **Primary:** `{VAULT_ROOT}/scripts/compile_wiki.py`  
   (e.g., `/opt/data/obsidian-vault/Vault/scripts/compile_wiki.py`)

2. **Fallback 1:** `~/.hermes/scripts/compile_wiki.py`  
   (e.g., `/opt/data/home/.hermes/scripts/compile_wiki.py`)

3. **Fallback 2:** `/opt/data/home/.hermes/scripts/compile_wiki.py`  
   (explicit check for the common location)

If none of these exist, the script logs a warning and defers compilation to the next scheduled pipeline.

## Demo Environment Limitations

The script `/opt/data/scripts/multi_link_poller.py` is a **demo/template version** with limited functionality:

- **Platform polling not implemented:** When run without `--urls` arguments, it prints:  
  `"[info] No URLs provided — platform polling not yet implemented in this demo."`

- **Only processes explicit URLs:** The script currently supports manual URL input via `--urls` but does not fetch messages from Discord/Telegram/Slack channels automatically.

- **Intended for extension:** This script serves as a scaffold for implementing full platform polling when API tokens and channel integrations are configured.

## Script Execution Context

- **Location:** `/opt/data/scripts/multi_link_poller.py`
- **Execution environment:** Should be run from the scripts directory or with proper PYTHONPATH
- **Dependencies:** Requires `requests` library (already installed)
- **State persistence:** Uses `~/.hermes/state/multi_link_poller_state.json` for tracking processed URLs and platform cursors

## Operational Notes from 2026-05-05 Session

- **Run parameters:** `python3 /opt/data/scripts/multi_link_poller.py --limit 50`
- **Result:** 0 URLs found, 0 files saved
- **Compilation triggered:** No (due to 0 newly saved)
- **Vault status:** No changes to Raw/ directory
- **Environment:** Docker-based Hermes Agent setup with persistent volumes

## Recommendations for Future Runs

1. **To trigger compilation:** Ensure the poller actually finds and saves content. If testing with explicit URLs, provide valid X/Twitter or GitHub links.

2. **To enable full platform polling:** Configure API tokens for Discord/Telegram/Slack and set the appropriate channel IDs in the script or environment variables.

3. **Verify compile_wiki.py location:** Check that the compilation script exists in one of the three fallback locations before expecting automatic compilation.

4. **Monitor state file:** The state file (`multi_link_poller_state.json`) should be preserved across runs for proper deduplication.

## Related Files

- **Script:** `/opt/data/scripts/multi_link_poller.py`
- **State:** `~/.hermes/state/multi_link_poller_state.json`
- **Vault Root:** `~/obsidian-vault/Vault/`
- **Raw directory:** `~/obsidian-vault/Vault/Raw/`

## See Also

- `references/multi-link-poller.md` — Deployment and architecture
- `references/cron-job-setup-and-troubleshooting.md` — Cron job patterns
- `references/polling-pattern.md` — State persistence schema