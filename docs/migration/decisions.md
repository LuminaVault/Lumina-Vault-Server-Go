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

## D-jobs: findings from the partial job inventory (Task 0.4, unfinished)

Registered in `Sources/App/App+build.swift`. Claim mechanisms are not read yet.

- `CronScheduler` (L3624) and `ReminderScheduler` (L3634) are documented as
  **single-replica** (comment at L3626), so they must never run in both servers.
- The in-process `EventBus` (built at L483) has subscribers `MeTodayCache` (L1851)
  and `skillRunner.startEventSubscriptions()` (unmanaged `Task`, L3612). It needs
  a row in `coupling.md`.
