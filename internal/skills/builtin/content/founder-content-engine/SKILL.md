---
name: founder-content-engine
category: content
description: Weekly founder content research and production engine that transforms trending news into ready-to-use content assets.
version: 1.0.0
author: Hermes Agent
license: MIT
prerequisites:
  env_vars: []
  commands: [python3, hermes]
metadata:
  hermes:
    tags: [Content, Research, Production, Weekly, Automation, Founder, Creator]
---

# Founder Content Engine

## Overview

The Founder Content Engine is an autonomous weekly system that runs every Monday at 6:00 AM to generate comprehensive content assets. It transforms trending tech/startup news into ready-to-use content, eliminating blank-page syndrome for founders and creators.

## Core Functionality

### Weekly Output (Every Monday at 6:00 AM)
- **5 Post Ideas** from this week's news
- **3 Founder Stories** worth telling
- **10 Hooks** based on what's already working
- **1 Long-Form Article Draft** (1,500+ words)
- **1 Short Video Script** (60-second explainer)
- **1 Newsletter Outline** with full structure

### Key Features
- **Automated News Aggregation**: Fetches 25+ news items from 5 key queries
- **Trend Analysis**: Identifies top trends using frequency analysis and pattern recognition
- **Content Adaptation**: Transforms trends into multiple content formats
- **Multi-Platform Delivery**: Saves to files and (optionally) delivers to Discord, Slack, Telegram
- **Customizable Topics**: Easily modify topics_of_interest to match your niche

## Technical Architecture

### Components
1. **NewsFetcher** – Aggregates news from multiple sources
2. **TrendAnalyzer** – Identifies trending topics and patterns
3. **ContentGenerator** – Multiple specialized generators:
   - PostIdeasGenerator
   - FounderStoriesFinder
   - HooksGenerator
   - ArticleDrafter
   - VideoScriptWriter
   - NewsletterOutliner
4. **DeliveryManager** – Handles multi-platform distribution
5. **FileSaver** – Persists content to organized files

### Data Flow
```
News Sources → NewsFetcher → TrendAnalyzer → ContentGenerator → DeliveryManager → User
     ↓               ↓               ↓                  ↓                  ↓
   Raw Data     Trend Data     Content Ideas   Draft Content    Published Content
```

## Usage

### Prerequisites
- Hermes Agent with web_search and send_message tools configured
- Python 3.7+ environment
- cron or similar scheduler

### Installation
1. Create the script directory:
   ```bash
   mkdir -p ~/content_engine/outputs
   ```

2. Save the founder_content_engine.py script to `~/content_engine/`

### Configuration
Edit the `topics_of_interest` list in the script to match your niche:

```python
self.topics_of_interest = [
    "Swift programming", "iOS development", "Swift on Server", 
    "AI agents", "autonomous systems", "content engines",
    "startup building", "bootstrapping", "indie hacking",
    "knowledge management", "Obsidian", "second brain",
    "Vapor", "Docker", "Hetzner", "cloud infrastructure"
]
```

### Scheduling

#### System Cron
```bash
0 6 * * 1 cd ~/content_engine && python3 founder_content_engine.py
```

#### Hermes Cron Job
```bash
cronjob create --name='founder-content-engine' \
               --schedule='0 6 * * 1' \
               --script='~/content_engine/founder_content_engine.py'
```

### Manual Execution
```bash
python3 ~/content_engine/founder_content_engine.py
```

## Output Format

### File Organization
```
~/content_engine/outputs/
├── article_YYYYMMDD_HHMMSS.md    # Long-form article draft
├── video_YYYYMMDD_HHMMSS.md      # Video script
├── newsletter_YYYYMMDD_HHMMSS.md # Newsletter outline
└── logs/                        # Delivery logs (optional)
```

### Content Structure
Each output follows industry-standard formats:
- **Articles**: Markdown with proper headings, subheadings, and formatting
- **Video Scripts**: Time-coded with visual and audio cues
- **Newsletter Outlines**: Complete with all sections and placeholders

## Customization Options

### Modify News Sources
Edit the `search_terms` list in `fetch_news()` to include your preferred sources:

```python
search_terms = [
    "tech news this week",
    "startup funding this week",
    "AI developments this week",
    "Swift programming news",
    "iOS development updates"
]
```

### Adjust Content Volume
Modify the `deliverables` dictionary to change output quantities:

```python
self.deliverables = {
    "post_ideas": 5,      # Change to desired number
    "founder_stories": 3, # Change to desired number
    "hooks": 10,          # Change to desired number
    "article_draft": 1,
    "video_script": 1,
    "newsletter_outline": 1
}
```

### Add New Content Types
Extend the `ContentEngine` class with additional generation methods:

```python
def generate_social_media_posts(self, trends: List[str]) -> List[str]:
    """Generate 5 social media posts from trends"""
    posts = []
    for trend in trends[:3]:
        posts.append(f"Quick take on {trend}: {self._generate_social_hook(trend)}")
    return posts[:5]
```

## Integration with Existing Skills

### Complementary Skills
- **content-digests**: Broader content aggregation
- **scheduled-reports**: Delivery patterns and multi-platform publishing
- **superpowers-brainstorming**: Ideation and creative exploration
- **linear**: Project management for content tasks

### Skill Dependencies
- **web_search**: Required for news aggregation
- **send_message**: Required for Discord/Slack/Telegram delivery
- **session_search**: Optional for conversation history analysis

## Best Practices

### For Optimal Results
1. **Customize topics_of_interest** to match your audience's interests
2. **Review and edit** generated content before publishing
3. **Add personal anecdotes** to make content more authentic
4. **Schedule delivery** for optimal engagement times
5. **Track performance** to refine future content

### Maintenance
- **Weekly review**: Check outputs and adjust topics as needed
- **Monthly audit**: Update search terms and hook templates
- **Quarterly refresh**: Add new content formats and remove underperforming ones

## Troubleshooting

### Common Issues
1. **No news results**: Check web_search configuration and API keys
2. **Empty trends**: Expand search terms or check news sources
3. **Delivery failures**: Verify send_message tool configuration
4. **Permission errors**: Ensure proper file permissions on outputs directory

### Debugging
Run the script with verbose output:
```bash
python3 ~/content_engine/founder_content_engine.py
```

Check logs in `~/content_engine/outputs/` for detailed error information.

## References

### Related Skills
- `/skill_view/content-digests` – Content aggregation patterns
- `/skill_view/scheduled-reports` – Delivery orchestration
- `/skill_view/superpowers-brainstorming` – Ideation techniques

### External Resources
- Linear Board: https://linear.app/hermiesvault
- HermesVault Kanban: `/opt/data/home/obsidian-vault/Vault/Kanban/`
- Obsidian Knowledge Base: `/opt/data/home/obsidian-vault/Vault/_wiki/`

## Version History
- **1.0.0** (2025-06-20): Initial release with full content generation capabilities
- **1.0.1** (2025-06-21): Added error handling and batch processing improvements
- **1.0.2** (2025-06-22): Enhanced trend analysis and content quality

## License
MIT – Feel free to adapt and use for personal or commercial purposes.