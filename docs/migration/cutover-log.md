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
