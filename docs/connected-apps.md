# Connected apps: operator notes

Connected apps let a user's Lumina act as an MCP client of their own other
first-party apps (Norviq, Aaru, Khepri, Loci). Quick Assistant turns and
`/v1/mcp` agent keys can then call those apps' read-only tools, named
`<app>__<tool>` (for example `aaru__read_library`). v1 is read-only: only tools
on each app's allowlist in `internal/connectedapps/registry.go` are exposed.

## Configuration

All keys are read in `internal/config/config.go`. Go only; Swift never had them.

| Key | Default | Meaning |
| --- | --- | --- |
| `CONNECTED_APPS_ENABLED` | `false` | Master switch. Read with `strings.EqualFold(v, "true")`. |
| `CONNECTED_APPS_NORVIQ_MCP_URL` | `https://mcp.norviq.org/mcp` | Norviq MCP endpoint (access token pasted by the user). |
| `CONNECTED_APPS_AARU_MCP_URL` | `https://api.aaruapp.com/v1/mcp` | Aaru MCP endpoint (OAuth). |
| `CONNECTED_APPS_KHEPRI_MCP_URL` | `https://www.kheprios.com/mcp` | Khepri MCP endpoint (OAuth). |
| `CONNECTED_APPS_LOCI_MCP_URL` | `https://api.lociai.fyi/mcp` | Loci MCP endpoint (API key pasted by the user). |

Related keys that connected apps depend on:

| Key | Default | Why it matters |
| --- | --- | --- |
| `LV_SECRET_MASTER_KEY` | empty | secretbox master key. Tokens are only stored sealed. With the flag on but this key missing or unusable, connected apps are NOT mounted (a warning is logged; no error). |
| `MCP_PUBLIC_BASE_URL` | empty | Base of the OAuth callback URL (see below). |
| `INGESTION_PUBLIC_BASE_URL` | empty | Fallback base when `MCP_PUBLIC_BASE_URL` is empty. If both are empty, OAuth connects answer 503 (pasted-token connects still work). |
| `CORS_ALLOWED_ORIGINS` | see config | Allowlist for a web `returnTo` after the OAuth callback. |

URLs come from config only. A user-supplied URL is never accepted, every
outbound call goes through the SSRF guard (HTTPS required, checked at the URL
and again at dial time), and redirects are refused.

### `CONNECTED_APPS_ENABLED=false` (the default)

With the flag off: no `/v1/me/connected-apps*` routes and no OAuth callback
route are mounted, no `<app>__*` tools reach Quick Assistant or `/v1/mcp`, and
the per-app grants field is absent from the agent-connection DTO. Nothing
about connected apps surfaces to clients.

## OAuth callback URL

Registered automatically by Dynamic Client Registration (RFC 7591) on the first
OAuth connect of an app (Aaru and Khepri use OAuth; Norviq and Loci use a
pasted token and never register a client):

    <MCP_PUBLIC_BASE_URL, else INGESTION_PUBLIC_BASE_URL>/v1/connected-apps/oauth/callback

(trailing slash on the base is trimmed; see `connectedAppsCallbackURL` in
`cmd/App/connectedapps.go`). The registration is `client_name` "LuminaVault",
a public client (`token_endpoint_auth_method: none`), `authorization_code` plus
`refresh_token`. If an app's operator wants to allowlist redirect URIs by hand,
this is the exact string.

**Runbook: the DCR registration is cached per app.** The resulting `client_id`
is stored in `connected_app_clients` (one row per app) and reused forever; the
callback URL is NOT re-registered. If you change the public base URL (or the
callback path), the cached client still carries the old redirect URI and the
app will reject the authorize request. Clear the cache so the next connect
registers anew:

    DELETE FROM connected_app_clients WHERE app_id = 'aaru';   -- or 'khepri', or all rows

Existing users' refresh tokens were issued to the old client; after clearing,
those users will move to `needs_reauth` on the next refresh and must reconnect.

Clearing the cache also makes later disconnects skip the upstream revoke: a
disconnect revokes with the cached client only and never registers a new one,
so with no row there the refresh token is left to expire at the app. The local
row is still deleted.

**OAuth state lives in memory.** The `state` of a connect in flight (10 minute
lifetime) and the parked code waiting for `.../complete` (60 seconds) are held
in the pod, not the database. A pod restart drops every in-flight connect (the
user sees the error status and starts again), and a callback or completion
that lands on another pod would not find its state. The service must
therefore stay at **one replica** (staging and prod are both `replicaCount: 1`).
Move the state to the database before scaling out.

## Revoking

- **User:** `DELETE /v1/me/connected-apps/:app` (authenticated). For an OAuth
  app it first makes a best-effort RFC 7009 revocation of the refresh token at
  the app's authorization server (5 s limit; skipped when the app advertises no
  revocation endpoint or no client is cached), then deletes the local account
  row, then drops the cached tool list. Revoke and delete run under the same
  per-user, per-app lock as a token refresh, and a refresh only ever updates an
  existing row, so a refresh in flight cannot bring a deleted row back. The
  delete has its own 5 s limit and still runs if the client goes away. A failed
  upstream revoke is logged (kind only) and does not block the local delete. Pasted-token apps (Norviq, Loci) have no upstream
  revoke: the user must also delete the token in that app. A failed local
  delete answers 500.
- **Operator:** delete the user's row directly, which is equivalent to the
  local half of Disconnect:

      DELETE FROM connected_app_accounts WHERE user_id = '<user uuid>' AND app_id = '<app>';

  To cut one agent key's access without touching the user's connection, clear
  that key's grant (`agent_connections.connected_app_grants`) or revoke the
  key. Upstream revocation is the user's or the app operator's job.
- Deleting a user cascades (`ON DELETE CASCADE`).

## Deploy order

Migrations `00010_connected_apps.sql` and `00011` (nullable origin column on
pending actions) are expand-only. They run as the ArgoCD PreSync goose job
(`platform/infra/charts/app/templates/migrate-job.yaml`, enabled by
`migrate.enabled` in `apps/api/values-common.yaml`), with `backoffLimit: 0`: a
failed migration leaves the old ReplicaSet serving.

- **Both migrations must succeed before the new image serves, flag on or off.**
  Every `lv_` agent key lookup selects `agent_connections.connected_app_grants`
  (added by 00010), even with the flag off. If the column is missing, all
  agent-key auth breaks. Every pending-action insert, claim and cancel names
  the origin column (added by 00011), also with the flag off. If it is missing,
  every Quick Assistant confirm card breaks.
- **Rolling the image back is safe.** The columns and tables are additive and
  the old image ignores them.
- **Do NOT run goose Down on 00010 while the new image is running.** It drops
  `connected_app_grants`, which the new image selects on every key lookup, and
  drops the tables holding users' sealed tokens. Roll the image back first.
- Merging this repo does not deploy; the infra repo's `main` does.

## Privacy model

- **Key = user id** (`auth.Identity`), never the vault id or `X-Vault-ID`.
- **Tokens are sealed per user** with secretbox (`Seal/Open` bound to the user
  id), never logged, never returned. Logs record only
  `(app, tool, ms, ok, error kind)`: no user, no arguments, no results. Calls
  are not metered.
- **Anthropic only** (R21, R22, R24, R25). A turn that offers app tools runs on
  Anthropic's own endpoint, pinned to the trial model. Free-lane users, and
  users whose Anthropic route points at a tenant custom base URL, get the turn
  without app tools. If the pinned Anthropic call fails before any tool ran,
  the turn reruns on its original route with no app tools. App data never
  reaches another provider.
- **Flagged history is never replayed** (R27). User and assistant rows of a
  turn that ran an app tool carry `origin = 'connected_app'`; every path that
  builds a model prompt skips them. Follow-up questions therefore do not see
  earlier app answers (accepted v1 UX loss).
- **Writes after app data** (R28, R30). Once an app answered in a turn, Run-class
  write tools (memory_save, app_reminder_create, space_create, space_update,
  file_move, ...) are refused with fixed text. **v1 limitation: connected-app
  data cannot be saved to memory, reminders or spaces.** Confirm-class writes
  (for example calendar_create) are still allowed, because the user's tap is an
  explicit ask.
- **A confirmed memory update still embeds** (R35, owner-accepted 2026-11-19).
  `memory_update` is Confirm-class, so after app data it runs once the user
  taps confirm, and its text goes to the embedding provider, which is not
  Anthropic. The tap counts as consent, as in R30. This is the only path by
  which app-derived text can reach a non-Anthropic provider, and only text the
  user chose to save.
- **Searches after app data** (R31). Once an app answered in a turn, `search`
  and `memory_context` are refused with fixed text for the rest of the turn. A
  search embeds the query the model wrote, the embedding provider is not
  Anthropic, and by then the query can quote the app. Searches made before the
  app answered stand; `browse`, `read` and `recent` embed nothing and still
  run.
- **Tool load is bounded** (R32). A Quick Assistant turn waits at most 4 s for
  the user's app tools; past that, or on any error, the turn runs without them
  (kind logged only). An app whose tool list failed to load is not asked again
  for 30 s for that user; calls are not held back by this.
- **Confirm lines are flagged** (R29): a pending action carries the proposing
  turn's origin, so its confirm line is flagged too.
- **`/v1/mcp` grants default off.** An agent key sees `<app>__*` tools only
  when the user connected the app AND the key has that app's grant (Settings,
  Agent connections). The grant is re-checked on every call.
- A static test (`internal/connectedapps/privacy_test.go`) fails if the admin,
  posthog, telemetry or llm packages reference `connected_app_accounts` or
  `connectedapps.Store`.

## OAuth completion is bound to the signed-in user (R19)

The OAuth callback is unauthenticated, because the iOS web-auth session cannot
carry the user's JWT and cross-site web cookies are unreliable. Before the fix,
the callback finished the flow from the `state` alone. An attacker could start
a connect on their own Lumina account and send the authorize link to a victim.
If the victim approved, the victim's app tokens bound to the attacker's
account.

The flow now has two halves (owner decision 2026-11-19: fix rather than
accept):

1. **Callback** (`GET /v1/connected-apps/oauth/callback`, public). It takes the
   state, parks the code in memory for 60 seconds, and redirects with
   `status=authorized&completion=<handle>`. The handle is a fresh 32-byte
   random value, used once. The callback makes no outbound request and stores
   no token.
2. **Complete** (`POST /v1/me/connected-apps/:app/complete`, authenticated,
   body `{"completion": "<handle>"}`). The handle is spent on arrival. The
   server exchanges the code and saves the tokens only when the session's user
   started the flow, and answers 204. Other outcomes:
   - Another account: **403**. The code is never exchanged and nothing is
     stored.
   - Unknown, used, expired or another app's handle: **400**.
   - Token exchange failed: **502**.

The attacker holds the state but never sees the handle: it goes to the
victim's browser. The victim holds the handle but the wrong session. Under
attack the victim's tokens are never minted.

Aaru and Khepri codes live 60 seconds, so the client must post `complete` as
soon as the redirect arrives. iOS: the `luminavault://connected-apps/<app>`
deep link. Web: the allowed `returnTo` page.

**Residual risk:** the handle is a bearer value for the account that started
the flow. If it leaks from the victim's device within 60 seconds (for example
from browser history, or a referer from the `returnTo` page) to the attacker
who started the flow, the attacker can still complete. Whoever starts the
flow picks the `returnTo` path, so every page on an allowed web origin must
strip `completion` from the URL after reading it, not just the settings page.

**Client contract** (not built yet): an `authorized` status means "post the
handle to complete". `error` means the flow failed or was declined. Add both
to the OpenAPI spec and the client plan.

## Staging verification checklist

Run after the owner deploys with `CONNECTED_APPS_ENABLED=true` on staging only.
Record request IDs for each step.

1. Connect your own Aaru with OAuth: `POST /v1/me/connected-apps/aaru/connect`,
   open the returned URL and approve. Then, within 60 s, post the redirect's
   `completion` value to `POST /v1/me/connected-apps/aaru/complete` with the
   same account (204). Repeat with a second account's token: it must answer 403
   and connect nobody.
2. Ask Quick Assistant "what am I watching?" and get a live answer.
3. `DELETE /v1/me/connected-apps/aaru`, then ask again: there must be no Aaru
   tool.
4. A second staging account lists `aaru` as `disconnected`.
5. (R19, defence in depth, no longer a gate) Check that the Aaru and Khepri
   consent screens prompt for the Lumina DCR client instead of auto-approving.
   With completion bound to the user, an auto-approve can no longer bind a
   victim's tokens to another account. A consent screen still stops a user's
   app being authorized without them seeing it.
6. Confirm `00010` and `00011` ran in the PreSync job before the pod served.
