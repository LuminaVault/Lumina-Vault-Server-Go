## AI Cohort Scoreboard Directory Structure

**Location:** `/opt/data/obsidian-vault/Vault/Wiki/Finance/AI Cohort/`

**Expected Files:**
```
2026-04-27 — AI Scoreboard.md
2026-04-28 — AI Scoreboard.md
2026-04-29 — AI Scoreboard.md
2026-04-30 — AI Scoreboard.md
2026-05-01 — AI Scoreboard.md
2026-05-03 — AI Scoreboard.md
2026-05-04 — AI Scoreboard.md
2026-05-05 — AI Scoreboard.md  # (if market was open)
2026-05-06 — AI Scoreboard.md  # (current day)
```

**File Format:**
- Markdown with specific sections:
  - `# 🤖 AI Cohort Scoreboard`
  - `## Cohort Split`
  - `## 📊 Live Scoreboard` (table format)
  - `## 📈 60-Day Relative Performance Gap` (chart + commentary)
  - `## 🕵️  Insider Buys (90-day lookback)`
  - `## 🎯 Top Signals — Buy / Sell`
- Generated daily by `ai_scoreboard.py` at ~8:30am ET

**Directory Permissions:**
- Owner: `hermes` (user 1001)
- Group: `hermes`
- Permissions: `drwxr-xr-x` (755)
- The cron job runs as `hermes` and has write access
- Other users may have read access but not write access

**Related Scripts:**
- `ai_scoreboard.py` - Main scoreboard generator
- `generate_marketing_content.py` - Marketing content generator (consumes scoreboard files)
- `ai_scoreboard_alerts.py` - Alert monitoring

**Vault Integration:**
- Part of the user's Obsidian vault
- Integrated with Hermes Agent automation
- Data flows from Yahoo Finance (yfinance) → scoreboard → marketing content → social platforms

**Troubleshooting:**
- If directory missing: Check vault sync status
- If permission denied: Verify user execution context matches cron job user
- If no data: Check market holidays, API connectivity, ticker issues