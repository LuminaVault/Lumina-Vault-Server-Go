---
name: Market Alert Service
description: A self-deployable service for continuous market monitoring and alerting with Discord/Telegram/Slack delivery
version: 1.0
tags: [market, trading, alerts, discord, telegram, slack, service, docker, systemd]
---

# Market Alert Service

A self-deployable service for continuous market monitoring and alerting that can push to Discord, Telegram, and Slack. This skill covers the complete architecture, implementation, and deployment of a robust market alert system.

## Overview

The Market Alert Service is a continuously running daemon that:
- Monitors stock, crypto, and news markets
- Detects significant price movements and market-moving events
- Generates actionable alerts with context
- Delivers alerts to configured messaging channels
- Persists all data and alerts for auditing

## Architecture

### Core Components:
1. **MarketAlertService** - Main service class that runs in a loop
2. **MarketDataFetcher** - Modular data fetching component (can use real APIs or mock data)
3. **Configuration** - JSON-based configuration for watchlists, thresholds, and deployment
4. **Alert Engine** - Detects significant movements and generates formatted messages
5. **Delivery System** - Sends alerts to Discord, Telegram, Slack

### Deployment Options:
- **Systemd Service** - Native Linux service management
- **Docker Container** - Portable containerized deployment
- **Direct Python** - Simple execution for testing

## Implementation Details

### Service Core (`service_core.py`)
- Uses a continuous loop with configurable interval
- Implements proper error handling and logging
- Persists data and alerts to JSON files
- Supports graceful shutdown

### Data Fetching
- **Real APIs**: Yahoo Finance, CoinGecko, etc.
- **Mock Data**: For sandbox/development environments
- **Rate Limiting**: Built-in delays to avoid API bans
- **Symbol Mapping**: Crypto symbols mapped to exchange IDs

### Alert Generation
- **Price Spike Alerts**: Significant price movements (>X%)
- **Crypto Move Alerts**: Cryptocurrency volatility detection
- **News Alerts**: Market-moving news detection
- **Formatted Messages**: Rich embeds with emojis, context, and action items

### Delivery System
- **Discord**: Webhook integration
- **Telegram**: Bot API integration  
- **Slack**: Incoming webhook integration

## Best Practices

### Sandbox Considerations
- When running in a restricted sandbox, use mock data for demonstration
- Real API calls may be blocked or rate-limited
- Use User-Agent headers to mimic browsers
- Implement proper error handling for API failures

### Production Deployment
1. **Replace Mock with Real APIs**: Implement actual data fetching
2. **Set Up Webhooks**: Configure Discord/Telegram/Slack integrations
3. **Add Monitoring**: Set up health checks and logging
4. **Implement Backups**: Persist data safely
5. **Add Security**: Use environment variables for API keys

### Performance
- Use batch processing for multiple symbols
- Implement exponential backoff for retries
- Cache frequently accessed data
- Monitor API rate limits

## Common Pitfalls

### ❌ DON'T use complex shell quoting in f-strings
**DO** use subprocess with argument lists or separate the command building logic.

### ❌ DON'T make synchronous API calls without rate limiting
**DO** add delays between requests to avoid being blocked.

### ❌ DON'T hardcode API keys or webhooks
**DO** use configuration files or environment variables.

### ❌ DON'T ignore error handling
**DO** implement comprehensive error handling and logging.

### ❌ DON'T run without persistence
**DO** save all data and alerts to durable storage.

## Code Structure

```
market-alerts/
├── service_core.py          # Main service class
├── market_data_fetcher.py   # Data fetching module
├── config.json             # Configuration
├── market-alert-service.service  # Systemd service file
├── Dockerfile              # Container build
├── docker-compose.yml      # Orchestration
├── README.md               # Documentation
└── logs/                  # Service logs
```

## Usage

### Testing:
```bash
python3 market_alert_service.py
```

### Deployment:
```bash
# Systemd
sudo cp market-alert-service.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable market-alert-service
sudo systemctl start market-alert-service

# Docker
docker build -t market-alert-service .
docker run -d --name market-alert-service market-alert-service
```

### Configuration:
Edit `config.json` to customize:
- Stock/crypto watchlists
- Price change thresholds
- News keywords
- Messaging channel integrations

## References

- Yahoo Finance API documentation
- CoinGecko API documentation
- Discord webhook API
- Telegram Bot API
- Slack incoming webhooks

## Related Skills
- **market-alert**: Conceptual framework for market alert systems
- **stock-trading**: Portfolio management and price alerts
- **scheduled-reports**: Automated report generation