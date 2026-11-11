---
name: AI Cohort Scoreboard Maintenance
description: Configuration, maintenance, and troubleshooting of AI Cohort scoreboard scripts including vault path setup and script updates.
triggers: []
---

# AI Cohort Scoreboard Maintenance

This skill covers the configuration, maintenance, and troubleshooting of the AI Cohort scoreboard scripts. It includes setting up the vault path, updating scripts, and verifying functionality.

## Overview

The AI Cohort scoreboard consists of three main Python scripts that generate daily, weekly, and marketing content from the AI Cohort data:

- `ai_scoreboard.py` - Generates the daily scoreboard
- `generate_marketing_content.py` - Creates Reddit and X/Twitter threads
- `weekly_review.py` - Produces the enhanced weekly review

These scripts write their output to the Obsidian vault in the `Wiki/Finance/AI Cohort/` directory.

## Configuration

### Vault Path Setup

The scripts use a `VAULT_ROOT` constant to define the vault location. This should be set to:

```
'/opt/data/home/.hermes/obsidian-vault/Vault'
```

**Steps to configure:**

1. **Create symlink** (if not already present):
   ```bash
   mkdir -p ~/.hermes/obsidian-vault
   ln -s /opt/data/obsidian-vault/Vault ~/.hermes/obsidian-vault/Vault
   ```

2. **Create required directories**:
   ```bash
   mkdir -p ~/.hermes/obsidian-vault/Vault/Wiki/Finance/AI\ Cohort/Enriched
   ```

### Script Updates

To update any of the AI Cohort scripts to use the new vault path:

1. **Add VAULT_ROOT constant** after imports (typically after the last import statement or after a Config header)

2. **Replace hardcoded paths**:
   - In `ai_scoreboard.py`: Replace `vault_dir = '/opt/data/obsidian-vault/Vault/Wiki/Finance/AI Cohort'` with `vault_dir = VAULT_ROOT`
   - In `generate_marketing_content.py`: Replace `path = f'/opt/data/obsidian-vault/Vault/Wiki/Finance/AI Cohort/{today} — AI Scoreboard.md'` with `path = f'{VAULT_ROOT}/{today} — AI Scoreboard.md'`
   - In `weekly_review.py`: Replace `p = f'/opt/data/obsidian-vault/Vault/Wiki/Finance/AI Cohort/{date_str} — AI Scoreboard.md'` with `p = f'{VAULT_ROOT}/{date_str} — AI Scoreboard.md'`

## Troubleshooting

### File Corruption Issues

If file updates result in corruption (duplicate lines, syntax errors), restore from backup:

```bash
cp /opt/data/home/.hermes/scripts/ai-scoreboard/ai_scoreboard.py.backup /opt/data/home/.hermes/scripts/ai-scoreboard/ai_scoreboard.py
```

Then use a clean Python script to apply updates (see full skill for details).

### Verification

To verify vault access, create a test file:

```python
import os
import sys
sys.path.insert(0, '/opt/data/home/.local/lib/python3.13/site_packages')

test_path = os.path.join(os.path.expanduser('~'), '.hermes', 'obsidian-vault', 'Vault', 'Wiki', 'Finance', 'AI Cohort', 'test.txt')
with open(test_path, 'w') as f:
    f.write("Test successful")
os.remove(test_path)
```

## References

- Script locations: `/opt/data/home/.hermes/scripts/ai-scoreboard/`
- Vault location: `~/.hermes/obsidian-vault/Vault/`
- Original script source: `/opt/data/scripts/ai-scoreboard/`