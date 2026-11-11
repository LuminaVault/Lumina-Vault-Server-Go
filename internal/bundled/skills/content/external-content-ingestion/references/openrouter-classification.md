# OpenRouter LLM Classification

OpenRouter API used for robust topic classification of X/Twitter articles when keyword matching is ambiguous.

## API Details

**Endpoint:** `POST https://openrouter.ai/api/v1/chat/completions`

**Authentication:** Header `Authorization: Bearer <OPENROUTER_API_KEY>`

**Headers:**
```
Content-Type: application/json
HTTP-Referer: https://hermes-agent.nousresearch.com
X-Title: Hermes X-Link Poller
```

**Request body:**
```json
{
  "model": "anthropic/claude-3-haiku",
  "messages": [
    {"role": "user", "content": "Classify this X/Twitter article into ONE of these topics:\nAI, Dev/Swift, Stocks, Health, Tech, Business, News, XFeed\n\nArticle title: <title>\nArticle snippet: <content[:1000]>\n\nRespond ONLY with the topic name. No punctuation."}
  ],
  "max_tokens": 10
}
```

**Response parsing:**
```python
result = json.loads(resp.read().decode())
prediction = result["choices"][0]["message"]["content"].strip()
```
Normalize: `prediction.lower().strip()` then match against allowed topic list via case-insensitive equality.

## Default Model

`anthropic/claude-3-haiku` — fast (~300ms), inexpensive (~$0.0001/req), sufficiently accurate for single-label classification.

**Alternatives** (change via `OPENROUTER_MODEL` env var):
- `anthropic/claude-3-opus` — higher accuracy, slower, pricier
- `openai/gpt-4.1-mini` — balanced
- `mistralai/mistral-large` — strong multi-lingual
- `google/gemini-2.5-flash` — Google's fast model

## Retry & Backoff

Implement 3-attempt retry with exponential backoff:
```python
import time
for attempt in range(3):
    try:
        # ... make request ...
        return validated_topic
    except Exception as e:
        if attempt < 2:
            time.sleep(2 ** attempt)  # 1s, then 2s
        else:
            log.error("LLM classification failed after 3 attempts")
            return None
```

Backoff mitigates transient 504s (gateway timeout), 429s (rate limit), and network hiccups.

## Fallback Chain

1. Try LLM → validate response → return if valid
2. LLM fails (network, timeout, invalid response) → retry up to 3×
3. All retries exhausted → fall back to keyword matching
4. Keyword matching also fails → `XFeed`

## Cost Estimation

At ~$0.0001/request and ~100 articles/month expected, monthly LLM cost is pennies (< $0.01). Even with 3× retry overhead, negligible.

## Pitfalls

- **504 Gateway Timeout:** OpenRouter under heavy load; backoff+retry solves.
- **429 Rate Limit:** Not expected at low volume; if hit, add longer backoff (5s, 10s) or switch model.
- **Unexpected response format:** Sometimes LLM adds punctuation or lowercase. Always normalize via case-insensitive equality against known topics.
- **OpenRouter key missing:** Script should check `if not OPENROUTER_API_KEY:` and skip LLM silently; fall back to keywords.

## Environment Variables

| Variable | Required? | Default | Description |
|----------|-----------|---------|-------------|
| `OPENROUTER_API_KEY` | No (optional) | — | Enables LLM classification |
| `OPENROUTER_MODEL` | No | `anthropic/claude-3-haiku` | Model ID to use |
