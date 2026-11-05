#!/usr/bin/env python3
"""
YouTube link saver — periodically scans conversation history for YouTube links
and saves them to the Obsidian vault with minimal metadata.

This script is designed for the "link saver" pattern: when user wants to track
YouTube videos without fetching full transcripts or video content.

Usage: Run as a cron job every 15-30 minutes.
"""

import re
import os
import json
import subprocess
from datetime import datetime
from pathlib import Path

# Configuration
VAULT_ROOT = Path.home() / "obsidian-vault" / "Vault"
RAW_YOUTUBE_DIR = VAULT_ROOT / "Raw" / "YouTube"
STATE_FILE = Path.home() / ".hermes" / "state" / "youtube_link_saver_state.json"

# YouTube URL patterns
YOUTUBE_REGEX = re.compile(
    r'(https?://(?:www\.)?(?:youtube\.com/watch\?v=|youtu\.be/)[^?\s]+)'
)

def load_state() -> dict:
    """Load state file tracking processed URLs."""
    if STATE_FILE.exists():
        try:
            return json.loads(STATE_FILE.read_text())
        except Exception:
            pass
    return {"last_check": None, "processed_urls": {}}

def save_state(state: dict) -> None:
    """Save state to file."""
    STATE_FILE.parent.mkdir(parents=True, exist_ok=True)
    STATE_FILE.write_text(json.dumps(state, indent=2))

def fetch_conversation_history() -> list:
    """
    Fetch recent conversation history from all platforms.
    Uses Hermes session_search tool to get messages from Discord, Telegram, Slack.
    """
    # Use session_search to get recent messages
    # This is a simplified version - actual implementation may vary
    cmd = [
        "hermes", "session_search",
        "--query=youtube.com/watch\\?v=|youtu.be/",
        "--limit=100"
    ]
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        print(f"[error] session_search failed: {result.stderr}")
        return []
    
    try:
        data = json.loads(result.stdout)
        return data.get("messages", [])
    except json.JSONDecodeError:
        print(f"[error] Invalid JSON from session_search: {result.stdout}")
        return []

def extract_youtube_urls(text: str) -> list:
    """Extract YouTube URLs from text."""
    urls = []
    for match in YOUTUBE_REGEX.finditer(text):
        url = match.group(1).strip()
        # Normalize URL (convert youtube.com to youtu.be if needed)
        if "youtube.com/watch?v=" in url:
            video_id = url.split("v=")[1].split("&")[0] if "&" in url.split("v=")[1] else url.split("v=")[1]
            url = f"https://youtu.be/{video_id}"
        urls.append(url)
    return urls

def fetch_video_title(url: str) -> str:
    """
    Fetch video title using minimal approach.
    For simple link saving, we may not fetch content; use video ID as title.
    """
    # Extract video ID from URL
    if "youtu.be/" in url:
        video_id = url.split("youtu.be/")[1]
    else:
        video_id = url.split("v=")[1].split("&")[0] if "&" in url.split("v=")[1] else url.split("v=")[1]
    
    # Try to fetch title using yt-dlp or YouTube API if available
    # For minimal implementation, just return video ID
    return video_id

def sanitize_filename(name: str) -> str:
    """Sanitize filename for filesystem."""
    name = re.sub(r'[<>:"/\\|?*]', '_', name)
    return name.strip()[:120]

def save_youtube_link(url: str, video_id: str, title: str) -> None:
    """Save YouTube link to vault."""
    date_str = datetime.now().strftime("%Y-%m-%d")
    safe_title = sanitize_filename(f"{title}-{video_id}")
    filename = f"{date_str} — {safe_title}.md"
    dest = RAW_YOUTUBE_DIR / filename
    dest.parent.mkdir(parents=True, exist_ok=True)
    
    frontmatter = f"""---
source: YouTube
url: {url}
date: {date_str}
tags: [Video]
---
"""
    dest.write_text(frontmatter)
    print(f"✅ Saved: Raw/YouTube/{filename}")

def main():
    print("Running YouTube link saver...")
    state = load_state()
    messages = fetch_conversation_history()
    
    newly_saved = 0
    for msg in messages:
        # Extract URLs from message content
        urls = extract_youtube_urls(msg.get("content", ""))
        for url in urls:
            url_hash = hashlib.sha256(url.encode()).hexdigest()[:16]
            if url_hash in state["processed_urls"]:
                continue
            
            # Fetch video title (minimal approach)
            video_id = url.split("/")[-1]
            title = fetch_video_title(url)
            
            # Save to vault
            save_youtube_link(url, video_id, title)
            
            # Update state
            state["processed_urls"][url_hash] = {
                "url": url,
                "title": title,
                "saved_at": datetime.now().isoformat()
            }
            newly_saved += 1
    
    # Save state
    save_state(state)
    
    print(f"✅ Done — {newly_saved} new YouTube links saved.")

if __name__ == "__main__":
    import hashlib
    main()