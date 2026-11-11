# File Name Too Long Error

## Issue

When running `kb-compile`, the compilation may fail with an `OSError: [Errno 36] File name too long` error. This occurs when the `compile_wiki.py` script generates filenames that exceed the filesystem's maximum filename length.

## Cause

The `normalize_filename` function in `compile_wiki.py` was converting raw file titles into wiki filenames without any truncation. When raw files have very long titles (e.g., GitHub repository scrapes, detailed system descriptions), the resulting filenames can be 255+ characters, which exceeds typical filesystem limits.

## Solution

The actual working fix (verified 2026-05-11) has TWO parts:

### Part 1 — Filename truncation (UTF-8 byte-aware for ext4)

```python
def normalize_filename(name: str) -> str:
    """Convert a title into a safe wiki filename."""
    name = name.replace(' ', '-')
    name = re.sub(r'[<>:"/\\|?*]', '', name)
    name = name.lower()
    # ext4 max filename is 255 bytes; truncate by encoded length, not char count
    encoded = name.encode('utf-8')
    if len(encoded) > 250:
        name = encoded[:245].decode('utf-8', errors='ignore')
    return f"{name}.md"
```

The old "200 character" truncation was wrong — ext4 limits by byte length, not characters. UTF-8 multibyte characters meant some filenames still hit the 255-byte ceiling. The byte-aware truncation fixes this.

### Part 2 — Content truncation + per-file error isolation

In the write loop: titles truncated to 200 chars, definitions to 500 chars, and each `wiki_file.write_text()` wrapped in `try/except OSError`. This prevents a single raw file with an entire vault specification as its first H1 from either creating unmanageable wiki pages or crashing the whole compile run.

```python
failed = []
for page in pages:
    filename = normalize_filename(page['title'])
    wiki_file = wiki_dir / filename
    try:
        safe_title = page['title'][:200] + '...' if len(page['title']) > 200 else page['title']
        safe_def = page['definition'][:500] + '...' if len(page['definition']) > 500 else page['definition']
        # ... write content ...
        wiki_file.write_text(content, encoding='utf-8')
    except OSError as e:
        failed.append((filename, str(e)))
```

## Verification

Compiled 305 raw files (some with extremely long H1 titles) without a single crash. All wiki files under 255-byte filenames, all content reasonably sized.

## Prevention

Ensure any future versions of `compile_wiki.py` maintain filename truncation logic to prevent recurrence.