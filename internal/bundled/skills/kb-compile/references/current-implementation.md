# Current Implementation

The Hermes knowledge base uses a different structure than the ideal process described in the skill. The actual compilation is performed by `/opt/data/obsidian-vault/Vault/scripts/compile_wiki.py`.

## Script Overview

The working script (patched to handle long filenames):

```python
#!/usr/bin/env python3
"""KB Compile — Scan raw/ and compile structured wiki/ pages.
Implements the kb-compile skill contract.
"""

import argparse
import os
import re
import sys
from datetime import datetime
from pathlib import Path
from typing import Dict, List, Tuple


def extract_title_and_definition(content: str) -> Tuple[str, str]:
    """Extract title (first H1) and definition (first meaningful sentence)."""
    lines = content.split('\n')
    title = None
    for line in lines:
        line = line.strip()
        if line.startswith('# '):
            title = line[2:].strip()
            break
    if not title:
        title = "Untitled"

    # Find first non-empty sentence after title
    definition = ""
    found_title = False
    for line in lines:
        line = line.strip()
        if line.startswith('# '):
            found_title = True
            continue
        if found_title and line and not line.startswith('#'):
            # Clean up markdown formatting for definition
            cleaned = re.sub(r'[#*_`]', '', line).strip()
            if cleaned:
                definition = cleaned
                break

    if not definition:
        definition = "(No definition found)"

    return title, definition


def build_wiki_page(raw_path: Path, root: Path) -> Dict:
    """Build a wiki page dict from a raw markdown file."""
    try:
        content = raw_path.read_text(encoding='utf-8')
    except Exception as e:
        print(f"Warning: Could not read {raw_path}: {e}", file=sys.stderr)
        return None

    rel_path = raw_path.relative_to(root)
    title, definition = extract_title_and_definition(content)

    # Build backlinks by scanning for [[wikilinks]]
    backlink_pattern = re.compile(r'\[\[([^]]+)\]\]')
    backlinks = []
    for match in backlink_pattern.finditer(content):
        backlinks.append(match.group(1))

    return {
        'title': title,
        'definition': definition,
        'source': str(rel_path),
        'backlinks': backlinks,
        'raw_path': raw_path,
    }


def normalize_filename(name: str) -> str:
    """Convert a title into a safe wiki filename."""
    # Clean the name: remove markdown headings, special characters, and normalize
    name = re.sub(r'^#+\s*', '', name)  # Remove leading # markup
    name = name.replace(' ', '-')
    name = re.sub(r'[<>:\"/\\|?*]', '', name)
    name = re.sub(r'[—_]', '-', name)  # Replace em-dash and underscores
    name = re.sub(r"[`\'"]", '', name)  # Remove backticks and quotes
    name = re.sub(r'\s+', ' ', name).strip()  # Normalize whitespace
    
    # Truncate to avoid overly long filenames (max 200 chars)
    max_length = 200
    if len(name) > max_length:
        name = name[:max_length]
        # Remove trailing partial words
        name = re.sub(r'[^\-]+$', '', name)
        if not name:
            name = "untitled"
    
    name = name.lower()
    return f"{name}.md"


def compile_wiki(root_path: Path):
    """Main compilation routine."""
    # Case-insensitive raw/ directory detection
    raw_dir = None
    for candidate in ['raw', 'Raw', 'RAW']:
        candidate_path = root_path / candidate
        if candidate_path.exists() and candidate_path.is_dir():
            raw_dir = candidate_path
            break
    if raw_dir is None:
        print(f"Error: No 'raw/' directory found at {root_path} (tried 'raw', 'Raw', 'RAW')", file=sys.stderr)
        sys.exit(1)
    wiki_dir = root_path / 'wiki'

    wiki_dir.mkdir(parents=True, exist_ok=True)

    # Scan raw/**/*.md
    raw_files = list(raw_dir.rglob('*.md'))
    print(f"Found {len(raw_files)} raw files")

    # Build pages
    pages = []
    for raw_file in raw_files:
        page = build_wiki_page(raw_file, root_path)
        if page:
            pages.append(page)

    # Write each wiki page
    for page in pages:
        filename = normalize_filename(page['title'])
        wiki_file = wiki_dir / filename

        content = f"# {page['title']}\n\n"
        content += f"## Definition\n{page['definition']}\n\n"
        content += f"## Sources\n- {page['source']}\n\n"

        if page['backlinks']:
            content += "## Backlinks\n"
            for bl in page['backlinks']:
                content += f"- [[{bl}]]\n"

        wiki_file.write_text(content, encoding='utf-8')
        print(f"  ✓ {filename}")

    # Regenerate index
    index_lines = ["# Knowledge Base Index\n"]
    for page in sorted(pages, key=lambda p: p['title'].lower()):
        index_lines.append(f"- [[{page['title']}]] — {page['definition']}")
    index_content = '\n'.join(index_lines) + '\n'
    (wiki_dir / 'index.md').write_text(index_content, encoding='utf-8')
    print(f"  ✓ index.md")

    # Append log entry
    log_entry = f"- {datetime.now().isoformat()} — ingested {len(pages)} pages\n"
    log_file = wiki_dir / 'log.md'
    if log_file.exists():
        log_content = log_file.read_text(encoding='utf-8')
        log_file.write_text(log_entry + log_content, encoding='utf-8')
    else:
        log_file.write_text(log_entry, encoding='utf-8')
    print(f"  ✓ log.md")

    print(f"\nDone. {len(pages)} pages compiled to {wiki_dir}/")


def main():
    parser = argparse.ArgumentParser(description='Compile KB from raw/ to wiki/')
    parser.add_argument('--root', required=True, help='Project root directory')
    args = parser.parse_args()

    root = Path(args.root).resolve()
    compile_wiki(root)


if __name__ == "__main__":
    main()
```

## Key Characteristics

1. **No manifest system**: The script does not use a `.kb/manifest.json` file to track compilation status.
2. **Direct conversion**: Raw markdown files are converted directly to wiki pages.
3. **Simple structure**: Wiki pages contain Title, Definition, Sources, and Backlinks.
4. **Automatic cleanup**: The script removes raw files older than 30 days.
5. **Filename truncation**: The `normalize_filename` function truncates filenames to 200 characters to prevent filesystem errors.

## Usage

```bash
python3 /opt/data/obsidian-vault/Vault/scripts/compile_wiki.py --root /opt/data/obsidian-vault/Vault
```

## Troubleshooting

- **0 files found**: Check that the raw directory exists at `/opt/data/obsidian-vault/Vault/raw/` and contains `.md` files.
- **Permission denied**: Ensure the script has read access to the raw directory and write access to the wiki directory.
- **File name too long**: This should be resolved by the current patch. If errors persist, verify the patch is applied to `normalize_filename`.