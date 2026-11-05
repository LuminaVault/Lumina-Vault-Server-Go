---
name: market-alert
description: Self-deployable market monitoring and alerting service with Discord/Telegram/Slack delivery
license: MIT
version: 1.0.0
author: Hermes Agent
metadata:
  tags: [market, alerts, monitoring, discord, telegram, slack, service]
  related_skills: [market-data-fetcher, alert-engine, multi-channel-delivery, service-deployment]
---

# Market Alert Agent

## Overview
A self-deployable service for continuous market monitoring and alerting. This service runs 24/7, monitors markets (stocks, crypto, news), detects significant movements, and delivers actionable alerts to Discord, Telegram, and Slack.

## Key Features
- **Continuous Monitoring**: Runs as a service with configurable intervals
- **Multi-Channel Delivery**: Discord, Telegram, Slack integration
- **Actionable Alerts**: Context-rich alerts with recommended actions
- **Persistent Storage**: Local storage of market data and generated alerts
- **Multiple Deployment Options**: Systemd, Docker, Direct Python

## When to Use This Skill
Use this skill when:
- Building a market monitoring system that needs to run continuously
- Creating a self-deployable service for alerts
- Needing multi-channel delivery (Discord, Telegram, Slack)
- Wanting a robust system with error handling and logging
- Requiring configurable thresholds and watchlists

## Core Components

### 1. Configuration Management
The service uses a JSON configuration file (`config.json`) that defines:
- Market monitoring settings (stocks, crypto, news)
- Thresholds for alerts (price change %, volume change %)
- Deployment channels (Discord, Telegram, Slack)
- Quiet hours and rate limiting

### 2. Service Architecture
The service consists of:
- **MarketAlertService** class - Main service loop
- **Data Fetcher** - Collects market data from web sources
- **Alert Engine** - Detects significant movements and generates alerts
- **Delivery System** - Sends alerts to configured channels

### 3. Deployment Options

#### Systemd Service (Linux)
Create a systemd service file for automatic startup and management:
```ini
[Unit]
Description=Market Alert Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/path/to/market-alerts
ExecStart=/usr/bin/python3 /path/to/market-alerts/market_alert_service.py
Restart=always
RestartSec=10
StandardOutput=syslog
StandardError=syslog
SyslogIdentifier=market-alert-service
KillSignal=SIGINT

[Install]
WantedBy=multi-user.target
```

#### Docker Deployment
Build a Docker image and run as a container:
```dockerfile
FROM python:3.11-slim
WORKDIR /app
COPY . /app
RUN pip install --no-cache-all hermes-agent
RUN mkdir -p /app/data /app/logs /app/alerts
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=30s --start-period=5s --retries=3 \\
    CMD python -c "import json, os; os.path.exists('data')" || exit 1
CMD ["python", "market_alert_service.py"]
```

#### Direct Python Service
Run directly with Python:
```bash
python market_alert_service.py
```

### 4. Configuration File Structure

```json
{
  "market_alert_agent": {
    "monitoring": {
      "stocks": {
        "enabled": true,
        "watchlist": ["AAPL", "TSLA", "MSFT", "NVDA", "META"],
        "thresholds": {"price_change_percent": 5.0}
      },
      "crypto": {
        "enabled": true,
        "watchlist": ["BTC", "ETH", "SOL", "DOGE"],
        "thresholds": {"price_change_percent": 10.0}
      },
      "news": {
        "enabled": true,
        "keywords": ["earnings", "IPO", "merger", "acquisition"]
      }
    },
    "deployment": {
      "telegram": false,
      "discord": ["#stock-alerts"],
      "slack": false
    }
  }
}
```

## Operating Rules

### 1. Service Lifecycle
- **Start**: Service starts automatically via systemd/cron/Docker
- **Run**: Continuous loop fetching data, generating alerts, delivering
- **Stop**: Graceful shutdown on interrupt signal
- **Restart**: Auto-restart on failure (systemd) or manual restart

### 2. Error Handling
- **Network Errors**: Retry with exponential backoff
- **Data Fetch Failures**: Log and continue with available data
- **Delivery Failures**: Queue and retry alerts
- **Configuration Errors**: Fall back to defaults

### 3. Alert Generation Logic
- **Price Spike**: Stock/crypto price moves beyond threshold
- **Volume Spike**: Unusual trading volume (requires volume change %)
- **Crypto Move**: Cryptocurrency price action
- **Breaking News**: Market-moving news detection

### 4. Multi-Channel Delivery
- **Discord**: Send to configured channels via webhook
- **Telegram**: Send to configured chat via bot API
- **Slack**: Send to configured webhook

## Implementation Steps

### Step 1: Set Up Directory Structure
```bash
mkdir -p ~/hermes/market-alerts/{{data,logs,alerts,scripts}}
```

### Step 2: Create Configuration File
Create `config.json` with monitoring and deployment settings.

### Step 3: Create Service Script
Implement the `MarketAlertService` class with:
- `__init__()` - Initialize service
- `load_config()` - Load configuration
- `fetch_stock_data()` - Fetch stock prices
- `fetch_crypto_data()` - Fetch crypto prices
- `fetch_news()` - Fetch market news
- `check_stock_alerts()` - Detect stock alerts
- `check_crypto_alerts()` - Detect crypto alerts
- `check_news_alerts()` - Detect news alerts
- `generate_alert_message()` - Format alert messages
- `send_alerts()` - Deliver to configured channels
- `process_cycle()` - Single processing cycle
- `run()` - Continuous service loop

### Step 4: Create Deployment Scripts
- `market_alert_service.py` - Main service script
- `market-alert-service.service` - Systemd service file
- `Dockerfile` - Containerized deployment
- `docker-compose.yml` - Orchestration

### Step 5: Test the Service
```bash
python market_alert_service.py  # Test run
python test_service.py          # Comprehensive test
```

### Step 6: Deploy
Choose deployment method:
- **Systemd**: Copy service file, enable, start
- **Docker**: Build, run container
- **Direct**: Run as background process

## Common Pitfalls and Solutions

### Pitfall 1: Sandbox String Limitations
The Hermes sandbox has strict string length limitations. When creating files in the sandbox:
- Break content into smaller chunks
- Avoid special characters (emojis) in Python strings
- Use multiple write operations

### Pitfall 2: Import Errors
The sandbox environment may not have all expected modules. Use available tools directly:
```python
# Instead of: from hermes_tools import web_search
# Use: web_search() directly (top-level function)
```

### Pitfall 3: Service Not Starting
Check:
- Python is installed: `python3 --version`
- Dependencies are installed: `pip list | grep hermes`
- Configuration file is valid JSON
- Log files for errors: `tail -f logs/service.log`

### Pitfall 4: Alerts Not Delivering
- Verify channel configurations in `config.json`
- Check network connectivity
- Test `send_message` tool separately
- Verify webhook URLs are correct

### Pitfall 5: Data Fetch Failures
- Check `web_search` tool availability
- Verify API access if using specific sources
- Add retry logic with exponential backoff

## Testing and Validation

### Unit Tests
Create test scripts to verify each component:
```python
# test_fetcher.py - Test data fetching
# test_alert_engine.py - Test alert detection
# test_service.py - End-to-end service test
```

### Integration Tests
```bash
# Run a single cycle
python market_alert_service.py --test-cycle

# Check data files created
ls -la data/

# Check alerts generated
ls -la alerts/
```

### Health Checks
```bash
# Check service status
systemctl status market-alert-service  # Systemd
docker ps                               # Docker
tail -f logs/service.log                # Direct
```

## Monitoring and Maintenance

### Log Management
- **Service Logs**: `~/hermes/market-alerts/logs/service.log`
- **Data Files**: `~/hermes/market-alerts/data/`
- **Alerts**: `~/hermes/market-alerts/alerts/`

### Performance Metrics
- **Uptime**: Check service status
- **Alerts Generated**: Count files in alerts directory
- **Data Freshness**: Check timestamps in data files

### Backup Strategy
- Regular backup of `data/` and `alerts/` directories
- Version control for configuration files
- Database dumps if using SQLite (not used here)

## Security Considerations

### 1. Never Expose to Internet
The service should run locally or in a private network. Do not expose APIs without authentication.

### 2. Use Environment Variables for Sensitive Data
Store API keys, webhook URLs, and other sensitive data in environment variables, not in config files.

### 3. Implement Rate Limiting
Add rate limiting to avoid API bans from data sources.

### 4. Regular Updates
Keep dependencies updated for security patches:
```bash
pip list --outdated
pip install --upgrade hermes-agent
```

### 5. Monitor Logs
Regularly check logs for suspicious activity or errors.

## Troubleshooting Guide

### Service Fails to Start
1. Check Python version: `python3 --version`
2. Verify dependencies: `pip list | grep hermes`
3. Check configuration file syntax: `python -m json.tool config.json`
4. Review logs: `tail -50 logs/service.log`

### No Alerts Being Generated
1. Check thresholds aren't too high
2. Verify market data is being fetched
3. Check web_search tool availability
4. Review alert logic in `market_alert_service.py`

### Alerts Not Delivering
1. Verify channel configurations in `config.json`
2. Check network connectivity
3. Test `send_message` tool separately
4. Verify webhook URLs are correct

### Data Fetch Failures
1. Check `web_search` tool availability
2. Verify API access if using specific sources
3. Add retry logic with exponential backoff

## Extending the Service

### Add New Alert Types
1. Create a new check function in `market_alert_service.py`
2. Add it to the main alert generation
3. Create a message formatter in `generate_alert_message()`

### Integrate New Data Sources
1. Add a data fetcher function
2. Update the main fetch function to include it
3. Add configuration in `config.json`

### Change Alert Format
Modify the `generate_alert_message()` function to change the output style.

## Related Skills
- **market-data-fetcher**: Deep dive into market data collection
- **alert-engine**: Advanced alert detection algorithms
- **multi-channel-delivery**: Patterns for multi-platform delivery
- **service-deployment**: Patterns for deploying Python services

## References
- [Market Microstructure](https://en.wikipedia.org/wiki/Market_microstructure)
- [Technical Analysis](https://en.wikipedia.org/wiki/Technical_analysis)
- [Behavioral Finance](https://en.wikipedia.org/wiki/Behavioral_finance)
- [Risk Management](https://en.wikipedia.org/wiki/Risk_management)