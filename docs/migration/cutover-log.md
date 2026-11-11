# Cutover log

One section per module and gate (roadmap "Module status lifecycle"). Record
evidence, not intent: commands, counts, links, numbers.

## projects (wave 1, pilot)

### G0 entry: met 2026-10-29

- Routes: 4 in `docs/migration/routes.json` (`projectsList`, `projectsCreate`,
  `projectsPatch`, `projectsDelete`). All are in the spec with `x-lv-module: projects`
  (`LuminaVaultServer` `scripts/tag-spec.py --check` → all operations marked).
- Prefix: `/v1/projects`, disjoint from every other module (`internal/manifest`
  `TestEveryRouteHasExactlyOneOwner`).
- Coupling: none. Projects publishes and subscribes to no in-process channel
  (`coupling.md`).
- Jobs: none (`jobs.md`).
- Route middleware: only `jwtAuthenticator`. No rate limit, tier or Hermes
  middleware on the group (`App+build.swift`, `projectsGroup`).
- Swift tests: `Tests/AppTests/Skills/ProjectModelTests.swift` (2 tests).

### G1 parity: met 2026-10-29

- Parity: `scripts/parity-up.sh` against `lv-swift:fdbed9b0`, then
  `parity compare` gives **4 scenarios, 0 differences** (base/health, projects
  crud, errors, isolation). 42 steps cover every route with success and error
  cases: no token, bad token, malformed, empty, array, missing, null and
  wrong-type bodies, whitespace names, random and malformed ids, cross-tenant
  patch and delete. 11 of those steps take DB snapshots of `projects`.
- Self-test (Swift vs Swift): **0 differences**, so the scenarios are deterministic.
- Go tests: `internal/modules/projects` (9 tests on Postgres + baseline + seed)
  port both Swift `ProjectModelTests` assertions (DTO round trip,
  tenant scoping). `TestResponsesConformToSpec` validates the bodies against
  `api/openapi.yaml`.
- `golangci-lint run`: 0 issues. `govulncheck`: no reachable vulnerabilities.
- CI: PR #1, run 40874765801. `parity` gives **4 scenarios, 0 differences**
  against `ghcr.io/luminavault/luminavaultserver:fdbed9b0…`
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/40874765801/job/123452432211).
  `lint`, `test` and `vuln` pass.

### G2 staging deploy: met 2026-10-31

- Deploy: LuminaVaultInfra #488 merged 2026-10-30 19:38Z. `api-go-staging` ran
  `4325ccaa` for 15 h with 0 ERROR log lines. Release run 41198570420 then moved
  it to `dd434496` (wave 2 merged, projects code unchanged), rolled out
  2026-10-31 11:54Z, healthy. At 12:02:58Z it moved to `d7297f97` (wave 3
  prep, docs only). The smoke below was re-run on `d7297f97`, so that is the
  SHA G3 pins.
- Smoke user: `smoke+g2@facorreia.com` (`BA2E7201-987C-47F2-A0E3-2C232BC9C549`),
  registered on Swift staging by the agent. Its password is a random secret,
  kept only in the agent's session scratchpad.
- `parity smoke --module projects` against `api-go-staging` (port-forward),
  with Swift transcripts from main CI run 41198741960, on `d7297f97` with 0 ERROR logs: `crud.yaml` matches
  except two steps, `patch seeded keeps todoCount` and `lowercase id accepted`
  (404, transcript 200). Both PATCH the parity seed project
  `5EED0000-…-0000000000A1`, which exists only in the parity DB.
- Control: the same smoke against Swift `api-staging`, which shares the
  `luminavault_staging` database, gives the same two 404s and matches on every
  other step. So Go and Swift agree step for step on staging, and the
  mismatches are a scenario artifact. Follow-up: smoke should skip steps that
  address seed IDs, or compare against a live Swift target instead of CI
  transcripts.

### G3 production: routed 2026-10-31, watch running

- LuminaVaultInfra #537 merged 2026-10-31 12:15Z: `/v1/projects` on
  `api.luminavault.fyi` goes to `api-go-production` (`d7297f97`).
- Swift baseline before the flip (`api-production` logs, 2026-10-29 06:27Z to
  10-31 12:04Z): 33,981 requests, 0 on `/v1/projects*`, 0 5xx on any route.
- Check on 2026-11-04 09:16Z: responses on the route lack Swift's `Server`
  header, so Go is serving it. Pod has 0 restarts in 3d20h. Logs show 0 5xx and
  0 ERROR, but only reach back about 12 h, and the 37 `/v1/projects` requests
  in them are essentially the agent's probes.
- So far the watch shows stability, not behaviour under real use: the route has
  had almost no real traffic. The probes also found routing differences, see
  decisions.md D-router-parity (PR #15).
- 7 d mark: 2026-11-07 12:15Z. Then set `done` and write `pilot-retro.md`.

## kanban (wave 2)

### G0 entry: met 2026-10-31

- Routes: 15 in `docs/migration/routes.json` (`boards*`, `cards*`, `columns*`),
  under the disjoint prefixes `/v1/boards` and `/v1/cards`.
- Coupling: writes `vault_activity_events` (Teams' "Vault activity → SSE"
  channel, coupling.md). Go writes the row; the live SSE push stays Swift-only
  until Teams (wave 3) moves or the channel is bridged. **Flip blocker.**
- Jobs: none moved. Promote writes `skills_state` plus
  `<vault>/tenants/<ID>/skills/<slug>/SKILL.md`, which the Swift `CronScheduler`
  reads from the DB and the PVC. There is no in-process cache, so jobs authored in
  Go are scheduled by Swift.
- Route middleware: `jwtAuthenticator` + `RateLimitMiddleware(.settingsByUser)`,
  ported as `internal/ratelimit` (in-memory, like Swift's default
  `rateLimit.storageKind=memory`). During the split each server counts
  separately, so the effective limit doubles.
- Swift tests: `Tests/AppTests/Kanban/` RankStringTests (5), CardExtraTests (3),
  KanbanServiceTests (11), KanbanControllerTests (1).

### G1 parity: met 2026-10-31 (CI)

- CI `parity` (PR #6, run 41122278875): Swift-vs-Swift self-test and Swift-vs-Go
  both give **10 scenarios, 0 differences**
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41122278875/job/124291230424).
  Kanban accounts for 6 of those scenarios (134 steps: crud, default board, errors,
  isolation, shared team boards with version checks, promote).
- Go tests: `internal/modules/kanban` (rank, internal, module: 30 tests) port the
  Swift assertions above, plus error order, team vaults, the rate limit and spec
  conformance. lint, test and vuln are green.
- Known differences (not reachable by parity): Swift traps on a cron field that
  is exactly `/` while Go answers 400. Slugs and excerpts are cut by rune, not
  grapheme cluster; switch them to `internal/swiftstr` (PR #4) once both are
  merged into prep.
## spaces (wave 2)

### G0 entry: met 2026-10-31

- Routes: 5 in `docs/migration/routes.json` (`spacesList`, `spacesCreate`,
  `spacesGet`, `spacesUpdate`, `spacesDelete`), prefix `/v1/spaces`.
- Coupling: writes `vault_activity_events` (Teams' "Vault activity → SSE",
  coupling.md). Go writes the row; the live SSE push stays Swift-only until
  Teams (wave 3) moves or the channel is bridged. **Flip blocker.**
- Jobs: none. Disk: creates `<VAULT_ROOT_PATH>/tenants/<VAULT UUID>/raw/<slug>`
  (0755) and soft-deletes it to `_deleted_<unix>_<slug>`, so `api-go-*` must mount
  the same PVC (it does: `apps/api-go/values-*.yaml` `persistence.existingClaim`).
- Route middleware: only `jwtAuthenticator` (`App+build.swift:2559`); no rate limit.
- Swift tests: `Tests/AppTests/Spaces/SpaceSlugDerivationTests.swift` (5),
  `SpacesCategoryTests.swift` (3).

### G1 parity: met 2026-10-31 (CI)

- CI `parity` (PR #7, run 41195436271): the Swift-vs-Swift self-test and
  Swift-vs-Go both give 0 differences
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41195436271/job/124520541028).
  Spaces has 3 scenarios and 69 steps: crud, errors, vaults (personal, foreign,
  team viewer, header-before-identity). Snapshots cover `spaces` and
  `vault_activity_events`.
- Go tests: `internal/modules/spaces` ports all 8 Swift tests (the seed-defaults
  test goes through `SeedDefaults`, since `/v1/vault/create` is wave 4), plus
  error wording, team vaults, the folder layout, soft delete, activity rows,
  live note counts and spec conformance. lint, test and vuln are green.
- Not compared by parity: on-disk folders (each server has its own container
  filesystem). Go tests cover them.
## profiles (wave 2)

### G0 entry: met 2026-10-31

- Routes: 6 in `docs/migration/routes.json` (`profilesList`, `profilesCreate`,
  `profilesGet`, `profilesPatch`, `profilesDelete`, `profilesActivate`). All are
  in the spec with `x-lv-module: profiles`.
- Prefix: `/v1/profiles`, disjoint from every other module (`internal/manifest`).
- Coupling: none. No route calls Hermes; `activate` only moves
  `user_hermes_profiles.is_default` in one transaction. Other Swift code shares
  the table: `HermesProfileMiddleware` and `HermesProfileService.ensureDefaultPersona`
  read it and lazily create a default row, and `AgentRoomsController` and
  `DashboardController` read it. That is shared data through Postgres, not
  process-local state.
- Jobs: none (`HermesProfileReconcilerService` in Admin re-ensures the 1:1
  `hermes_profiles` container slot, not these rows).
- Route middleware: only `jwtAuthenticator` (`App+build.swift`, `profilesGroup`).
- Swift tests: `Tests/AppTests/Profiles/ProfilesControllerTests.swift` (9 tests).

### G1 parity: met 2026-10-31

- Parity: CI against `ghcr.io/luminavault/luminavaultserver:fdbed9b0…`.
  `parity compare` gives **8 scenarios, 0 differences** (base/health, profiles
  crud, errors and isolation, projects ×3, sessions list). The profiles
  scenarios have 127 steps covering every route with success and error cases:
  no token, bad token, malformed, empty, array and string bodies, missing, null
  and wrong-type keys in Swift's declaration order, `skillsEnabled` element
  errors (`skillsEnabled.<i>` key path), slug and label rules (ICU `$` before a
  trailing newline, grapheme-counted labels, CoreFoundation whitespace including
  U+200B), templates, 8 KiB prompt limit, duplicate-slug ordering, PATCH
  no-op versus write, idempotent activate, NULL timestamps and alice/bob
  isolation. 21 of those steps take DB snapshots of `user_hermes_profiles`.
- Self-test (Swift vs Swift): **0 differences**.
- Go tests: `internal/modules/profiles` (17 tests on Postgres + baseline +
  seed) port all 9 Swift `ProfilesControllerTests`, plus quirk, error-wording
  and spec-conformance tests. `internal/swiftstr` has 3 more.
- The first CI run found one difference: Swift treats a label of only U+200B as
  blank. Fixed in `internal/swiftstr`.
- `golangci-lint run`: 0 issues. No new dependencies.
- CI: PR #4, run 41122016403. `parity` gives **8 scenarios, 0 differences**
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41122016403/job/124290173112).
  `lint`, `test` and `vuln` pass.

## sessions (wave 2)

### G0 entry: met 2026-10-31

- Routes: 1 in `docs/migration/routes.json` (`sessionsList`), in the spec with
  `x-lv-module: sessions`.
- Prefix: `/v1/sessions`, disjoint from every other module (`internal/manifest`).
- Coupling: none. A "session" is a chat conversation (`conversations` +
  `conversation_messages`), not an auth session or refresh token. The route
  only reads; the chat modules (Swift) write those tables. No JWT claim is used
  beyond the user id, and there is no "current session" marker.
- Jobs: none.
- Route middleware: only `jwtAuthenticator` (`App+build.swift`, `sessionsGroup`).
- Swift tests: none.

### G1 parity: met 2026-10-31

- Parity: same CI run as profiles, **8 scenarios, 0 differences**. The sessions
  scenario has 23 steps: ordering, message counts, the latest-message preview
  cut at 140 grapheme clusters (combining mark, ZWJ family, flag, skin tone,
  CRLF), `limit` defaults and clamps (0, negative, 999, non-numeric, decimal,
  `+3`, overflow, empty), `workspace` in upper and lower case, unknown,
  unparseable (which means all workspaces), cross-tenant, no token, bad token
  and the Basic scheme. Seed: `parity/seed/sessions.sql`.
- Self-test (Swift vs Swift): **0 differences**.
- Go tests: `internal/modules/sessions` (5 tests on Postgres + seed), including
  spec conformance.
- CI: PR #4, run 41122016403
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41122016403/job/124290173112).
  `lint`, `test` and `vuln` pass.
## apple (wave 2, flips with devices and reminders)

### G0 entry: met 2026-10-31

- Routes: 6 in `docs/migration/routes.json` (`appleConsentGet`, `appleConsentPut`,
  `appleRemindersSync`, `photosIndex`, `deviceCommandResult`, `deviceCommandsPending`),
  all tagged `x-lv-module: apple` in the spec.
- Prefixes: shared with devices (`/v1/devices/command`) and reminders (`/v1/reminders/sync`),
  so the three flip together (decisions.md D-shared-prefixes).
- Coupling: `DeviceCommandBroker` (coupling.md row 7). Its continuations are in Swift's
  memory and its callers (`PersonalDataTools`, from MCP and Skills) stay in Swift. Go's
  result route updates only the DB-backed `device_command_queue`. `ConnectionManager`
  (WebSockets) is not touched.
- Jobs: none.
- Route middleware: only `jwtAuthenticator` (`appleGroup`, `photosGroup`,
  `appleRemindersGroup`, `deviceGroup` in `App+build.swift`).
- Swift tests: `Tests/AppTests/Apple` `AppleRemindersControllerTests` (6),
  `PhotoIndexControllerTests` (9), `DeviceCommandQueueTests` (3), `DeviceCommandBrokerTests`
  (2, not ported: the broker stays in Swift). `AppleCalendarControllerTests` tests
  `/v1/calendar/*`, which belongs to calendar.

### G1 parity: met 2026-10-31

- Parity: CI run 41195542014 gives **13 scenarios, 0 differences** against
  `ghcr.io/luminavault/luminavaultserver:fdbed9b0…`, and the Swift-vs-Swift self-test gives
  0 differences (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41195542014/job/124520848592).
  `parity/scenarios/apple` has 92 steps (21 with snapshots of `apple_consent`, `users`,
  `apple_reminders`, `photo_index`, `embedding_usage`, `device_command_queue`): consent
  upsert and purge, consent-before-decode 403s, last-writer-wins, 413 over 1000 items,
  nested decode errors, deterministic embeddings, the embedding cap with a partial batch,
  the device queue and cross-tenant results. Seed: `parity/seed/apple.sql`.
- Go tests: `internal/modules/apple` (22 tests) port the 18 Swift tests above (all but
  the broker's 2) and validate bodies against `api/openapi.yaml`.
- `lint`, `test`, `vuln` pass on PR #5.

### G2 staging deploy: blocked

Beyond the pilot gate (D-wave2-early):
1. **Device RPC split.** Once `/v1/devices/command/:id/result` routes to Go, every live
   Swift device RPC times out after 12 s, and for `reminder_create`/`calendar_create`
   Swift then queues the write the app already ran, so the phone runs it twice. Needs a
   cross-process bridge (e.g. Valkey from Go's result route to Swift's broker), or keep
   `/v1/devices/command` on Swift until MCP/Skills move. `/v1/devices/commands/pending`
   is DB only and safe to split.
2. **Embeddings.** Go ports only the deterministic provider plus the usage cap. With
   `EMBEDDING_PROVIDER` set to a real provider, `/v1/photos/index` answers 500 instead of
   writing fake vectors. Needs the embedding chain (providers, fallback, BYOK; wave 4).

## devices (wave 2, flips with apple)

### G0 entry: met 2026-10-31

- Routes: 2 (`deviceRegister`, `deviceUnregister`), tagged `x-lv-module: devices`.
- Coupling: none. Jobs: none. Middleware: only `jwtAuthenticator`.
- Swift tests: `Tests/AppTests/Devices/DeviceBundleIDTests.swift` (2).

### G1 parity: met 2026-10-31

- Parity: same CI run as apple (13 scenarios, 0 differences, self-test 0).
  `parity/scenarios/devices` has 27 steps (8 with `device_tokens` snapshots), including the
  globally unique token (another account's token is a 500), untrimmed tokens, unknown
  platforms, and the undecoded percent-escaped DELETE path.
- Go tests: `internal/modules/devices` (5 tests) port both Swift tests.
- `APNS_BUNDLE_ID` is unset in the parity env, so the kept-bundle-id path is covered only by
  the Go test (`TestStoresAllowedBundleIDAndDropsForeign`).

### G2 staging deploy: blocked on apple's blockers (they flip together).

## reminders (wave 2, flips with apple)

### G0 entry: met 2026-10-31

- Routes: 5 (`remindersList`, `remindersCreate`, `remindersDetect`, `remindersPatch`,
  `remindersDelete`), tagged `x-lv-module: reminders`.
- Coupling: none. Jobs: `ReminderScheduler` (single replica) stays in Swift.
- Swift tests: none.

### G1 parity: not met (status stays `swift`)

- CI run 41195542014: `parity/scenarios/reminders` (70 steps, 12 with `reminders`
  snapshots) gives 0 differences, and `internal/modules/reminders` has 11 Go tests.
- Gap: `/v1/reminders/detect` calls an LLM through Swift's `RoutedLLMTransport`, which is
  not ported. The parity env has no reachable provider, so both servers answer
  `{"isReminder":false}`, but the success path cannot be compared there. Go ports the
  classifier (prompt, payload, fenced-JSON parse, fail-closed) and tests it with a stub
  transport, while `cmd/App` wires `reminders.UnavailableTransport`. In production Go would
  silently turn off chat-to-reminder proposals. The brief's rule for routes that need a
  service the env lacks keeps this module at `swift` until LLM routing is ported (waves 5/6)
  or a human accepts the gap. Because the three modules flip together, apple and devices
  cannot flip before this is settled.
## achievements (wave 2)

### G0 entry: met 2026-10-30

- Routes: 2 in `docs/migration/routes.json` (`achievementsList`, `achievementsRecent`),
  both `x-lv-module: achievements` in the spec.
- Prefix: `/v1/achievements`, disjoint (`internal/manifest`).
- Coupling: the routes only read `achievement_progress`. The writers
  (`AchievementsWorker` inbox, EventBus `achievement_unlocked`) stay in Swift
  (`coupling.md` rows 2 and 15, `jobs.md`).
- Jobs: none move. `AchievementsWorker` stays in Swift.
- Route middleware: `jwtAuthenticator` plus `RateLimitMiddleware(.achievementsByUser)`
  (60/min per user or IP). Ported in the wave-2 integration branch
  (`ratelimit.AchievementsByUser`, chain identify → limit → `auth.Require`,
  `TestRateLimitBeforeIdentity`).
- Spec: the spec's `AchievementsListResponse` and `AchievementsRecentResponse` do not
  describe Swift's output. The Go DTOs are hand-written from LuminaVaultShared 5.32.0.
  The canary `TestSpecAchievementsSchemasAreStale` fails once the spec is fixed.
- Swift tests: `AchievementsFlowTests` (2), `AchievementsServiceTests` (4) and the
  PowerLevel half of `PowerLevelTests` (5). The 4 streak tests belong to Dashboard.

### G1 parity: met 2026-10-30

- Parity: 3 scenarios (`read`, `errors`, `isolation`), 19 steps:
  - list and recent for alice and bob
  - recent `limit` default, 1, 2, 0, -3, 999, `abc`, empty, and an Int overflow
  - a retired key's label fallback, and fractional `unlocked_at`
  - no token, bad token, and a non-Bearer token on both routes
  - 2 `achievement_progress` snapshots
  
  Seed: `parity/seed/achievements.sql`.
- Self-test (Swift vs Swift) and Swift vs Go: **8 scenarios, 0 differences**
  (whole suite, with news and projects).
- Go tests: `internal/modules/achievements` (11 tests), including a verbatim catalog pin.
- CI: PR #3, run 41120945344. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41120945344/job/124286993965).

## news (wave 2)

### G0 entry: met 2026-10-31

- Routes: 1 in `docs/migration/routes.json` (`newsTicker`, `x-lv-module: news`).
- Prefix: `/v1/news`, disjoint (`internal/manifest`).
- Coupling: none. `NewsTickerCache` is per process and nobody else invalidates it
  (`coupling.md`). It reads `plugin_installs` (Plugins) and opens its config
  with SecretBox.
- Jobs: none.
- Route middleware: `jwtAuthenticator` (identify, never rejects), then
  `RateLimitMiddleware(.settingsByUser)` (30/min, bucket shared with
  `/v1/plugins`, kanban and spaces), then `requireTenantID`. Go uses
  `auth.Identify`, then `internal/ratelimit` with the process-wide store, then
  `auth.Identity`, so an unauthenticated request counts against `ip:` and can
  get a 429 before its 401.
- Mounted only when `LV_SECRET_MASTER_KEY` opens a SecretBox, as in Swift.
- Swift tests: `Tests/AppTests/Plugins/NewsTickerPluginTests.swift`, 4 of 5. The
  `PluginCatalog`/`NewsTickerConnector` test belongs to Plugins.

### G1 parity: met 2026-10-31

- Environment: `parity/stubs/feeds` is a stdlib-only stub aggregator with fixed
  JSON Feed answers, and fixed errors for feeds named `/error`, `/notfound`,
  `/malformed`, `/missingkey` and `/hangup`. It runs as the `feeds` service in
  `docker-compose.parity.yml`. Both servers get `FEEDS_BASE_URL=http://feeds:8080`
  and one curated feed.
- Seed: `parity/seed/news.sql` has 13 `news_*` users, one per install state, with
  SecretBox-sealed configs. Each bucket stays far under 30 per minute:
  `news_amy` 6 requests, every other user 1, `ip:anon` 2.
- Parity: 4 scenarios (`ticker`, `errors`, `upstream`, `feedlist`), 20 steps:
  - 401 with no token and with a bad token
  - 404 not installed, 409 disabled
  - an unopenable config and a config without `feed_urls` (curated only)
  - curated plus user feeds: private, invalid and duplicate entries dropped
  - `limit` set to 1, 0, 500 and `abc`, and a repeat served from the cache
  - a response without `_feeds`, and `_feeds` without `generated_at`
  - aggregator 503 or 404 → 502; non-JSON body, missing key and a closed
    connection → 500
  - a 17-entry `feed_urls` probe (letter case, space, `+`, `&`, nip.io, fdic.gov,
    IPv6, CRLF, non-ASCII, ftp, cap of 10)
- The probe found two Foundation behaviours, both now ported:
  - `URL(string:)` percent-encodes invalid characters instead of failing, so a
    piece with an inner CRLF is kept.
  - `URLComponents.queryItems` leaves `+` raw (the aggregator reads it as a space)
    and escapes `&` and `=`.
- Self-test (Swift vs Swift) and Swift vs Go: **11 scenarios, 0 differences**
  (whole suite).
- Go tests: `internal/modules/news` covers the service against a stub
  aggregator, the routes on Postgres, the rate limit (30 then 429; 429 before
  401), and the spec conformance of the 200.
- CI: PR #3, run 41196472076. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41196472076/job/124523530129).

## teams (wave 3)

### G0 entry: met 2026-10-31

- Routes: 19 in `docs/migration/routes.json` (12 under `/v1/teams`, 6 under `/v1/vaults`,
  1 under `/v1/team-invitations`), all tagged `x-lv-module: teams` in the spec.
- Prefixes: `/v1/team-invitations`, `/v1/teams`, `/v1/vaults`, disjoint (`internal/manifest`).
- Coupling: vault activity SSE (coupling.md row "Vault activity → SSE"). The publisher is
  in-process in both servers, so this is a flip blocker for Spaces, Kanban and Teams.
- Jobs: none. Expired archives are purged inline by `GET /v1/teams`.
- Route middleware: `jwtAuthenticator` (identify, never rejects), then
  `RateLimitMiddleware(.settingsByUser)`, then the handler, which parses path ids before it
  requires identity. Go uses `auth.Identify`, then `internal/ratelimit` with the process-wide
  store.
- Swift tests: no Teams directory. The `createTeamVault` helpers in
  `Ingestion/MultimodalIngestionAPIIntegrationTests` and
  `Knowledge/KnowledgeVaultScopeIntegrationTests` are the only tests that call these routes;
  their two assertions are ported.

### G1 parity: met 2026-11-04

- Seed: `parity/seed/teams.sql` adds 14 `team_*` users (at most 26 requests each, 27 for the
  anonymous `X-Forwarded-For` bucket, so no `settingsByUser` bucket fills), 5 teams, 7 team
  vaults and 11 invitations. Accept scenarios use seeded tokens because Swift never returns
  one. The `parity_team_invitations` view makes the table comparable: it shows the shape of
  `token_hash`, whether the token is still a seeded one, and `vault_grants` as jsonb.
- Parity: 8 scenarios, 209 steps, covering every route:
  - no token, bad token, and path ids checked before identity
  - malformed, empty, array, missing, null and wrong-type bodies, including nested
    `vaultGrants` errors
  - bad, unknown and foreign ids, and cross-team isolation
  - the purge before the 401, name limits counted in graphemes, heap-order lists
  - archive, restore, transfer and leave
  - invite, list, resend, revoke and accept
  - snapshots of `teams`, `team_memberships`, `vaults`, `vault_memberships`,
    `vault_activity_events` and the invitations view
- Harness: a step can set `stream: true` (status and headers only, for a body that never
  ends) and `headers: [...]` (compare response headers). Both stream success steps match
  Swift on status, `Content-Type`, `Cache-Control` and `X-Accel-Buffering`.
- Not compared with Swift, covered only by Go tests:
  - SSE frame bytes. Swift sends no initial frame, so parity has no body to read. The format
    comes from reading `VaultActivitySSEResponse`.
  - `ResendTeamInvitationSender`. Swift hard-codes `api.resend.com`, so the parity
    environment runs the logging sender and cannot reach the Resend path or its 500, 502 and
    503 errors. Check the Resend path on staging before this module goes to production.
- Self-test (Swift vs Swift) and Swift vs Go: **41 scenarios, 0 differences** (whole suite).
- Go tests: `internal/modules/teams` (14 tests: routes, rate limit, stream frames, a Spaces
  write reaching the stream, Resend against a stub server, spec conformance) and
  `internal/vaultactivity` (2 hub tests).
- CI: PR #13, run 41804872584. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41804872584/job/126489291749).

### G2 staging deploy: blocked

- Waits for the pilot (decisions.md D-wave3-early) and for a cross-process vault activity
  bridge (proposal in PR #13).
## me (wave 3)

### G0 entry: met 2026-11-04

- Routes: 13 in `docs/migration/routes.json`: today, brief and card feedback,
  recap, recap/period, resurface, learned, commitments (GET, PATCH), location
  (GET, DELETE), apns-categories (GET, PUT).
- Prefixes: `/v1/me/apns-categories`, `/v1/me/brief`, `/v1/me/commitments`,
  `/v1/me/learned`, `/v1/me/location`, `/v1/me/recap`, `/v1/me/resurface`,
  `/v1/me/today`. Gateways, providers, LLM preferences and connections move later
  (`decisions.md` D-me-split).
- Coupling, both flip blockers:
  - `MeTodayCache`: same 300 s per-process TTL, but Swift invalidates it from the
    in-process EventBus (`memory_upserted`, `achievement_unlocked`), which Go never
    hears. `Module.InvalidateToday` exists and nothing calls it yet. A write can
    stay invisible on a Go-served today for up to 300 s.
  - `PeriodRecapThemesWorker` stays in Swift and drains an in-process inbox. Go
    runs the same claim SQL, but a row Go claims is never generated by Go, so a
    period served only by Go never gets themes.
- Jobs stay in Swift: `BriefComposer` (daily-brief skill run),
  `CommitmentsService.record` (kb-compile) and the themes worker.
- Route middleware: `jwtAuthenticator`, then
  `RateLimitMiddleware(.meTodayByUser)` (60/60 s), then `requireTenantID`.
  Go uses `auth.Identify`, `internal/ratelimit` and `auth.Require`, so an
  unauthenticated request can get a 429 before its 401. Every limited route
  shares one bucket. apns-categories is JWT only and unlimited.
- `/v1/me/suggestions` shares the bucket in Swift and is not ported here. The
  policy is local to the module, so that port needs the same policy name.
- Swift tests: 50 of the 73 in `Tests/AppTests/Me`. Two use seeded rows because their writers stay in Swift:
  briefs in place of `BriefComposer`, `recap_themes` in place of the worker. Not
  ported, because the code stays in Swift or lives elsewhere: commitment parsing
  and kb-compile (7), brief composing and delivery (11), the themes generator (4)
  and memory review actions (1).

### G1 parity: met 2026-11-04

- Seed: `parity/seed/me.sql` has seven `me_*` users, one per concern. Each bucket
  stays under 60 per run: `me_brf` 34, `me_ann` 29, `me_cmt` 27, `me_new` 20,
  `ip:anon` 13.
- Parity: 8 scenarios, 168 steps: success and error paths on every route, malformed
  bodies, bad and foreign ids, isolation, and snapshots of `brief_feedback`,
  `memories`, `commitments`, `apns_category_prefs` and `users`.
- Not compared by parity:
  - Headers: `ETag`, `Cache-Control` and the 304. Go tests only.
  - `recap_themes` after a claim: Swift's worker flips the row to `fallback`
    asynchronously, so a snapshot would race. Each claiming period is read once per
    user. The claim SQL was compared by reading it; a Go test checks one claim per
    in-flight window.
  - Today cache invalidation: see the flip blocker above.
- Date limits: rows are placed relative to seed time, so a run straddling midnight
  UTC could differ. The `learned` scenarios pin `since` to late October 2026 and will
  only exercise the 90-day 400 after January 2027.
- Self-test (Swift vs Swift) and Swift vs Go: **41 scenarios, 0 differences**
  (whole suite). The Swift transcripts in the run's artifact show the seeded success
  data, including brief cards, ready themes and the snooze.
- Go tests: `internal/modules/me` (window, builder, policy and parsers without a
  database; routes on Postgres; rate limit; 304; spec conformance). The spec's
  `MeTodayResponse` is stale and has no required properties, so it validates anything;
  a canary fails once the spec describes the real keys. Unknown brief kinds go on
  the wire as the literal `unknown`, outside the spec's enums, as in Swift.
- CI: PR #16, run 41806060351. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41806060351/job/126493247648).
## account (wave 3)

### G0 entry: met 2026-11-04

- Routes: 1 in `docs/migration/routes.json` (`DELETE /v1/account`, `x-lv-module: account`).
- Prefix: `/v1/account`, disjoint (`internal/manifest`).
- Coupling: writes tables other modules own, through `TenantDataEraser`, ported
  as `internal/modules/account/eraser.go`. Soft-renames the vault folder and the
  Hermes profile for `DeletedDataPurgeService`.
- Jobs: none move. `DeletedDataPurgeJob` stays in Swift and purges what Go renames.
- Route middleware: `jwtAuthenticator`. Go uses `auth.Middleware`.
- Swift tests: `AccountDeletionTests` 10 of 11; the other tests migration M150.
  `DeletedDataPurgeJobTests` (6) stay with the job.

### G1 parity: met 2026-11-04

- Seed: `parity/seed/account.sql` has 9 `acct_*` users; alice and bob are never
  deleted.
- Parity: 4 scenarios, 28 steps. Snapshots cover every table the delete changes.
  - 401 with no token, a bad token, a wrong password, stale, future and no-`iat`
    tokens
  - 204 by password and by fresh token, and the old session answering 401
  - team owner: bare 500, with the eraser's deletes rolled back
  - activity in the user's own personal vault: 204, row gone
- FK trigger order differs between schemas. The parity database is built by
  Swift `migrate`; Go tests use the goose baseline, where the `actor_user_id`
  SET NULL trigger fires before the vault cascade. A row the eraser has already
  updated gets its `vault_id` rechecked, which failed there with a 500 while
  Swift's schema gave 204. The eraser now skips the rows that cascade anyway, so
  the end state matches on both. Production's trigger order was not inspected.
- Not observable in parity: the filesystem soft-rename. A Go test covers it, with
  the name format read from Swift source.
- Flip blocker: `apps/api-go` must mount the volume `apps/api` uses (`/app/data`,
  with `VAULT_ROOT_PATH` and `HERMES_DATA_ROOT`). Otherwise `os.Stat` finds
  nothing, the rename is skipped silently, and deleted users' folders stay under
  live names and are never purged.
- Self-test (Swift vs Swift) and Swift vs Go: **44 scenarios, 0 differences**
  (whole suite).
- Go tests: `internal/modules/account`, 15 test functions.
- CI: PR #12, run 41811507269. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41811507269/job/126511630338).

## onboarding (wave 3)

### G0 entry: met 2026-11-04

- Routes: 2 in `docs/migration/routes.json` (`GET` and `PATCH /v1/onboarding`).
- Prefix: `/v1/onboarding`, disjoint (`internal/manifest`).
- Coupling: `OnboardingLatches` is not ported. Swift's vault, capture, compile and
  chat paths set these flags as a side effect, so those later waves must port it.
- Jobs: none.
- Route middleware: `jwtAuthenticator`. Go uses `auth.Middleware`.
- Swift tests: `OnboardingTests` 9 of 9, `GuidedStartTests` 5 of 13. The other 8
  exercise the latches above.

### G1 parity: met 2026-11-04

- Seed: `parity/seed/onboarding.sql` has `onb_ann` and `onb_bob`, with no state
  rows until the scenarios create them.
- Parity: 2 scenarios, 38 steps: 401s, 17 decode errors with Foundation's wording,
  lazy creation on GET and on PATCH, one-way latches, two-way
  `guidedStartDismissed`, and the kb/memory-compile dual write. PATCH writes only
  the assigned columns plus `updated_at`, as Fluent does.
- Spec drift: `OnboardingStateDTO` marks the flags optional, but Swift always
  emits them, so Go uses a hand-written DTO.
- Self-test (Swift vs Swift) and Swift vs Go: **44 scenarios, 0 differences**
  (whole suite).
- Go tests: `internal/modules/onboarding`, 12 test functions.
- CI: PR #12, run 41811507269. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41811507269/job/126511630338).

## health (wave 3)

### G0 entry: met 2026-11-04

- Routes: 3 in `docs/migration/routes.json` (`POST` and `GET /v1/health`,
  `GET /v1/health/daily`). This is the HealthKit module, not the `/health` probe.
- Prefix: `/v1/health`, disjoint (`internal/manifest`).
- Coupling: none in the routes. Not ported: the log-only EventBus
  `health_event_synced` and the PostHog `health_events_synced` capture.
- Jobs: none in `jobs.md`. `HealthCorrelationService` runs from the admin route
  `POST /v1/admin/health/correlate`, driven by host cron; it stays in Swift.
- Route middleware: `jwtAuthenticator`; POST adds
  `EntitlementMiddleware(.healthIngest)`. Go uses `auth.Middleware` and the new
  `internal/entitlement`.
- Swift tests: `HealthReadTests` 5 of 5, `HealthDailyAggregateTests` 7 of 7.
  `HealthCorrelationServiceTests` (6) stay with the correlation service.

### G1 parity: met 2026-11-04

- Seed: `parity/seed/health.sql` has `hea_hal`, `hea_bea` and 17 `gate_*` users.
- Parity: 5 scenarios, 94 steps: 401s, 28 decode and query errors, windows,
  filters, clamps, daily sum and average with gap fill, per-row skip rules, and
  the 1000-event cap (413).
- Billing gate: CI runs parity with enforcement off, so the regular run does not
  compare the 402 path. It was compared once, with
  `BILLING_ENFORCEMENT_ENABLED: "true"` in `docker-compose.parity.yml` on a commit
  reverted straight after: **44 scenarios, 0 differences**, run 41810757076
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41810757076/job/126509102071).
  `05-gate.yaml` gave 402 for lapsed, free, archived with a good key, a base URL
  on a keyed provider, an empty key, a good key with a pool key that does not
  open, and an unlinked, missing or undecryptable xAI container. It gave 200 for
  pro (also past its expiry date), a pro override, a good key, a keyless provider,
  one pool key and a linked xAI container. The gate also answers 402 before the
  body is read.
- Known difference with enforcement on: Swift's credential store rotates one
  candidate per call with a per-process cursor, so a provider with both spendable
  and unspendable candidates alternates between 402 and 200. Go exempts whenever
  any candidate is spendable. Its 600-second cache is not ported either.
- Known gap in `wire.Bind` (shared): a body over 2 MiB gives 400 "not valid JSON"
  where Hummingbird gives 413. Read from source, not compared. It also applies to
  `PATCH /v1/onboarding`.
- Spec drift: `HealthEventDTO`, `HealthEventInput` and `HealthIngestResponse` do
  not match Swift (`type` vs `eventType`; `inserted`, `skipped`, `events` vs
  `ingested`), so conformance tests validate only daily and an empty list.
- Self-test (Swift vs Swift) and Swift vs Go: **44 scenarios, 0 differences**
  (whole suite).
- Go tests: `internal/modules/health` 17 and `internal/entitlement` 4 test
  functions.
- CI: PR #12, run 41811507269. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41811507269/job/126511630338).

## community (wave 3)

### G0 entry: met 2026-11-04

- Routes: 17 in `docs/migration/routes.json`, under `/v1/community` and
  `/v1/admin/community`, tagged `x-lv-module: community`.
- Prefix: disjoint (`internal/manifest`).
- Coupling: moderation and the daily post call `RoutedLLMTransport` (wave 5).
- Jobs: `DailyPostWorker` and the `recheckPending` sweep stay in Swift.
- Route middleware: `jwtAuthenticator`, rate buckets `settingsByUser` and
  `communityWriteByUser`. `community.enabled` and `community.postingEnabled` gate
  routes, and both states are compared.
- Swift tests: 7 controller tests, plus the content filter, handle, cursor and config
  flag unit tests. Page-size clamp and DTO mapping have only scenarios. Not ported:
  moderator parse and payload, DailyPost rules and `DailyPostWorkerTests`, which test
  code that stays in Swift.

### G1 parity: not met (status stays `swift`)

- CI run 41818972732: `parity/scenarios/community` (9 scenarios, 248 steps, 25 with
  snapshots) gives 0 differences, and `internal/modules/community` has 16 Go tests.
  The switch states `community-off`, `posting-off` and `unconfigured` give 0 differences
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41818972732/job/126536959709).
- Gap: Swift moderates each post that passes the filter with `RoutedLLMTransport`. Go has
  the same filter and nothing behind it, so the post stays `pending` until Swift's sweep
  (15-minute tick, 5-minute minimum age), roughly 5 to 20 minutes. In production Swift
  publishes it at once. The parity gateway is unroutable, so both servers hold it and CI
  cannot see this.
- Gap: `POST /v1/admin/community/daily/run` can only record a failed attempt in Go. Three
  runs mark the day `skipped`, which would cost a real daily post.
- Keeps this module at `swift` under the rule in `D-reminders-detect`, until LLM routing is
  ported (wave 5) or a human accepts the delay and flips with `/v1/admin/community` left on
  Swift.

## marketplace (wave 3)

### G0 entry: met 2026-11-04

- Routes: 19 in `docs/migration/routes.json`, under `/v1/marketplace`, tagged
  `x-lv-module: marketplace`.
- Prefix: disjoint (`internal/manifest`).
- Coupling: none. Install, upgrade, the runner client and the capability broker are
  self-contained, and no call waits on a later wave.
- Jobs: none.
- Route middleware: `jwtAuthenticator`, rate bucket `settingsByUser`. Mounted only with a
  usable `LV_SECRET_MASTER_KEY`, as in Swift.
- Swift tests: `MarketplaceSafetyTests` 7, plus the featured curation check.

### G1 parity: met 2026-11-04

- Seed: `parity/seed/marketplace.sql`, with a `luminavault` publisher so the catalog seed
  is skipped, and versions inserted directly (see the Swift bug below).
- Parity: 9 scenarios, 328 steps, 38 with snapshots. The stub in `parity/stubs/feeds`
  answers `POST /v1/execute` as the runner.
- Switch states (`scripts/parity-switches.sh`, 69 steps across all five states, community included):
  `unconfigured` (short signing key, no runner token), `runner-off`
  (`plugin_runner_disabled`) and `catalog-unseeded` give 0 differences.
- Self-test (Swift vs Swift) and Swift vs Go: **78 scenarios, 0 differences** (whole suite).
- Go tests: `internal/modules/marketplace` 18 test functions.
- CI: PR #18, run 41818972732. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41818972732/job/126536959709).
- Swift bug, matched on purpose: at `fdbed9b0`, `MarketplaceVersion.configFields` binds as
  `jsonb[]` into a `jsonb` column, so every `marketplace_versions` INSERT fails.
  `POST .../versions` is always 409 `version_already_exists`. The catalog seed inserts the
  publisher and the first listing, then answers 500, and after that a list that reaches the
  orphan listing is 404 `marketplace_version_not_found`. Go matches this behind one switch
  (`versionInserts`, false in production). Tests set it to true to cover the working path.
- Boot guard: outside dev, a runner token with a signing key under 32 bytes stops both
  servers (`marketplace.CheckConfig`). Go test only, as a server that does not boot gives
  parity nothing to compare.
- Coverage only in Go tests: `network.fetch` success, redirect refusal and size cap, because
  the parity stub has no TLS.
- Before flipping: production's `PLUGIN_*` values must reach the Go deployment, and
  `PLUGIN_ARTIFACT_ROOT` must be the same volume. Check
  `SELECT count(*) FROM marketplace_versions` in production, since this log cannot say what
  state the Swift bug left there.
## calendar (wave 3)

### G0 entry: met 2026-11-04

- Routes: 8 in `docs/migration/routes.json`: `POST /v1/calendar/connect`,
  `POST /v1/calendar/disconnect`, `GET` and `POST /v1/calendar/events`,
  `GET /v1/calendar/events/{id}/brief`, `GET /v1/calendar/oauth/callback`,
  `GET /v1/calendar/status`, `POST /v1/calendar/sync`.
- Prefix: `/v1/calendar`, disjoint (`internal/manifest`).
- Coupling, all flip blockers:
  - `mail` must flip with `calendar`. Gmail connect finishes at
    `/v1/calendar/oauth/callback`, and the callback only finds sessions from its own
    process. `flipsWith` is generated by `scripts/gen-modules.py` and does not list
    the pair yet.
  - `CalendarOAuthSessionStore` is in memory in both servers. A consent in flight
    (at most 600 s) at a flip or rollback comes back `session_not_found`, and the user
    retries. Go must run one replica until `state` stops depending on process memory.
    A Postgres store cannot stay compatible without Swift changes: Swift's store is an
    in-process actor and migrations run from the Swift image. Not built.
  - The brief makes one embedding call, not an LLM call. Go has only the deterministic
    embedder, as apple does. With `EMBEDDING_PROVIDER` set to a real provider, a brief
    with no stored match answers 500 and writes nothing. BYOK
    `TenantAwareEmbeddingService` is not ported either. Needs the embedding chain
    (wave 4), or keep the `/v1/calendar/events` prefix on Swift until then: prefix
    routing cannot separate `GET`, `POST` and brief. The production value is sealed
    and was not checked.
- Jobs stay in Swift: `CalendarSyncWorker` and `MeetingBriefScheduler`. Tokens use the
  same SecretBox envelope and columns, so the worker reads what Go stores.
- Route middleware: `jwtAuthenticator` on everything except the callback. Brief adds
  `RateLimitMiddleware` with `queryByUser` scaled by tier, in the module because
  `internal/ratelimit` has no tier scale.
- Not ported: PostHog captures (`calendar_connected`, `calendar_event_created`,
  `apple_calendar_synced`), and Fluent's eager decode of `attendees` and `recurrence` on
  `GET /v1/calendar/events`: malformed JSON gives 500 in Swift and 200 in Go. No writer
  can produce it.
- Swift tests: 24 ported. `CalendarWebReturnTests` 3, `MeetingBriefWordingTests` 6
  (without the push title and body assertions), `MeetingBriefHTTPTests` 4 of 5 (the
  apns-prefs test is in `me`), `AppleCalendarControllerTests` 7, and 4 Gmail tests in
  `MuseProactiveIntegrationTests`. `MeetingBriefSchedulerTests` (8) stay with the
  scheduler.

### G1 parity: met 2026-11-04

- Seed: `parity/seed/calendar.sql` has 13 `cal_*` users, one per concern. No row can
  make a server call Google: `connected` rows sit only on paths that never use a token,
  and token columns stay NULL.
- Parity: 7 scenarios, 134 steps: 401s, 503s, decode errors, limit clamps, EventKit sync
  caps and last-writer rules, 413, disconnect, brief stored and live matches, the 40%
  quota, the floor, and the embedding cap.
- **Not compared by parity**, because the environment has no Google. Go tests against a
  stub server only:
  - `connect` success and `invalid_return_to` (the 503 comes first in parity).
  - The callback with a real session: exchange, decline, `missing_code`, redirect bases.
  - `POST /v1/calendar/events` success and Google's errors.
  - Token refresh, rotation, `needs_reauth`, revoke on a live grant, initial sync.
- `POST /v1/calendar/sync` never calls Google and is fully compared. The brief's live
  match is compared with the deterministic embedder only.
- Swift quirks kept:
  - In `CalendarSyncService.sync`, `continue` inside `repeat-while` on a 410 jumps to
    the condition. The full resync never runs and the expired token is stored again, so
    `CalendarSyncWorker` gets 410 forever for that account. Worth fixing in Swift.
  - A lexical-only chunk hit (distance 1.0) hides a passing document hit for the same
    memory.
- Parity found two differences that apply beyond this module:
  - A known path with the wrong method is a 404 in Hummingbird. Echo answered 405, so
    `internal/httpserver` now answers 404 for every module.
  - Hummingbird keeps a `+` in a query value and Go decodes it to a space. Calendar reads
    the raw query. Other modules still use `c.QueryParam`.
- Self-test (Swift vs Swift) and Swift vs Go: **68 scenarios, 0 differences** (whole
  suite).
- Go tests: `internal/modules/calendar` (logic without a database; routes on Postgres;
  stub Google; rate limit; disabled paths; spec conformance; scenarios run on Go).
- CI: PR #19, run 41819569386. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41819569386/job/126539035089).

## mail (wave 3)

### G0 entry: met 2026-11-04

- Routes: 3 in `docs/migration/routes.json`: `POST /v1/mail/gmail/connect`,
  `POST /v1/mail/gmail/disconnect`, `GET /v1/mail/gmail/status`.
- Prefix: `/v1/mail`, disjoint (`internal/manifest`).
- Coupling: flips with `calendar`, see that section. Same grant, same
  `calendar_accounts` row, same OAuth client and callback.
- Jobs: none. Route middleware: `jwtAuthenticator`.
- Not ported: `GmailClient` and `GmailInboxService`, which Muse chat uses and no route
  does, and the PostHog `gmail_connect_started` capture.
- Swift tests: 4 of 10 in `GmailClientTests` (scope checks, scope merge, consent URL,
  deep links). The other 6 test the inbox reader and its tool, which stay in Swift. The
  Gmail route tests are counted under calendar.

### G1 parity: met 2026-11-04

- Parity: 1 scenario, 21 steps: 401s, 503s, each half of the grant in status, and
  disconnect keeping or deleting the row.
- **Not compared by parity**, because the environment has no Google: `connect` success,
  `invalid_return_to`, the Gmail callback, revoke on a live grant. Go tests against a
  stub server only, in `internal/modules/calendar`.
- Self-test (Swift vs Swift) and Swift vs Go: **68 scenarios, 0 differences** (whole
  suite).
- CI: PR #19, run 41819569386. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41819569386/job/126539035089).

## dashboard (wave 3)

### G0 entry: met 2026-11-04

- Routes: 16 in `docs/migration/routes.json`, under `/v1/analytics`, `/v1/dashboard`,
  `/v1/insights` and `/v1/tasks`.
- Prefixes: disjoint (`internal/manifest`).
- Coupling: the routes read many tables and write only `analytics_events`,
  `analytics_daily_rollups`, `analytics_recommendation_states` and `insights.dismissed_at`.
- Jobs: none move. `AnalyticsMaintenanceService` and `SynthesisWorker` stay in Swift.
- Swift tests: 37 in scope, 36 ported.
  - In scope: the 33 tests in `Tests/AppTests/Dashboard` and the 4 streak tests in
    `PowerLevelTests`.
  - Not ported: "Memory health queues validate their filter", which hits `/v1/memory`
    (wave 4).
- DTOs: hand-written from LuminaVaultShared 5.32.0, because Swift's `Double` text differs
  from Go's (`swiftDouble`).

### G1 parity: not met (status stays `swift`)

- CI run 41849219723 on head 7d63bc8: self-test (Swift vs Swift) and Swift vs Go give
  **92 scenarios, 0 differences** (whole suite). `parity/scenarios/dashboard` has 6 files
  and 373 steps, 48 of them with snapshots. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41849219723/job/126640690180).
- Go tests: `internal/modules/dashboard` has 34 cases on Postgres, including spec
  conformance, and 10 unit tests.
- Gap: `cronJobs` on `/v1/dashboard/home` comes from Swift's `CronBridgeService`, which runs
  `docker exec` in the tenant's managed container or calls a BYO Hermes dashboard. Neither
  is ported, so Go always answers `[]`. The parity env has neither source, so Swift answers
  `[]` there too and parity cannot see the difference. In production, Go would silently
  empty the cron preview for tenants who have cron jobs. The brief's rule keeps this module
  at `swift` until the bridge is ported (wave 6, Hermes) or a human accepts the gap.
- Proposal for the owner: route `/v1/analytics`, `/v1/insights`, `/v1/tasks` and the other
  `/v1/dashboard` routes to Go, and leave the `/v1/dashboard/home` prefix on Swift until
  Hermes moves (longest-prefix routing keeps it there).

## Flip batch 1: profiles, sessions, news, achievements, health

### G2 staging deploy: smoke met 2026-11-04, on `c17c3f6`

- Image: `api-go-staging` ran `c17c3f6` from 2026-11-04 14:40:50Z, 1/1 Ready.
- Smoke: `parity smoke` per module with throwaway staging users, against Swift
  transcripts from CI run 41847129028. Swift staging ran the same smoke as the
  control, because the gate's user has no parity seed data.

| Module | Go mismatches | Swift control | Same set |
|---|---|---|---|
| profiles | 11 | 11 | yes |
| sessions | 0 | 0 | yes |
| news | 10 | 10 | yes |
| achievements | 0 | 0 | yes |
| health | 0 | 0 | yes |

- Every mismatch is a 404 where the transcript has 2xx, on a step that needs
  parity seed rows. `profiles` had to be re-run with a separate user per server:
  on one shared user, Swift answered 409 for slugs Go had just created.
- `profiles/crud` and `health/02-read` gained `smoke: true`, since the gate needs
  a smoke scenario per module.
- Coupling accepted for the flip: `AchievementsWorker` and
  `HealthCorrelationService` stay in Swift and write the same tables.

### Production flip: 2026-11-04

- Infra #546 merged 15:48:52Z. `api-go-production` runs `c17c3f6`, rolled out and
  confirmed 15:52:55Z.
- Public check: `/v1/profiles`, `/v1/sessions`, `/v1/news/ticker`, `/v1/achievements`,
  `/v1/health` and `/v1/projects` answer from Go, with no Swift `Server` header.
  `/v1/spaces`, `/v1/me/today` and `/health` still answer from Swift.
- The owner's merge waives the pilot gate: D-flip-before-pilot.

### G3 production watch: running, 72 h ends 2026-11-07 15:53Z

- Baseline: Swift's logs from 2026-11-01 15:55Z to the flip hold 7 lines for these
  prefixes (`/v1/news` 3, the others 1 each) out of 47,053, and no 5xx-like status.
  Some of those are the migration's own probes.
- Limit of this evidence: these routes see almost no traffic, so a clean watch shows
  the deploy is healthy, not that real clients behave the same.

## Flip batch 2: spaces, kanban, teams

### G2 staging deploy: smoke met 2026-11-04, on `f31b21f`

- Image: `api-go-staging` ran `f31b21f` from 2026-11-04 16:22:07Z, with 0 ERROR lines in
  the 70 minutes before the smoke.
- Smoke: throwaway staging users, one per server, against Swift transcripts from CI run
  41862034099, with Swift staging as the control.

| Scenario | Go mismatches | Swift control | Same set |
|---|---|---|---|
| spaces/crud | 4 | 4 | yes |
| kanban/01-crud | 0 | 0 | yes |
| teams/01-teams | 0 | 0 | yes |
| teams/07-activity | 10 | 10 | yes |

- Mismatches are 403 and 404 on seed-only IDs.
- Run `teams` scenarios one at a time: run together they exceed the 30/min limit, and all
  32 mismatches were 429s that proved only that the limiter matches.
- Not observed on a deployed server: a real invitation email, and a pushed activity event.

### Production flip: 2026-11-04

- Infra #547 merged 19:37:45Z. `api-go-production` runs `f31b21f`; ingress confirmed
  19:42:38Z with 12 paths on Go.
- Public check: `/v1/spaces`, `/v1/boards`, `/v1/cards`, `/v1/teams`,
  `/v1/team-invitations` and `/v1/vaults` answer from Go. `/v1/vault/files`, `/v1/me/today`
  and `/health` answer from Swift.

### G3 production watch: running, 72 h ends 2026-11-07 19:42Z

- Owner's check still open: one real team invitation email.

## knowledge (wave 4)

### G0 entry: met 2026-11-05

- Routes: 4 in `docs/migration/routes.json`: `GET /v1/knowledge/graph`,
  `POST /v1/knowledge/connections/explain`, `POST /v1/knowledge/edges/{id}/confirm` and
  `/dismiss`. `/v1/knowledge/reason` is `knowledge-reason`, held for group 4F.
- Prefixes: `/v1/knowledge/connections`, `/edges`, `/graph`, disjoint (`internal/manifest`).
- Coupling: reads and reviews the `knowledge_*` tables only. No vault-file writes, no path
  handling, no PostHog captures.
- Jobs: `KnowledgeExtractionWorker` stays in Swift and writes the same tables.
- Swift tests: `KnowledgeVaultScopeIntegrationTests` 1 ported. `KnowledgeReasoningPathTests`
  (3) go with 4F, `KnowledgeExtractionWorkerTests` (9) stay with the worker.
- Quirks kept: the BYO exemption can never decide these routes, since `memoryQuery` reaches
  everyone but archived and archived is excluded from BYO. A NaN `minimumConfidence` gives
  an empty graph. With enforcement off, a bad body answers before 401. An edge into another
  tenant's node is a 500. Path `id` is a new UUID per response.

### G1 parity: met 2026-11-05

- CI run 41947259214 on head cc3d0ea: self-test (Swift vs Swift) and Swift vs Go give
  **115 scenarios, 0 differences** (whole suite), and every switch state has 0 differences.
  `parity/scenarios/knowledge` has 7 files and 152 steps, 9 with snapshots, plus 9 steps in
  `billing-on`. `parity`, `lint`, `test` and `vuln` pass (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41947259214/job/126982428061).
- Go tests: `internal/modules/knowledge` has 8 tests on Postgres, including spec conformance.

## vision (wave 4)

### G0 entry: met 2026-11-05

- Routes: 2: `POST /v1/vision/embed` and `POST /v1/vision/search`.
- Prefix: `/v1/vision`, disjoint (`internal/manifest`).
- Coupling: writes `memories.embedding` (`memorystore.SetEmbedding`, no `updated_at` change)
  and upserts `usage_meter` once per successful embed. Both servers read the same memories.
- Jobs: none.
- Swift tests: `VisionSearchLimitTests` 7 ported, plus 10 of the 15 in
  `BYOEntitlementMiddlewareTests` in `internal/entitlement/platform_test.go`. Of the other 5,
  two are about Hermes resolution, which these routes don't use, and three are covered by
  neighbouring cases and `TestEntitledMatrix` rather than ported one for one.
- Config: the sealed names in infra are `VISION_EMBED_PROVIDER_COHERE_APIKEY` and `_BASEURL`.
  Swift's key encoder reads `_API_KEY` and `_BASE_URL`, and Go reads the same names. The
  `vision-cohere` state shows both servers read those spellings. So production and staging
  most likely answer 503 on these routes from either server. That is read from the code and
  the sealed names, not checked on the cluster. Sealing the right name starts paid calls, so
  it is the owner's decision.
- Quirks kept: `hermesVision` can't be selected, because the name is lowercased first. A
  Cohere key with `provider=openai` is a 503. `/search` checks `limit` before reading the
  body and `/embed` checks `indexAs` after. An empty body goes to the provider. `;image/png`
  is accepted. `X-Vault-ID` is ignored. Search counts no hits.

### G1 parity: met 2026-11-05, with one uncompared failure path

- CI run 41947259214 on head cc3d0ea: **115 scenarios, 0 differences** (whole suite).
  `parity/scenarios/vision` has 7 files and 59 steps, 1 with a snapshot. These run without a
  key, so success paths are in switch states: `vision-stub`, `vision-cohere` (Cohere stand-in
  on the feeds stub), `vision-cohere-down` and `billing-on`, 5 files and 37 steps, 8 with
  snapshots, all 0 differences (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41947259214/job/126982428061). Those states save no transcripts.
- Go tests: `internal/modules/vision` has 16 tests, on Postgres and with `httptest` stand-ins
  for Cohere. No real key is used.
- Gap, not compared: when the upstream closes the connection without a response, Swift gave
  no answer within the harness's 30 s (run 41946466833), and Go answers 502 at once. Swift's
  final answer is unknown, so the step was removed. A refused connection reaches the same
  branch and matches. Only reachable once a Cohere key is live.
- Small differences: the Cohere client has a 60 s total timeout where URLSession's is
  idle-based. Embed uses `context.WithoutCancel`, to mirror Swift running on after a client
  disconnect.

## kb (wave 4)

### G0 entry: met 2026-11-05

- Routes: 1: `GET /v1/memory-compile/pending`. `POST /v1/memory-compile` is `kb-compile`,
  held for group 4F.
- Prefix: `/v1/memory-compile/pending` (`internal/manifest`).
- Coupling: read-only count on `vault_files`, by identity. `X-Vault-ID` is not read.
- Jobs: none.
- Swift tests: `KBCompilePendingTests` 4 ported. The other `Tests/AppTests/KB` suites cover
  `POST /v1/memory-compile` and its services, and go with 4F.

### G1 parity: met 2026-11-05

- CI run 41947259214 on head cc3d0ea: **115 scenarios, 0 differences** (whole suite).
  `parity/scenarios/kb` has 2 files and 11 steps, 1 with a snapshot, plus 2 steps in
  `billing-on`. `parity`, `lint`, `test` and `vuln` pass (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41947259214/job/126982428061).
- Go tests: `internal/modules/kb` has 6 tests on Postgres, including spec conformance.

## todos (wave 4)

### G0 entry: met 2026-11-05

- Routes: 4 in `docs/migration/routes.json` (`GET` and `POST /v1/todos`, `PATCH` and
  `DELETE /v1/todos/{id}`).
- Prefix: `/v1/todos`, disjoint (`internal/manifest`).
- Coupling: a todo is a vault file, so it uses `internal/vaultfiles` and
  `internal/memorystore`. Swift publishes nothing from the controller.
- Jobs: none.
- Route middleware: `jwtAuthenticator`. Go uses `auth.Middleware`.
- Swift tests: no suite of their own. The todo half of `DiskSpaceGuardHTTPTests` is ported.

### G1 parity: met 2026-11-05

- Seed: `parity/seed/todos.sql` has `todo_tia` and `todo_tom`, with views that mask the
  random file name. Swift's list has no `ORDER BY`, so the seeds and scenarios keep each
  `done` and `dueAt` position unique: a tie comes back in whichever order Postgres scans.
- Parity: `parity/scenarios/todos` has 5 files and 109 steps: order, filters, create,
  patch, promotion of any vault file, delete with the memory cascade, decode errors and
  isolation. `disk-full` proves the 507 with nothing written.
- CI run 41972727611 on head 5d5df73: self-test and Swift vs Go both give **123 scenarios, 0
  differences**, and every switch state gives 0. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41972727611/job/127064448711).
- Go tests: `internal/modules/todos` has 13 tests on Postgres, including spec conformance.
- Quirks kept: `PATCH` and `DELETE` take any vault file of the tenant, and `PATCH` makes it
  a todo. A due date or project can't be cleared. The file name's UUID isn't the row's id.
- Path check: `delete` passes `row.Path` to `ResolveInside` without `SanitizePath`, as Swift does.

## capture (wave 4)

### G0 entry: met 2026-11-05

- Routes: 1 in `docs/migration/routes.json` (`POST /v1/capture/safari`).
- Prefix: `/v1/capture`, disjoint (`internal/manifest`).
- Coupling: `AchievementsWorker` inbox through `internal/achievementsink`,
  `OnboardingLatches` through `internal/onboardlatch`. EventBus `vault_file_created` is
  dropped because its only subscriber logs.
- Jobs: link enrichment is a detached task in both servers, lost if the process stops.
- Route middleware: `jwtAuthenticator`, `EntitlementMiddleware(.capture)`, then
  `captureByUser`, in that order.
- Swift tests: `URLExtractionServiceTests` 11, `URLEnricherGuardTests` 5,
  `CaptureHookDispatcherTests` 6, `JinaEnricherTests` 7 and the capture half of
  `DiskSpaceGuardHTTPTests` are ported. `CaptureFilingServiceTests` 3 go to 4F.

### G1 parity: met 2026-11-05

- Seed: `parity/seed/capture.sql` has six users, spaces, plugin installs and views that
  mask the time and random suffix in the path.
- Enrichment race: both servers enrich for real against the feeds stub, with `r.jina.ai`
  refused in compose. Steps that compare the result wait on `parity_capture_pending`
  (`waitEmpty`). `/capture/slow` holds a page so the placeholder is compared too.
- Parity: `parity/scenarios/capture` has 3 files and 117 steps. `disk-full` proves the 507
  and `billing-on` the gate.
- Spec drift: `api/openapi.yaml` answers 200 where Swift answers 202. The DTOs are
  hand-written, and the spec test validates the body against the 200 schema.
- CI run 41972727611 on head 5d5df73: **123 scenarios, 0 differences** in both comparisons, and
  every switch state gives 0. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41972727611/job/127064448711).
- Go tests: `internal/capture` has 48 tests, including 1143 `URL(string:)` cases captured
  from Swift. `internal/modules/capture` has 15 on Postgres.
- Differences kept on purpose: the fetch timeout covers the whole request, pages are capped
  at 32 MiB, and oEmbed requests carry Go's default User-Agent.
- Open for the owner: `URLEnricherGuard` has no DNS or redirect check, and it passes `127.1`,
  `2130706433`, `0x7f.0.0.1` and `[::ffff:127.0.0.1]`. Go keeps this for parity.
- Path check: `CaptureLink` and `enrichAndRewrite` use `ResolveInside` without `SanitizePath`,
  as Swift does.

## vault (wave 4)

### G0 entry: met 2026-11-05

- Routes: 12 under `/v1/vault`. The prefix is a string prefix of `/v1/vaults`, already on Go
  in production, so the flip's ingress entry must name `/v1/vault` and `/v1/vaults` both.
- Coupling: `SOUL.md` and the default Spaces are written by `create`. `vault_file_created`
  has one log-only subscriber and is dropped. No route writes `memory_chunks`: move updates
  them, a re-upload's memory update deletes them, a file delete cascades.
- Jobs: none move. `DiskSpaceMonitorService` stays in Swift. `VaultNoteMemoryUpsertQueue` has
  no caller in the controller, whose note upload is inline, so it isn't ported.
- Swift tests: 32 in scope, 31 ported.
  - In scope: `VaultCRUDTests` 13, `VaultUploadSpaceTests` 6, `VaultInitTests` 3,
    `VaultExportTests` 3, `VaultLargePayloadTests` 2, `TenantVectorIndexServiceTests` 4, and
    the vault half of the `DiskSpaceGuardTests` route case.
  - Not ported: `dropIndex removes it`. `dropIndex` has no caller in Swift, despite a comment
    in `App+build.swift` saying it's wired to account deletion.
- DTOs: `VaultFileDTO` and `VaultMoveRequest` are hand-written, because the spec has drifted
  (no `spaceId`, `updatedAt` or actor fields; `{from, to}` where the server takes
  `{path, newPath}`). Spec conformance tests skip list and move for that reason, and read,
  delete and export, which have no JSON body to check.

### G1 parity: met 2026-11-05

- CI run 41971535305 on head 89ce369: self-test (Swift vs Swift) and Swift vs Go give
  **112 scenarios, 0 differences** (whole suite). `parity/scenarios/vault` has 13 files and
  258 steps, 39 with snapshots and 32 with vault-tree checks. `parity`, `lint`, `test` and
  `vuln` pass (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41971535305/job/127060707141).
- Export needed a new harness step, `zip: true`: entry names, flags, sizes and normalized
  contents, plus the archive's total size. Entry times are not compared.
- The oracle settled four behaviours:
  - Hummingbird never decodes a path, so `/v1/vault/files/inbox/caf%C3%A9.md` is a 400 even
    though upload can store `inbox/café.md`.
  - Over 32 MiB with an Idempotency-Key, the handler sees an empty body: upload answers 400
    `empty body`, not 413. `internal/idempotency` now does the same for every module.
  - GET and DELETE `/v1/vault/files/move` are a bare 404, with no auth.
  - `String(data:encoding:)` and `String(contentsOf:encoding:)` drop one leading byte order
    mark, in note memories and in lint's empty-document check.
- Not visible to parity, covered by Go tests: the Hermes profile copy of `SOUL.md`, the 507
  from a full disk, the achievement sink and PostHog captures, and a connection cut when an
  export fails mid-stream.
- Gaps:
  - The note upload's Space suggestion (`CAPTURE_AUTOFILE_ENABLED`) isn't ported. The switch is
    off in production, and the suggestion arrives with group 4F. Turning it on before then has
    no effect on Go.
  - A vault over 65,535 entries traps the whole Swift process. In Go only that export fails.
  - `SOUL.md` covers the default template only. `/v1/soul` stays in wave 8 and moves the rest.

## ingestion (wave 4)

### G0 entry: met 2026-11-05

- Routes: 10, under `/v1/ingestions` and `/v1/ingestion-sources`.
- Prefixes: disjoint (`internal/manifest`).
- Coupling: the routes write `ingestion_batches`, `ingestion_items`, `ingestion_events`,
  `vault_files` and the volume. Capability checks, LLM calls, link capture and push all stay in
  the worker.
- Jobs: none move. `MultimodalIngestionWorker` stays in Swift (`SKIP LOCKED` plus a lease, on the
  same volume). Go's chunk and assembled files are mode 0644 and their directories 0755, the same as
  Swift's (`files` steps).
- Swift tests: 14 ported.
  - 5 of 6 `MultimodalIngestionAPIIntegrationTests`, the ingestion route of `DiskSpaceGuardTests`,
    the ingestion payload of `APNSNotificationServiceTests`, and 7 `StorageQuotaServiceTests`.
  - Not ported, because they test the worker: "expired worker lease is recovered",
    `MultimodalIngestionCapabilityTests` (3) and `HermesIngestionTransportTests` (2).
- `INGESTION_PUBLIC_BASE_URL`: Go doesn't read it. Only the worker builds source URLs from it. It
  keeps pointing at the API host, so Hermes reaches Go through the ingress after the flip.

### G1 parity: met 2026-11-05

- CI run 41971580058 on head eabd4e3: self-test (Swift vs Swift) and Swift vs Go give
  **108 scenarios, 0 differences** (whole suite). `parity/scenarios/ingestion` has 9 files and 263
  steps: 41 with snapshots, 25 with `files`, 14 with `frames`. `parity`, `lint`, `test` and `vuln`
  pass (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41971580058/job/127060848060).
- Swift's storage quota never counts stored bytes. `usedBytes` decodes `COALESCE(SUM(size_bytes), 0)`,
  a numeric, as `Int64`. Under `try?` that reads as 0, so only one batch's declared bytes meet the
  ceiling, and the 413 always says "0 B of ...". Parity shows it: a free tenant holding 1073741000 B
  and a pro tenant holding 99 GiB were both allowed past their ceilings. Go keeps this
  (`usedBytes` in `internal/modules/ingestion/quota.go`). Fixing it is an owner decision.
- The source route reads `vault_files.path` without `SanitizePath`, as Swift does.
- `DiskSpaceMonitorService`'s periodic `Sample` isn't wired in Go. The guard still checks the disk
  on each request.

## memory (wave 4)

### G0 entry: met 2026-11-05

- Routes: 13: `GET /v1/me/suggestions`, `POST /v1/memory/upsert`, `GET /v1/memory`,
  `/graph`, `/facets`, `/local-sync`, and on `/v1/memory/{id}`: `GET`, `PATCH`, `DELETE`,
  `GET /lineage`, `/related`, `/provenance`, `POST /review`. `/v1/memory/search` stays on
  Swift (`heldPaths`, `memory-agent`), and `/v1/memory-compile` is its own module.
- Coupling: writes `memories`, `memory_contributions`, `memory_chunks` (content patch),
  `memory_sync_tombstones` and `memory_index_jobs` (delete), `kb_compile_reject_list`
  (reject), `analytics_events`, and `vault_files` plus the file itself when a note-backed
  memory moves. No PostHog captures in these routes.
- Paid calls: upsert and a content patch call `internal/embedding`, so they spend on
  staging. The `smoke: true` scenario (`08-smoke.yaml`) reaches neither path.
- Jobs: the index, heal, pruning and layout workers stay in Swift. `HitCountQueue.Run` is
  not wired: `related` skips hit counts and `/search` is held. Memory shares vault's
  `startAchievementSink` in `cmd/App`, which now takes `InvalidateToday` as its `OnUnlock`.
- Paths: the move's destination goes through `vaultfiles.MoveDestination`, so
  `SanitizePath` applies. Its source is `vault_files.path` from the database, which only
  `ResolveInside` checks.
- Swift tests: 49 ported. `Memory/MemoryCRUDTests` 9, `Geo` 3, `GraphController` 3,
  `GraphService` 8, `Lineage` 4, `Related` 1, `SavedID` 4, `SpaceMove` 3, `Correction` 3,
  `Kind` 2, `LocalMemorySyncCursor` 3, `Suggestions` 2, the health filter 1, and
  `EmbeddingDegradation` 2 plus one more `Related` already in `internal/memorystore`. Left
  out: `a note upload returns its memory id` and `moving a note into a Space folder` (both
  drive `/v1/vault/files`, group 4A); `MemoryScoring`, `MemoryPruningService`,
  `MemoryKeepGate`, `GraphLayoutService` (workers, per the plan).
- Quirks kept:
  - `/v1/me/suggestions` needs no token and spends the `meTodayByUser` bucket.
  - Upsert never dials Hermes, but a BYO Hermes that fails resolution answers 502 before
    the body is read.
  - `/v1/memory/graph` skips `ModelDisclosurePolicy`, so a managed tenant sees origin
    models there and nowhere else.
  - A patch with `spaceId` and empty `content` moves the memory, then answers 400.
  - Review bodies are read to 4096 bytes, and bytes 0x85 and 0xA0 count as blank.
  - A local-sync cursor can land a microsecond before its row, so that row repeats on the
    next page.
  - A path ending on a literal (`DELETE /v1/memory/graph`, `GET /v1/memory/upsert`) is a
    404 with no body, while `/v1/memory/graph/lineage` reaches `:id` and answers 400. With
    `HYBRID_EXECUTION_ENABLED=false`, `/v1/memory/local-sync` reaches `:id` too.
  - The capture gate's own-Hermes exemption can't decide this route: `capture` reaches
    everyone but archived, and archived is excluded from BYO.
- Differences from Swift, both deliberate:
  - Space hubs on the graph are sorted by id. Swift maps a `Dictionary`, so its order
    changes per process, and the fixture has one hub per tenant. A Go test covers several.
  - `?offset=` near `Int.max` traps Swift's `offset + limit`. Go answers normally. Not in
    parity, since it would kill the Swift server.
- Spec: wrong for upsert's response (names `MemoryDTO`), `MemoryLineageResponse` (another
  shape), and thin on `MemoryDTO` and `MemoryUpsertRequest` (no geo or actor fields). Those
  DTOs are hand-written from LuminaVaultShared 5.32.0.
- Harness: `stableOrder` also orders by `occurred_at` and `deleted_at`, and snapshots
  normalize UUIDs inside text, for `analytics_events` and `memory_sync_tombstones`.

### G1 parity: met 2026-11-05

- CI run 41975659123 on head 735b083: self-test (Swift vs Swift) and Swift vs Go give
  **147 scenarios, 0 differences** (whole suite), and every switch state has 0 differences,
  including the new `hybrid-off` and `billing-on/memory`. `parity/scenarios/memory` has 10
  files and 629 steps, 137 with snapshots, with `memory_chunks` chunk IDs compared after
  content patches. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/41975659123).
- The file move with bytes on disk is compared too: `03-patch.yaml` uploads a note through
  `/v1/vault/files` and moves its memory, with `files` steps on the tenant tree.
- Go tests: `internal/modules/memory` has 70 tests, on Postgres where they need it,
  including spec conformance. `internal/memorystore/provenance_test.go` has 2.

## llm-voice (wave 5)

### G0 entry: met 2026-11-05

- Routes: 3: `POST /v1/transcribe`, `POST /v1/audio/transcriptions`, `POST /v1/audio/speech` (the 501).
- Prefix: `/v1/audio`, `/v1/transcribe`, disjoint (`internal/manifest`).
- Coupling: writes `usage_events` (metric `voice_transcribe`) and `usage_meter`. Hermes calls whisper
  directly, so voice rows stay at zero in production; accepted.
- Jobs: none.
- Swift tests: `MultipartFormParserTests` 15, `OpenAICompatibleTranscribeAdapterTests` 7,
  `TranscribeProviderConfigTests` 9, `TranscribeServiceUsageTests` 8, `VoiceUsageTests` 12.

### G1 parity: met 2026-11-05

- CI run https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42071272337 on `b4f3711`: 165
  scenarios and every switch state, 0 differences. The stub answered 62 calls on `/v1/audio/transcriptions`.
- Not compared: the in-flight limit under concurrency and a slow upload (both Go tests), metrics, daily
  limits other than free's cap of 10.
- For G2: send one scoped audio token through the ingress. Check the sealed `TRANSCRIBE_PROVIDER` and
  `TRANSCRIBE_PROVIDER_OPENAI_BASE_URL`: this group couldn't read them.

## tts (wave 5)

### G0 entry: met 2026-11-05

- Routes: 1: `POST /v1/tts`, mounted only when `TTS_BASE_URL` is set. Unset, it returns 404, which is
  production's inferred behaviour. A `curl` settles that. Owner to confirm: open question 12.
- Prefix: `/v1/tts`, disjoint (`internal/manifest`).
- Coupling: upserts `usage_meter` characters. No OpenAI speech adapter: the service is self-hosted and gets
  no `Authorization` header.
- Jobs: none.
- Swift tests: `OpenAITTSAdapterTests` 5, against the self-hosted service. The bearer-key assertion is
  inverted, since Go sends none.

### G1 parity: met 2026-11-05

- CI run https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42071272337 on `b4f3711`: switch
  states `tts-on`, `tts-billing` and `billing-on`, plus the off state, 0 differences. The stub answered 42
  calls on `/v1/audio/speech`.
- Not compared: `RouteTelemetry`, which Go has no equivalent for, and metrics.

## Flip batches 3 and 4: account, onboarding, marketplace, calendar, mail

### G2 staging deploy: smoke met 2026-11-05, on `aaad82e`

- Image: `api-go-staging` ran `aaad82e` for about 4 h with 0 restarts and 0 errors or 5xx in 2,625 log lines.
- Smoke: a throwaway staging user per server against Swift transcripts from that commit's CI run, with
  Swift staging as the control: 0 mismatches. `account` needs its own user and a token under 5 minutes old.

### Production flip: 2026-11-05

- Infra #557 merged 07:08:11Z. `api-go-production` runs `aaad82e`; its pod started 07:10:48Z. All five paths
  are on `api-go-production` in the live ingress.

### G3 production watch: running, 72 h ends 2026-11-08 07:11Z

- To 11:00Z: 0 restarts and 0 errors, but only `/health` in Go's log. Swift logged `/health`, `/v1/mcp` and
  scanner noise in the same hours. This shows the deploy is healthy, not that real clients behave the same.
- Not yet observed: `health_events_synced` in PostHog, and a marketplace plugin tool run.

## Flip batch 5: vault, todos, capture, ingestion, memory, knowledge, vision, kb

### G2 staging deploy: met 2026-11-05, on `cda9718`

- CI run 41977996741 on that commit: 155 scenarios, 0 differences.
- Image: `api-go-staging` ran `cda9718` from 06:19Z to at least 10:59Z: 0 restarts, 0 5xx, 2,936 log lines.
  One ERROR: OpenRouter 402 on a todo create, because staging's key is out of credits. The todo saved without
  an embedding. Swift staging logged the same failure 110 times in 24 h.
- Smoke: a fresh throwaway user per module and server, Swift staging as the control, 22 modules. 17 had
  0 mismatches. projects, profiles, news, spaces and teams report the same mismatches on both servers
  (seed-ID steps), with identical output.
- Real capture of `https://example.com/` on staging: 202 and the same enriched 555-byte file on both
  servers, through the keyless Jina path, which is also production's: `JINA_API_KEY` isn't in
  `api-env-production`.

### Production flip: 2026-11-05 11:32:09Z, infra #563

- Image `cda9718`. Verified publicly after the sync: no `Server` header on the moved paths.
- First real client traffic at 12:22Z: vault list and read, onboarding, memory-compile/pending and
  me/suggestions returned 200 after the client's token refresh (401 first, then 200 on retry).
  `/v1/news/ticker` returned 404 for the same client: the plugin isn't installed, pinned by `news/errors`.
- G3 72 h watch ends 2026-11-08 11:35Z.

- Holds on Swift: `/v1/memory/search` and `/v1/memory-compile`. `/v1/memory-compile/pending` goes to Go.
  `/v1/knowledge/reason` stays on Swift because only three knowledge sub-paths move.

## memory-agent (wave 4)

### G0 entry: met 2026-11-05

- Routes: 5: `POST /v1/memory/search`, `POST /v1/query`, `POST /v1/query/stream`, and
  `GET`+`POST /v1/memos`. `/v1/memory/search` stays in `memory`'s
  `heldPaths` until the ingress flip.
- Coupling: writes `memories` hit counts through `HitCountQueue` (one instance, in
  `cmd/App/agent.go`, on the agent's repositories only), `retrieval_telemetry_events` and
  `analytics_events`. A memo writes a vault file through `vaultfiles` and its own slug path.
  No PostHog captures in these routes.
- Paid calls: `/v1/query`, `/v1/query/stream` and `POST /v1/memos` call Hermes, and search
  embeds its query. The `smoke: true` scenario stops at validation or an empty result, so
  no smoke step spends money.
- Jobs: `RetrievalTelemetryWorker` stays in Swift. Go inserts telemetry directly
  (`PGTelemetry`), so the worker's inbox isn't used.
- Route middleware: `Identify` and the BYO Hermes check on all. Search adds the `MemoryQuery`
  gate and tiered `CaptureByUser`. Query and stream add tiered `QueryByUser`, the same gate
  and `ratelimit.InFlightLimit`. Memos add the `MemoGenerator` gate.
- Paths: memo persist builds its own slug path and skips `SanitizePath`, as Swift does.
- Spec drift: `QueryRequest` lacks `session_id`, `MemorySearchResponse` says `results` where
  Swift sends `{hits, summary}`, and the memo DTOs differ. Hand-written DTOs; spec
  conformance covers `GET /v1/memos` only, and parity pins the other bodies.
- Swift tests: 30 ported. `Memory/HermesMemoryServiceTests` 8, `FollowUpGeneratorTests` 9,
  `QueryStreamTests` 5, `MemoGeneratorTests` 4, `MemoControllerTitleTests` 4.
- Differences: hit counts and telemetry are asynchronous, so parity snapshots leave them out;
  Go tests cover them. The parallel executor isn't ported: `CERBERUS_PARALLEL_ENABLED` is
  false in production and the stack warns if it is set.

### G1 parity: met 2026-11-05

- CI run 42066861088 on head e5cc4ea: self-test (Swift vs Swift) and Swift vs Go give
  **169 scenarios, 0 differences**, and every switch state has 0 differences, including `llm`
  (8 scenarios) and `import-links` (1). `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42066861088).
- `parity/scenarios/memory-agent` has 5 files and 73 steps;
  `parity/scenarios-switched/llm/memory-agent` has 46 steps against the deterministic
  `parity/stubs/llm`, whose `[[stub:echo]]` replies with the canonical request so prompts
  and tool schemas are compared.
- Go tests: `internal/modules/memoryagent` has 36 tests, on Postgres where they need it.

## knowledge-reason (wave 4)

### G0 entry: met 2026-11-05

- Routes: 2: `POST /v1/knowledge/reason`, `/reason/stream`, under the module's own prefix, so
  the ingress flip moves them.
- Coupling: reads the knowledge graph and evidence through `internal/modules/knowledge`.
  Writes nothing. No PostHog captures.
- Paid calls: both call Hermes. Its scenarios that can run on staging stop at validation or
  an empty graph.
- Jobs: none.
- Route middleware: the same stack as the rest of `/v1/knowledge`: `Identify`, the
  `MemoryQuery` gate and tiered `CaptureByUser`.
- Swift tests: 3 ported, `Knowledge/KnowledgeReasoningPathTests`.
- Differences: with the gateway port closed, Swift holds the frame after `context` for more
  than 20 s, up to its 180 s deadline (inferred: AsyncHTTPClient retries the connect), and
  Go falls back to the grounded answer at once. `parity/scenarios` therefore compares the
  `context` frame only, and the frames after it are compared in the `llm` state.

### G1 parity: met 2026-11-05

- CI run 42066861088 on head e5cc4ea: self-test (Swift vs Swift) and Swift vs Go give
  **169 scenarios, 0 differences**, and every switch state has 0 differences, including `llm`
  (8 scenarios) and `import-links` (1). `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42066861088).
- `parity/scenarios/knowledge-reason` has 3 files and 45 steps;
  `parity/scenarios-switched/llm/knowledge-reason` has 10 steps.
- Go tests: `internal/modules/knowledge/reason_test.go` has 3 and `reason_http_test.go` 5,
  including conformance for `/reason`, not for the stream.

## kb-compile (wave 4)

### G0 entry: met 2026-11-05

- Routes: 1: `POST /v1/memory-compile`. `heldPaths` keep it on Swift, and they release at the
  flip below. `LegacyCompileAliasTests` stays with `app`, which owns the alias.
- Coupling: `internal/memorycompile` writes `memories`, `commitments`, `usage_events`,
  `vault_files.processed_at` and the `spaces` counters, and reads wiki files (no
  `SanitizePath`, as Swift). No PostHog captures.
- Paid calls: the compile calls Hermes and embeds. The smoke uses a user with no files, so
  it spends nothing.
- Jobs: none here. Commitment parsing runs inside the compile, so 4F took
  `Me/CommitmentsTests`' parse and compile cases.
- Route middleware: `Identify`, idempotency, the BYO Hermes check, the `MemoryCompile` gate and
  tiered `ratelimit.MemoryCompileByUser`.
- Flip needs: Go's `/v1/ws`, a `ConnectionManager` behind `BroadcastPublisher.Broadcast`, and
  `DeviceCommandBroker` callers. Flip `/v1/memory-compile`, `/v1/ws`, `/v1/devices/command`,
  `mcp` and `skills` in one infra PR at the end of wave 5, after `/v1/ws` is split from `app`.
  Until then Go publishes through `NoopPublisher`.
- Spec drift: `forceFullRecompile` is required by Swift.
- Swift tests: 22 ported, plus 7 from `Me/CommitmentsTests` (5 parse, 2 compile) and 2 from
  `Onboarding/GuidedStartTests`, whose stranded-latch case relies on `internal/onboardlatch`.
  `KB/KBCompileControllerTests` 5, `MemoryCompileControllerTests` 6 (duplicates merged, 6 Go
  tests), `HappyPath` 1, `MemoryUpsert` 1, `ProgressPublisher` 2, `ProgressService` 3,
  `SpaceCounters` 4. The first publisher test asserts the `Broadcast` message, as Go has no
  `ConnectionManager` yet; the commitments test reads the rows in SQL, not through
  `CommitmentsService.list`.
- Differences: progress frames are not sent until the flip. `durationMs` is ignored where
  rows exist, and Go tests assert it is positive.

### G1 parity: met 2026-11-05

- CI run 42066861088 on head e5cc4ea: self-test (Swift vs Swift) and Swift vs Go give
  **169 scenarios, 0 differences**, and every switch state has 0 differences, including `llm`
  (8 scenarios) and `import-links` (1). `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42066861088).
- `parity/scenarios/kb-compile` has 2 files and 28 steps, with snapshots;
  `parity/scenarios-switched/llm/kb-compile` has 11 steps.
- Go tests: `internal/modules/kbcompile` has 10 and `internal/memorycompile` 23.

## import (wave 4)

### G0 entry: met 2026-11-05

- Routes: 7: `POST /v1/import`, `/bookmarks`, `/files`, `/vault-bulk`, `GET /v1/import/{id}`,
  `POST /{id}/categorize`, `/{id}/approve`. `GET /v1/import/files`, `/bookmarks` and
  `/vault-bulk` answer 404, which reproduces Hummingbird's trie.
- Coupling: writes `import_sessions`, `import_items`, `vault_files` and the files; approve
  moves files and may create Spaces through `spaces.Module.CreateSpace`. Links stage through
  the shared `linkcapture.Service`, the one `POST /v1/capture/safari` uses. No PostHog captures.
- Paid calls: categorize calls Hermes, and staged links reach enrichment. The smoke stops at
  validation.
- Jobs: none. Enrichment runs in the shared link pipeline.
- Route middleware: `Identify` only, as in Swift.
- Paths that skip `SanitizePath`, as Swift does: approve (moves from `rawRoot + vf.path`, new
  path from the last component), the categorize snippet read, and vault-bulk (`SafePath` plus
  `ResolveInside`).
- Swift tests: 13 ported, `Import/ImportLogicTests`. This group also ported the note-upload
  Space suggestion and `Capture/CaptureFilingServiceTests` 3 (`internal/capture/filing.go`,
  which skips `SanitizePath` as Swift does). It runs behind `CAPTURE_AUTO_FILE_ENABLED`, off in
  production, and the `llm` state turns it on.
- Differences: the categorize mapping can't be pinned in parity because item ids are random; a
  Go test reads them out of the prompt, and covers the cap of 6 new Spaces. Links get their
  own `import-links` state because their paths hold a time and a random suffix.

### G1 parity: met 2026-11-05

- CI run 42066861088 on head e5cc4ea: self-test (Swift vs Swift) and Swift vs Go give
  **169 scenarios, 0 differences**, and every switch state has 0 differences, including `llm`
  (8 scenarios) and `import-links` (1). `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42066861088).
- `parity/scenarios/import` has 4 files and 74 steps, with snapshots;
  `parity/scenarios-switched/llm/import` has 9 steps and `import-links/import` 18.
- Go tests: `internal/modules/imports` has 14 tests, including conformance for `/files`,
  `/vault-bulk`, status, categorize and approve.

## grok (wave 5)

### G0 entry: met 2026-11-05

- Routes: 4 in `docs/migration/routes.json`: `POST /v1/grok/chat`, `/x-search`, `/vision`, `/tts`.
- Prefix: `/v1/grok`.
- Coupling: reads `users` through `entitlement.PremiumGuardMiddleware` and
  `hermes_tenant_containers` through `hermescontainers.Handle`. No writes, no vault paths, no
  PostHog captures, no rate limit. `/tts` is always 501, so no speech provider is involved.
- Jobs: none.
- Paid path: the tenant's own xAI OAuth, confirmed in code. The server sends the container's
  `API_SERVER_KEY`; the xAI token stays in the container (`hermes auth add xai-oauth`). The
  `smoke: true` scenario stops at 401 and 402.
- Mounting: only with `LV_SECRET_MASTER_KEY` set, as in Swift (state `no-master-key`).
- Swift tests: `GrokControllerTests` 4 and `PremiumGuardMiddlewareTests` 2, all ported.
- Quirks kept:
  - Order is 402, then 400 (decode, validation), then 409, then the upstream call.
  - A reply that isn't valid HTTP, including a connection closed mid-reply, is a bare 400 that
    closes the client's connection: Hummingbird's server treats AsyncHTTPClient's
    `HTTPParserError` as its own.
  - An upstream body over 8 MiB is a bare 413, on any status.
  - A key that won't open, a reset, a redirect cycle and a 21st redirect are bare 500s.
  - Redirects follow AsyncHTTPClient, not net/http: 304 and 305 with a `Location` are followed,
    and a redirect to another origin drops the bearer but keeps the body.
  - `stream` is validated and never sent. x-search has no missing-answer failure. Vision drops
    `usage`. Upstream keys go through `convertFromSnakeCase`, so camelCase keys match too.

### G1 parity: met 2026-11-05

- CI run 42067436288 on head 9f772a4: self-test (Swift vs Swift) and Swift vs Go give
  **164 scenarios, 0 differences** (whole suite), and every switch state has 0 differences.
  `parity/scenarios/grok` has 9 files and 201 steps, 4 with snapshots, plus 6 steps in
  `no-master-key`. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42067436288/job/127385406130).
- Upstream calls are compared: the stub's default answer repeats the host, bearer and canonical
  body it received. Swift's transcript shows 200s with the seeded host and key for each tenant.
- Go tests: `internal/modules/grok` has 11 tests, 10 on Postgres, including spec conformance on
  the 200s and the 402.
- Not compared:
  - An unreachable container. Swift doesn't answer within the runner's 30 s timeout, so the step
    was removed. Go answers 500 at once. Swift's final status is read from code as a 500.
  - The value of `publishedAt`: the runner masks timestamps, so only the format is compared. A Go
    test pins an offset date to UTC.
  - The 60-second upstream deadline.
  - Compressed replies. Swift's client negotiates decompression with a 25x limit; Go uses
    net/http's transparent gzip.
- Spec: the 409, 501 and 502 responses point at `ErrorEnvelope`, whose `error` is a string. Both
  servers send `{"error":{"message":...}}`. That schema has 107 uses, so it wasn't changed here.

## workflows (wave 5)

### G0 entry: met 2026-11-05

- Routes: 25 in `docs/migration/routes.json`: 21 under `/v1/workflows`, 3 under
  `/v1/workflow-templates`, and `POST /v1/workflow-hooks/{hookID}`.
- Prefixes: `/v1/workflow-hooks`, `/v1/workflow-templates`, `/v1/workflows`.
- Coupling: writes `workflows`, `workflow_versions`, `workflow_runs`, `workflow_node_runs`,
  `workflow_run_events`, `workflow_approvals` and `workflow_webhooks`. Creating a workflow also
  notes self-improvement activity. It reads `users`, `user_llm_preferences`, `memories` and
  `workflow_spend_buckets`. No vault paths, no PostHog captures.
- Jobs: none. `WorkflowEngine`, `WorkflowScheduler`, `WorkflowMaintenanceService` and
  `LegacyJobWorkflowMigrator` stay in Swift and poll the rows these routes write. They don't run
  in parity (`LV_ENVIRONMENT=test`).
- Paid path: none on the request path. Runs are executed by Swift's engine.
- Mounting: `CERBERUS_STUDIO_ENABLED` (default true) mounts every route or none, as in Swift
  (states `studio-off` and `studio-on`). The entitlement gate covers `/v1/workflows` only
  (`billing-on`).
- Swift tests: `WorkflowLifecycleIntegrationTests` 2, `WorkflowValidatorTests` 12 (with
  `WorkflowTierPolicyTests` and `WorkflowSchedulePolicyTests`) and `WorkflowWebhookSignatureTests`
  2, all ported. The engine's reserve and reconcile stay in Swift, so the lifecycle test writes the
  spend they would book.
- Quirks kept:
  - `WorkflowEventStore.list` decodes rows with SQLKit's default keys and fails on any row. The
    event list is a 500 whenever the run has events, and the stream is a 200 that ends at once.
    `Options.DecodeEventRows` turns on the fix.
  - Handlers read the body, then the token, then the path id.
  - Routes without `mapErrors` (limits, run lists, approvals, the webhook) answer bare 500s
    where others say 403, 409 or 429. Trial can't list but gets 403 on details. A duplicate
    workflow name is a 500.
  - A decision isn't one transaction: a bad memory attachment answers 400 after the approval is
    decided.
  - Hummingbird resolves the path before the method, so `GET /v1/workflows/validate`, and `:id`
    routes reached through `runs/` or `approvals/`, are bare 404s.
  - Webhook payloads flatten as corelibs' JSONSerialization and `NSNumber.stringValue` do: Int64,
    UInt64, Decimal, then Double. A Double overflow keeps only `raw`. Booleans are `1` and `0`.
  - A cron field of `/` alone traps the Swift process. Go answers `invalidCron`.

### G1 parity: met 2026-11-05

- CI run 42073449919 on head a4f2d6d: self-test (Swift vs Swift) and Swift vs Go give
  **166 scenarios, 0 differences** (whole suite), and every switch state has 0 differences.
  `parity/scenarios/workflows` has 11 files and 693 steps, 70 with snapshots. There are 48 more
  steps in `studio-off`, `studio-on` and `billing-on`. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42073449919/job/127407261812).
- Go tests: `internal/modules/workflows` has 33 tests, 12 on Postgres, including spec
  conformance, event frames with decoding on, client disconnect on the stream, and the rate limit.
- Not compared:
  - Live execution, because the engine doesn't run in parity.
  - Event frames with rows, because Swift can't produce them. A Go test covers them.
  - The number of keep-alives and the timing of polls on the event stream.
  - The rate limit itself, because a scenario would depend on the clock.
  - A webhook payload whose nested object has two or more keys: Swift's key order is random.

## self-improvement (wave 5)

### G0 entry: met 2026-11-05

- Routes: 12, under `/v1/me/improvement`.
- Prefixes: disjoint (`internal/manifest`).
- Coupling: the routes write `self_improvement_settings`, `self_improvement_runs`,
  `self_improvement_changes`, `skills_state`, and on the volume `SOUL.md` (approve) and skill files
  (rollback). Reject writes no file. Rollback never writes SOUL. Approve also copies SOUL into the Hermes
  profile, which only Go tests cover: `files` steps see `tenants/<ID>` only.
- Jobs: none move. `SelfImprovementScheduler` stays in Swift and claims the rows `curator/runs` and
  `soul/reviews` queue. A Go test mimics the claim SQL. Parity doesn't prove the pickup, so check it at G2.
- Swift tests: 12 of 13 ported: 6 `SelfImprovementPolicyTests` and 6 of 7 `SelfImprovementServiceTests`.
  Not ported, because it tests the scheduler: "tick dry-run skips pinned skill and applies zero mutations".
- Paths: `skillFile` and `restore`'s `os.MkdirAll` and `os.RemoveAll` skip `SanitizePath`, as Swift does.
  Their names and paths come from `snapshot_json`. Parity shows `../outside-skills` writing outside
  `skills/` on both servers. Hardening them is an owner decision.

### G1 parity: met 2026-11-05

- CI run 42071476680 on head 4301416: self-test (Swift vs Swift) and Swift vs Go give
  **164 scenarios, 0 differences** (whole suite). `parity/scenarios/self-improvement` has 9 files.
  Switch state `improvement-off`: 1 scenario, 0 differences. `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/42071476680/job/127400255939).
- First parity coverage of real SOUL bodies: `05-soul-bodies.yaml` approves 22 bodies with a `files` step
  after each, and the transcripts show each approve returned 200. `internal/soul` showed no difference.
- Swift bug kept: Fluent's `@ID(custom: "tenant_id")` on `SkillsState` makes `save()` on a loaded row
  update every `skills_state` row of the tenant. So pinning one skill pins all of them, and a rollback
  sets one curator state on all of them. Parity confirms it. Fixing it is an owner decision.
- An empty `archivedPath` is skipped in Go. Swift would target the working directory. The curator never
  writes that value, so parity doesn't cover it.

## agents (wave 5)

### G0 entry: met 2026-11-11

- Routes: 10, under `/v1/agents`: instances, sessions, a session's messages, room candidates, and rooms
  (list, create, get, delete, post as SSE `event: agent.room`, stop).
- Prefixes: disjoint (`internal/manifest`).
- Coupling: reads `conversations`, `conversation_messages`, `user_hermes_profiles`, `hermes_profiles` and
  `user_hermes_config` (through `hermesresolve`). Writes only `agent_rooms`, `agent_room_members` and
  `agent_room_messages`. No vault paths, so no `SanitizePath` call sites.
- Jobs: none.
- LLM: the central agent speaks through the shared stack's `ChatService` (`internal/llm`, not forked). BYO
  members go to the user's own gateway, which never follows redirects.
- Swift tests: all 23 ported: 8 `AgentGatewayClientTests`, 6 `AgentRoomTurnPolicyTests`,
  6 `AgentRoomOrchestratorTests`, 2 `AgentRoomsControllerTests`, 1 `AgentsControllerTests`.

### G1 parity: met 2026-11-11

- CI run 43003550424 on head 198f80f: Swift vs Go gives **213 scenarios, 0 differences** (whole suite).
  `parity/scenarios/agents` has 5 files. The new switch state `agents` gives 3 scenarios, 0 differences.
  `parity`, `lint`, `test` and `vuln` pass
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/43003550424/job/130465298732).
- The `agents` state points the gateway at `llmstub` and allows private ranges over HTTP. Its stub
  counters prove that both servers really ran the BYO and central paths: 10 calls on `/api/instance`,
  10 on `/api/sessions/{id}/chat` and 20 on `/v1/chat/completions`.
- Go tests only: the 409 `room_busy` and a chain stopped by the client leaving the stream, because both
  depend on timing.
- Not compared: the key order of a re-encoded multi-key `tool_calls` object, because Swift's
  `JSONSerialization` order is unspecified.
- G2 needs a staging stub for the success path. Staging has no Hermes.

## me-providers (wave 5)

### G0 entry: met 2026-11-11

- Routes: 14, under `/v1/me/providers`, `/v1/me/preferences/llm` and `/v1/me/connections`. The nine
  provider routes are mounted only with `LV_SECRET_MASTER_KEY`, as in Swift (state `no-master-key`).
- Prefixes: disjoint (`internal/manifest`).
- Coupling: writes `user_provider_credentials`, `user_provider_credential_pool_keys`,
  `user_llm_preferences`, `router_profiles` (the preferences save rewrites the default profile) and
  `connection_diagnostic_events`. Every write goes through the agent's one `llm.Stack`, so the
  router's credential, preference and profile caches see it. Swift keeps its own 600 s caches, so
  this module flips with 5C. The memory and workflows modules build their own `PreferenceRepository`
  and keep a stale preference until their cache expires.
- Paid path: `/test` and `/models` call the user's own provider key. The tool probe after a passing
  `/test` runs detached, as a Swift `Task`. User base URLs are dialed unchecked, as in Swift (no SSRF
  guard). Hardening that is an owner decision.
- Swift tests: `ProviderCatalogTests` 6, `ProviderModelListingTests` 5, `ProviderToolProbeTests` 9,
  the probe half of `OpenAICompletionReaderTests` and `Settings/LLMPreferencesControllerTests` 11,
  all ported.

### G1 parity: met 2026-11-11

- CI run 43007663996 on head 2337ec9: self-test and Swift vs Go give **232 scenarios, 0 differences**
  (whole suite), and every switch state shows 0 differences
  (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/43007663996).
  `parity/scenarios/me-providers` has 8 files. Switch states: `no-master-key` (provider routes 404)
  and `llm` (`/test` and `/models` success against the stub).
- Spec drift: the spec's `HermesGatewayID` has no `photon`, which Swift lists in every summary.

## llm-router (wave 5)

### G0 entry: met 2026-11-11

- Routes: 17, under `/v1/router`.
- Prefixes: disjoint (`internal/manifest`).
- Coupling: writes `router_profiles`, `router_bindings`, `router_synthesis_presets` and deletes
  `router_executions`. It shares the `llm.Stack` caches with `me-providers`.
- Parallel: every `/parallel` and `/synthesis-presets` route is 403 unless `cerberus.parallelEnabled`
  is on and the user is Ultimate. It is off in production and parity. `ParallelExecutor` stays a hook
  (owner decision 7). With parallel on, Go's stream writes `token`, `done` and `error` frames only,
  and records no execution.
- Swift tests: `RouterPlaceholderRoutesTests` 7 and `RouterPlaceholderSaveTests` 4, ported.
  `ParallelExecutorTests` 2 not ported: waiver proposed in PR #55.

### G1 parity: met 2026-11-11

- CI run 43007663996 on head 2337ec9: 0 differences (see me-providers). `parity/scenarios/llm-router`
  has 9 files.
- Swift bug kept: `/v1/router/dashboard` is a 500 on every call, because `RouterController` decodes
  `SUM(bigint)`, which Postgres returns as numeric, into `Int64`. `Options.FixDashboard` serves it.
  Turning that on is an owner decision.
- Quirks kept: a BYOK write without `routingPolicy` defaults to `autoSmart` and needs an OpenRouter
  credential (`AUTO_REQUIRES_OPENROUTER`). A duplicate profile name and unbinding the user scope are
  bare 500s. A workflow binding loads the Fluent model, so a workflow whose draft doesn't decode is a
  500 in Swift (Go reads only the row's existence).

## llm-admin (wave 5)

### G0 entry: met 2026-11-11

- Routes: 1, `GET /admin/llm/health`, behind `X-Admin-Token` (empty token: 404, state `unconfigured`).
- It pings every OpenAI-compatible adapter with the deployment key. It is not in smoke.
- Swift tests: none.

### G1 parity: met 2026-11-11

- CI run 43007663996 on head 2337ec9: 0 differences. The error text and latency are ignored: the error
  is Go's text, not Foundation's `String(describing:)`.

## chat (wave 5)

### G0 entry: met 2026-11-11

- Routes: 15. `/v1/conversations`: create, list, get, delete, traces and the SSE `messages/stream`. Local
  execution: `messages/prepare`, `messages/commit`, `local-executions/{executionID}` (DELETE) and its
  `/tools`. Also `/v1/chat/inbox`, `/v1/me/chat-preferences` and `/v1/me/preferences/hybrid-execution`. The
  local-execution routes and the hybrid document are mounted only with `hybridExecution.enabled` (state
  `hybrid-off`), and `/tools` only with `localExecution.toolBroker.enabled`.
- Prefixes: disjoint (`internal/manifest`).
- Coupling: writes `conversations`, `conversation_messages`, `prepared_local_executions`,
  `user_chat_preferences`, `agent_turn_traces`, memory outputs (`EnqueueOutput`), retrieval telemetry and
  `hermes_profiles` (through the profile middleware). No vault paths are read or written, so there are no
  `SanitizePath` call sites. SOUL.md is read through `internal/soul`.
- Escalation: owner decision 1, option A. An escalated turn starts its run on the Swift API through
  `CHAT_HERMES_RUNS_BRIDGE_URL` (`chat.Bridge`, behind the `chat.HermesRuns` interface). When it is unset,
  turns never escalate, which matches Swift without a master key. Through the bridge, Swift keys the run on
  `activeHermesSessionKey`, not on the vault UUID. Chat and 6C must flip in one infra commit (B12).
- LLM: `internal/llm` is reused as is (`StreamService`, the Cerberus and failover sinks, the disclosure
  scrubs, `SystemPromptGuard`, `StreamErrorFor`, `SanitizeToolOutput` and `TraceRecorder`). No
  temperature, top_p, top_k or prefill was added to the Anthropic adapter.
- Swift tests ported: `ChatAttachmentPrompt` 9, `HermesEscalationPolicy` 9, `ChatGrounding` 3,
  `ChatSoulPrompt` 2, `ConversationE2E` 11 of 12, `ConversationEscalationE2E` 4, `Conversation` 7,
  `AgentTurnTraceRoundTrip` 4 of 5, and the chat half of `UsabilityEndpoints` (2).
- Swift tests not ported:
  - `LLMPreferencesChatE2E` 3: they need signup and `/v1/me/preferences/llm` (me-providers) end to end.
  - The free-tier `/v1/query/stream` case in `ConversationE2E`: that route belongs to memory-agent.
  - The Fluent re-save case in `AgentTurnTraceRoundTrip`.
  - The first-query cases in `GuidedStart`.

### G1 parity: met 2026-11-11

- CI run 43034886380 on head 52d3c1a: the self-test and Swift vs Go each give **240 scenarios,
  0 differences** (whole suite), and every switch state shows 0 differences. `parity`, `lint`, `test` and
  `vuln` pass (https://github.com/LuminaVault/Lumina-Vault-Server-Go/actions/runs/43034886380).
  - `parity/scenarios/chat` has 6 files, with seed `parity/seed/chat.sql`.
  - State `hybrid-off`: the unmounted routes.
  - New state `chat`: the stub as the gateway, a fake OpenRouter key, and Go's runs bridge on
    `swift2-switched` sharing Go's database. Its stub counters show 22 calls on `/v1/chat/completions` and
    4 on `/v1/runs`.
- Swift bug kept: `prepare` is always a bare 500 after the user's message is stored.
  `PreparedLocalExecution.messages` is a `[ChatMessage]` `@Field`, which Fluent binds as `jsonb[]` into a
  `jsonb` column. So no execution ever exists, and tools, commit and cancel can only answer 404.
  `Options.FixLocalPrepare` stores the row instead. Turning it on is an owner decision.
- Not compared: what an escalated run does after its `hermes_run` frame, because it finishes
  asynchronously.

## llm-chat (wave 5)

### G0 entry: met 2026-11-11

- Routes: 1, `POST /v1/llm/chat`. Middleware: jwt, BYO Hermes, `Entitlement .chat`, chatByUser, the
  in-flight limit, and the context router (`skills.ContextRouter`).
- It covers URL pre-enrichment (`X-Skip-URL-Enrichment`), the usage budget (`X-LuminaVault-Degraded`),
  the detached APNs push, the `chatCompleted` achievement and the `.firstQuery` latch.
- Swift tests ported: `URLExtractor` 8 and `LLMControllerPushSideEffect` 4.

### G1 parity: met 2026-11-11

- CI run 43034886380 on head 52d3c1a: 0 differences. `parity/scenarios/llm-chat` has 2 files, and state
  `chat` has the success path through the stub.
- Bug found and fixed: a message without `tool_calls` was rejected. Shared's `ChatMessage` makes the field
  optional.

## ws (wave 5)

### G0 entry: met 2026-11-11

- Routes: 1, `GET /v1/ws` (`App+build` `websocketGroup`), split from `app` into pseudo-module `ws`.
  Middleware: JWTAuthenticator only. The device result route `POST /v1/devices/command/{id}/result` is
  already on Go in `apple`, on the same `devicerpc.Broker`; it stays in apple's `heldPaths` until the
  bundle flips.
- Upgrade decision, from hummingbird-websocket 2.7.0 `router.ws`: no user gives a 405 to a plain GET and
  a bare 400 to a handshake (NIO's upgrader); a user with a plain GET gives an empty 200.
- kb-compile's progress publisher now broadcasts on the same sockets (`memorycompile.BroadcastPublisher`).
- Swift tests: `WebSocketBroadcastGuard` 9 and `DeviceCommandBroker` 2 were ported in P5.

### G1 parity: met 2026-11-11

- `parity/scenarios/ws`: refusals, the plain GET, guard drops and relay, and a smoke file. Fan-out to a
  second socket and the 1009 close are Go tests.
- Flip: only with the ws bundle (`ws`, the device result route, `kb-compile`, `mcp`, skills).

## mcp (wave 5)

### G0 entry: met 2026-11-11

- Routes: 6. `POST /v1/mcp` (originGuard, `mcpAnonymousByIP`, lv_ key or JWT, `mcpByUser`) and
  `/v1/me/agent-connections` list, preview, issue, update and revoke (JWT, `settingsByUser`).
- JSON-RPC 2.0, protocol `2025-06-18`, server `0.2.0`, 1 MiB body (413), notifications 202, any
  `Origin` 403. All 17 tools in Swift's order with its annotations and limits.
- `internal/devicetools` ports PersonalDataTools for the personal tools; 5E reuses it.
  `chunkindex.Backfiller` ports ChunkBackfillService for `index`.
- Config: `MCP_PUBLIC_BASE_URL`, falling back to `INGESTION_PUBLIC_BASE_URL`, then
  `https://api.luminavault.com`.
- Swift tests: `MCPContract` 16 and `MCPSetup` 10 as unit tests; `AgentConnectionsController` 7 and
  `MCPMemoryTools` 9 as DB tests.

### G1 parity: met 2026-11-11

- `parity/scenarios/mcp` 01-06 with `parity/seed/mcp.sql`. Ignored: tool `content` text (Swift
  serializes a dictionary in random key order; `structuredContent` is compared), `memory_context`'s
  `soul` (the default SOUL embeds the current second), and a new key's token, prefix and setup.
- Memories saved over MCP are forgotten within the scenario, so their randomly keyed chunks never reach
  later modules' `memory_chunks` snapshots.
- Flip: only with the ws bundle.

## skills (wave 5)

### G0 entry: met 2026-11-11

- Routes: 8. `GET /v1/skills`, `PATCH /v1/skills/:name`, `GET /v1/skills/:name/runs`,
  `POST /v1/skills/:name/run`, `POST /v1/skills/slash`, `GET /v1/skills/outputs`, `POST /v1/jobs`,
  `POST /v1/jobs/detect`.
- `internal/modules/skills/runner.go` ports SkillRunner: cap guard, meter budget check, CerberusScope
  routing, 6-iteration agent loop with the 13 tools in Swift's order (`tools.go`), outputs (memory,
  vault file, push, chat message, job result), `skill_run_log` + `skills_state` persistence, cap
  refund on failure. daily-brief composes the brief through `me.ComposeBrief` first.
- `/v1/skills/slash`'s `/kb-compile` reuses `kbcompile.Compile`; device tools go through
  `internal/devicetools` and this process's broker.
- Kept from Swift, pinned until the post-archive security pass: `skills_state` PATCH updates every
  row for the name.
- Known gaps: `skill_run_log.error` is Swift's `String(describing:)` text; snapshots compare
  `error IS NOT NULL`. A constant-segment 404 runs auth first in Go (differs only unauthenticated).

### G1 parity: met 2026-11-11

- `parity/scenarios/skills` 01-09 with `parity/seed/skills.sql` (views `parity_sk_runs`,
  `parity_sk_state`, `parity_sk_briefs`), and `parity/scenarios-switched/llm/skills/01-run.yaml`
  (jobs, cap, blocks, tool loop, slash `/patterns`, daily-brief) on the LLM stub.
- Flip: only with the ws bundle (device tools resolve through `/v1/ws`).

## hermes-runs (wave 6, 6C)

The `hermes` module stays `swift`: 6A and 6B (`/v1/hermes` mirror and webhooks) are not ported. 6C's routes are
served by Go but flip only with chat, in one infra commit (B12, decisions.md D-6c-runs).

### G0 entry: met 2026-11-11

- Routes: 6, `Hermes/Runs/HermesRunsController.swift:34-39`. `POST`/`GET /v1/hermes/runs`, `GET /{id}`,
  `GET /{id}/events` (SSE, event name `hermes.run.event`, replay then live), `POST /{id}/approval`,
  `POST /{id}/stop`. They are mounted only with `LV_SECRET_MASTER_KEY`, as in Swift (state `no-master-key`).
- Middleware, in Swift's order: jwt, byo, hermesProfile, `conversationByUser`, `Entitlement .chat`.
- Coupling: reads and writes `hermes_runs` and `hermes_run_events`, and inserts assistant turns into
  `conversation_messages` (idempotent on `hermes_run_id`). Events are published on an in-process bus, and the feed
  re-reads every 5 s, so a run watched by another process still streams.
- Job: `HermesRunsService` (boot re-attach for runs younger than 300 s), registered and off unless `LV_GO_JOBS`
  names it.
- Pushes: `apns.CategoryApproval` and `apns.CategoryRunCompleted`, through `apns.Service`.
- Swift tests: all 78 ported.
  - `HermesRunEvent` 15, `HermesRunsClient` 14, `HermesRunsService` 17, `HermesRunConversationPersistence` 5,
    `HermesRunsListFilter` 4, `HermesRunsController` 11.
  - `HermesRunPersistence` 8: the M117–M119 cases check the goose baseline.
  - `HermesRunPushNotifier` 4: the prefs-table case goes through `apns.NewWithSender` with a recording sender.
  - Go adds a stream-idle client test and a tool-name test.
