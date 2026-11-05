---
name: market-data-fetching
title: Market Data Fetching for Hermes Agent
description: Strategies for fetching market data in sandbox environments with rate limiting and authentication challenges
---

# Market Data Fetching for Hermes Agent

## Overview
Fetching real-time market data within the Hermes sandbox environment presents unique challenges including rate limiting, authentication requirements, and bot detection mechanisms. This skill covers strategies for overcoming these limitations and implementing robust market data integration.

## Key Challenges in Sandbox Environment

### Rate Limiting
External APIs (Yahoo Finance, CoinGecko) enforce strict rate limits that are easily triggered during development and testing.

### Authentication Requirements
Many financial data APIs require API keys or OAuth tokens, which may not be readily available in the sandbox.

### Bot Detection & Consent Walls
Browser-based data fetching is often blocked by consent management platforms and bot detection systems.

## Strategies for Sandbox Development

### 1. Mock Data First Approach
**When to use:** Early development, testing alerting logic, demonstration purposes
**How:**
- Create mock data generators that simulate realistic market movements
- Use historical data or random walks with realistic volatility
- Structure code to easily swap mock with real API calls

**Benefits:**
- Avoids rate limiting and authentication issues
- Enables rapid development of business logic
- Provides immediate feedback on alerting thresholds

### 2. Staged API Integration
**When to use:** When real data is required for testing
**Approach:**
- Start with free APIs that have generous rate limits (e.g., Alpha Vantage free tier)
- Implement proper error handling and retry logic
- Use caching to minimize API calls
- Add realistic delays between requests

### 3. Browser Automation Alternative
**When to use:** When APIs are unavailable or require complex authentication
**Approach:**
- Use browser automation tools with proxy support
- Handle consent management cookies
- Implement headless browsing with realistic user agents
- Parse HTML content for data extraction

## Production Deployment Patterns

### 1. Environment-Based Configuration
```python
# Use environment variables for API keys
API_KEY = os.getenv("YAHOO_FINANCE_API_KEY")
if not API_KEY:
    # Fallback to mock data in development
    API_KEY = os.getenv("MOCK_DATA", "true") == "true"
```

### 2. Feature Flags
```python
# Enable real data fetching only in production
USE_REAL_DATA = os.getenv("USE_REAL_DATA", "false").lower() == "true"
```

### 3. Rate Limit Handling
```python
import time
from functools import wraps

def rate_limited(max_per_second):
    min_interval = 1.0 / max_per_second
    def decorate(func):
        last_called = [0]
        @wraps(func)
        def wrapper(*args, **kwargs):
            elapsed = time.time() - last_called[0]
            left_to_wait = min_interval - elapsed
            if left_to_wait > 0:
                time.sleep(left_to_wait)
            ret = func(*args, **kwargs)
            last_called[0] = time.time()
            return ret
        return wrapper
    return decorate

@rate_limited(0.5)  # 2 requests per second
def fetch_stock_data(symbol):
    # API call implementation
    pass
```

## Common Pitfalls & Solutions

### Pitfall 1: 429 Too Many Requests Errors
**Solution:** Implement exponential backoff with jitter:
```python
import random
import math

def backoff_retry(retries, base_delay=1, max_delay=60):
    delay = min(base_delay * (2 ** retries) + random.uniform(0, 0.1), max_delay)
    time.sleep(delay)
    return delay
```

### Pitfall 2: Missing Environment Variables
**Solution:** Use configuration files with fallback to mock data:
```python
config = {
    "api_key": os.getenv("API_KEY", "mock_key"),
    "use_mock": os.getenv("USE_MOCK", "true").lower() == "true"
}
```

### Pitfall 3: Script Execution in Sandbox
**Solution:** When running standalone scripts, use the Hermes cron job delivery mechanism:
- Place scripts in `~/.hermes/scripts/`
- Use `no_agent: true` for simple output delivery
- Capture stdout and deliver via Hermes

## References
- [Yahoo Finance API unofficial documentation](https://github.com/ranaroussi/ydata-python)
- [CoinGecko API documentation](https://www.coingecko.com/en/api)
- [Alpha Vantage API documentation](https://www.alphavantage.co/documentation)

## Related Skills
- `market-alert` - For market alerting logic
- `linear` - For Linear board integrations