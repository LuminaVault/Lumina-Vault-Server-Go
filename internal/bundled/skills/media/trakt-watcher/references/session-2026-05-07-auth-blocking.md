# Trakt watcher session note: auth blocking + refresh rate limit

Date: 2026-05-07 UTC

Observed during cron execution:

- `~/.hermes/tokens/trakt.json` existed and contained access/refresh tokens.
- Direct `GET /sync/history` with the saved access token returned `403` with Cloudflare-style `error code: 1010` when using the script's default headers.
- Repeating the same request with a browser-like `User-Agent` changed the failure mode to `401`.
- Other authenticated Trakt endpoints (`/users/settings`, `/users/me`, `/sync/playback`) also returned `401` with the same token set.
- `POST /oauth/token` for refresh returned `429` with:
  `REFRESH_TOKEN_API_GET_LIMIT rate limit exceeded. Please wait 1411 seconds then retry your request.`

Interpretation:

- This was not a normal empty-history run.
- The environment had credentials on disk, but they were not usable at that moment.
- The failure path is a credential/provisioning / upstream-blocking issue, not a code-path logic bug.

Operational takeaway:

- When the watcher sees `401` plus a valid token file, treat it as a likely revoked/invalid token or Trakt anti-bot block and verify headers, token freshness, and account/app pairing.
- When refresh itself returns `429`, back off and retry later instead of hammering the refresh endpoint.
