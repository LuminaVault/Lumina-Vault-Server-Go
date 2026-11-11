# Vault Synchronization Best Practices

When syncing your Obsidian vault between multiple environments (e.g., Mac and VPS), follow these guidelines to prevent data loss:

## 🔄 Sync Before You Push

Always ensure both sides have the same content before running a sync with `--delete`.

**Problem:** If you push from Mac to VPS when the Mac vault is missing files that exist on the VPS, those files will be **permanently deleted** from the VPS.

**Solution:** 
1. Pull from VPS to Mac first to get the latest content
2. Verify both sides match
3. Then push if needed

## 🛡️ Use Dry-Run First

Always test with `--dry-run` before executing a destructive sync:

```bash
rsync -avz --progress --delete --dry-run /path/to/source/ user@host:/path/to/destination/
```

This shows what would happen without making changes.

## 📦 Use Git for the Vault

The safest long-term solution is to put your vault under version control:

```bash
cd /opt/data/obsidian-vault/Vault
git init
git add .
git commit -m "Initial commit"
```

Then on each machine:
```bash
git clone <remote-url>
```

Benefits:
- Change history and recovery
- No accidental deletions (commits are permanent)
- Easy branching/merging
- Sync via push/pull with remote

## ⚙️ Modify Rsync Script Temporarily

When actively adding content from one environment, temporarily remove `--delete`:

```bash
# During content creation
rsync -avz --progress /path/to/mac/ user@vps:/path/to/vps/

# After both sides are aligned, run with --delete for cleanup
rsync -avz --progress --delete /path/to/mac/ user@vps:/path/to/vps/
```

## 🔍 Verify Before Deleting

If you must use `--delete`, verify what will be deleted:

```bash
# See what files would be deleted
rsync -avz --progress --delete --itemize-changes /path/to/source/ user@host:/path/to/destination/ | grep '^deleting'
```

## 🎯 Hermes-Specific Workflow

Given Hermes's autonomous nature, consider this workflow:

1. **Add content via Hermes** (`/kb-ingest`) on the VPS where Hermes runs
2. **Let Hermes compile** (`/kb-compile`) on the VPS
3. **Pull from VPS to Mac** to get the latest content
4. **Only push from Mac to VPS** when you've made changes on Mac

This ensures the VPS (where Hermes lives) always has the canonical version.

## 🚨 Emergency Recovery

If files were deleted:
1. Recreate entries via `/kb-ingest` if you have the URLs
2. Restore from any available backups
3. Check if files exist in the Mac vault (they might have been there originally)

## 📚 Related Skills

- `/kb-compile` - Compile raw content into wiki
- `/kb-ingest` - Ingest URLs and content
- `/kb-reflect` - Discover connections across content