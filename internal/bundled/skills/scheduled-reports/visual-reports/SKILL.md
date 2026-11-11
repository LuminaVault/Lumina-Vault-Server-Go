---
name: visual-reports
version: 1.0
description: Generate standardized 4-panel visual reports from structured data sources for automated delivery via Discord, Slack, and other channels.
---

# Visual Report Generation

**Skill Description**: Generate standardized 4-panel visual reports from structured data sources. This pattern transforms data from any domain (portfolio, server monitoring, entertainment, news, etc.) into consistent PNG charts for automated delivery via Discord, Slack, or other channels.

**Trigger Conditions**:
- User requests automated visual reporting
- Need to generate charts from data stored in the Kanban database
- Desire to standardize report format across multiple domains
- Integration with scheduled jobs for regular delivery

**Key Components**:
1. **4-Panel Layout**: Consistent structure across all domains:
   - Panel 1: Categorical distribution (pie or bar chart)
   - Panel 2: Trend over time (line chart)
   - Panel 3: Top items or performers
   - Panel 4: Summary or alert analysis

2. **Database Integration**: Reads from the Kanban `tasks` table where each profile stores results in `workspace_path/result.json`.

3. **System Python Execution**: Visual report generators must run with system Python (`/usr/bin/python3`) which has matplotlib and pandas installed, as the Hermes virtual environment lacks these packages.

4. **Delivery Mechanism**: Reports are sent as image attachments to Discord and Slack via the Report Generation Agent.

## Implementation Pattern

### 1. Create Visual Report Generator Script
Each domain gets its own script in `~/hermes/kanban/` following this template:

```python
#!/usr/bin/env python3
import sqlite3
import os
import sys
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import pandas as pd
from datetime import datetime
import numpy as np

KANBAN_DB = os.path.expanduser('~/hermes/kanban/board/board.db')
VISUALS_DIR = os.path.expanduser('~/hermes/kanban/visuals')

def ensure_visuals_dir():
    os.makedirs(VISUALS_DIR, exist_ok=True)

def generate_report():
    ensure_visuals_dir()
    
    # Query database for latest task from this profile
    conn = sqlite3.connect(KANBAN_DB)
    df = pd.read_sql_query("SELECT * FROM tasks WHERE profile = 'YOUR_PROFILE' ORDER BY created_at DESC LIMIT 1", conn)
    conn.close()
    
    if df.empty:
        print("No data found")
        return None
    
    # Load result.json from workspace
    try:
        result_path = df['workspace_path'].iloc[0]
        with open(os.path.join(result_path, 'result.json'), 'r') as f:
            result = json.load(f)
    except Exception as e:
        print(f"Error loading result: {e}")
        return None
    
    # Extract domain-specific data and create 4-panel chart
    # ... domain-specific logic ...
    
    # Save PNG
    timestamp = datetime.now().strftime('%Y%m%d_%H%M%S')
    filename = f"{profile}_report_{timestamp}.png"
    filepath = os.path.join(VISUALS_DIR, filename)
    plt.savefig(filepath, dpi=150, bbox_inches='tight')
    plt.close()
    
    return filepath

def generate_and_print_report():
    try:
        filepath = generate_report()
        if filepath:
            print(f"MEDIA:{filepath}")
            print(f"✅ Visual report generated: {os.path.basename(filepath)}")
            return filepath
        else:
            print("❌ Failed to generate visual report")
            return None
    except Exception as e:
        print(f"❌ Error generating visual report: {e}")
        return None

if __name__ == "__main__":
    generate_and_print_report()
```

### 2. Integrate with Report Generation Agent
Modify `~/hermes/profiles/report_generator/report_generator_agent.py` to call the appropriate visual report generator based on the agent profile:

```python
def generate_portfolio_report(workspace):
    import subprocess
    import sys
    result = subprocess.run(
        [sys.executable, os.path.expanduser("~/hermes/kanban/portfolio_visual_report.py")],
        capture_output=True, text=True
    )
    # Parse MEDIA: line from stdout
    for line in result.stdout.splitlines():
        if line.startswith("MEDIA:"):
            return line[6:].strip()
    return None
```

### 3. Populate Database with Sample Data for Testing
Use this script to create test data:

```python
#!/usr/bin/env python3
import sqlite3
import json
from datetime import datetime, timedelta
import os
import random

KANBAN_DB = os.path.expanduser('~/hermes/kanban/board/board.db')
WORKSPACES_DIR = os.path.expanduser('~/hermes/kanban/workspaces')

def create_tables(conn):
    cursor = conn.cursor()
    cursor.execute('''
        CREATE TABLE IF NOT EXISTS tasks (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            title TEXT NOT NULL,
            status TEXT NOT NULL,
            priority TEXT,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL,
            profile TEXT NOT NULL,
            domain TEXT,
            comments TEXT,
            workspace_path TEXT,
            result_summary TEXT
        )
    ''')
    conn.commit()

def insert_task(conn, task):
    cursor = conn.cursor()
    cursor.execute('''
        INSERT INTO tasks (title, status, priority, created_at, updated_at, profile, domain, comments, workspace_path, result_summary)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    ''', (
        task['title'], task['status'], task.get('priority'),
        task['created_at'], task['updated_at'], task['profile'],
        task.get('domain'), task.get('comments', ''), task['workspace_path'], task.get('result_summary', '')
    ))
    conn.commit()
    return cursor.lastrowid

# Create sample data for each profile...
```

### 4. Technical Requirements
- **System Python**: Must use `/usr/bin/python3` for matplotlib/pandas support
- **Matplotlib Backend**: Use `'Agg'` for non-interactive PNG generation
- **Database Schema**: Tasks table with columns: id, title, status, priority, created_at, updated_at, profile, domain, comments, workspace_path, result_summary
- **Result Format**: Each task's workspace contains `result.json` with domain-specific data structure

### 5. Delivery Patterns
Visual reports are delivered via:
- **Discord**: Using `send_discord()` with image attachment
- **Slack**: Using `send_slack()` with image attachment
- **Telegram**: Via Telegram delivery in scheduled jobs

The Report Generation Agent automatically sends the generated PNG to configured channels.

## Existing Implementations

### Portfolio Visual Report
- **Profile**: `portfolio_tracker`
- **Data Structure**: 
  ```json
  {
    "portfolio": {
      "total_value": 45678.90,
      "daily_change": 1.23,
      "allocation": {"Technology": 45.2, "Healthcare": 15.8, ...},
      "performance": [{"date": "2026-05-01", "value": 45000}, ...],
      "top_performers": [{"ticker": "AAPL", "change_pct": 2.5}, ...],
      "recent_alerts": [{"type": "threshold", "ticker": "TSLA", ...}]
    }
  }
  ```
- **Panels**: Allocation pie, Performance trend, Top performers, Alert types

### Server Monitoring Visual Report
- **Profile**: `server_monitor`
- **Data Structure**:
  ```json
  {
    "metrics": {
      "cpu": [{"timestamp": "...", "usage_pct": 45.2}, ...],
      "memory": [{"timestamp": "...", "used_gb": 8.5, "total_gb": 16}, ...],
      "disk": [{"timestamp": "...", "used_pct": 62.3}, ...],
      "alerts": [{"type": "high_cpu", "severity": "warning"}, ...]
    }
  }
  ```
- **Panels**: CPU usage, Memory usage, Disk usage, Recent alerts

### Entertainment Digest Visual Report
- **Profile**: `entertainment_scraper`
- **Data Structure**:
  ```json
  {
    "content": {
      "movies": [{"title": "Movie", "genres": ["Action", "Sci-Fi"], "rating": 7.5, "trend_score": 85}, ...],
      "tv_shows": [{"title": "Show", "network": "HBO", "rating": 8.2}, ...],
      "trending": [{"title": "Trending", "score": 92}, ...]
    }
  }
  ```
- **Panels**: Movie genre distribution, Trending content, TV shows by network, Content type distribution

### News Digest Visual Report
- **Profile**: `news_digest`
- **Data Structure**:
  ```json
  {
    "content": {
      "articles": [{"title": "News", "source_name": "TechCrunch", "published_at": "...", 
                   "categories": ["Technology", "Business"], "sentiment": {"score": 0.2}}, ...]
    }
  }
  ```
- **Panels**: News by source, Hourly volume, Top categories, Sentiment analysis

## Best Practices

1. **Always use system Python** (`/usr/bin/python3`) for running visual report generators
2. **Follow the 4-panel structure** for consistency across domains
3. **Store results in `workspace_path/result.json`** for easy retrieval
4. **Use `MEDIA:` prefix** in stdout to indicate image path
5. **Test with sample data** before deploying to production

## Troubleshooting

**Q: Why do visual report generators fail with "No module named 'matplotlib'"?**
A: The Hermes virtual environment lacks matplotlib. Always run visual report generators with system Python (`/usr/bin/python3`).

**Q: How do I add a new visual report domain?**
A: Create a new script in `~/hermes/kanban/` following the pattern, then update the Report Generation Agent to call it for the appropriate profile.

**Q: Where are the generated images saved?**
A: In `~/hermes/kanban/visuals/` with filename pattern `{profile}_report_{timestamp}.png`

**Q: How are reports delivered to Discord/Slack?**
A: The Report Generation Agent automatically sends the PNG as an image attachment to configured channels.

## Related Skills
- `kanban-orchestrator`: For the Multi-Agent Kanban system that uses these visual reports
- `scheduled-reports`: For patterns in automated report delivery
- `report-generation`: For the Report Generation Agent itself