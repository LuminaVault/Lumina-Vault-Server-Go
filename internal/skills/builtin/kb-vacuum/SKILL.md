---
name: kb-vacuum
description: "Scan vault raw/ folder for hyperlinks embedded in markdown notes; POST each unseen URL to LuminaVault server /v1/capture/safari so Hermes ingests + memorizes. Usage: /kb-vacuum [folder=raw/]"
trigger: /kb-vacuum
metadata:
  capability: medium
---

# KB Vacuum

Walk the local vault's `raw/` folder, extract every HTTP(S) hyperlink found in markdown notes, and POST each unseen one to LuminaVault server `/v1/capture/safari` so the server-side enrichment pipeline (YouTube / X / GenericOG / Jina tier-2) runs and the result lands in `vault_files` for Hermes memory compilation.

**Companion to `/kb-ingest`:** Whereas `/kb-ingest` takes ONE URL/PDF/text and stages it, `/kb-vacuum` is the batch tool — vacuum up every URL the user has accumulated in their notes since the last run.

## Steps

### 1. Read config

```bash
cat kb-config.json
```

Required keys:
- `vault_path` — absolute path to the local vault root (e.g. `/opt/data/obsidian-vault/Vault`)
- `server_base_url` — LuminaVault server origin (e.g. `https://your-tenant.luminavault.app`)
- `auth_token` — JWT for the authenticated user (Bearer token used against `/v1/capture/safari`)

If any are missing, stop and tell the user which key needs to be set, then exit.

Set `VAULT_PATH`, `SERVER_BASE_URL`, `AUTH_TOKEN` from the config for subsequent steps.

### 2. Resolve target folder

The argument after `/kb-vacuum` is the folder to scan, relative to the vault root.

- No argument → default to `raw/`
- Argument like `raw/AI` → scan only that subdirectory
- Argument like `notes/` → scan that

Compute `SCAN_ROOT="$VAULT_PATH/<arg-or-raw>"`. Verify it exists:

```bash
test -d "$SCAN_ROOT" || { echo "scan root not found: $SCAN_ROOT"; exit 1; }
```

### 3. Load the seen manifest

The manifest lives at `$VAULT_PATH/.kb/vacuum_seen.json` and stores the set of URLs already POSTed in prior runs. Format:

```json
{
  "seen": [
    "https://example.com/article-a",
    "https://www.youtube.com/watch?v=..."
  ]
}
```

Create the file if it doesn't exist (with `{"seen": []}`). Load into memory as a `Set<String>` for O(1) dedup.

```bash
mkdir -p "$VAULT_PATH/.kb"
test -f "$VAULT_PATH/.kb/vacuum_seen.json" || echo '{"seen": []}' > "$VAULT_PATH/.kb/vacuum_seen.json"
```

### 4. Extract URLs from every `*.md` under SCAN_ROOT

Walk recursively. For each markdown file, extract all HTTP(S) URLs. Handle both bare URLs and `[label](url)` Markdown link wrappers — extract just the `url` part. Strip trailing punctuation (`.,;:!?'"`). Dedupe within the run.

Recommended approach: a small Python or jq+sed pipeline. Example Python:

```python
import json, os, re, pathlib, sys, urllib.parse

vault = pathlib.Path(os.environ["VAULT_PATH"])
scan_root = pathlib.Path(os.environ["SCAN_ROOT"])
seen_path = vault / ".kb" / "vacuum_seen.json"
seen = set(json.loads(seen_path.read_text())["seen"])

# Match http(s) URLs both bare and inside Markdown link wrappers.
url_re = re.compile(r"https?://[^\s)>\]\"']+", re.IGNORECASE)
trailing_punct = ".,;:!?\"'"

discovered = []
discovered_set = set()
for md in scan_root.rglob("*.md"):
    try:
        text = md.read_text(encoding="utf-8", errors="ignore")
    except Exception:
        continue
    for raw in url_re.findall(text):
        url = raw
        while url and url[-1] in trailing_punct:
            url = url[:-1]
        if not url or url in discovered_set:
            continue
        discovered_set.add(url)
        discovered.append(url)

new_urls = [u for u in discovered if u not in seen]
print(f"DISCOVERED={len(discovered)}")
print(f"NEW={len(new_urls)}")

# Write the URL list to a temp file the bash steps can iterate.
out = vault / ".kb" / "vacuum_pending.txt"
out.write_text("\n".join(new_urls))
```

### 5. POST each unseen URL to `/v1/capture/safari`

For each URL in `.kb/vacuum_pending.txt`, POST:

```bash
while IFS= read -r url; do
  [ -z "$url" ] && continue
  CAPTURED_AT=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    "$SERVER_BASE_URL/v1/capture/safari" \
    -H "Authorization: Bearer $AUTH_TOKEN" \
    -H "Content-Type: application/json" \
    -d "$(jq -nc --arg u "$url" --arg t "$CAPTURED_AT" '{url:$u, source:"kb-vacuum", capturedAt:$t}')")
  case "$STATUS" in
    2*)
      echo "POSTED $url"
      echo "$url" >> "$VAULT_PATH/.kb/vacuum_posted.txt"
      ;;
    4*)
      echo "CLIENT_ERROR ($STATUS) $url — skipping"
      ;;
    5*|000)
      echo "TRANSIENT ($STATUS) $url — retry once"
      sleep 1
      RETRY_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
        "$SERVER_BASE_URL/v1/capture/safari" \
        -H "Authorization: Bearer $AUTH_TOKEN" \
        -H "Content-Type: application/json" \
        -d "$(jq -nc --arg u "$url" --arg t "$CAPTURED_AT" '{url:$u, source:"kb-vacuum", capturedAt:$t}')")
      if [[ "$RETRY_STATUS" =~ ^2 ]]; then
        echo "POSTED (retry) $url"
        echo "$url" >> "$VAULT_PATH/.kb/vacuum_posted.txt"
      else
        echo "FAILED ($RETRY_STATUS) $url — will retry next run"
      fi
      ;;
  esac
done < "$VAULT_PATH/.kb/vacuum_pending.txt"
```

### 6. Update the seen manifest

Merge every successfully-POSTed URL from this run into the manifest:

```python
import json, pathlib, os
vault = pathlib.Path(os.environ["VAULT_PATH"])
seen_path = vault / ".kb" / "vacuum_seen.json"
posted_path = vault / ".kb" / "vacuum_posted.txt"

seen = set(json.loads(seen_path.read_text())["seen"])
if posted_path.exists():
    for line in posted_path.read_text().splitlines():
        url = line.strip()
        if url:
            seen.add(url)

seen_path.write_text(json.dumps({"seen": sorted(seen)}, indent=2))
posted_path.unlink(missing_ok=True)
(vault / ".kb" / "vacuum_pending.txt").unlink(missing_ok=True)
```

### 7. Print the run summary

```
<DISCOVERED> urls discovered, <NEW> new, <POSTED> posted, <FAILED> failed
```

Example:
```
27 urls discovered, 14 new, 13 posted, 1 failed
```

If `failed > 0`, list the failed URLs so the user knows what to investigate (auth, server reachability, etc).

## Notes

- **Dedup contract**: client-side manifest (`vacuum_seen.json`) is primary. Server also dedupes via `VaultFile (tenant_id, source_url)` unique constraint, so re-POSTing the same URL accidentally is idempotent.
- **To re-vacuum everything**: delete `.kb/vacuum_seen.json` and re-run. Server-side dedup keeps you safe from duplicate `VaultFile` rows.
- **Token rotation**: when the JWT expires, update `auth_token` in `kb-config.json` and re-run.
- **Why client-side, not server-cron**: the vault lives on the user's machine. The server has no filesystem read access. Mirrors the kb-ingest / kb-compile / kb-import pattern.
- **iOS in-app surface**: the iOS app has Safari share-extension capture but no "Vacuum vault" button yet. That's a separate ticket targeting a future `/v1/capture/bulk` endpoint.
