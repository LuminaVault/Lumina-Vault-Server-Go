# Permission Troubleshooting for kb-compile

## Common Issues

### 1. Permission Denied Errors
**Symptom**: `ls: cannot access '/opt/data/obsidian-vault/Vault': Permission denied`

**Cause**: The vault directory is owned by `root:root` with permissions `drwxr-x---`. The Hermes agent user (`hermes`) is not in the root group and cannot access the directory.

**Solution**: Change ownership of the vault directory to the hermes user.

#### Method A: Using Makefile (Recommended)
```bash
cd /opt/data
make fix-permissions
```
This runs:
```bash
docker compose exec -u root hermes chown -R hermes:hermes /opt/data
```

#### Method B: Manual chown
If Docker is not running:
```bash
sudo chown -R hermes:hermes /opt/data/obsidian-vault/Vault
```

### 2. Docker Daemon Not Running
**Symptom**: `Cannot connect to the Docker daemon` when running `make fix-permissions`

**Cause**: Docker service is stopped.

**Solution**: Start Docker:
```bash
sudo systemctl start docker
```
Or if using Docker Compose directly:
```bash
sudo docker compose up -d
```

### 3. Vault Directory Missing
**Symptom**: `File not found: /opt/data/obsidian-vault/Vault`

**Cause**: The vault hasn't been set up yet, or the symlink is broken.

**Solution**: 
1. Check if the vault exists:
   ```bash
   ls -la /opt/data/obsidian-vault/
   ```
2. If missing, restore from backup or run the vault setup script:
   ```bash
   ~/.hermes/scripts/setup-obsidian-vault.sh
   ```

### 4. Git Commit Failures
**Symptom**: Compilation succeeds but changes are not saved to git.

**Cause**: The vault directory is not a git repository, or the agent lacks permission to commit.

**Solution**: 
1. Initialize git if needed:
   ```bash
   cd /opt/data/obsidian-vault/Vault
   git init
   git config user.email "hermes@agent"
   git config user.name "Hermes Agent"
   ```
2. Ensure the agent has write access to the repository.

## Prevention

To avoid permission issues in the future:
1. Run `make fix-permissions` after any manual file operations in `/opt/data`
2. Ensure the Hermes agent runs with consistent UID/GID by setting `HERMES_UID=1000` and `HERMES_GID=1000` in the Docker compose environment
3. Regularly check directory ownership with `ls -la /opt/data/obsidian-vault/`

## Verification

After fixing permissions, verify access:
```bash
ls -la /opt/data/obsidian-vault/Vault
ls ~/.hermes/obsidian-vault/Vault/Raw/
python3 -c "import pathlib; p = pathlib.Path('~/.hermes/obsidian-vault/Vault/Raw'); print(p.exists(), p.is_dir())"
```

## Related Resources
- `~/.hermes/scripts/compile_wiki.py` — Main compilation script
- `Makefile` — Contains `fix-permissions` target
- `/opt/data/obsidian-vault/Vault` — Actual vault location
- `~/.hermes/obsidian-vault/Vault` — Symlink location

---
*Documented by Hermes Agent during routine operations. This reference captures permission troubleshooting steps for future agents.*