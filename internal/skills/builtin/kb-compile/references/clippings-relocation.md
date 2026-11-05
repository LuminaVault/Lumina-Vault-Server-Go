# Clippings Directory Relocation Pattern

When a `Clippings/` directory accumulates files from the Readwise or similar clipping pipeline, move them to `raw/<Topic>/` subdirectories before running kb-compile.

## Classification Heuristics

1. **Read `tags` or `topic` from YAML frontmatter** — if present, use directly.
2. **Parse `source` URL**:
   - `youtube.com` → check content topic (stocks for stock videos, health for fitness, etc.)
   - `x.com` / `twitter.com` → typically `Stocks` (financial threads), `News` (politics/news threads)
3. **Filename heuristics**:
   - `$TICKER`, `Stock`, `Fib` → `Stocks/`
   - `UFC`, `MMA`, `Khamzat` → `Sports/`
   - `WWE`, `Tony Khan` → `WWE/`
   - `Weight`, `Fat`, `Health` → `Health/`
   - `App Store`, `MRR`, `indie dev` → `Business/`
   - `Nick Fuentes`, `Trump`, `Vance` → `News/`
4. **Default**: `uncategorized/` (not `Clippings/`)

## Procedure

```python
import os, shutil, yaml

src_dir = '{KB_PATH}/Clippings'
raw_base = '{KB_PATH}/raw'

for filename in os.listdir(src_dir):
    filepath = os.path.join(src_dir, filename)
    if not filename.endswith('.md'):
        continue
    
    with open(filepath) as f:
        # Simple frontmatter extraction
        lines = f.readlines()
        if lines[0].strip() != '---':
            continue
        end_idx = next(i for i, l in enumerate(lines[1:], 1) if l.strip() == '---')
        fm_text = ''.join(lines[1:end_idx])
        fm = yaml.safe_load(fm_text)
    
    topic = fm.get('topic') or fm.get('tags', ['uncategorized'])[0]
    
    # Normalize to known subdirectories
    known_topics = {'AI', 'Books', 'Business', 'Careers', 'Dev', 'Health',
                    'Hermes', 'HermesPortfolio', 'NBA', 'News', 'Sports',
                    'Stocks', 'Swift', 'TV and Movies', 'Tech', 'WWE',
                    'XFeed', 'YouTube', 'uncategorized'}
    
    if topic not in known_topics:
        # Apply filename heuristics
        topic = classify_by_filename(filename) or 'uncategorized'
    
    dest_dir = os.path.join(raw_base, topic)
    os.makedirs(dest_dir, exist_ok=True)
    shutil.move(filepath, os.path.join(dest_dir, filename))
```

## After Relocation

Delete the empty Clippings directory:
```bash
rmdir {KB_PATH}/Clippings
```
