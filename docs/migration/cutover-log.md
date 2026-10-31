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
- Go tests: `internal/modules/apple` (22 tests) port 18 Swift assertions sets
  (all of the above except the broker) and validate bodies against `api/openapi.yaml`.
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
