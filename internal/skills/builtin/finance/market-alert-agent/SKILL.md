---
name: market-alert-agent
category: finance
description: Continuous market monitoring service that fetches real-time data, generates alerts, and delivers them via multiple channels.
triggers:
  - market data fetching
  - alert generation
  - daemon/service management
  - deployment (systemd/Docker)
related_skills:
  - scheduled-reports
  - stock-trading
---

# Market Alert Agent

## Overview

The Market Alert Agent is a self-contained service that continuously monitors financial markets (stocks and cryptocurrencies) and generates alerts based on configurable thresholds. It can be deployed as a systemd service or Docker container.

## Key Features

- **Real-time monitoring**: Fetches market data at regular intervals
- **Configurable alerts**: Price changes, volume spikes, crypto movements
- **Multi-channel delivery**: Discord, Telegram, Slack via webhooks
- **Hybrid data fetching**: Supports mock data for sandbox, real Yahoo Finance data via yfinance in production
- **Deployment flexibility**: systemd service or Docker container

## Architecture

The service consists of:
1. **Main daemon** (`market_alert_service.py`) - The main loop that runs continuously
2. **Data fetcher** (`market_data_fetcher.py`) - Fetches stock and crypto data
3. **Configuration** (`config.json`) - Watchlists, thresholds, and delivery settings
4. **Deployment artifacts** - systemd service file, Dockerfile, docker-compose.yml

## Data Fetching Strategies

### Hybrid Approach
The service supports both mock data (for sandbox/development) and real data from Yahoo Finance via the `yfinance` library.

**Environment Variable**: `USE_YFINANCE`
- If set to `true` and `yfinance` is available, uses real Yahoo Finance data
- If not set or `false`, uses mock data (for sandbox/testing)

### Mock Data
Generates realistic random market data for demonstration and testing.

### Real Yahoo Finance Data
Uses the `yfinance` Python library to fetch real-time stock and crypto data.

## Alert Types

- **Price Spike**: Significant price movements (configurable threshold, e.g., 5%)
- **Volume Spike**: Unusual trading volume (configurable threshold, e.g., 100%)
- **Crypto Move**: Cryptocurrency price action (configurable threshold, e.g., 10%)
- **Breaking News**: Market-moving news events (requires news API integration)

## Configuration

Edit `config.json` to customize:

```json
{
  "watchlists": {
    "stocks": ["AAPL", "TSLA", "MSFT", "NVDA", "META"],
    "crypto": ["BTC", "ETH", "SOL", "DOGE"]
  },
  "thresholds": {
    "price_change_percent": 5.0,
    "volume_change_percent": 100.0,
    "crypto_change_percent": 10.0
  },
  "delivery_channels": {
    "discord": [
      {
        "name": "stock-alerts",
        "webhook_url": "https://discord.com/api/webhooks/..."
      }
    ]
  }
}
```

## Deployment

### Systemd Service (Linux)

1. Copy service file:
```bash
sudo cp market-alert-service.service /etc/systemd/system/
```

2. Edit service file to set environment variables (optional):
```ini
Environment="USE_YFINANCE=true"
Environment="DISCORD_WEBHOOK=https://discord.com/api/webhooks/..."
```

3. Reload and start:
```bash
sudo systemctl daemon-reload
sudo systemctl enable market-alert-service
sudo systemctl start market-alert-service
```

### Docker Deployment

```bash
# Build image
docker build -t market-alert-service .

# Run container
docker run -d \
  --name market-alert-service \
  -e USE_YFINANCE=true \
  -v $(pwd)/config.json:/app/config.json \
  -v $(pwd)/logs:/app/logs \
  -v $(pwd)/alerts:/app/alerts \
  market-alert-service
```

## Logging and Monitoring

- Logs are written to `logs/market_alert.log`
- Alerts are saved to `alerts/` directory
- Systemd service status: `systemctl status market-alert-service`

## Troubleshooting

| Symptom | Likely Cause | Fix |
| :--- | :--- | :--- |
| Service fails to start | Missing dependencies | Install `yfinance` and `requests` |
| No alerts generated | Mock mode enabled | Set `USE_YFINANCE=true` for real data |
| Delivery failures | Invalid webhook URLs | Verify webhook URLs in config |
| Rate limiting from Yahoo | Too many requests | Add delays between requests |

## References

- yfinance documentation: https://pypi.org/project/yfinance/
- Yahoo Finance API: https://pypi.org/project/yfinance/
- systemd service management: https://www.freedesktop.org/software/systemd/man/systemd.service.html
- Docker deployment: https://docs.docker.com/engine/reference/commandline/run/
- Hermes cron framework: https://hermes-agent.nousresearch.com/docs