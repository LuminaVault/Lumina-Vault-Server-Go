## Error Transcript — 2026-05-07

**Context:** Daily cron job to generate Reddit post and X thread from AI Cohort scoreboard failed with "❌ Today's scoreboard not found".

**Error Message:**
```bash
Script exited with code 1
stderr:
❌ Today's scoreboard not found
```

**Investigation Steps:**

1. **Check current date:** 2026-05-07
2. **Check if scoreboard file exists:** The scoreboard file `2026-05-07 — AI Scoreboard.md` was generated successfully at 12:35 UTC and exists in the vault root.
3. **Check marketing script's VAULT_ROOT path:**
   - Marketing script: `VAULT_ROOT = '/opt/data/home/.hermes/obsidian-vault/Vault'`
   - Scoreboard script: `VAULT_ROOT = '/opt/data/home/obsidian-vault/Vault'`
   - **Mismatch:** Extra `.hermes` directory in marketing script's path

**Root Cause:** The marketing script had an incorrect VAULT_ROOT path that included the `.hermes` subdirectory, causing it to look in the wrong location for the scoreboard file.

**Resolution:**
- Patched `generate_marketing_content.py` to use the correct VAULT_ROOT: `/opt/data/home/obsidian-vault/Vault`
- Re-ran the script successfully
- Generated marketing assets for 2026-05-07

**Key Learnings:**
1. **Path Consistency is Critical**: All scripts in the AI Cohort system must use the same VAULT_ROOT path. The correct path is `/opt/data/home/obsidian-vault/Vault` (without the `.hermes` prefix).
2. **Verification Step**: When a "scoreboard not found" error occurs, first verify the actual file location and compare with the script's VAULT_ROOT.
3. **Patch Immediately**: When a script has a hardcoded incorrect path, patch it immediately rather than working around it.

**Files Modified:**
- `/root/.hermes/skills/content/ai-cohort-scoreboard-marketing/SKILL.md` - Updated path documentation
- `/root/.hermes/home/.hermes/scripts/ai-scoreboard/generate_marketing_content.py` - Fixed VAULT_ROOT path

**Next Steps:**
- Monitor future runs to ensure path consistency
- Consider adding a configuration file or environment variable for VAULT_ROOT to avoid hardcoded paths