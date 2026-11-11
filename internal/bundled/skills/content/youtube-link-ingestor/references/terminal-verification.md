# Terminal-Based Verification for YouTube Link Ingestor

When verifying YouTube link ingestion results, especially when outputting `[SILENT]`, terminal tools (grep, awk, sort, uniq) often provide the most reliable and efficient methods for processing the YouTube-Links.md file and comparing video IDs. This is particularly true when dealing with the specific markdown format of the links file.

## Extracting Video IDs from YouTube-Links.md

The YouTube-Links.md file uses markdown format: `YYYY-MM-DD - [Title](URL)`. Use these commands to extract video IDs:

### Basic Extraction
```bash
# Extract all video IDs from the file
cat /opt/data/home/obsidian-vault/Vault/Raw/YouTube-Links.md | grep -o 'v=[a-zA-Z0-9_-]\{11\}' | cut -c 3- | sort | uniq
```

### Extract with Line Numbers (for debugging)
```bash
# Show which lines contain YouTube links and their video IDs
grep -n 'youtube\.com/watch\|youtu\.be/' /opt/data/home/obsidian-vault/Vault/Raw/YouTube-Links.md | while read line; do
    linenum=$(echo "$line" | cut -d: -f1)
    url=$(echo "$line" | grep -o 'https://[^)]*')
    if [[ $url =~ v=([a-zA-Z0-9_-]{11}) ]]; then
        echo "$linenum: ${BASH_REMATCH[1]}"
    elif [[ $url =~ youtu\.be/([a-zA-Z0-9_-]{11}) ]]; then
        echo "$linenum: ${BASH_REMATCH[1]}"
    fi
done
```

## Comparing Video IDs

### Compare Two Sets of IDs
```bash
# Compare extracted IDs against existing file IDs
comm -23 <(echo "$extracted_ids" | sort) <(sort /tmp/existing_ids.txt)
```

### Find IDs in File but Not in Scan
```bash
# IDs that are in the file but not found in recent sessions
comm -13 <(echo "$extracted_ids" | sort) <(sort /tmp/existing_ids.txt)
```

## Verifying No New Links Exist

### Comprehensive Verification Script
```bash
#!/bin/bash
# verify_no_new_links.sh - Verify that no new YouTube links exist since last update

LAST_UPDATE="2026-05-06 07:01:00"  # Update this to last successful run time
YOUTUBE_FILE="/opt/data/home/obsidian-vault/Vault/Raw/YouTube-Links.md"

echo "=== Verification Started ==="
echo "Last update: $LAST_UPDATE"
echo ""

# 1. Check for newer session files
echo "Checking for sessions newer than $LAST_UPDATE..."
find /opt/data/sessions -name "session_*.json" -newermt "$LAST_UPDATE" | wc -l
echo ""

# 2. Extract video IDs from newer sessions
echo "Extracting video IDs from newer sessions..."
find /opt/data/sessions -name "session_*.json" -newermt "$LAST_UPDATE" -exec grep -h 'youtube\.com/watch?v=\|youtu\.be/' {} \; | \
    grep -o 'v=[a-zA-Z0-9_-]\{11\}' | cut -c 3- | \
    sort | uniq > /tmp/new_ids.txt

if [ -s /tmp/new_ids.txt ]; then
    echo "Found new video IDs:"
    cat /tmp/new_ids.txt
    echo ""
    echo "ERROR: New YouTube links exist but were not ingested!"
    exit 1
fi

# 3. Extract all existing video IDs from file
echo "Existing video IDs in file:"
cat "$YOUTUBE_FILE" | grep -o 'v=[a-zA-Z0-9_-]\{11\}' | cut -c 3- | sort | uniq | head -10
echo ""

# 4. Final verification
if [ ! -s /tmp/new_ids.txt ]; then
    echo "VERIFIED: No new YouTube links found since last update."
    echo "Outputting [SILENT]..."
    echo "[SILENT] - Verified no new YouTube links after $LAST_UPDATE"
    exit 0
fi
```

## Common Issues and Solutions

### Issue: grep not matching due to file format
The YouTube-Links.md file may have empty lines or lines starting with numbers. Use `grep -a` to treat binary files as text if needed.

### Issue: Video IDs with special characters
Ensure your regex pattern handles all valid YouTube video ID characters: `a-zA-Z0-9_-`.

### Issue: Performance on large files
For large vaults, limit scanning to recent session files only:
```bash
find /opt/data/sessions -name "session_*.json" -mtime -7 | xargs grep -h 'youtube\.com/watch?v=\|youtu\.be/'
```

## When to Use Terminal vs Python

**Use terminal tools when:**
- Processing large text files (faster and more memory efficient)
- Working with well-formatted files like YouTube-Links.md
- Need to quickly grep, sort, and compare IDs
- Debugging file parsing issues

**Use Python when:**
- Implementing the core ingestion logic in a script
- Need more complex processing or API integration
- Working with structured data or JSON

This reference provides practical terminal-based techniques for verifying YouTube link ingestion results. For the systematic verification process, see the main SKILL.md documentation.