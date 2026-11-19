# Connected Apps (server) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Lumina user connects their own Norviq / Aaru / Khepri / Loci accounts. Quick Assistant and Lumina's `/v1/mcp` can then call those apps' read-only MCP tools live, as that user only.

**Architecture:**
- A new domain package `internal/connectedapps` holds the pieces: a first-party app registry, token storage sealed per user, OAuth (discovery + DCR + PKCE), and a Streamable-HTTP MCP client.
- A thin HTTP module `internal/modules/connectedapps` serves the connect/disconnect routes.
- Quick Assistant gets per-turn extra tools and an Anthropic-only pin.
- `/v1/mcp` lists and proxies connected-app tools behind a new per-app grant on agent keys.
- Chat marks connected-app turns with `origin='connected_app'`, so they are never replayed to Hermes.

**Tech Stack:** Go, Echo, pgx/v5 (raw SQL, like the `mcp` and `chat` modules), goose migrations, `internal/secretbox`, `internal/ssrfguard`, testcontainers Postgres (`internal/dbtest`).

**Spec:** `/Volumes/PortableSSD/Work/production/apps/lumina/docs/superpowers/specs/2026-11-18-connected-apps-design.md` (Track 1)

## Global Constraints
- Work on a branch: `git switch -c feat/connected-apps`. Repo root: `LuminaVaultServerV2`. Go is production; Swift is archived.
- **Key = user id**, from `auth.Identity(c)`. Never the vault id or `X-Vault-ID`. `secretbox.Seal/Open(…, userID)`.
- **Read-only v1.** Expose only tool names on each app's allowlist in the registry. Drop everything else, even if the server lists it.
- **First-party only.** Endpoints come from the registry/config. A user-supplied URL is never accepted. Call `ssrfguard.Guard{RequireHTTPS: true}.Validate` before every outbound request. Tests inject a guard that allows the httptest loopback.
- **Anthropic only** for any Quick Assistant turn that offers connected-app tools. There is no fallback. If the turn's decision has no Anthropic route, the turn runs **without** connected-app tools. If the Anthropic call fails mid-turn, the turn ends with `failedLead`; it never switches provider. Haiku trap: no `temperature`/`top_p`/`top_k`/prefill (`platform/infra/docs/llm-haiku-trial.md`).
- **Never log** tokens, tool arguments or tool results. Metering and logs record only `(user, app, tool, latency_ms, ok)`.
- **Hermes replay:** user and assistant rows of a turn that ran a connected-app tool get `origin = 'connected_app'`. `historyMessages` skips them.
- **Feature flag:** `CONNECTED_APPS_ENABLED` (default `"false"`, read with `strings.EqualFold(…, "true")`). When it is off, no routes, no tools and no grants surface.
- **Go-only config keys** go into the `known` allowlist in `internal/config/config_test.go:41`, with a comment.
- **Tests:** stub servers only (`httptest`). No real app tokens or LLM keys. `go test` needs Docker for `dbtest.Postgres`.
- Tool name format: `<appID>__<toolName>`, for example `aaru__read_library`. `appID` matches `^[a-z]+$`.

## Review Focus
1. **Expired access token mid-chat.** It should refresh once, single-flight, and retry. If refresh fails, mark the account `needs_reauth` and tell the model "Reconnect <App> in Settings". Pinned in Task 4 (`TestCallRefreshesOnce`, `TestCallMarksNeedsReauth`) and Task 5 (`TestCallRetriesOnceOn401`).
2. **App MCP server down or slow.** The call should time out at 15 s and return a model-readable error. The turn still answers. Pinned in Task 3 (`TestClientTimeout`).
3. **Huge tool result.** Truncate at 64 KiB with a marker. Never OOM, never send more to the model. Pinned in Task 3 (`TestClientCapsResponse`).
4. **The user disconnects while another tab's turn is in flight.** The next call must fail as not connected, never use a cached token. Pinned in Task 5 (`TestDisconnectInvalidatesCache`).
5. **The app server adds a new write tool.** It must not appear in Lumina. Pinned in Task 5 (`TestToolsDropUnlistedNames`).

---

### Task 1: Migration, config keys, registry

**Files:**
- Create: `internal/migrations/sql/00003_connected_apps.sql`
- Modify: `internal/config/config.go` (new fields), `internal/config/config_test.go:41` (`known` allowlist)
- Create: `internal/connectedapps/registry.go`, `internal/connectedapps/registry_test.go`

**Interfaces:**
- Produces:
  ```go
  type AuthMode string // "oauth" | "pat"
  type App struct {
      ID, Name, Blurb string   // Blurb: one line on what Lumina can read
      MCPURL   string          // Streamable HTTP endpoint
      Auth     AuthMode
      PATPrefix string         // "nvq_pat_" for Norviq; "" otherwise
      ReadTools []string       // allowlist of the app's own tool names
  }
  type Registry struct{ /* ordered apps */ }
  func NewRegistry(cfg config.Config) Registry
  func (r Registry) All() []App
  func (r Registry) Get(id string) (App, bool)
  ```
- Migration tables (consumed by Task 2 store, Task 4 DCR client cache and Task 9 grants):
  - `connected_app_accounts(id uuid pk, user_id uuid not null references users(id) on delete cascade, app_id text not null, access_ciphertext bytea, access_nonce bytea, refresh_ciphertext bytea, refresh_nonce bytea, access_expires_at timestamptz, scope text, status text not null default 'connected', last_failure_code text, created_at, updated_at, unique(user_id, app_id))`
  - `connected_app_clients(app_id text pk, client_id text not null, created_at timestamptz not null default now())`
  - `ALTER TABLE agent_connections ADD COLUMN IF NOT EXISTS connected_app_grants text[] NOT NULL DEFAULT '{}'`

- [ ] **Step 1: Write the migration** with the header from `00002_assistant_pending_actions.sql`: `-- go-only: connected apps (spec 2026-11-18)`, `-- +goose Up` … `-- +goose Down` (drop both tables, drop the column). Do not touch `schema-swift.sql`.
- [ ] **Step 2: Add config fields**
  - `ConnectedAppsEnabled string env:"CONNECTED_APPS_ENABLED" envDefault:"false"`
  - `ConnectedAppsNorviqMCPURL env:"CONNECTED_APPS_NORVIQ_MCP_URL" envDefault:"https://mcp.norviq.org/mcp"`
  - `…_AARU_MCP_URL`, `…_KHEPRI_MCP_URL`, `…_LOCI_MCP_URL`. Their defaults are each app's public MCP URL, found in that app's repo: `aaru/aaru-server-v2/internal/oauth/service.go:94` (issuer + `/v1/mcp`), `north/north-client/cmd/mcp-server`, `Loci/loci-connect-server/cmd/loci-mcp`.
  - Add all five names to `known`.
- [ ] **Step 3: Write the failing tests** in `registry_test.go`:
  ```go
  func TestRegistryOrderAndLookup(t *testing.T) {
      r := NewRegistry(config.Config{ConnectedAppsAaruMCPURL: "https://a.test/v1/mcp"})
      ids := []string{}
      for _, a := range r.All() { ids = append(ids, a.ID) }
      require.Equal(t, []string{"norviq", "aaru", "khepri", "loci"}, ids)
      a, ok := r.Get("aaru"); require.True(t, ok)
      require.Equal(t, "https://a.test/v1/mcp", a.MCPURL)
      require.Equal(t, AuthMode("oauth"), a.Auth)
      require.ElementsMatch(t, []string{"read_library", "list_airs", "search_titles"}, a.ReadTools)
      _, ok = r.Get("evil"); require.False(t, ok)
  }
  func TestRegistryNorviqIsPAT(t *testing.T) { /* Auth=="pat", PATPrefix=="nvq_pat_" */ }
  ```
- [ ] **Step 4: Run** `go test ./internal/connectedapps/ -run TestRegistry -v`. Expected: FAIL (undefined).
- [ ] **Step 5: Implement `registry.go`.**
  - The `ReadTools` lists come from each app's MCP catalog. Copy only non-write tools:
    - Aaru: `read_library`, `list_airs`, `search_titles` (`aaru/aaru-server-v2/internal/mcp/catalog.go`).
    - Norviq: `norviq/norviq-mcp/internal`; tools whose names start with `get_`/`list_`/`search_`, after checking each handler only reads.
    - Khepri: `north/north-client/internal/mcpserver`.
    - Loci: `Loci/loci-connect-server/internal/mcp`.
  - Loci `Auth`: read `loci-connect-server` auth middleware. Use `oauth` if it serves `/.well-known/oauth-protected-resource`, else `pat` with its token prefix.
- [ ] **Step 6: Run** `go test ./internal/connectedapps/ ./internal/config/ ./internal/migrations/ -v`. Expected: PASS. `TestConfigEnvNamesComeFromSwiftKeys` must stay green.
- [ ] **Step 7: Commit** `feat(connectedapps): registry, migration, config keys`

### Task 2: Token store, sealed per user

**Files:**
- Create: `internal/connectedapps/store.go`, `internal/connectedapps/store_test.go`

**Interfaces:**
- Consumes: `secretbox.Box`, the migration from Task 1.
- Produces:
  ```go
  type Account struct {
      AppID, Status string            // "connected" | "needs_reauth"
      AccessToken, RefreshToken string // plaintext, only in memory
      ExpiresAt *time.Time; Scope *string; ConnectedAt time.Time
  }
  type Store struct{ pool *pgxpool.Pool; box *secretbox.Box; now func() time.Time }
  func NewStore(pool *pgxpool.Pool, box *secretbox.Box) *Store
  func (s *Store) Save(ctx context.Context, user uuid.UUID, a Account) error    // upsert on (user_id, app_id)
  func (s *Store) Get(ctx context.Context, user uuid.UUID, appID string) (Account, error) // ErrNotConnected
  func (s *Store) List(ctx context.Context, user uuid.UUID) ([]Account, error) // tokens blank in results
  func (s *Store) Delete(ctx context.Context, user uuid.UUID, appID string) error
  func (s *Store) MarkNeedsReauth(ctx context.Context, user uuid.UUID, appID, code string) error
  var ErrNotConnected = errors.New("connectedapps: not connected")
  ```

- [ ] **Step 1: Write the failing tests** (`dbtest.Postgres`, `migrations.Up`, seed two users like `mcp/module_test.go:59`, `secretbox.New(calendar.TestMasterKey)`):
  - `TestStoreRoundTrip`: Save, then Get returns the same tokens.
  - `TestStoreIsolatesUsers`: alice saves `aaru`. `Get(bob,"aaru")` gives `ErrNotConnected`. `List(bob)` is empty.
  - `TestSealedRowMovedToOtherUserFailsOpen`: `UPDATE connected_app_accounts SET user_id=$bob`, then `Get(bob,"aaru")` gives an `errors.Is(err, secretbox.ErrDecryptionFailed)` error.
  - `TestListNeverReturnsTokens`: each `Account` from `List` has empty `AccessToken` and `RefreshToken`.
  - `TestDeleteRemovesRow`.
- [ ] **Step 2: Run** `go test ./internal/connectedapps/ -run TestStore -v`. Expected: FAIL.
- [ ] **Step 3: Implement `store.go`.** Raw pgx SQL. Seal/open the way `calendar/tokens.go:49-97` does. A nil refresh token stores NULLs.
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(connectedapps): per-user sealed token store`

### Task 3: MCP client

**Files:**
- Create: `internal/connectedapps/client.go`, `internal/connectedapps/client_test.go`

**Interfaces:**
- Produces:
  ```go
  type RemoteTool struct{ Name, Description string; InputSchema json.RawMessage }
  type Validator interface{ Validate(ctx context.Context, raw string) (*url.URL, error) } // ssrfguard.Guard satisfies
  type Client struct{ HTTP *http.Client; Guard Validator; Timeout time.Duration; MaxBytes int64 }
  func (c *Client) ListTools(ctx context.Context, endpoint, bearer string) ([]RemoteTool, error)
  func (c *Client) CallTool(ctx context.Context, endpoint, bearer, name string, args map[string]any) (string, error)
  var ErrUnauthorized = errors.New("connectedapps: unauthorized") // 401 from app
  const DefaultTimeout = 15 * time.Second; const DefaultMaxBytes = 64 << 10
  ```
- [ ] **Step 1: Write the failing tests** against an `httptest.Server` that speaks JSON-RPC 2.0 MCP, protocol `"2025-06-18"`:
  - `TestClientInitializesThenLists`: the first POST is `initialize`, then `notifications/initialized`, then `tools/list`. `Mcp-Session-Id` from the initialize response is echoed on later requests. The `Authorization: Bearer tok` header is present.
  - `TestClientParsesSSEResponse`: the server answers `tools/call` with `Content-Type: text/event-stream` and one `data: {...}` event. The result text is concatenated from `content[].text`.
  - `TestClientCallReturnsToolErrorText`: `isError: true` returns the text, not a Go error.
  - `TestClient401IsErrUnauthorized`.
  - `TestClientTimeout`: a handler that sleeps longer than `Timeout` produces an error containing `"timed out"`.
  - `TestClientCapsResponse`: a 1 MiB body produces a result of at most `MaxBytes` ending in `"\n[truncated]"`.
  - `TestClientRejectsGuardFailure`: the guard returns an error, and no request reaches the server.
- [ ] **Step 2: Run** `go test ./internal/connectedapps/ -run TestClient -v`. Expected: FAIL.
- [ ] **Step 3: Implement `client.go`.**
  - Call `Guard.Validate(endpoint)` before every POST.
  - Use `io.LimitReader(MaxBytes+1)`.
  - Branch on the response Content-Type: `application/json` vs `text/event-stream` (read the first `data:` event that carries a matching `id`).
  - No logging.
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(connectedapps): streamable-http MCP client`

### Task 4: OAuth (discovery, DCR, PKCE, refresh)

**Files:**
- Create: `internal/connectedapps/oauth.go`, `internal/connectedapps/oauth_test.go`

**Interfaces:**
- Consumes: `Registry`, `Store`, `Client.HTTP`/`Guard`.
- Produces:
  ```go
  type OAuth struct{ /* registry, store, pool, http, guard, sessions, callbackURL string, now */ }
  func NewOAuth(pool *pgxpool.Pool, reg Registry, store *Store, httpc *http.Client, guard Validator, callbackURL string) *OAuth
  func (o *OAuth) Start(ctx context.Context, user uuid.UUID, appID string, returnTo *string) (authorizeURL string, err error)
  func (o *OAuth) Finish(ctx context.Context, state, code string) (appID string, returnTo *string, err error)
  func (o *OAuth) Fresh(ctx context.Context, user uuid.UUID, appID string) (accessToken string, err error) // refresh if <120s left, single-flight per (user,app)
  func (o *OAuth) SavePAT(ctx context.Context, user uuid.UUID, appID, token string) error // checks App.PATPrefix
  var ErrBadState, ErrNotOAuth, ErrBadPAT error
  ```
- [ ] **Step 1: Write the failing tests** with a stub authorization server plus a resource server on one `httptest.Server`:
  - `TestStartDiscoversRegistersAndBuildsURL`:
    - Discovery is `GET <mcp origin>/.well-known/oauth-protected-resource<path>`, then `authorization_servers[0]` + `/.well-known/oauth-authorization-server`.
    - DCR is `POST registration_endpoint` with `token_endpoint_auth_method:"none"` and `redirect_uris:[callbackURL]`.
    - The URL has `response_type=code`, `code_challenge_method=S256`, `state`, and `resource=<MCPURL>`.
    - A second `Start` reuses `connected_app_clients` and makes no second registration.
  - `TestFinishExchangesAndSeals`: after `Finish`, `Store.Get(user,app)` has the access and refresh tokens. The token POST carried `code_verifier`.
  - `TestFinishRejectsUnknownOrReusedState`: `ErrBadState`. A state is one-shot with a 600 s TTL, like `calendar/oauth.go` `sessionStore`.
  - `TestCallRefreshesOnce`: the token expires in 10 s, and 5 concurrent `Fresh` calls produce exactly 1 refresh POST.
  - `TestCallMarksNeedsReauth`: the refresh returns 400 `invalid_grant`, so `Fresh` returns `ErrNotConnected` and the row status is `needs_reauth`.
  - `TestSavePATValidatesPrefix`: `"nvq_pat_x"` is OK for norviq, `"abc"` gives `ErrBadPAT`, and an OAuth app gives `ErrNotOAuth`.
- [ ] **Step 2: Run** `go test ./internal/connectedapps/ -run 'TestStart|TestFinish|TestCall|TestSavePAT' -v`. Expected: FAIL.
- [ ] **Step 3: Implement `oauth.go`.**
  - State is `ids.Upper(uuid.New())+"."+ids.Upper(uuid.New())`, as in `calendar/oauth.go:275`.
  - The PKCE verifier is 32 random bytes, base64url-encoded.
  - Sessions live in memory, in the same shape as calendar's `sessionStore`, with a `user` field.
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(connectedapps): oauth discovery, DCR, PKCE, refresh`

### Task 5: Service (tools for a user, call as a user)

**Files:**
- Create: `internal/connectedapps/service.go`, `internal/connectedapps/service_test.go`

**Interfaces:**
- Consumes: `Registry`, `Store`, `OAuth`, `Client`.
- Produces:
  ```go
  type AppTool struct{ App App; Name string /* "<app>__<tool>" */; Remote RemoteTool }
  type Service struct{ /* reg, store, oauth, client, cache (user,app)->[]RemoteTool TTL 5m, log *slog.Logger */ }
  func NewService(reg Registry, store *Store, oauth *OAuth, client *Client, log *slog.Logger) *Service
  func (s *Service) Tools(ctx context.Context, user uuid.UUID, onlyApps []string /* nil = all connected */) ([]AppTool, error)
  func (s *Service) Call(ctx context.Context, user uuid.UUID, name string, args map[string]any) (string, error)
  func (s *Service) Forget(user uuid.UUID, appID string) // drop cache; called on disconnect
  func SplitName(name string) (appID, tool string, ok bool)
  ```
- [ ] **Step 1: Write the failing tests** (Postgres plus stub MCP servers per app; the guard allows loopback):
  - `TestToolsOnlyConnectedApps`: alice connected aaru only, so only `aaru__*` tools come back. Bob gets nothing.
  - `TestToolsDropUnlistedNames`: the stub lists `read_library` and `apply_library_patch`. Only `aaru__read_library` comes back.
  - `TestCallUsesCallersToken`: the stub asserts `Authorization: Bearer alice-token`. Bob calling `aaru__read_library` gets `ErrNotConnected`, and the stub sees zero requests.
  - `TestCallRejectsUnlistedTool`: `aaru__apply_library_patch` gives an error, with zero requests.
  - `TestDisconnectInvalidatesCache`: `Tools`, then `Store.Delete`, then `Forget`, then `Call` gives `ErrNotConnected`.
  - `TestCallLogsNoSecrets`: build the service with `slog.New(slog.NewJSONHandler(&buf,nil))`. After `Call`, `buf` does not contain the token, the args value or the result text, but does contain `"app":"aaru"`.
  - `TestCallRetriesOnceOn401`: the first call gets 401, then `Fresh` forces a refresh, then the retry succeeds. A second 401 means `MarkNeedsReauth` and returns `"Reconnect Aaru in Lumina Settings."`.
- [ ] **Step 2: Run** `go test ./internal/connectedapps/ -run 'TestTools|TestCall|TestDisconnect' -v`. Expected: FAIL.
- [ ] **Step 3: Implement `service.go`.**
  - PAT apps use the stored access token directly. OAuth apps use `oauth.Fresh`.
  - Log one line per call: `slog.Info("connected_app_call","app",…,"tool",…,"ms",…,"ok",…)`.
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit** `feat(connectedapps): per-user tool listing and calls`

### Task 6: HTTP module and wiring

**Files:**
- Create: `internal/modules/connectedapps/module.go`, `internal/modules/connectedapps/module_test.go`
- Create: `cmd/App/connectedapps.go` (`func connectedAppsModule(pool, cfg, …) (httpserver.Module, *connectedapps.Service)`)
- Modify: `cmd/App/main.go` around line 387 (append the module when it is enabled, and keep the `*connectedapps.Service` on `agent` for Tasks 8–9). Also `modules.yaml` / `scripts/gen-modules.py` if CI checks the module manifest.

**Interfaces:**
- Consumes: `Service`, `OAuth`, `Store`, `Registry`; `auth.Identity`.
- Produces routes, all JWT except the callback:
  - `GET /v1/me/connected-apps` returns `[{id,name,blurb,auth,status:"connected"|"needs_reauth"|"disconnected",connectedAt?}]`
  - `POST /v1/me/connected-apps/:app/connect` with body `{returnTo?}` (oauth) returns `{authorizeUrl}`; with `{token}` (pat) returns 204
  - `DELETE /v1/me/connected-apps/:app` returns 204 and calls `Service.Forget`. If the app's authorization-server metadata has a `revocation_endpoint`, it also POSTs the refresh token there; a failure there is best-effort and still returns 204.
  - `GET /v1/connected-apps/oauth/callback?state&code` (unauthenticated) returns a 302 to `returnTo`, or to `luminavault://connected-apps/<app>?status=connected|error`
  - `returnTo` is validated like calendar's `webReturn` allowlist.

- [ ] **Step 1: Write the failing tests** (the `setup`/`v.do`/`v.jwt` rig copied from `mcp/module_test.go:59-138`):
  - `TestListShowsRegistryWithStatus`.
  - `TestConnectPATThenList`: the status becomes `connected`.
  - `TestUserCannotSeeOrDeleteOthersConnection`: alice connects. Bob's GET shows `disconnected`. Bob's DELETE of `aaru` returns 204 but alice's row still exists.
  - `TestUnknownAppIs404`.
  - `TestCallbackBadStateRedirectsError`.
  - `TestDisabledFlagRegistersNothing`: with `CONNECTED_APPS_ENABLED=false`, the routes return 404.
- [ ] **Step 2: Run** `go test ./internal/modules/connectedapps/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement the module and wiring.** Use `secretbox.New(cfg.LVSecretMasterKey)` as `calendarModules` does (`main.go:660`). The callback URL is `cfg.MCPPublicBaseURL` (falling back to `IngestionPublicBaseURL`) + `/v1/connected-apps/oauth/callback`.
- [ ] **Step 4: Run** `go test ./internal/modules/connectedapps/ ./cmd/App/ -v`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(connectedapps): settings API and wiring`

### Task 7: LLM pin and Quick Assistant per-turn tools

**Files:**
- Modify: `internal/llm/decided.go` (next to `Removing`, line 34)
- Modify: `internal/modules/quickassistant/loop.go` (`Answer` at 97, `rejection` at 371, `payload` at 450, `Reply` at ~76, ranPersonalTool at 279)
- Test: `internal/llm/decided_test.go`, `internal/modules/quickassistant/loop_test.go` (reuse `scriptedTransport` at :45)

**Interfaces:**
- Produces:
  ```go
  // llm
  func (d Decision) OnlyProvider(kind ProviderKind) (Decision, bool) // first route of kind among Primary+Fallbacks becomes Primary, Fallbacks=nil; false if none
  // quickassistant
  type TurnOptions struct {
      ExtraTools   []Tool      // Personal=true, Writes=false; names "<app>__<tool>"
      ExtraRunner  ToolRunner  // runs only ExtraTools names
      AnthropicOnly bool       // set iff len(ExtraTools)>0
  }
  func (s *Service) AnswerWith(/* Answer's existing params */, opts TurnOptions) (Reply, error) // Answer delegates with TurnOptions{}
  // Reply gains: UsedConnectedApp bool
  ```
- [ ] **Step 1: Write the failing tests:**
  - `TestOnlyProviderPicksAnthropicDropsFallbacks`: Primary openrouter, Fallbacks [anthropic/haiku, groq] produce Primary anthropic/haiku with nil Fallbacks. `ok=false` when there is no anthropic route.
  - `TestExtraToolsOfferedAndRun`: scripted step calls `aaru__read_library`. `ExtraRunner` gets the call. `Reply.UsedConnectedApp` and `Reply.RanPersonalTool` are true. `Indexable()` is false.
  - `TestExtraToolsForceAnthropic`: the scripted transport records each `ChatCompletionsDecided` decision. All have Primary.Provider `llm.Anthropic` and no Fallbacks.
  - `TestExtraToolsDroppedWhenNoAnthropicRoute`: a free-lane decision without anthropic means the payload's tools contain no `aaru__` name, `ExtraRunner` is never called, and the answer is still returned.
  - `TestAnthropicFailureDoesNotFallOver`: the Anthropic step errors, so the reply text starts with `failedLead` and no other provider is called.
  - `TestUnknownDoubleUnderscoreToolRefused`: the model calls `norviq__x`, which is not in ExtraTools, so the model content is `refusal`.
- [ ] **Step 2: Run** `go test ./internal/llm/ ./internal/modules/quickassistant/ -run 'OnlyProvider|ExtraTools|AnthropicFailure|DoubleUnderscore' -v`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - In `AnswerWith`, after the existing `Removing` calls, apply `OnlyProvider(llm.Anthropic)` when `AnthropicOnly` is set. If it returns false, clear `ExtraTools`.
  - `payload` appends `OpenAITools(opts.ExtraTools)`.
  - `rejection` treats any name in the extra set as `Run`.
  - Calls to extra names go to `ExtraRunner`; all others go to `Runner`.
  - Set `ranPersonalTool` and `usedConnectedApp` on any extra call.
- [ ] **Step 4: Run** the same command plus `go test ./internal/modules/quickassistant/...`. Expected: PASS, with existing tests unchanged.
- [ ] **Step 5: Commit** `feat(quickassistant): per-turn connected-app tools, anthropic pin`

### Task 8: Chat wiring, origin flag, Hermes history filter

**Files:**
- Modify: `internal/modules/chat/quick.go` (around 58–67: build the runner and `answerQuick`)
- Modify: `internal/modules/chat/stream.go` (`insertMessage` at 129 writes `origin`; `persistTurn` at 561; `historyMessages` at 121; user row insert at 263)
- Modify: `internal/modules/chat/local.go:124,148` (same filter)
- Modify: `chat.Options` (add `ConnectedApps interface{ Tools(ctx, user, onlyApps []string) ([]connectedapps.AppTool, error); Call(ctx, user, name string, args map[string]any) (string, error) }`) and `cmd/App/chat.go` wiring
- Test: `internal/modules/chat/quick_test.go` (reuse `scriptedAdapter` at :81, `fixedRouter` at :166, `haikuRoute`)

**Interfaces:**
- Consumes: `connectedapps.Service` (Task 5), `quickassistant.TurnOptions` (Task 7).
- Produces: `const OriginConnectedApp = "connected_app"`.

- [ ] **Step 1: Write the failing tests:**
  - `TestQuickTurnWithConnectedAppFlagsBothRows`: after a QA turn that calls `aaru__read_library`, both the user and assistant rows of that turn have `origin='connected_app'`.
  - `TestHermesHistoryExcludesConnectedAppTurns`: the next classic turn's captured hermesGateway adapter payload contains none of the flagged contents but does contain earlier unflagged ones.
  - `TestQuickTurnWithoutConnectionsOffersNoAppTools`.
  - `TestSharedVaultMemberGetsNoOwnersTools`: alice owns a team vault and is connected to aaru. Bob, a member, runs a QA turn with `X-Vault-ID` set to that vault. The payload has no `aaru__` tools and the aaru stub sees 0 requests.
  - `TestConnectedAppTurnNeverReachesNonAnthropicAdapter`: with `fixedRouter` primary groq and fallback haiku, the groq adapter gets 0 calls and the anthropic adapter ≥ 1.
- [ ] **Step 2: Run** `go test ./internal/modules/chat/ -run 'ConnectedApp|HermesHistory' -v`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `answerQuick` loads `Tools(ctx, actor, nil)`, maps each to `quickassistant.Tool{Name, Title: App.Name, Description, InputSchema, Personal: true}`, and passes `TurnOptions` with an `ExtraRunner` that calls `Service.Call(ctx, actor, …)`.
  - The actor comes from the turn (`stream.go:342`), never the vault.
  - On `reply.UsedConnectedApp`, write `origin` on the assistant insert and `UPDATE conversation_messages SET origin=$1 WHERE id=$2` for the user row.
  - `historyMessages` skips rows whose `Origin` equals `OriginConnectedApp`.
- [ ] **Step 4: Run** `go test ./internal/modules/chat/... ./cmd/App/...`. Expected: PASS.
- [ ] **Step 5: Commit** `feat(chat): connected-app tools in quick assistant; keep them out of Hermes history`

### Task 9: `/v1/mcp` per-app grants and proxied tools

**Files:**
- Modify: `internal/modules/mcp/connections.go` (`Update` at 204, DTO at 78, new `AllowsConnectedApp`), `internal/modules/mcp/module.go` (`dispatch` at 181, `callTool` at 202, PATCH handler `update` at 387), `cmd/App/mcp.go` (pass the service)
- Test: `internal/modules/mcp/module_test.go`

**Interfaces:**
- Consumes: `connectedapps.Service`; column `agent_connections.connected_app_grants`.
- Produces:
  - `func (c *Connections) AllowsConnectedApp(ctx context.Context, token, appID string) (bool, error)`. Always true for JWT sessions, matching how `callPersonal` treats them.
  - The `ConnectionDTO` gains `ConnectedAppGrants []string json:"connectedAppGrants"`.
  - The PATCH body accepts `connectedAppGrants`. Only registry ids are allowed; anything else is a 400.
  - `mcp.Options` gains `ConnectedApps` (same interface as Task 8).

- [ ] **Step 1: Write the failing tests:**
  - `TestToolsListIncludesGrantedConnectedAppTools`: alice is connected to aaru. Her `lv_` key without a grant gets no `aaru__` tools. After PATCH `{"connectedAppGrants":["aaru"]}`, the list includes `aaru__read_library` with `readOnlyHint: true`.
  - `TestCallConnectedAppWithoutGrantIsForbidden`: the error message names Settings → Agent connections.
  - `TestGrantDefaultsOff`: a newly issued key has `connectedAppGrants: []`.
  - `TestAllowPersonalDataDoesNotImplyGrant`: `allowPersonalData=true` and no grant mean no `aaru__` tools.
  - `TestOtherUsersKeyCannotReachMyApps`: bob's key with grant `aaru` but no bob connection gets no tools and `ErrNotConnected` on call.
  - `TestPatchRejectsUnknownGrant`: 400.
- [ ] **Step 2: Run** `go test ./internal/modules/mcp/ -run 'ConnectedApp|Grant|OtherUsersKey' -v`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - The `tools/list` result is static `toolListing()` plus `ConnectedApps.Tools(ctx, auth.Identity(c), grantedApps)`.
  - In `callTool`, if `SplitName` succeeds: check the grant, then `ConnectedApps.Call(ctx, auth.Identity(c), name, args)`, and wrap the result like `personalResult`.
  - The user always comes from `auth.Identity(c)`, never `X-Vault-ID`.
- [ ] **Step 4: Run** `go test ./internal/modules/mcp/...`. Expected: PASS, with existing tests unchanged.
- [ ] **Step 5: Commit** `feat(mcp): per-app grants and connected-app tool proxy`

### Task 10: Privacy guard tests, docs, staging check

**Files:**
- Create: `internal/connectedapps/privacy_test.go`
- Modify: `internal/modules/mcp/catalog.go` `Instructions` (one paragraph naming `<app>__*` tools as read-only, and that they need the user's connection and the key's grant)
- Create: `docs/connected-apps.md` (operator notes: env keys, flag, callback URL to register with each app, how to revoke)

- [ ] **Step 1: Write the static guard test** `TestNoAdminOrTelemetryReadsConnectedAppAccounts`. Walk `internal/modules/admin`, `internal/posthog`, `internal/telemetry` and `internal/llm` and assert no `.go` file contains `connected_app_accounts` or `connectedapps.Store`.
- [ ] **Step 2: Run** `go test ./internal/connectedapps/ -run TestNoAdmin -v`. Expected: PASS. If it fails, remove the reference; don't weaken the test.
- [ ] **Step 3: Run the full suite** `make check`. Expected: lint, test and vuln all green.
- [ ] **Step 4: Commit** `test(connectedapps): privacy guards; docs`
- [ ] **Step 5: Staging verification**, after the user deploys with `CONNECTED_APPS_ENABLED=true` on staging only:
  - Connect the user's own Aaru with OAuth via `POST /v1/me/connected-apps/aaru/connect`, then open the URL.
  - Ask Quick Assistant "what am I watching?" and get a live answer.
  - `DELETE` the connection, then ask again: there should be no Aaru tool.
  - A second staging account lists `disconnected`.
  - Report the results with request IDs.

## Follow-up plans (not this one)
- Web `LuminaVaultWebApp` and iOS "Connected apps" settings screens, plus the per-app grant toggles in Agent connections. They need this API to be fixed first.
- v2: main Hermes chat through a per-tenant Lumina MCP entry.
