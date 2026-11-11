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

## D1: wire date format (CONFIRMED by golden capture 2026-10-29)

The iOS client decodes every API response with `JSONDecoder.dateDecodingStrategy = .iso8601`
(`LuminaVaultClient/API/Core/Endpoint.swift:70`), which accepts only whole-second
internet date-time. So Swift must emit `2026-10-29T10:30:15Z`. `wire.Time`
marshals that way. The capture corrected one assumption: on input the Swift
server **accepts** fractional seconds and numeric offsets
(`"2026-11-01T09:00:00.500Z"` and `"…11:00:00+02:00"` both give 200, and both
are emitted as `"2026-11-01T09:00:00Z"`), so `wire.Time` accepts them too.
`TestTimeMatchesGolden` round-trips every timestamp in the capture.

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

## D3: runtime UID/GID 999 (CONFIRMED)

The Swift runtime stage runs `useradd --user-group --create-home --system ... hummingbird`
on `ubuntu:noble` after installing `libjemalloc2 ca-certificates tzdata libcurl4`.
Replaying exactly that sequence in a clean `ubuntu:noble` container gives
`uid=999(hummingbird) gid=999(hummingbird)`, so the Go image (distroless/static)
runs as `999:999`. Confirm it against the real image with
`docker run --rm --entrypoint id ghcr.io/luminavault/luminavaultserver:<tag>`
once the image can be pulled. Confirmed 2026-10-29 on an image built locally from
main @ fdbed9b0: `uid=999(hummingbird) gid=999(hummingbird)`.

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

## D-err: error and decode wording (CONFIRMED by golden capture)

`testdata/golden/errors-http.json`, captured from the Swift binary:

- Errors with a message: `{"error":{"message":"…"}}`, `Content-Type: application/json; charset=utf-8` (`wire.JSON`, `wire.HTTPErrorHandler`). The spec's `ErrorEnvelope` schema (`{error: string, message}`) is wrong and must be fixed in the spec.
- Unknown route: 404 with an empty body and no Content-Type.
- Missing, invalid or scoped token on a protected route: 401 `Authenticated identity is unavailable` (`wire.ErrUnauthenticated`).
- `wire.Bind` reproduces Foundation's DecodingError wording verbatim:
  - not JSON or an empty body: `The given data was not valid input: The given data was not valid JSON.`
  - missing key: ``Coding key `name` not found.``
  - null: ``Value not found for `name` key.``
  - wrong type: ``Type mismatch for `archived` key, expected `Bool` type.`` (a top-level non-object gives key `` and `Dictionary<String, Any>`)
  - bad UUID or date: `The given data was not valid input: Attempted to decode UUID from invalid UUID string.` / `…Expected date string to be ISO8601-formatted.`
- Unknown fields are ignored.
- Nested fields (confirmed by the wave-2-apple parity run): a missing key names only the key
  (``Coding key `completed` not found.``); null and type mismatches name the dotted coding
  path with array indices (``Type mismatch for `reminders.1.priority` key, expected `Int` type.``).
  A null where a non-optional **Bool** is required is a type mismatch
  (``Type mismatch for `allowed` key, expected `Bool` type.``), not "Value not found";
  every other type gives "Value not found". `wire.Bind` does all of this.
- Create endpoints that return a DTO answer **200**, not 201. Delete answers 204.

## D-jwt-numeric-dates: Swift mints fractional iat/exp

jwt-kit encodes `iat`/`exp` as JSON doubles (`1793276047.4986272`), and `hpid`
is the username. golang-jwt verifies them fine. When Go starts minting (wave 8),
it must emit the same claim set; whether integers are acceptable to every
client must be checked then.

## D-jobs: findings from the partial job inventory (Task 0.4, unfinished)

Registered in `Sources/App/App+build.swift`. Claim mechanisms are not read yet.

- `CronScheduler` (L3624) and `ReminderScheduler` (L3634) are documented as
  **single-replica** (comment at L3626), so they must never run in both servers.
- The in-process `EventBus` (built at L483) has subscribers `MeTodayCache` (L1851)
  and `skillRunner.startEventSubscriptions()` (unmanaged `Task`, L3612). It needs
  a row in `coupling.md`.

## D-wave2-early: wave 2 is ported while the pilot is still in G2/G3

The phase-0-4 plan (Task 4.5 step 5) starts wave 2 only after the pilot's
retro. The owner chose on 2026-10-30 to port wave 2 to `parity` in parallel,
because the pilot's G2/G3 steps are waiting on human merges and a 7-day watch.
What stays gated: no wave-2 module goes past `parity` (no staging status, no
`extraPaths`) until `projects` is `done` and `pilot-retro.md` exists. Anything
the retro changes in the recipe is applied to the wave-2 modules before they
flip.

## D-seed-files: one seed file per module

`parity/seed/base.sql` holds the shared users, vaults and pilot rows.
`scripts/parity-up.sh` now loads it first and then every other
`parity/seed/*.sql` in name order, so each module adds `parity/seed/<module>.sql`
instead of editing `base.sql`. That keeps the parallel wave-2 branches from
conflicting. CI runs the Swift-vs-Swift self-test before the Go comparison, so
every PR proves its scenarios are deterministic.

## D-identify: auth.Identify for routes that check something before identity

Swift's `JWTAuthenticator` never rejects. It attaches the user when the token
is valid, and the 401 comes later from `requireIdentity()` inside the handler.
`auth.Middleware` rejects up front, which matches every route that calls
`requireIdentity()` first. `VaultAccessService.resolve` does not: it reads
X-Vault-ID first, so a malformed header is a 400 even with no token. Routes
that resolve vault access sit behind `auth.Identify` and get identity from
`auth.Identity`. `internal/vaultaccess` (Teams/VaultAccessService.swift) and
`internal/vaultactivity` (the DB half of VaultActivityRecorder) are shared by
Spaces, Kanban and the later vault modules.

## D-settings-wave6: Settings moves to wave 6, with Hermes

`HermesConfigController` (`/v1/settings/hermes`) is not leaf CRUD. Every `PUT`
and every successful `POST /test` calls
`HermesRemoteCapabilitiesService.capabilities(force: true)`. That service
resolves the tenant's endpoint through `HermesEndpointResolver`, probes the
remote `/v1/capabilities` and the dashboard through `HermesDashboardCredentialStore`,
and writes `user_hermes_config.capabilities`/`capabilities_checked_at`. PUT also
nudges the Hermes mirror worker (coupling.md, HermesMirrorNudging). All of that
is Hermes (wave 6). Porting Settings in wave 2 would mean porting those pieces
early, or diverging on the DB columns the probe writes. So Settings is wave 6
(`scripts/gen-modules.py` WAVES). Its SSRF guard and no-redirect probe port
with it.

## D-reminders-detect: `/v1/reminders/detect` waits for LLM routing (wave 5)

`POST /v1/reminders/detect` runs `ReminderIntentClassifier` through Swift's LLM
routing (wave 5). The parity environment has no LLM, so both servers answer
`{"isReminder":false}` and parity is 0 differences. In production, though, Go
would quietly turn off reminder suggestions. Reminders therefore stays `swift`.
Proposal for the owner: route only `/v1/reminders` and `/v1/reminders/{id}` to
Go when apple/devices/reminders flip, and leave the `/v1/reminders/detect`
prefix on Swift until LLM routing lands in wave 5 (longest-prefix routing keeps
it there). Until the owner accepts that, the apple/devices/reminders group can't
flip. The group has a second flip blocker: `DeviceCommandBroker` waiters live
in Swift memory. Once `/v1/devices/command/{id}/result` reaches Go, live device
RPCs from Swift time out after 12 s and reminder or calendar writes are replayed.
It needs a Valkey bridge, or that route stays on Swift until MCP and Skills
(wave 5) move.

## D-wave3-early: wave 3 is ported while waves 1–2 wait on human merges

The owner's goal (2026-10-30) is to keep going until LuminaVaultServer can be
archived, and wave 2 went ahead on the same terms (D-wave2-early). Wave 3 is
ported to `parity` on branches stacked on `go-migration/wave-2`. Two gates
apply. No wave-3 module goes past `parity` until `projects` is `done`. Wave-3
branches never touch the infra repo.

## D-me-split: Me's gateway and provider controllers move with their owners

`Sources/App/Me` holds three kinds of controller:
- HermesGatewaysController (`/v1/me/hermes-gateways/*`, plus
  `/v1/gateways/photon/inbound` in App+build.swift) is an HTTP face on the
  HermesGateways services. Its apply/pair/setup sessions are in-process SSE
  (coupling.md), so it moves with Hermes in wave 6 as `me-gateways`.
- ProvidersController, LLMPreferencesController and ConnectionsController
  (`/v1/me/providers`, `/v1/me/preferences/llm`, `/v1/me/connections`) write
  and probe the BYO credentials behind `UserCredentialStore`'s 600 s cache, and
  `test-all` calls the LLM routing adapters. They move with LLM in wave 5 as
  `me-providers`.
- Everything else (today, brief, recap, resurface, learned, commitments,
  location, apns-categories) stays `me` in wave 3.

`scripts/gen-modules.py` assigns routes by controller file (`SPLITS`). The
prefixes don't overlap, so each part flips with its own `extraPaths` entry.

## D-router-parity: routing edges now match Hummingbird

Probing production after the pilot flip (2026-11-04) showed differences the
scenarios had never exercised, in every module:
- Echo answers a wrong method with 405 + `Allow`, and `OPTIONS` with 204.
  Hummingbird answers 404 with an empty body. With an allowed `Origin`, its CORS
  middleware still answers `OPTIONS` with 204.
- Hummingbird ignores empty path segments, so `/v1/spaces/` is `/v1/spaces`.
  Echo gave 404.
- Echo lets a trailing `:param` swallow slashes, so `DELETE /v1/devices/a/b`
  reached the handler with token `a/b`. Hummingbird's parameters are one segment.
- `AllowedOriginsMiddleware` was not ported: Swift rejects an unlisted `Origin`
  with 403 `origin not allowed` on every path, including unknown ones.

All four are fixed in `internal/httpserver` and covered in
`parity/scenarios/base/health.yaml`. Not copied on purpose: Swift's
`Server: ObsidianClaudeBrainServer` header, a stale name that no client reads.
It is also how to tell which server answered a request.

## D-wave3-notes: facts recorded while integrating wave 3

Each item is a flip blocker or an open owner decision unless it says otherwise.

**Shared code**

- A known path with the wrong method answers 404, as Hummingbird does (parity CI).
  D-router-parity covers the remaining router edges.
- Hummingbird keeps a literal `+` in a query value; Go's `c.QueryParam` decodes it
  to a space. `calendar`, `health`, `community` and `dashboard` read the raw query,
  each with its own copy of the parser. `achievements`, `me`, `news`, `projects`,
  `reminders` and `sessions` still differ. Open gap: move the parser to one shared
  helper.
- `ids.Parse` claims strictness it doesn't have: `uuid.Parse` also accepts bare
  hex, braces and `urn:`. The calendar brief route works around it. Other callers
  are unchecked.
- `wire.Bind` answers 400 on a body over 2 MiB, where Swift answers 413.
- Harness additions: `stream` and `headers` step options, `CheckRedirect` so
  redirects aren't followed, `scripts/parity-switches.sh` for switch-state runs.
- `gen-modules.py` has `COUPLED` for modules that must flip together without
  sharing a prefix.

**teams**

- `parity_team_invitations` is a parity-only view. Grants are compared sorted.
- A slow subscriber's stream drops events past 256.
- Lowercase comparison approximates Swift's. It isn't byte-for-byte for non-ASCII.

**me**

- `meTodayByUser` lives in the module. `/v1/me/suggestions` must share its
  rate-limit policy name.
- `MeTodayCache` invalidation doesn't cross processes, and
  `PeriodRecapThemesWorker` stays in Swift.

**account, onboarding, health**

- goose FK trigger order matters: see the account seed.
- `OnboardingLatches` must be ported when vault, capture, compile and chat move.
- `POST /v1/admin/health/correlate` has no owner in `jobs.md`. Owner decision.

**marketplace**

- At `fdbed9b0` the Swift `marketplace_versions` INSERT fails every time. Go
  reproduces this behind `versionInserts`. Production's row count is unverified.
  Owner decision: fix it in Swift, or flip `versionInserts` after cutover.
- The flip needs the `PLUGIN_*` env and `PLUGIN_ARTIFACT_ROOT` on `api-go`.
  `marketplace.CheckConfig` refuses to boot on a bad configuration.

**community**

- Stays `swift`: moderation and the daily run need LLM routing (wave 5).
- Owner decision: accept hold-then-sweep and leave `/v1/admin/community` on
  Swift, or wait for wave 5.

**dashboard**

- `cronJobs` on `/v1/dashboard/home` comes from `CronBridgeService`, which needs
  Hermes. The module stays `swift` for that reason.
- Owner decision: accept an empty preview at flip, or keep the route on Swift
  until wave 6.

**calendar, mail**

- They flip together: Gmail connect finishes at `/v1/calendar/oauth/callback`,
  which only finds sessions from its own process.
- `CalendarOAuthSessionStore` stays in memory. A consent in flight at a flip gets
  `session_not_found` and the user retries. More than one Go replica breaks the
  flow; the cheap fix then is a sealed stateless `state`, which loses one-shot.
- A brief with no stored match needs an embedding call. With a real
  `EMBEDDING_PROVIDER`, Go answers 500. Check the production value before flipping.
  Prefix routing can't split the brief from `/v1/calendar/events`.
- Swift bug, reproduced on purpose: in `CalendarSyncService.sync`, `continue` on a
  410 skips the full resync and stores the expired token again, so that account
  gets 410 forever.
- Fluent eagerly decodes `attendees` and `recurrence`: malformed JSON gives 500 in
  Swift and 200 in Go. No writer can produce it.
- Google paths are covered only by Go tests against a stub, not by parity.

## D-flip-before-pilot: batch 1 flipped before the pilot finished

D-wave2-early and D-wave3-early hold wave 2 and 3 modules at `parity` until `projects` is
`done` and `pilot-retro.md` exists. On 2026-11-04 the owner said they want the app live
and the Swift server archived as soon as possible, and merged infra #546, whose PR body
stated that merging waives this gate. So later batches don't wait for the pilot either.
G2 and G3 still apply per batch. The retro is still written after the pilot's watch, and
anything it changes in the recipe applies to modules not yet flipped.

The pilot's own watch was not restarted when production moved to `e948c69`, also at the
owner's call, so its mark stays 2026-11-07 12:15Z.

## D-wave4-prep: facts from the wave 4 prep PRs (P1 #26, P2 #24, P4 #23 and #25)

Reported by the prep agents. Items marked **[checked]** were confirmed by the
integrator; the rest are the agents' readings of the Swift code.

**Reading parity counts.** A parity job prints `92 scenarios, 0 differences`
twice, once for the Swift-vs-Swift self-test and once for Swift-vs-Go, then five
switch-state compares (six scenarios). The 92 equals the number of YAML files under
`parity/scenarios` **[checked]**. Quote 92, not the sum of the lines. These PRs add
no routes, so parity only shows that existing modules didn't change. It says
nothing about the new packages, which are covered by Go tests against stubs.

**P1, embedding**

- Corrects the calendar note in D-wave3-notes: with a real `EMBEDDING_PROVIDER` and
  no key, Go now returns deterministic vectors and a warning, as Swift does, instead
  of 500. A brief with no stored match therefore no longer fails on this path.
- Once this reaches the staging image, `api-go-staging` makes real paid OpenRouter
  calls. Nothing has called real OpenRouter from Go yet, so smoke it after merge.
- Secret spelling: Swift and Go read only `LLM_PROVIDER_OPENAI_API_KEY` for
  embeddings. The sealed `LLM_PROVIDER_OPENAI_APIKEY` is ignored, so
  `EMBEDDING_PROVIDER=openai` on current secrets lands on deterministic in both
  servers. OpenRouter accepts its legacy spelling. Existing behaviour, not new.
- `EstimateTokens` now counts grapheme clusters, not runes, so `tokens_used` matches
  Swift. Vectors don't change.
- Plan correction: `Memory/EmbeddingDegradationTests` (7) can't land in P1. 2 drive
  routes (4C), 2 need the memory repository (P3), 3 test the heal pass, which stays
  in Swift. Rows stored with a NULL vector heal only while the Swift worker runs, so
  the heal pass is a decommission blocker.
- Plan correction: `hermesLocal` isn't a no-op in Swift. `App+build.swift`:957 wires
  it to the container manager when the master key is set, and the comment at :1969
  is stale. Go has no container manager, so it answers `endpointMissing`.
- Quirks kept: `endpointMissing` stops the chain; BYOK embeds skip the cap, usage
  rows and the fallback chain; a failed install read counts as no BYOK and is cached
  5 minutes; Nomic truncates oversize vectors; the usage increment is a read then a
  write, so concurrent embeds can lose an update; a cancelled request can still get
  a deterministic vector from the chain.

**P2, vault files and disk guard**

- Upload's write flow (temp file, remove, move, `.atomic`) isn't in P2. 4A and 4B
  both need it, so whichever lands first puts it in `internal/vaultfiles`.
- 4A must check that its catch-all route decodes percent-escapes as Hummingbird's
  does. Not yet checked.
- "Soft delete" is only a rename on disk: the row is hard-deleted.
- Go's Unicode 17 tables accept 6,227 code points that Foundation on Darwin refuses,
  all letters, marks or numbers. Linux's tables weren't measured. Swift comparisons
  ran on Darwin with Swift 6.4.1, as no Linux Swift image is available locally.
- 2 of the 11 `DiskSpaceGuardTests` are route tests and go to 4A, 4B and 4E.
- **Security, owner decision open.** Carried over from Swift and pinned by
  `TestSwiftWeakness*`: a symlink inside the vault reads outside it, and
  `link/../nope.md` passes when the final file doesn't exist, so a write through it
  lands outside `raw` for a caller that skips `SanitizePath`. Whether Hermes can
  plant a symlink hasn't been checked.

**P4, side effects**

- `sideshow/apns2` can't send `apns-expiration: 0`, which Swift sends, so it needs a
  small `RoundTripper`. Open option: drop the dependency for `net/http` plus the
  `jwt/v5` already in `go.mod`, estimated at 90 lines, not built.
- Swift's idempotency header comment is wrong: a thrown `HTTPError` skips storage,
  so only responses a handler returned are stored. Go does the same.
- Deliberate differences: `dominant_type` ties are random per process in Swift and
  first-seen in Go. Idempotency keys are parsed strictly. A body over 32 MiB with a
  key passes through whole in Go, while Swift reads part of the stream first; this
  was never run on Swift, so 4A should check it if it matters. Bounds Swift doesn't
  have: 10 s final drain for PostHog and the sink, 5 s on a latch and on the
  idempotency insert, 30 s per sink job. Go's and Foundation's URL parsers can word
  a rejection differently on malformed URLs.
- Quirks kept: a full achievement inbox drops the oldest job, though Swift's log
  line reads as if the new one was dropped. The Hermes 502 text is Swift's
  `String(describing:)`, for example `privateAddress("10.0.0.1")`.
- `calendar`, `mail` and `apple` capture PostHog events in Swift but not in Go. The
  client is ready; wiring is still to do, and it matters before those modules flip.
- `health_events_synced` has been lost in production since the 15:53Z flip and
  stays lost until #23's image is promoted. Nothing backfills the gap.
- #23 and #25 add config fields at the same spot, just above `LVGoJobs`. The second
  to merge will conflict, and CI won't run until that is resolved keeping both.
- Nothing has reached real PostHog or APNs, only stubs.

**Dependencies to add to D-deps when #25 merges:** `sideshow/apns2` v0.25.0 and the
indirect `golang-jwt/jwt/v4` v4.5.2. The Sonatype check hasn't been run. `govulncheck`
lists `x/net` v0.61.0 (GO-2026-6866, GO-2026-6860) and `x/crypto` v0.58.0
(GO-2026-5932) as uncalled; bump them separately.

## D-wave4-ports: facts from the wave 4 ports (4A #34, 4B #36, 4C #37, 4D #33, 4E #35)

Each PR's body has the full list. Recorded here: what later waves need, and what the owner must decide.

**Open owner decisions. Swift has each of these today; Go copies it for parity.**

- **SSRF in `URLEnricherGuard`.** It checks literal hosts only: no DNS check, and it passes `127.1`,
  `2130706433`, `0x7f.0.0.1`, `[::ffff:127.0.0.1]` and short in-cluster names such as `whisper.horus.svc`.
  Any logged-in user reaches it through capture, and the RSS connector, news ticker and URL extraction
  use the same guard. Recommended: use `internal/ssrfguard` in Go when capture flips.
- **Storage quota never counts stored bytes.** `usedBytes` decodes a numeric `SUM` as `Int64` under `try?`,
  so it reads 0 and only one batch's declared size meets the ceiling. The 413 always says "0 B of ...". Go:
  `internal/modules/ingestion/quota.go`, pinned by `ingestion/02-create`.
- **Path weaknesses** (`TestSwiftWeakness*`, D-wave4-prep) are still open. Call sites that skip
  `SanitizePath` and use `ResolveInside` only: lint's stored paths, `MoveFile` and `moveToSpace` sources
  (`vault_files.path`), `CaptureLink`, `enrichAndRewrite`, `todos.delete`, the ingestion source route.
- **`/v1/memory/graph` skips the disclosure scrub**, so origin models reach managed tenants there.
- **Enrichment deletes the note** a user typed in the share sheet.
- **Vision keys.** `/v1/vision/*` answers 503 on both servers because the sealed names are misspelled
  (`*_APIKEY`; Swift reads `_API_KEY`). Sealing the right name starts paid Cohere calls.

**Facts for later waves**

- Hummingbird never decodes path parameters. `/v1/vault/files/inbox/caf%C3%A9.md` is a 400, though an upload
  via `?path=` can store that file.
- Over 32 MiB with an Idempotency-Key, Swift's handler sees an empty body. `internal/idempotency` does the
  same for every module, which corrects the prep behaviour.
- `String(data:encoding:)` and `String(contentsOf:encoding:)` drop one leading BOM.
- Literal paths given a method they lack return an empty 404 (`registerLiteralShadows`). Deeper paths such
  as `/v1/memory/graph/lineage` reach `:id` and return 400.
- Lists with no `ORDER BY` tie in scan order, so scenarios and seeds keep sort positions unique.
- Enrichment and todo memory writes don't call `InvalidateToday`. Whether Swift publishes `memory_upserted`
  there wasn't checked, so `me`'s cache is covered only by the flip-blockers check or its 300 s gap.
- `/v1/vault` is a string prefix of `/v1/vaults`: a flip entry must name both.
- `/v1/vault/export` ignores `X-Vault-ID`, so a team vault can't be exported.
- `dropIndex` has no Swift caller. The `App+build.swift` comment saying account deletion uses it is stale.
- `INGESTION_PUBLIC_BASE_URL` is read only by the worker, which stays in Swift and reaches Go through the
  ingress after the flip.
- `DiskSpaceMonitorService`'s periodic sample isn't wired in Go. The guard checks the disk on each request.

**Deliberate differences from Swift**

- A vault over 65,535 entries traps the whole Swift process; in Go only that export fails.
- Fetch timeout covers the whole request (URLSession's is idle), and fetched pages are capped at 32 MiB.
- oEmbed requests carry Go's default User-Agent. EventBus `vault_file_created` is dropped: its only
  subscriber logs.
- Graph Space hubs are sorted by id (Swift's order is random). `?offset=` near `Int.max` doesn't trap.

**Not proven by parity**

- The `URL(string:)` golden file (1143 cases) was captured on Darwin; production runs Foundation on Linux.
  Only the odd URLs in `capture/01-flow` were compared on Linux. IDNA tables also differ (x/net vs ICU).
- X, YouTube and Jina enrichers run only against stubs, and capture's smoke sends only refusals. A real
  fetch needs a manual check at G2. `JINA_API_KEY` must reach `api-go`, or Jina's lower keyless limit applies.
- Hermes profile copy of `SOUL.md`, the 507, the sink, PostHog and mid-stream aborts: Go tests only. Zip entry
  times aren't compared.
- `CAPTURE_AUTOFILE_ENABLED` is off in production and moves with 4F. Turning it on before then does nothing
  on Go.

## D-wave5-prep: facts from the wave 5 prep PRs (#39, #40, #41, #42, #43, merged as #46 and #41)

**Auth and hermesprofile**

- `auth.Verify` refuses `"scp":""`.
- `hermesprofile` provisions rows and directories before the slug check. Call `EnsureProfilesWritable` at boot
  and mount the Hermes volume in the Go pod. Neither is checked yet.
- `soulByUser`, `kbCompileByUser`, `memoryCompileByUser` and the auth policies aren't in `ratelimit`.
- `net/http` needs the request body read before a streaming response starts. Workflow events SSE fits neither
  `internal/sse` writer.

**Chunks and soul**

- `VaultLinkRepository` is already a `chunkindex.LinkRepository`.
- `ChunkBackfillService` is unbuilt. It blocks the MCP `index` tool and `POST /chunks/backfill/:userID`. Quirks to
  keep: `alreadyIndexed` is 0, and `isComplete` ignores `review_state`.
- `hermescontainers` is built and unwired. Real soul bodies have no parity until wave 8 or 5H.

**Skills**

- `internal/skills/builtin` holds 518 files; only each SKILL.md is embedded. `scripts/check-skills-sync.sh` runs in
  the parity job and goes away at archive.
- The local Swift checkout has an untracked `stock-trading` skill and is on `fix/chat-source-citations`. Compare
  against the image, not the checkout.
- The catalog follows symlinks. Open decision.

**Harness and stubs**

- `coalesce` on frames, a `ws` step, stubs, image `ghcr.io/luminavault/lumina-hermes-stub:<sha>`.
- Paid chat reaches the gateway only when the OpenRouter key loads; otherwise `free_lane_unavailable`.
- `/v1/ws` with no token is 400 with an empty body.
- Holds: batch 5-4 `/v1/memory-compile`, `/v1/devices/command`; batch 5-3b `/v1/reminders/detect`,
  `/v1/me/recap/period`, `/v1/admin/community`. `/v1/dashboard/home` isn't in `heldPaths`.
- The stub's infra PR needs a Deployment and Service, NetworkPolicy peers, `HERMES_GATEWAY_URL` with a scheme, a
  fake non-empty OpenRouter key with `LLM_PROVIDER_OPEN_ROUTER_BASE_URL`, blanked provider keys, and a watch on
  `/stub/stats`.
- `free-lane.yaml`'s `latencyMs` is real elapsed time: `ignore: ["$.payload.latencyMs"]`. Stream scenarios need
  the same.

**Job runner**

- Metrics go through OTel only with `OTEL_ENABLED=true`, with a `service` label. `ErrSkipped` exists. Z11 needs
  `max_over_time`.
- `LV_GO_JOBS` isn't set in infra. Any value fails boot until a job is registered.

**Configuration in production**

- Both servers read `HERMES_GATEWAY_URL` (key `hermes.gatewayUrl`), not `HERMES_BASE_URL`. Staging's sealed value
  is the dead host `hermes.staging.svc.cluster.local:8642` with no scheme.
- Swift reads `CAPTURE_AUTO_FILE_ENABLED`; sealed `*_APIKEY` names aren't read (Swift reads `_API_KEY`).
- Absent from the production secret: `EMBEDDING_PROVIDER`, `JINA_API_KEY`, `STORAGE_QUOTA_ENABLED`,
  `CAPTURE_AUTOFILE_ENABLED`, `OAUTH_GOOGLE_CALENDAR_*`.
- `CERBERUS_EXECUTION_MODE=active`, `CERBERUS_ENSEMBLES_ENABLED=false`, `CERBERUS_PARALLEL_ENABLED=false`.

**Coverage**

- At Swift `fdbed9b0`, 214 of 332 test files aren't cited by a Go test (1385 of 2218 tests). `make
  swift-test-coverage`; job `swift-test-coverage` doesn't fail CI. Waivers go in `test-waivers.md`.

**Watch**

- `vision-cohere` failed once on #39 attempt 1 (`parity_vision_usage$[1].mtok_in` 3 vs 2) and passed on rerun.
  Suspected fire-and-forget goroutine; not proven.
- One local `modules/profiles` failure is unexplained.

## D-owner-2026-11-11: owner answered "all recommended"

Asked on 2026-11-05 (16 questions) and 2026-11-11 (decisions raised by the wave 5, J1 and wave 6 prep PRs).
The owner replied "all recommended" on 2026-11-11. Each line is the recommendation that now stands.

**Wave 5 shape**

1. Chat escalation: option A, the bridge. 5C ports with it; 6C runs order with 5C (B12).
2. `hermes-stub` on staging: yes. An infra PR adds it; staging is never pointed at production Hermes.
3. `coder/websocket`: yes. P5 (`internal/ws`, `internal/devicerpc`) uses it, then 5F, 5J, then 5E.
4. Jobs with no Swift flag (`CronScheduler`, `LegacyJobWorkflowMigrator`, `HermesRunsService`, `ReminderScheduler`,
   `CalendarSyncWorker`) move at Swift replicas 0 (O4).

**Security and data fixes, made in Go now** (each is a deliberate difference from Swift, pinned by a test and named in
the cutover log)

5. SSRF: outbound fetches go through `internal/ssrfguard`, including BYOK base URLs (5A had none).
6. `internal/vaultfiles`: refuse symlink escapes and `link/../nope.md` (the `TestSwiftWeakness*` cases). Snapshot
   restore paths in self-improvement go through the same checks.
7. Storage quota: enforced.
8. `/v1/memory/graph`: disclosure scrub.
9. Share-sheet note deletion: fixed.
- `skills_state`: a save touches only its own row, not every row of the tenant.

**Product and operations**

10. A production test account for smokes: yes, and the owner tries the app on a phone.
11. Vision stays off.
12. `/v1/tts` mounts only with a self-hosted `TTS_BASE_URL`; the web speak button is hidden.
13. Docker-backed routes (O1): port the routes and wire, with a signed waiver for what can't run in a pod with no docker.
    The owner runs the `kubectl exec` docker check before 6D/6E/6F/7C flip.
14. Pruning, scoring, retention: ported, off (O3). Turning pruning on is a separate decision.
15. Skills-cron in wave 6; `/v1/ws` bundled with a Swift restart; ParallelExecutor unported; `bootstrap-admin` ported and
    backfills retired (O5); `go-webauthn` (O6); 7-day soak (O7); dashboard hold dropped if `cronJobs` is always `[]` (O8);
    reminder classifier through `internal/llm` (O9).
16. Infra merges stay the owner's (O10). Go repo PRs merge when fully green.

**From the PRs**

- 5A: `FixDashboard` on (Swift's `/v1/router/dashboard` 500 is a bug); `ParallelExecutorTests` waived; `photon` added
  to the spec's `HermesGatewayID`.
- 5B: the `TTS_BASE_URL` gate stands (item 12).
- P1: mid-stream connection errors classify as `upstream_unreachable`.
- 5H: spec follows Swift (`/runs` limit default 50, `curator/runs` body optional).
- J1: Swift's leak-report flag name is checked against the D-keys rule before it is switched off.
- Wave 6 prep: Go keeps `service.instance.id = hostname`.

## D-security-after-archive: the security and data fixes wait until Swift is archived

Later on 2026-11-11 the owner deferred items 5–9 above and the `skills_state` fix: "You can do the security-fixes once
the full port to Go is made, API live on Infra and the Vapor project is archived. For now focus on 5C and then full
port."

- Until the A gate passes, Go keeps Swift's behaviour for SSRF (including BYOK base URLs), the vaultfiles path
  weaknesses, snapshot restore paths, storage quota, the memory graph disclosure, share-sheet deletion and
  `skills_state`. Ports keep pinning these with `TestSwiftWeakness*` style tests.
- `FixDashboard` stays off for the same reason: it is a deliberate difference, not a port.
- Order now: 5C chat first, then the rest of the port.
- The other answers above stand.

## D-6de-waivers: no waivers for 6D/6E; `docker/photon-sidecar` retired

On 2026-11-11 the owner rejected the three waivers proposed in #71. All three are ported instead:

- **Container start and template seeding.** `internal/hermescontainers.Lifecycle` ports HermesContainerManager
  (ensureRunning, docker run args, config.yaml/.env seeding with SHA-256 drift, audio-proxy credential, evictIdle,
  reprovisionAll, seed/apply gateway config) behind the `Docker` interface, tested against a stub docker as Swift's
  tests stub `DockerExec`. Production still has no docker in the pod, so the default `ProcessDocker` fails at the first
  call, as Swift's does. `ContainerReaperService` (the periodic evictIdle job) is not part of this; it moves with the
  other wave 6 jobs.
- **Photon sidecar client and activation.** `megateways.HTTPSidecarClient` and the delivery service are ported and
  tested against httptest stubs; provisioning activates the sidecar when one is configured.
- **Photon setup start in parity.** Swift's Photon hosts are fixed https URLs, so the parity compose file points
  `app.photon.codes` and `spectrum.photon.codes` at 127.0.0.1 and both servers fail the device-code request the same
  way. The success path is a Go test against an httptest Photon stub.

`docker/photon-sidecar` (the Node spectrum-ts process) is retired with the Swift archive. It is not carried into the
Go repo; Go keeps only the client for whatever sidecar an operator runs.
## D-6c-runs: Go owns hermes runs; how the chat bridge changes; the double watcher

Wave 6C ports `/v1/hermes/runs` (6 routes) and `HermesRunsService` to `internal/hermesruns` and
`internal/modules/hermesruns`. One service instance serves both the routes and chat's escalation (B12), as
`App+build.swift:2228-2232` does in Swift.

**The chat bridge.** `cmd/App/chat.go` chooses chat's `HermesRuns` as follows:

- `CHAT_HERMES_RUNS_BRIDGE_URL` set: `chat.Bridge`, as deployed today. An escalated turn starts its run on Swift, and
  Swift's watcher follows it. Nothing changes for production until the 6C flip.
- Unset: `runsStarter`, the in-process Go service. The run is keyed on the session key chat passes, and Go's watcher
  follows it.

At the 6C flip, the one infra commit that moves `/v1/hermes/runs` and chat to Go (B12) also removes
`CHAT_HERMES_RUNS_BRIDGE_URL` and adds `HermesRunsService` to `LV_GO_JOBS`. After that a run started from chat and one
started on `/v1/hermes/runs` share one watcher budget (8 per tenant, 32 per process), as in Swift. `chat.Bridge` stays
in the tree until Swift is archived, so the flip can be rolled back by setting the env var again.

**Boot re-attach is a job.** `HermesRunsService` (the job) runs `Reattach` once at boot. Active runs younger than
300 s get a poll watcher. Older ones are marked `lost` without contacting Hermes. The job then holds until shutdown
and cancels every watcher. Go runs it only when `LV_GO_JOBS` names it (jobs.md: never both servers). Watchers started
by a request run whatever `LV_GO_JOBS` says. The process stop (`agent.go`) cancels them too.

**Accepted risk: the double watcher.** Swift's only gate on its re-attach is `LV_SECRET_MASTER_KEY`, which also
mounts its routes, so it can't be switched off alone.

- After the flip, a Swift restart within 300 s of a Go-started run re-attaches it. Both servers then watch it, which
  duplicates the push and appends each polled change twice. The second copy gets the next `seq`, so nothing is lost
  and the feed stays ordered.
- Before the flip, with the job off, a Go restart leaves Go-started runs (parity and tests only) unwatched until they
  age past 300 s. They then read as their last persisted status.
- Mitigation: don't restart Swift in the 300 s after a Go run starts. Or, if it bites, add a Swift flag that skips
  `run()`. Swift's archive ends it.

**Swift behaviour kept.**

- The run id path parameter fails with Hummingbird's message.
- An unparseable `conversationID` on list is ignored, not rejected.
- `pending_approval.requestedAt` is stored as Fluent writes a `Date` in jsonb (seconds since 2001-01-01).
- An unknown status string reads as `lost`.
- Event payload numbers come back as doubles.
- The push goes out with no rate limit of its own (category prefs only).

## D-ratelimit-memory: rate-limit storage stays in memory

On 2026-11-11 the owner confirmed that rate-limit storage stays in memory for now (`RATE_LIMIT_STORAGE_KIND` is
memory; the P10 question in `wave-6-8-plan.md`).

- Go gets no Valkey driver. `internal/ratelimit.Store` is the only storage. `RATE_LIMIT_STORAGE_KIND=redis` is parsed
  (Swift's `RateLimitStorageKind`, case-insensitive, anything else memory) and logged as an error at boot, not obeyed.
- Pairing records, which Swift keeps in the rate-limit driver, therefore live in process memory in 8B too, and are
  lost on restart like the other in-flight challenges.
- Swift tests that drive a live Valkey are ported against the memory store where Go has an equivalent. The owner
  rejects waivers in general.
