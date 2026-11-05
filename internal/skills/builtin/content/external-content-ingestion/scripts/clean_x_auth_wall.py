#!/usr/bin/env python3
"""
Find and optionally remove X auth-wall placeholder files from Raw/ vault folders.

Usage:
  python3 clean_x_auth_wall.py --dry-run    # List affected files only
  python3 clean_x_auth_wall.py --delete      # Delete them (permanent)
  python3 clean_x_auth_wall.py --rename      # Rename to .ignored extension

This script is intended to be run periodically or after an X ingestion batch
where r.jina.ai returned login-page content that slipped past content guards.
"""

import argparse
from pathlib import Path
import re

# X login page boilerplate fragments (minimum 3 matches = auth wall)
BOILERPLATE_PATTERNS = [
    "don't miss what's happening",
    "people on x are the first to know",
    "sign in to x",
    "log in to your account",
    "create account",
    "terms of service",
    "privacy policy",
    "cookie policy",
    "what's happening",
    "trending now",
    "see new posts",
    "new to x?",
    "© x corp",
    "© 2026 x corp",
]

# Files smaller than this that look like auth-wall are suspicious
SIZE_THRESHOLD = 1500  # bytes


def is_auth_wall(content: str) -> bool:
    lower = content.lower()
    hits = sum(1 for pattern in BOILERPLATE_PATTERNS if pattern in lower)
    return hits >= 3


def find_auth_wall_files(root: Path, min_size: int = 0):
    """Yield Path objects for files that appear to be X auth-wall placeholders."""
    for md_file in root.rglob("*.md"):
        if md_file.is_file():
            size = md_file.stat().st_size
            if min_size and size < min_size:
                continue
            try:
                text = md_file.read_text(encoding="utf-8", errors="ignore")
            except Exception:
                continue
            if is_auth_wall(text):
                yield md_file


def main():
    parser = argparse.ArgumentParser(description="Clean X auth-wall placeholder files from vault Raw/")
    parser.add_argument("--root", default="/opt/data/obsidian-vault/Vault", help="Vault root path")
    parser.add_argument("--dry-run", action="store_true", help="List files but take no action")
    parser.add_argument("--delete", action="store_true", help="Permanently delete matched files")
    parser.add_argument("--rename", action="store_true", help="Rename matched files to .ignored extension")
    args = parser.parse_args()

    root = Path(args.root) / "Raw"
    if not root.exists():
        print(f"❌ Raw/ directory not found: {root}")
        return 1

    matches = list(find_auth_wall_files(root))
    print(f"🔍 Scanned {root} — found {len(matches)} auth-wall placeholder file(s)")

    if not matches:
        print("✅ No issues detected.")
        return 0

    if args.dry_run:
        print("\nAffected files (dry-run — no changes):")
        for f in matches:
            size = f.stat().st_size
            print(f"  • {f.relative_to(root)} ({size} bytes)")
        return 0

    if args.delete:
        for f in matches:
            f.unlink()
            print(f"🗑️  Deleted: {f.relative_to(root)}")
        print(f"\n✅ Deleted {len(matches)} file(s)")
        return 0

    if args.rename:
        for f in matches:
            new_path = f.with_suffix(f.suffix + ".ignored")
            f.rename(new_path)
            print(f"✏️  Renamed: {f.name} → {new_path.name}")
        print(f"\n✅ Renamed {len(matches)} file(s)")
        return 0

    print("\n⚠️  No action specified. Use --dry-run, --delete, or --rename.")
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
