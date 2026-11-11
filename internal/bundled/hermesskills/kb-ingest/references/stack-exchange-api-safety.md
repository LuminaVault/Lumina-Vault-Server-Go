# Safe Stack Exchange API Usage

When using the Stack Exchange API (including Stack Overflow), be aware of security considerations around command pipelines and JSON parsing.

## Security Warning: Avoid Pipe-to-Interpreter

The following pattern is **dangerous** and should be avoided:

```bash
# DANGEROUS - never do this
curl -s "https://api.stackexchange.com/2.3/questions?tagged=swift&site=stackoverflow" | python3 -m json.tool
```

**Why it's dangerous**: 
- The `curl` command downloads arbitrary content from the internet
- Piping directly to `python3` means the content is executed without inspection
- A malicious API response could contain harmful code that gets executed
- This pattern bypasses security safeguards and can lead to code injection

## Safer Alternatives

### Option 1: Save to File First

```bash
# Save API response to a file
curl -s -o response.json "https://api.stackexchange.com/2.3/questions?tagged=swift&site=stackoverflow&filter=withbody"

# Then safely parse the file
python3 -c "
import json
with open('response.json', 'r') as f:
    data = json.load(f)
for item in data.get('items', [])[:5]:
    print(f\"{item['title']}\\n{item['link']}\\n\")
"
```

### Option 2: Use jq for JSON Processing

```bash
# Use jq (a safe JSON processor) to extract specific fields
curl -s "https://api.stackexchange.com/2.3/questions?tagged=swift&site=stackoverflow" | \
  jq -r '.items[] | "\(.title) \(.link)"' | head -10
```

jq processes JSON safely without executing arbitrary code.

### Option 3: Inline Python with Complete Parsing

```bash
python3 -c "
import sys, json, urllib.request

url = 'https://api.stackexchange.com/2.3/questions?tagged=swift&site=stackoverflow&filter=withbody'
data = json.load(urllib.request.urlopen(url))

for item in data.get('items', [])[:5]:
    print(f\"{item['title']}\\n{item['link']}\\n\")
"
```

This approach fetches the URL within Python, avoiding shell pipelines entirely.

## Recommended Pattern for kb-ingest

For knowledge base ingestion tasks involving APIs, use the file-based approach:

```bash
# 1. Fetch and save
curl -s -o api_response.json "API_ENDPOINT"

# 2. Process safely (example with Python)
python3 -c "
import json, sys
with open('api_response.json', 'r') as f:
    data = json.load(f)
# Process data and output markdown
"

# 3. Clean up if needed
rm api_response.json
```

This pattern ensures:
- Content is inspected before execution
- No arbitrary code execution via pipes
- Easier debugging and error handling
- Better reproducibility
"

## Stack Exchange API Specifics

When using the Stack Exchange API:
- Always include your API key if you have one (higher rate limits)
- Use appropriate filters (`filter=withbody` for full content)
- Handle pagination if you need more than 30 items
- Respect rate limits (default is 300 requests per day without key)

Example with API key:
```bash
curl -s "https://api.stackexchange.com/2.3/questions/featured?site=stackoverflow&filter=withbody&key=YOUR_API_KEY"
```