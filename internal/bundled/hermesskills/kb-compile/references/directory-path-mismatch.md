# Directory Path Mismatch

## Symptom
The compilation script runs but finds 0 files to compile, even though there are files in the raw directory.

## Cause
The script expects the raw directory to be named `Raw` (capitalized), but the actual vault uses `raw` (lowercase). This mismatch can occur if the script was installed with a different convention or if the vault structure was manually changed.

## Solution
Update the script's configuration to use the correct path. The configuration is in the `compile_wiki.py` script:

1. Open the script:
   ```bash
   nano /root/.hermes/home/.hermes/scripts/compile_wiki.py
   ```

2. Find the configuration section (around lines 14-18):
   ```python
   # Configuration
   VAULT_ROOT = "/opt/data/obsidian-vault/Vault"
   RAW_DIR = os.path.join(VAULT_ROOT, "Raw")
   WIKI_DIR = os.path.join(VAULT_ROOT, "_wiki")
   ```

3. Change `\"Raw\"` to `\"raw\"` and `\"_wiki\"` to `\"wiki\"` (if needed):
   ```python
   RAW_DIR = os.path.join(VAULT_ROOT, "raw")
   WIKI_DIR = os.path.join(VAULT_ROOT, "wiki")
   ```

4. Save and exit, then re-run the compilation.

## Prevention
Ensure the script matches the actual vault structure. The Hermes knowledge base at `/opt/data/obsidian-vault/Vault` uses lowercase `raw/` and `wiki/` directories.