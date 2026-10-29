# Migration decisions

Decisions taken while executing
`LuminaVaultServer/docs/superpowers/plans/2026-10-29-go-echo-migration-phase-0-4.md`.
Where a decision overrides the plan, the plan's wording is quoted.

## D-repo: Go code lives in this repo, not `LuminaVaultServer/go/`

The owner created `LuminaVaultServerV2` (`github.com/LuminaVault/Lumina-Vault-Server-Go`)
for the Go server. Every plan path `go/<x>` maps to `<x>` at this repo's root
(`go/modules.yaml` → `modules.yaml`, `go/testdata/golden` → `testdata/golden`, …).
Migration docs (`routes.json`, `config-keys.txt`, `jobs.md`, `coupling.md`,
`cutover-log.md`, this file) live in `docs/migration/` here. The Swift repo keeps
only its own tests and spec changes. `api/openapi.yaml` is a vendored copy of
`LuminaVaultServer/Sources/AppAPI/openapi.yaml`, and `api/SPEC_SOURCE` holds the
Swift commit it was copied from. The Swift repo stays the source of truth.

## D-stack: Echo v5 and Go 1.27, from the owner's scaffold

The plan says "Go 1.26, Echo v4". The owner's scaffold uses
`github.com/labstack/echo/v5 v5.4.2`, `go 1.27.3` and module path `lumina`. These
win. What that means in practice:

- Handlers take `*echo.Context`. The logger is `*slog.Logger`. Startup goes through `echo.StartConfig`.
- JWT uses `github.com/labstack/echo-jwt/v5` (v5.0.2) with `golang-jwt/jwt/v5`.
- `otelecho` has no v5 module, so tracing wraps the Echo handler with the official `otelhttp`. A small middleware renames the span to the matched route (`c.Path()`).
- oapi-codegen's `echo-server`/`strict-server` generators and `oapi-codegen/echo-middleware` target Echo v4. Codegen decision D2 (Task 1.1) settles what is generated for v5.

## D-deps: dependency vetting

The plan requires a Sonatype check before every dependency. The Sonatype MCP has
failed authentication since 2026-10-29, which is out of the agent's control.
Until it is restored, `govulncheck ./...` (Go's official vulnerability database)
runs in CI and locally, and every dependency is listed below for a Sonatype
pass once the token is refreshed.

| Module | Version | Purpose |
|---|---|---|
| github.com/labstack/echo/v5 | v5.4.2 | HTTP framework (owner's choice) |
| github.com/caarlos0/env/v11 | v11.4.1 | env → Config |
| github.com/golang-jwt/jwt/v5 | v5.3.1 | session JWT verification |
| github.com/google/uuid | v1.6.0 | UUIDs |
| golang.org/x/crypto | v0.58.0 | HKDF (SecretBox), bcrypt later |
| github.com/stretchr/testify | v1.11.1 (tidy resolved) | tests |

govulncheck as of 2026-10-29: 0 vulnerabilities reachable. One unreachable
advisory, GO-2026-5932 (`x/crypto/openpgp` is unmaintained). We never import
that package.

## D-keys: config key → env encoding

`config.KeyToEnv` follows swift-configuration's EnvironmentKeyEncoder as
described in `ProviderRegistry.swift`: split on `.`, camelCase boundaries become
`_`, uppercase. The behaviour of an acronym followed by a word
(`idleTTLSeconds` → `IDLE_TTL_SECONDS`) is an assumption. Check it against
the encoder source in the Swift `.build/checkouts/swift-configuration` before
porting any key that has one (`hermes.perTenant.idleTTLSeconds`).

## D-auth: own JWT middleware instead of echo-jwt

The roadmap names `labstack/echo-jwt`. The Go middleware is about 60 lines on
top of `golang-jwt/jwt/v5` instead, because Swift's semantics don't map onto
echo-jwt's: every failure is 401 (including scoped tokens and a missing user
row), a token without `kid` verifies with the active key, and an unknown `kid`
verifies with nothing. Writing it directly keeps each rule visible and tested
(`internal/auth/auth_test.go`). The legacy `JWT_HMAC_SECRET` path does not
enforce the 32-character minimum; it is unverified whether Swift does.

## D-golden-tenant: fixture tenant UUID has hex letters

The plan's Task 0.3 fixture tenant was `11111111-2222-3333-4444-555555555555`.
It contains no hex letters, so its upper and lower case are identical and it
cannot detect the UUID-case trap. Task 0.3 now uses
`ABCDEF01-2345-4678-9ABC-DEF012345678` (plan updated in the phase-0 worktree).

## D-lint: golangci-lint v2.15.0, no goimports local-prefix

The installed golangci-lint 2.13.2 can't read Go 1.27.3 export data. CI and
local runs use v2.15.0. Because module path `lumina` has no dot, gofumpt groups
it with the standard library, which conflicts with goimports `local-prefixes`,
so that setting is off. Renaming the module to
`github.com/LuminaVault/Lumina-Vault-Server-Go` would restore normal import
grouping and avoid a dotless path; that rename is the owner's call.

## D2: codegen is models-only oapi-codegen, handlers hand-written on Echo v5

Spike run 2026-10-29 with oapi-codegen v2.8.1 (pinned as a `go tool`):

- It reads the **3.1** spec directly. No 3.1→3.0 down-convert step is needed, so `@apiture/openapi-down-convert` is not used.
- With `type-mapping` (`string/uuid → wire.UUID`, `string/date-time → wire.Time`), all 11.5k generated lines compile, with zero `time.Time` and zero `openapi_types.UUID` fields left. Nullable `type: [x, 'null']` becomes a pointer with `omitempty`, matching Swift's omitted-nil encoding (still to be confirmed by the golden fixtures).
- None of its server generators target Echo v5. It has Echo v4, chi, gin, gorilla, fiber, iris and std-http, so no `strict-server` for Echo v5, and `oapi-codegen/echo-middleware` is Echo v4 too.

Decision: each module generates **models only** from its spec tag
(`internal/modules/<m>/oapi-codegen.yaml`, `include-tags: [<m>]`). Echo v5
handlers are hand-written, and each module test validates its responses
against `api/openapi.yaml` with kin-openapi, which already comes with
oapi-codegen. That keeps the spec as the enforced contract without the
generated server.

## D1: wire date format (provisional until Task 0.3)

The iOS client decodes every API response with `JSONDecoder.dateDecodingStrategy = .iso8601`
(`LuminaVaultClient/API/Core/Endpoint.swift:70`), which accepts only whole-second
internet date-time. So Swift must emit `2026-10-29T10:30:15Z`. `wire.Time`
marshals that way and rejects fractional seconds on input. `TestTimeMatchesGolden`
turns this assumption into a checked fact once the golden fixtures exist.

## D-pilot: the pilot is Projects, not Todos

Reading `TodosController.swift` showed that Todos is not CRUD:
- `create` writes `inbox/<UUID>.md` into the tenant vault on the PVC, inserts a `vault_files` row with JSON metadata, and creates an embedded memory through the embedding provider chain.
- `delete` moves the file to a timestamped `_deleted_*` path and cascades the memory.

Piloting it would mean porting vault paths, the memory repository and
embeddings, which is wave-4 machinery. `ProjectsController.swift` is
tenant-scoped CRUD on `projects`. Its only cross-module touch is reading
`vault_files.metadata` JSON for `todoCount`, which usefully exercises decoding
JSONB that Swift wrote. Projects is wave 1; Todos moves to wave 4
(`scripts/gen-modules.py` WAVES; the roadmap and the phase plan are updated).

## D-todos: Todos are vault notes

The spec describes `TodoDTO` as "Backed by a vault note (`metadata.isTodo`)".
The pilot therefore reads and writes the notes/memories data, and possibly vault
files on the PVC, not a dedicated `todos` table. Task 4.1 snapshots and the
Task 4.2 queries must target those tables. Read `Sources/App/Todos/TodosController.swift`
before Task 4.1.

## D3: runtime UID/GID 999 (provisional)

The Swift runtime stage runs `useradd --user-group --create-home --system ... hummingbird`
on `ubuntu:noble` after installing `libjemalloc2 ca-certificates tzdata libcurl4`.
Replaying exactly that sequence in a clean `ubuntu:noble` container gives
`uid=999(hummingbird) gid=999(hummingbird)`, so the Go image (distroless/static)
runs as `999:999`. Confirm it against the real image with
`docker run --rm --entrypoint id ghcr.io/luminavault/luminavaultserver:<tag>`
once the image can be pulled (needs `read:packages`). Until then 999 is derived, not observed.

## D-release: staging bump is a direct commit, like Swift's

The plan said `release.yml` would open an infra PR. The established
convention (LuminaVaultServer `release.yml`) commits the staging tag straight
to LuminaVaultInfra `main` with `INFRA_REPO_TOKEN`. Staging only; production
goes through human-reviewed promotion. The Go workflow mirrors that, bumping
`apps/api-go/values-staging.yaml`. Agents still never merge infra PRs:
production image tags and every `ingress.extraPaths` change are PRs a human
merges. The repo needs the `INFRA_REPO_TOKEN` secret before the first release.

## D0: route inventory is static, not a Swift test

Hummingbird 2.26 keeps the router trie `internal` (`Router.trie`), so a test
cannot list routes. The plan's fallback applies: `LuminaVaultServer/scripts/route-inventory.py`
statically resolves group prefixes, controller wiring and `addRoutes`
registrations. Result on main @ fdbed9b0: 468 routes; 0 unresolved; 38 were
missing from the spec, and the spec listed the OAuth exchange route twice in
concrete form. It flags every route-shaped call site it cannot reach, so silent
misses show up as "unresolved". `docs/migration/routes.json` is copied here
from the Swift repo.

## D-tools: migration tooling lives in this repo

`scripts/schema-snapshot.sh` (Task 0.5), `tools/golden` (Task 0.3),
`scripts/parity-up.sh`, `docker-compose.parity.yml` and `scripts/gen-modules.py`
live here, not in the Swift repo as the plan said. They drive the **Swift
image** (the real binary) rather than a Swift test target, so no 20-minute
Swift test build is needed and the fixtures come from production code paths.
The Swift repo keeps only what must run in its own CI: `scripts/route-inventory.py`,
`scripts/tag-spec.py` and `scripts/swift-freeze.sh`.

## D-parity-ci: scenarios define scope; the Swift image is pinned

The `parity` CI job runs every directory under `parity/scenarios` rather than
filtering by status, because scenarios only exist for modules being migrated.
The Swift image is pinned in `parity/SWIFT_IMAGE` and bumped deliberately, so
it doesn't need read access to the private infra repo. The job needs the
`luminavaultserver` GHCR package to grant this repo read access (a package
setting).

## D-shared-prefixes: Devices, Reminders and Apple flip together

`scripts/gen-modules.py` finds three modules whose routes cannot be separated
by path prefix. Apple registers `/v1/devices/command/{id}/result`,
`/v1/devices/commands/pending` and `/v1/reminders/sync`, under the prefixes of
Devices and Reminders. `modules.yaml` records them as `flipsWith` partners
(all wave 2); `internal/manifest` tests that the partnership is mutual and in
one wave.

## D-jobs: findings from the partial job inventory (Task 0.4, unfinished)

Registered in `Sources/App/App+build.swift`. Claim mechanisms are not read yet.

- `CronScheduler` (L3624) and `ReminderScheduler` (L3634) are documented as
  **single-replica** (comment at L3626), so they must never run in both servers.
- The in-process `EventBus` (built at L483) has subscribers `MeTodayCache` (L1851)
  and `skillRunner.startEventSubscriptions()` (unmanaged `Task`, L3612). It needs
  a row in `coupling.md`.
