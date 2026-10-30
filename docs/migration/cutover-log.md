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
  (60/min per user). Go has no rate limiting yet, so it must exist before G2.
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

### G1 parity: not met (stays `swift`)

- In the parity env, CI run 41120945344 gives 0 differences for 1 scenario, 7 steps:
  401 without a token or with a bad one, and 503 `news_ticker_not_configured` for
  alice (installed, enabled) and bob (installed, disabled).
- Gap: the env sets `LV_SECRET_MASTER_KEY` (so `/v1/news` is mounted) but has no
  `FEEDS_BASE_URL`. Swift therefore stops at the 503, so none of these are compared
  against Swift:
  - install state (404/409)
  - SecretBox config
  - feed list
  - aggregator fetch (502/500)
  - the 60 s per-process cache
  
  They are covered only by Go tests against a stub aggregator (`internal/modules/news`).
  Reaching G1 needs a stub aggregator in `docker-compose.parity.yml` with
  `FEEDS_BASE_URL` pointing at it, for both servers.
- Route middleware: `jwtAuthenticator` plus `RateLimitMiddleware(.settingsByUser)`
  (30/min, a bucket shared with `/v1/plugins`). Not ported.
