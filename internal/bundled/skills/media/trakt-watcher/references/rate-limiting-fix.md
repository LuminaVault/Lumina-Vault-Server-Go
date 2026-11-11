# Rate Limiting Fix Implementation

This document describes the cooldown mechanism added to the Trakt Watcher script to handle extreme rate limits (Retry-After > 600 seconds).

## Problem

The original script used exponential backoff with a hard cap of 300 seconds per sleep. When the Trakt API returned a `Retry-After` header with a very large value (e.g., 2914 seconds), the script would:

1. Attempt to retry up to `max_retries` times
2. Sleep for capped 300-second intervals on each retry
3. Exhaust its retry budget before the rate limit window expired
4. Fail prematurely and exit with `[SILENT]`

This caused the watcher to stop checking status for the remainder of the rate limit window, potentially missing watching activity updates.

## Solution

When encountering a 429 response with `Retry-After` > 600 seconds, the script now enters a **cooldown period**:

1. Save `rate_limit_cooldown_until` timestamp in the state file (current time + Retry-After)
2. Immediately return `False` from the refresh attempt
3. On subsequent runs, check the cooldown timestamp before any API calls
4. If cooldown is active, output `[SILENT]` and skip the check
5. When cooldown expires, automatically clear the flag and resume normal operation

## Changes Made

### 1. Updated `load_state()` to initialize `rate_limit_cooldown_until`

Added `rate_limit_cooldown_until: null` to both the loaded and default state configurations.

### 2. Added cooldown check in `run()`

At the start of the `run()` method, before checking watching status:

```python
# Check if we are in rate limit cooldown
now = time.time()
cooldown_until = self.state.get('rate_limit_cooldown_until')
if cooldown_until and now < cooldown_until:
    print(f"In rate limit cooldown until {time.ctime(cooldown_until)}. Skipping check.", file=sys.stderr)
    return "[SILENT]"
elif cooldown_until:
    # Cooldown has expired, remove the flag
    print("Rate limit cooldown expired.", file=sys.stderr)
    self.state['rate_limit_cooldown_until'] = None
    self.save_state()
```

### 3. Modified `refresh_access_token()` to trigger cooldown

In the HTTP error handler for 429 responses, added a check for large Retry-After values:

```python
if response.status_code == 429:
    # Respect rate limiting
    retry_after = int(response.headers.get('Retry-After', 1))
    # If the retry_after is very large (e.g., > 600 seconds), enter cooldown to avoid repeated attempts
    if retry_after > 600:
        print(f"Encountered large rate limit (Retry-After: {retry_after} seconds). Entering cooldown.", file=sys.stderr)
        self.state['rate_limit_cooldown_until'] = time.time() + retry_after
        self.save_state()
        return False
    # ... existing backoff logic for smaller Retry-After values
```

## Benefits

- Prevents repeated failed refresh attempts that could prolong the rate limit
- Reduces unnecessary API load and avoids keeping the rate limit active
- Automatically resumes operation when the rate limit window expires
- Maintains cron job stability with minimal runtime during cooldown
- Provides clear debug output about cooldown status

## Threshold Choice

The 600-second threshold was chosen as a safe margin above typical maximum backoff (300 seconds) while still being reasonable for a long-running cooldown. The exact value can be adjusted if needed, but it should be significantly larger than the maximum expected backoff sleep to avoid triggering cooldown for normal rate limit scenarios.

## Verification

After the fix, the script correctly handles the scenario:
- Initial run: encounters 429 with Retry-After=2831 → enters cooldown, outputs `[SILENT]`
- Subsequent runs during cooldown: immediately outputs `[SILENT]` without API calls
- After cooldown expires: clears cooldown flag, attempts API check again