# Trakt watcher session note: refresh hit rate limit, not empty history

Date: 2026-05-08 UTC

Observed during manual refresh probing:

- The Trakt OAuth refresh endpoint returned `HTTP 429`.
- Payload: `REFRESH_TOKEN_API_GET_LIMIT rate limit exceeded. Please wait 3173 seconds then retry your request.`
- The same token pair was tested with both JSON and `application/x-www-form-urlencoded` request bodies.
- Both request shapes produced the same 429 response.

Operational takeaway:

- Treat this as a cooldown, not as a history/missing-output problem.
- Do not keep hammering the refresh endpoint once the limit is hit.
- When the watcher surfaces this condition, it should remain silent after recording cooldown state and retry only after the wait window expires.
