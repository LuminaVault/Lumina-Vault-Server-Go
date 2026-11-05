# Fixing Vault Permissions

When the `kb-ingest` skill encounters permission errors while trying to write to the Obsidian vault, follow these steps to resolve the issue.

## Why Permissions Matter

The Hermes agent runs as the `hermes` user. For it to write directly to the vault, the vault directory must be owned by the `hermes` user or the `hermes` group.

## Quick Fix Commands

### Option 1: Change Ownership (Recommended)
Run this command to make the hermes user the owner of the entire `/opt/data` directory:

```bash
cd /opt/data && make fix-permissions
```

This executes:
```bash
docker compose exec -u root hermes chown -R hermes:hermes /opt/data
```

### Option 2: Manual Fix
If the `make` command doesn't work, run this directly:

```bash
docker compose exec -u root hermes chown -R hermes:hermes /opt/data/obsidian-vault/Vault
```

### Option 3: Grant Write Access Only
If you want to keep the current ownership but grant write access:

```bash
sudo chown -R hermes:hermes /opt/data/obsidian-vault/Vault
```

## Verifying Permissions

After running the fix, verify that hermes can write to the vault:

```bash
touch /opt/data/obsidian-vault/Vault/raw/test.txt && rm /opt/data/obsidian-vault/Vault/raw/test.txt
```

If this command succeeds without errors, the vault is now writable.

## Alternative: Use the Home Vault

If you cannot or do not want to change permissions, you can use the home vault instead:

```bash
mkdir -p /opt/data/home/obsidian-vault/Vault/raw/Tech
```

Then configure kb-ingest to use this path by creating a config file:

```bash
echo '{"kb_path": "/opt/data/home/obsidian-vault/Vault"}' > kb-config.json
```

## Permanent Solution

For a seamless experience, it's recommended to fix the permissions on `/opt/data` so that the hermes user owns all relevant directories. This allows kb-ingest and other Hermes tools to work without manual intervention.

## Troubleshooting

If you continue to experience issues:
1. Ensure Docker is running
2. Check that the hermes container is up: `docker compose ps hermes`
3. Verify the ownership: `ls -la /opt/data/obsidian-vault/Vault/raw/`

The ideal ownership should show `hermes hermes` for files and directories.