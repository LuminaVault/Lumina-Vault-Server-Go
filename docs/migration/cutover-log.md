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

### G2 staging deploy: not started

Blocked on human steps:
1. ~~Merge LuminaVaultInfra #486 (chart) and #487 (netpol).~~ Merged 2026-10-29.
2. Merge PR #1 to `main`, which publishes the first image. `INFRA_REPO_TOKEN` must be set.
3. Put that SHA into #488 and merge it.
4. Provide a staging session token for `make parity-smoke MODULE=projects`.

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
