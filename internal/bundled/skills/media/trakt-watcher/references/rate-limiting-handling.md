# Rate Limiting Handling for Trakt Watcher

When the Trakt API returns a 429 (Too Many Requests) error during token refresh, the current script fails immediately. This reference provides a robust solution using exponential backoff and respecting the `Retry-After` header.

## Problem

The OAuth token refresh endpoint may return 429 with a `Retry-After` header indicating how many seconds to wait before retrying (often around 1695 seconds). The script currently does not handle this and will fail, outputting `[SILENT]`.

## Solution

Implement exponential backoff with jitter and respect the `Retry-After` header. This prevents hammering the API and handles temporary rate limits gracefully.

## Implementation

Modify the `refresh_access_token` method in `trakt_watcher.py` to include retry logic:

```python
import random
import time

def refresh_access_token(self, max_retries=5):
    \"\"\"Refresh the OAuth access token with exponential backoff and Retry-After support\"\"\"
    if not self.config['trakt_client_secret'] or not self.config['trakt_refresh_token']:
        print(\"ERROR: trakt_client_secret is not configured. Cannot refresh access token.\", file=sys.stderr)
        return False

    for attempt in range(max_retries):
        try:
            data = {
                'grant_type': 'refresh_token',
                'refresh_token': self.config['trakt_refresh_token'],
                'client_id': self.config['trakt_client_id'],
                'client_secret': self.config['trakt_client_secret']
            }
            
            response = requests.post('https://trakt.tv/oauth/token', json=data, timeout=10)
            response.raise_for_status()
            
            tokens = response.json()
            self.config['trakt_access_token'] = tokens.get('access_token')
            self.config['trakt_refresh_token'] = tokens.get('refresh_token')
            
            # Save tokens to file
            token_data = {
                'access_token': self.config['trakt_access_token'],
                'refresh_token': self.config['trakt_refresh_token']
            }
            os.makedirs(os.path.dirname(TOKEN_FILE), exist_ok=True)
            with open(TOKEN_FILE, 'w') as f:
                json.dump(token_data, f)
            
            # Update headers
            self.headers['Authorization'] = f'Bearer {self.config["trakt_access_token"]}'
            return True
            
        except requests.exceptions.HTTPError as e:
            if response.status_code == 429:
                # Respect rate limiting
                retry_after = int(response.headers.get('Retry-After', 1))
                if attempt < max_retries - 1:
                    # Exponential backoff with jitter
                    backoff = min(retry_after * (2 ** attempt) + random.uniform(0, 1), 300)
                    print(f\"Rate limited. Retrying in {backoff:.2f} seconds (attempt {attempt + 1}/{max_retries})\", file=sys.stderr)
                    time.sleep(backoff)
                else:
                    print(f\"Max retries ({max_retries}) exceeded for rate limiting\", file=sys.stderr)
                    return False
            else:
                print(f\"HTTP error: {e}\", file=sys.stderr)
                return False
        except requests.exceptions.RequestException as e:
            print(f\"Network error: {e}\", file=sys.stderr)
            # For network errors, use exponential backoff
            if attempt < max_retries - 1:
                backoff = min(1 * (2 ** attempt) + random.uniform(0, 1), 300)
                print(f\"Network error, retrying in {backoff:.2f} seconds (attempt {attempt + 1}/{max_retries})\", file=sys.stderr)
                time.sleep(backoff)
            else:
                print(f\"Max retries ({max_retries}) exceeded for network errors\", file=sys.stderr)
                return False
```

## Integration Steps

1. Replace the existing `refresh_access_token` method in `trakt_watcher.py` with the implementation above.
2. Ensure `import random` and `import time` are at the top of the file (they likely already exist).
3. Test the script manually to verify the retry behavior.
4. Update the pitfalls section to reflect the improved handling.

## Benefits

- Prevents immediate failure on rate limits
- Automatically retries with appropriate backoff
- Respects API rate limits via `Retry-After` header
- Reduces need for manual intervention
- More reliable cron job execution

## Notes

- The `max_retries` parameter controls how many times to retry before giving up (default: 5).
- The backoff calculation includes jitter to avoid synchronized retries.
- A maximum backoff of 300 seconds prevents excessively long waits.
- Network errors also use exponential backoff for robustness.