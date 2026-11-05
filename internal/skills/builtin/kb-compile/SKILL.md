---
name: kb-compile
description: Compile all uncompiled raw/ content into the wiki. Writes source summaries, creates/updates concept articles with Obsidian backlinks, and updates the index. Run after /kb-ingest to process new content.
trigger: /kb-compile
metadata:
  capability: medium
---

## Environment Reality

**Use `compile_wiki.py` — ignore the manifest.** The vault at `/opt/data/obsidian-vault/Vault` is Syncthing-managed (not git). Both `/opt/data/home/obsidian-vault/Vault` and `/opt/data/obsidian-vault/Vault` resolve to the same location.

```bash
python3 /opt/data/obsidian-vault/Vault/scripts/compile_wiki.py --root /opt/data/obsidian-vault/Vault
```

Key facts:
- **raw/ directory** (lowercase): subdirs include `AI`, `Books`, `Business`, `Careers`, `Dev`, `Health`, `Hermes`, `HermesPortfolio`, `NBA`, `News`, `Sports`, `Stocks`, `Swift`, `TV and Movies`, `Tech`, `WWE`, `XFeed`, `YouTube`, `uncategorized`
- **wiki/ directory** (lowercase): compiled output
- **Clippings cleanup**: If a `Clippings/` directory exists with loose markdown files, move them to the correct `raw/<Topic>/` subdirectory before compiling. Read YAML frontmatter `tags` or `topic` fields, or infer from filename/content (YouTube → appropriate topic directory, X/Twitter threads → `Stocks` or `News`, App Store/indie dev → `Business`, etc.). See `references/clippings-relocation.md` for the pattern.

## Steps

### 1. Pre-compile: Clean Up Clippings Directory

Check if `{KB_PATH}/Clippings/` exists and contains files:
```bash
ls {KB_PATH}/Clippings/*.md 2>/dev/null
```

For each clipping file, read its YAML frontmatter and move it to the appropriate `raw/<Topic>/` subdirectory. Infer topic from `tags`, `topic`, `source`, or filename patterns. After moving, delete the empty `Clippings/` directory.

### 2. Run the Compilation Script

## Steps

### 1. Read Config

Run:
```bash
cat kb-config.json
```

Extract `kb_path`. Expand `~` to the actual home directory path.
Set this as `KB_PATH` for all subsequent steps.

### 2. Check Manifest (informational only — the actual script bypasses this)

Run:
```bash
cat {KB_PATH}/.kb/manifest.json 2>/dev/null || echo "No manifest found"
```

Note: In this environment the manifest tracks only ~4 of 300+ raw files. It will almost always report 0 uncompiled entries even when hundreds exist. The `compile_wiki.py` script performs its own filesystem scan. If the manifest shows uncompiled entries, process them per the steps below — but do NOT treat 0 uncompiled entries as meaning the vault is fully compiled.

### 3. Read Existing Index

Run:
```bash
cat {KB_PATH}/wiki/index.md
```

Keep this in memory — you will append to it throughout this process.

### 4. Process Each Uncompiled File

For each file at `{RAW_KEY}` with `status: uncompiled`, do the following sub-steps in order.

---

#### 4a. Read the raw file

Read `{KB_PATH}/{RAW_KEY}` using the Read tool.
Parse the YAML frontmatter to get `source`, `ingested_at`, and `type`.
The content below the frontmatter block is the main body.

---

#### 4b. Write source summary

Derive `SOURCE_SLUG` from `{RAW_KEY}`: take the filename portion without extension.
Example: `raw/web/abs-1706-03762.md` → `SOURCE_SLUG` = `abs-1706-03762`

Write to `{KB_PATH}/wiki/sources/{SOURCE_SLUG}.md`:

```markdown
---
source: {value of `source` from raw frontmatter}
ingested_at: {value of `ingested_at` from raw frontmatter}
type: {value of `type` from raw frontmatter}
tags: [{3–8 lowercase tags you assign based on content, comma-separated, e.g. ml, transformers, attention}]
---
# {Title: infer from content, URL, or filename}

## Summary
{2–4 sentence summary of the source's main contribution, argument, or subject matter}

## Key Concepts
{Bulleted list of 3–8 key concepts this source covers, each formatted as [[concepts/{concept-slug}]] — {brief description}}

## Notable Details
{Any specific facts, figures, quotes, findings, or techniques worth preserving verbatim}

## Backlinks
- Source file: [[{RAW_KEY without .md extension}]]
```

---

#### 4c. Create or update concept articles

From the Key Concepts list you wrote in 4b, extract each concept slug (the part inside `[[concepts/{concept-slug}]]`).

For each concept slug:

**If `{KB_PATH}/wiki/concepts/{concept-slug}.md` does NOT exist:**

Create it:
```markdown
---
tags: [{relevant tags from the source}]
---
# {Concept Name (title-case of slug, e.g. attention-mechanism → Attention Mechanism)}

{2–4 paragraph article explaining this concept clearly. Write it as a standalone reference: define the concept, explain why it matters, describe how it works, and note any important variants or related ideas. Assume the reader knows the field but is encountering this concept for the first time.}

## Sources
- [[sources/{SOURCE_SLUG}]]
```

**If `{KB_PATH}/wiki/concepts/{concept-slug}.md` DOES exist:**

Read it. Then update it:
1. Add any new information from the current source not already covered in the article body
2. Append `- [[sources/{SOURCE_SLUG}]]` to the `## Sources` section if not already present

---

#### 4d. Update wiki/index.md

For each new concept article created in 4c (skip if the concept entry already exists in the index):

Append under `## Concepts`:
```markdown
- [[concepts/{concept-slug}]] — {one-line description of the concept}
```

For the source summary (skip if already in index):

Append under `## Sources`:
```markdown
- [[sources/{SOURCE_SLUG}]] — {one-line description: what this source is and its main contribution}
```

Only add entries not already present. Check by scanning existing index content.

---

#### 4e. Update manifest entry for this file

Update the entry for `{RAW_KEY}` in the in-memory manifest JSON:

```json
"{RAW_KEY}": {
  \"status\": \"compiled\",
  \"ingested_at\": \"{original ingested_at}\",
  \"compiled_at\": \"{current UTC ISO timestamp}\",
  \"source\": \"{original source}\",
  \"type\": \"{original type}\",
  \"wiki_articles\": [\"sources/{SOURCE_SLUG}.md\", \"concepts/{slug1}.md\", \"concepts/{slug2}.md\"],
  \"tags\": [\"{tags you assigned in 4b}\"]
}
```

---

### 5. Write Updated Files

After processing all uncompiled files:

1. Write the full updated `wiki/index.md` back to disk (with all appended entries)
2. Write the full updated manifest back to `{KB_PATH}/.kb/manifest.json`

### 6. Rebuild Search Index

If `kb_search.py` exists in `{KB_PATH}`, rebuild the search index:

```bash
python3 {KB_PATH}/kb_search.py --rebuild
```

If the file doesn't exist (first run before search tool is installed), skip this step silently.

### 7. Commit Changes

```bash
cd {KB_PATH} && git add -A && git commit -m "kb: compile {N} source(s) into wiki"
```

**PITFALL**: The vault at `/opt/data/obsidian-vault/Vault` is Syncthing-managed, NOT a git repo. If `git commit` fails with "fatal: not a git repository", this is expected behavior. Syncthing handles file synchronization. Skip the commit silently.

Where N is the count of files just compiled.

### 8. Run Reflect

Invoke the `/kb-reflect` skill to discover connections across newly compiled content.

#### Troubleshooting

#### Permission Denied Errors - Enhanced Workaround
[Existing content...]

#### Directory Path Mismatch
**Symptom:** The compilation script runs but finds 0 files to compile, even though there are files in the raw directory.

**Cause:** The script expects the raw directory to be named `Raw` (capitalized), but the actual vault uses `raw` (lowercase). This mismatch can occur if the script was installed with a different convention or if the vault structure was manually changed.

**Solution:** Update the script's configuration to use the correct path. The configuration is in the `compile_wiki.py` script:

1. Open the script:
   ```bash
   nano /root/.hermes/home/.hermes/scripts/compile_wiki.py
   ```

2. Find the configuration section (around lines 14-18):
   ```python
# Configuration
VAULT_ROOT = "/opt/data/obsidian-vault/Vault"
RAW_DIR = os.path.join(VAULT_ROOT, "raw")
WIKI_DIR = os.path.join(VAULT_ROOT, "wiki")
   ```

3. Change `"Raw"` to `"raw"` and `"_wiki"` to `"wiki"` (if needed):
   ```python
   RAW_DIR = os.path.join(VAULT_ROOT, "raw")
   WIKI_DIR = os.path.join(VAULT_ROOT, "wiki")
   ```

4. Save and exit, then re-run the compilation.

**Prevention:** Ensure the script matches the actual vault structure. The Hermes knowledge base at `/opt/data/obsidian-vault/Vault` uses lowercase `raw/` and `wiki/` directories.
The most common issue when running kb-compile is permission problems with the vault directory. The vault at `/opt/data/obsidian-vault/Vault` is often owned by `root:root` with permissions `drwxr-x---`, which prevents the `hermes` user from accessing it.

**Symptoms**:
- `ls: cannot access '/opt/data/obsidian-vault/Vault': Permission denied`
- `python3: can't open file '~/.hermes/obsidian-vault/Vault/Raw/file.md': [Errno 13] Permission denied`

**Solution**: Change ownership of the vault directory to the `hermes` user.

1. **Using the Makefile** (recommended):
   ```bash
   cd /opt/data
   make fix-permissions
   ```
   This executes: `docker compose exec -u root hermes chown -R hermes:hermes /opt/data`

2. **Manual chown** (if Docker is not running):
   ```bash
   sudo chown -R hermes:hermes /opt/data/obsidian-vault/Vault
   ```

After fixing permissions, verify access:
```bash
ls -la /opt/data/obsidian-vault/Vault
ls ~/.hermes/obsidian-vault/Vault/Raw/
```

#### Docker Daemon Issues

If `make fix-permissions` fails with "Cannot connect to the Docker daemon", start Docker:
```bash
sudo systemctl start docker
```

#### Vault Not Found

If the vault directory is missing, you may need to set it up:
```bash
~/.hermes/scripts/setup-obsidian-vault.sh
```

### 9. Print Summary

```markdown
Compiled {N} file(s):
  - {RAW_KEY_1} → wiki/sources/{slug1}.md, wiki/concepts/...
  - {RAW_KEY_2} → wiki/sources/{slug2}.md, wiki/concepts/...
```

### References

- [clippings-relocation.md](references/clippings-relocation.md) — Pattern for classifying and moving files from a Clippings/ directory to raw/ subdirectories before compilation
- For the **actual implementation** in this environment, see: [references/current-implementation.md](references/current-implementation.md)
- This describes the ideal process; the working script is at `/opt/data/obsidian-vault/Vault/scripts/compile_wiki.py`
- [File Name Too Long Errors](references/file-name-too-long.md) — Troubleshooting long filename errors during compilation
- [Permission troubleshooting guide](references/permission-troubleshooting.md) — Common issues and solutions