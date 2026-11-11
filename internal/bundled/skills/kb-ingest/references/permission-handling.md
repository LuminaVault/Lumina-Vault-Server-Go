# Permission Handling Workflows

When encountering permission errors during ingestion, follow this systematic approach:

## Step 1: Test Vault Writability

Always test both the main vault and home vault before proceeding:

```bash
# Test main vault
touch /opt/data/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
# Test home vault (if main fails)
touch /opt/data/home/obsidian-vault/Vault/raw/test.txt 2>&1 | head -5
```

## Step 2: Use Temporary Directory (If Both Vaults Fail)

When neither vault is writable, create a temporary directory and save files there:

```python
import tempfile, os

# Create temporary directory
temp_dir = tempfile.mkdtemp()
raw_path = f"{temp_dir}/kb_ingest_output"
os.makedirs(raw_path, exist_ok=True)

# Save files to raw_path instead of vault
# Example: 
#   with open(f"{raw_path}/slug.md", "w") as f:
#       f.write(content)

print(f"Permission denied on all vaults. Using temporary directory: {raw_path}")
```

## Step 3: Provide User with Copy Instructions

After ingestion, inform the user to copy the files manually:

```markdown
Files saved to: {RAW_PATH}
Copy these files to your Obsidian vault raw directory:
  cp {RAW_PATH}/* /opt/data/obsidian-vault/Vault/raw/Tech/
```

## Step 4: Fix Permissions (Permanent Solution)

For a permanent fix, change the ownership of the vault directory:

```bash
cd /opt/data && make fix-permissions
# This executes: docker compose exec -u root hermes chown -R hermes:hermes /opt/data
```

## Step 5: Alternative Vault

If you frequently encounter permission issues, consider using the home vault at `/opt/data/home/obsidian-vault/Vault`. Create it and configure kb-ingest via `kb-config.json`.