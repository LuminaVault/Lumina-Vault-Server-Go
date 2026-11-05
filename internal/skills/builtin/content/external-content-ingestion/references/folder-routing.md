# Folder Routing — Theme Detection Rules

Determines which `Raw/<Category>/` folder an ingested article should be saved to based on its content.

## Keyword Priority Map

Keywords are checked in order; first match wins.

| Order | Category | Trigger Keywords / Patterns | Example Matches |
|-------|----------|----------------------------|-----------------|
| 1 | `AI` | `\b(AI|LLM|Claude|GPT|OpenAI|Anthropic|agent|Hermes|OpenClaw|OpenCode|OpenAI|model\.ai|embedding|vector|RAG|fine-tuning|LLM|GPT-4|Claude 3|Sonnet)\b` | "Hermes agent uses Claude API" → AI |
| 2 | `Swift` | `\b(Swift|iOS|SwiftUI|Vapor|Hummingbird|Xcode|UIKit|Combine|async/await|Apple|Objective-C|SwiftData|SwiftConcurrency)\b` | "Swift 6 introduces new macros" → Swift |
| 3 | `Stocks` | `\b(stock|portfolio|earnings|bull|bear|price target|buy|sell|hold|ticker|shares|ETF|dividend)\b` OR explicit ticker pattern `\$[A-Z]{1,5}` | "ZETA up 5% today" → Stocks |
| 4 | `Health` | `\b(weight-loss|GLP-1|Ozempic|Wegovy|telehealth|clinical trial|FDA approval|pharma|biotech|drug|therapy)\b` | "HIMS expands GLP-1 prescribing" → Health |
| 5 | `Careers` | `\b(hiring|job|career|remote|interview|salary|resume|CV|position|apply now|join our team)\b` | "Swift engineer remote position" → Careers |
| 6 | `Business` | `\b(revenue|growth|market cap|earnings|startup|funding|Series [ABC]|IPO|M&A|acquisition|partnership)\b` | "Zeta Global Q1 earnings beat" → Business |
| 7 | `Tech` | `\b(Go|Python|Docker|Kubernetes|K8s|backend|API|REST|GraphQL|microservice|serverless|cloud|AWS|GCP|Azure)\b` | "Go 1.22 released with improvements" → Tech |
| 8 | `News` | `\b(breaking|update|announcement|today|just in|exclusive|report)\b` | "Breaking: major tech announcement" → News |
| 9 | `HermesPortfolio` | `\b(Hermes portfolio|allocation|position size|trim|buy the dip|concentration|threshold alert)\b` | "Hermes weekly review — concentration warnings" → HermesPortfolio |

## Default & Fallback

If no keywords match with confidence score ≥ 0.6, route to `Raw/` (root).

## Custom Overrides

Users can define custom mappings in environment variable `EXTERNAL_INGEST_FOLDER_MAP` as JSON:
```bash
export EXTERNAL_INGEST_FOLDER_MAP='{"d psychiatry": "Health", "vapor": "Swift"}'
```

Overrides are checked before the default keyword map.

## Multi-Category Handling

If content matches multiple categories with similar scores:
- Prefer the *most specific* (longest keyword match)
- If still ambiguous, choose the category with highest overall vault file count (reflects user's existing focus)

## Examples

| Article Title Snippet | Matched Keywords | Chosen Category |
|----------------------|------------------|-----------------|
| "Sam Altman — ChatGPT subscriptions now work with OpenClaw" | "ChatGPT", "OpenClaw", "Anthropic" | `AI` |
| "Swift 6.1 introduces strict concurrency checking" | "Swift", "concurrency" | `Swift` |
| "HIMS stock outlook after earnings" | "HIMS", "stock", "earnings" | `Stocks` (or `Health` if GLP-1 focus) |
| "Docker best practices for production" | "Docker", "production" | `Tech` |
| "Zeta Global revenue grows 50%" | "revenue", "Zeta" | `Business` |

Note: Stock ticker symbols ($TICKER) strongly indicate `Stocks` category.
