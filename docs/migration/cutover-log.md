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
