# OpenRouter Classification Prompt — Exact Template

## Prompt Template (as used in `x_link_poller_v2.py`)

```python
prompt = f"""Classify this X/Twitter article into ONE of these topics:
AI, Dev/Swift, Stocks, Health, Tech, Business, News, XFeed

Article title: {title}
Article snippet: {content[:1000]}

Respond ONLY with the topic name. No punctuation."""
```

## Post-Prompt Format

The script sends this as a standard OpenAI-compatible chat completion to `https://openrouter.ai/api/v1/chat/completions`:

```json
{
  "model": "anthropic/claude-3-haiku",
  "messages": [
    {"role": "user", "content": "Classify this X/Twitter article..."}
  ],
  "temperature": 0.0,
  "max_tokens": 10
}
```

**Headers:**
```
Authorization: Bearer <OPENROUTER_API_KEY>
Content-Type: application/json
HTTP-Referer: https://hermes-agent.nousresearch.com   (or your app domain)
```

## Expected Response

Non-empty single token matching one of:
- `AI`
- `Dev/Swift`  ← Note: slash in name; treat as atomic token
- `Stocks`
- `Health`
- `Tech`
- `Business`
- `News`
- `XFeed`

**No period, comma, or explanation.** If the LLM returns `"AI."` or `"The topic is AI"`, strip non-token characters before matching.

## Fallback Path (No OPENROUTER_API_KEY)

When the API key is missing/empty, the script skips LLM classification and uses `TOPIC_KEYWORDS` instead. The topic is saved with `classification_method: "keywords"` in frontmatter.

## Topical Keyword Lists (Fallback Only)

```python
TOPIC_KEYWORDS = {
    "AI":        ["ai", "openai", "claude", "llm", "gpt", "anthropic", "hermes", "agent",
                  "openclaw", "model", "ml", "deepseek", "gemini", "openrouter"],
    "Dev/Swift": ["swift", "ios", "xcode", "apple", "uikit", "swiftui", "vapor",
                  "hummingbird", "appstore", "ipa", "macos", "visionos", "objective-c"],
    "Stocks":    ["stock", "ticker", "amd", "googl", "z", "zeta", "hims", "rdw", "smr",
                  "elf", "oust", "portfolio", "earnings", "buy", "sell", "market", "invest",
                  "cathie wood", "tesla", "nvda", "celh", "msft", "meta", "amzn"],
    "Health":    ["hims", "weight-loss", "glp-1", "telehealth", "biotech", "eli lilly",
                  "novo", "ozempic", "wegovy", "pfizer", "moderna", "abbvie"],
    "Tech":      ["google", "amazon", "microsoft", "startup", "saas", "tech", "api",
                  "cloud", "aws", "azure", "meta", "nvidia", "intel", "qualcomm"],
    "Business":  ["revenue", "profit", "startup", "funding", "acquihire", "ipo",
                  "valuation", "billion", "acquires", "merger"],
    "News":      ["breaking", "news", "report", "announcement", "update", "press release"],
}
```

## Model Selection

Default: `anthropic/claude-3-haiku` (fast + cost-effective). Alternatives: `anthropic/claude-3.5-haiku`, `openai/gpt-4o-mini`, `google/gemini-2.5-flash`.

Change via `OPENROUTER_MODEL` env var or config override in the script.

## Cost

At time of writing, Claude Haiku via OpenRouter is ~$0.0002/1K input tokens, effectively free for short article snippets (~500 tokens each). 1,000 classifications ≈ $0.10–$0.20.

## References

- OpenRouter API: https://openrouter.ai/docs/api-reference
- Model list: https://openrouter.ai/models
- Anthropic Claude messages API: https://docs.anthropic.com/en/api/messages
