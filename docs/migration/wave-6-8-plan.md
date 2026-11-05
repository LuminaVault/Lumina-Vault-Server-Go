# Waves 6, 7, 8 and decommission plan

Written 2026-11-05. This plan is the checklist that decides when archiving the Swift
server is safe.

Sources, all at fixed refs:

- Swift at `fdbed9b0`, the image production runs. `origin/main` (`cca8c5c9`) differs
  from it only by three dependabot bumps, with no `Sources` or `Tests` change.
- Infra at `301dcfc`. Go at `cda9718`.

Paths under Swift are relative to `Sources/App` unless they start with a repo folder.
Nothing here has been built or run. Claims marked **[inferred]** were not verified in
code. Checks marked **[owner]** need the production database or the cluster, which
agents do not read. If `docs/migration/jobs.md` disagrees with the source, the source
wins and the plan says so.

Wave 5 has its own plan, which did not exist when this was written. Items that depend
on it are marked **W5**.

## Totals

| Scope | Routes | Swift tests to port | Swift tests to audit |
|---|---|---|---|
| Prep packages | 0 | 152 | 21 |
| Wave 6 | 70 | 222 | 0 |
| Wave 7 | 21 | 72 | 67 |
| Wave 8 (excluding `/v1/ws`) | 42 | 158 | 0 |
| `/v1/ws` (flips at the end of W5) | 1 | 9 | 0 |
| Job ports J1, J2 | 0 | 63 | 26 |
| Decommission (D-tests) | 0 | 27 | 52 |
| **Total** | **134** | **703** | **166** |

"Port" means no Go test names the Swift file yet, and the code isn't written. "Audit"
means the Go code exists, and each Swift assertion must be checked against it and named in
a Go test. Test counts are `@Test` and `func test*` lines, counted by script, so a
parameterized test counts once.

Plus `reminders` 5, `dashboard` 16 and `community` 17 routes, whose code is on `main`
and which wait for flip decisions. See "Modules still on swift".

## What blocks archive and is in no plan yet

Read this section first. Each item has an owner group later in the document.

| # | Blocker | Evidence | Fix group |
|---|---|---|---|
| B1 | Go has no job runner. `LV_GO_JOBS` is parsed and no code calls `config.JobEnabled`, so Go can't take over any job today | `internal/config/config.go:200`; grep shows config only | P1 |
| B2 | Alerts go silent when Swift stops. Rules read `hb_requests_total`, `luminavault_llm_chat_*`, `luminavault_ingestion_*`. Go emits only `otelhttp` metrics. Silence looks healthy | infra `observability/alerts/platform_rules.py:340-342,437-440,455-490`; Swift `App+build.swift:340`, `Ingestion/IngestionMetrics.swift:5-14`, `LLM/RoutedHermesLLMService.swift:24-26`; Go `internal/httpserver/server.go:113` | P5, D3 |
| B3 | `api-production-data` is created by the Swift release, on `local-path` with reclaim policy **Delete**, and nothing else holds the tenant files. Deleting that release can delete the data Go also mounts | infra `charts/app/templates/pvc.yaml`, `cluster/backups/api-data-backup-cronjob.yaml` comment, `apps/api-go/values-production.yaml` `persistence.existingClaim` | D2 |
| B4 | The Ingress default path `/` is hard-coded to the release's own Service. No value retargets it | infra `charts/app/templates/ingress.yaml` | D1 |
| B5 | Isis reads `/internal/metrics` from the Swift Service directly, not through the Ingress, so `extraPaths` can't move it. `allow-metrics-from-isis-api` names only `api-production`. Go doesn't read `METRICS_SECRET` | infra `apps/isis/values.yaml:42`, `cluster/network-policies/horus.yaml:728-737` | 7B |
| B6 | Memory pruning, memory scoring, usage-events retention and health correlation are driven only by a "host cron" calling admin routes. No CronJob in infra calls them, so **[inferred]** they don't run in the cluster today | `App+build.swift:3306-3322,3342-3356`; Swift `docs/jobs.md`; infra grep for `admin/memory`, `usage-events/prune`, `correlate` finds nothing | 7B, decision O3 |
| B7 | The Swift repo builds things that aren't Swift server: the Hermes image, the restore drill, `plugin-runner`, `photon-sidecar`, the OpenAPI source | `.github/workflows/hermes-image.yml`, `.github/workflows/restore-drill.yml`, `plugin-runner/`, `docker/photon-sidecar`, `Sources/AppAPI/openapi.yaml` | D6 |
| B8 | Web codegen hard-codes the Swift checkout: `openapi-typescript ../LuminaVaultServer/Sources/AppAPI/openapi.yaml` | `LuminaVaultWebApp/package.json:19` | D6 |
| B9 | Swift `release.yml` writes `apps/api/values-staging.yaml`. Once that release runs a Go image, one stray Swift merge deploys a Swift image over it. The Go release writes `apps/api-go/values-staging.yaml`, which is removed at the end | Swift `.github/workflows/release.yml`, infra `.github/workflows/promote.yml` | D5 |
| B10 | The Swift freeze isn't in force. `go-migration/phase-0-contract`, which holds `swift-freeze` CI, is a local branch that was never pushed. Other sessions still work on Swift (`fix/chat-source-citations`, `feat/quick-assistant`) | `git branch -r` shows no such branch; `git worktree list` | P6 |
| B11 | 1383 Swift tests in 213 files have no matching name in any Go test at `cda9718`. Most are expected (4F to wave 8). Others had no group, because only wave plans assign tests | This plan's count, see "Test accounting" | P7, D-tests |
| B12 | Chat depends on `HermesRunsService`. There is one shared instance, and chat escalates turns into runs. So runs (wave 6 here) can't flip after chat (wave 5) unless Go chat already has a runs service | `App+build.swift:2228-2232,2274` | 6C, W5 |
| B13 | `/v1/me/hermes/cron` is `skills` (wave 5) in `modules.yaml`, but `CronBridgeService` is built on `DockerExec` and `HermesContainerManager` | `App+build.swift:1519-1524` | O1, W5 |
| B14 | Staging has no Hermes (`apps/api/values-staging.yaml` sets only `HERMES_DATA_ROOT`). G2 for the Hermes groups can't exercise a live Hermes on staging | infra `apps/api/values-staging.yaml`; auto-memory `staging-has-no-hermes` | O2 |
| B15 | Swift's `FLUENT_AUTOMIGRATE=false` is **[inferred]** inert. The code reads `fluent.autoMigrate` (`App+build.swift:86`), which encodes to `FLUENT_AUTO_MIGRATE` by the D-keys rule. Swift therefore also migrates at boot, not only in the PreSync hook | infra `apps/api/values-production.yaml`, `secrets/horus/api-env-production.yaml`; `App+build.swift:85-96` | D4 |
| B16 | `bootstrap-admin`, `backfill-hermes-profiles`, `backfill-vault-space-folders` return "not implemented" in Go, though the roadmap's image contract lists them | `cmd/App/main.go` | D4, O5 |
| B17 | Runbooks run `kubectl exec ... bash` in `api-production`. The Go image is distroless, so those stop working at the image swap | infra `docs/runbook-loci-telegram.md:265`, `apps/loci/data/hermes-egress.yaml:26` | D3 |
| B18 | Hermes and core skills are read through `Bundle.module`. Go must embed them, or the mirror's bundled skills and the skill catalog change | `Skills/SkillManifest.swift:6`, `Skills/SkillCatalog.swift:52`, `Hermes/Mirror/HermesBundledSkills.swift:18` | P3, W5 |
| B19 | Docker-backed routes **[inferred]** can't succeed in the cluster, yet G1 says port every assertion. The Swift runtime image installs no docker CLI and the chart mounts no socket | Swift `Dockerfile` run stage, infra `charts/app/templates/deployment.yaml` | O1 |
| B20 | Parity depends on the Swift image. Delete that GHCR package version and Go CI dies | Go `parity/SWIFT_IMAGE`, `.github/workflows/ci.yml:62-89` | D7 |

## Where the inventory is wrong

The router was scanned against `docs/migration/routes.json`. **The method and path
inventory is correct**: 446 single-line registrations and the 17 chained ones
(`.add(middleware:).post`) are all in the file, and a loose scan found no extra route.
Total 468. The problems are in what the file and `modules.yaml` imply:

1. **`/v1/ws` is in `app` (wave 8).** `wave-4-plan.md` and `flip-blockers.md` need it to
   flip at the end of wave 5, together with `kb-compile`, `/v1/devices/command`, `mcp`
   and `skills`. It needs a `SPLITS` entry in `scripts/gen-modules.py`.
2. **`/v1/me/hermes/cron` is in `skills` (wave 5)**, but the service behind it needs
   the wave 6 container layer (B13). `flip-blockers.md` expects the cron bridge in
   wave 6. It needs either a W5 port with the interface from P4, or a move.
3. **Conditional routes look unconditional.** Hummingbird registers these only under a
   condition, and `routes.json` doesn't record that:
   - WebAuthn: all 8 run `guard enabled`, default false.
   - Hub skills: `if let hubSkillsService` (`App+build.swift:2785`).
   - `chunks/backfill`: `if chunkBackfill != nil` (`Memory/MemoryAdminController.swift:36`).
   - Settings: `if var byoHermesController` (needs `LV_SECRET_MASTER_KEY`).
   - System and most Hermes routes: mounted only with the master key.
4. **Auth mode isn't recorded.** `POST /v1/hermes/mirror/webhook/:token` is public and
   registered on the root router (`Hermes/Mirror/HermesMirrorWebhookController.swift:39,71`),
   while its neighbours need a JWT.
5. **Aliases are not marked.** `/v1/auth/webauthn/*/options` duplicate `begin`.
   `POST /v1/kb-compile` is a 308 to `/v1/memory-compile` with no auth
   (`App+build.swift:2537`). `GET /v1/kb-compile/pending` is at `:2544`. Keep all of
   them: installed iOS versions and generated web types may call them.
6. **Module dir mismatch.** `HermesGateways` registers no routes (the controller is in
   `Me/`), and `Crypto`, `Telemetry` and `WebSockets` have none in their dirs.
   `/v1/gateways/photon/inbound` is a `me-gateways` route with its own sidecar-token auth
   (`App+build.swift:3473`).
7. **Two splits are needed that don't exist yet:** `/v1/me/hermes/capabilities` with
   settings (6A), and `PUT /v1/admin/users/:userID/tier-override` with billing (7A).
8. **`routes.json` source lines came from `go-p0`.** They still match `fdbed9b0`.

`jobs.md` disagreements with the source:

- It writes the gate as `FLUENT_AUTO_MIGRATE` while infra sets `FLUENT_AUTOMIGRATE` (B15).
- "Swift stays the only process running schema migrations" is true, but there are two
  runners in Swift: boot and the chart's PreSync `App migrate`.
- It lists no admin-driven jobs. See "Jobs with no ServiceGroup entry".
- Its request-task table omits about a dozen unstructured tasks, listed in the same
  section.
- Its 40 rows do match: 35 `Service` conformers, Fluent, `ValkeyPersistDriver`, OTel as
  one row for three exporters, `SOULCoreMigrationJob`, and the `skillRunner` task.

## Modules (134 routes)

| Module | Wave | Routes | Group(s) | Registered at |
|---|---|---|---|---|
| hermes | 6 | 33 | 6A (1), 6B (26), 6C (6) | `Hermes/Mirror/HermesMirrorController.swift:16-44,99-100`, `Hermes/Mirror/HermesMirrorWebhookController.swift:63-71`, `Hermes/Runs/HermesRunsController.swift:34-39`, `App+build.swift:3396` |
| integrations | 6 | 8 | 6E | `App+build.swift:3528,3537` (JWT only, no rate limit) |
| me-gateways | 6 | 16 | 6D | `Me/HermesGatewaysController.swift:68-89`, `App+build.swift:3473` |
| plugins | 6 | 9 | 6F | `Plugins/PluginController.swift:27-33`, `Plugins/HermesHubSkillsController.swift:18-19` |
| settings | 6 | 4 | 6A | `Settings/HermesConfigController.swift:86-89` |
| admin | 7 | 13 | 7A (1), 7B (12) | `Admin/`, `Memory/MemoryAdminController.swift`, `App+build.swift:372-376,3342-3356` (all `X-Admin-Token` except `/internal/metrics`) |
| billing | 7 | 1 | 7A | `Billing/RevenueCatWebhookController.swift` |
| system | 7 | 7 | 7C | `System/HermesUpdateController.swift:17-23` |
| app | 8 | 11 | 8A (4), 8E (6), W5 (1) | `App+build.swift:370,382,398,687-690,2537,2544,3244,3837` |
| auth | 8 | 32 | 8A (11), 8B (9), 8C (8), 8D (4) | `Auth/AuthController.swift:22-44`, `Auth/*`, `Auth/Soul*` |

## Groups (one branch each)

| Group | Modules | Routes | Swift tests | Flip blockers |
|---|---|---|---|---|
| 6A | settings + capabilities | 5 | 15 | Same flip batch as 6B. Then `MultimodalIngestionWorker` can move |
| 6B | hermes mirror | 26 | 62 | O1, O2. P2, P3 |
| 6C | hermes runs | 6 | 78 | Before or with W5 chat (B12). O2. Watcher must move in the same commit |
| 6D | me-gateways | 16 | 19 | O1. P4 |
| 6E | integrations (xAI, Nous) | 8 | 23 | O1. Start and finish routes flip together |
| 6F | plugins, hub skills | 9 | 25 | 4F import on `main` (`App+build.swift:2706-2712`). O1 for hub install |
| 7A | billing + tier-override + `/v1/auth/me/billing` read path audit | 2 | 41 + 67 audit | LapseArchiver dry run. P8 |
| 7B | admin rest | 12 | 28 | O3. B5. P8 for hermes-profiles |
| 7C | system | 7 | 3 | O1 |
| 8D | soul | 4 | 46 | `internal/llm` wired for compose. May flip first in wave 8 |
| 8E | app shell | 6 | 8 | Last; only in the image swap or `/` retarget |
| 8A | sessions + `/v1/auth/me*` | 15 | 38 (+ P9's 25) | P9, P10, cross-server scenario. Security review |
| 8B | oauth, X, phone, magic, pairing | 9 | 52 | 8A. Start and finish routes flip together |
| 8C | WebAuthn | 8 | 14 | O6 (library) |
| `/v1/ws` | app split | 1 | 9 | End of W5, with `kb-compile` |

8E's 8 tests are `AppTests` 2, `ClientCapabilities` 6. Prep tests in P8, P9, P10 are counted
only under prep, not under the groups that use them.

## Land first

| # | Package | Swift source (lines) | Tests (port / audit) | Used by |
|---|---|---|---|---|
| P1 | `internal/jobs` | Patterns in `jobs.md` | 0 / 0 | Every Go job |
| P2 | `internal/hermesmirror/transport` | `HermesMirrorService` transports, dashboard client, filesystem gateway, workspace path | 68 / 0 | 6A, 6B, 6C, dashboard |
| P3 | `internal/bundled` | `Resources/Skills`, `Resources/HermesSkills` | 0 / 0 (HermesBundledSkills is in 6B) | 6B, 6F, W5 skills |
| P4 | `internal/hermesdocker` | `Hermes/HermesContainerManager.swift` (577), `ContainerReaper` (70), seeds | 25 / 0 | 6D, 6E, 6F, 7C, W5 cron and grok |
| P5 | `internal/telemetry` metric names | `Services/Observability.swift`, `Ingestion/IngestionMetrics.swift`, `Workflows/WorkflowMetrics.swift`, `LLM/Transcribe/VoiceMetrics.swift` | 0 / 0 | Everything; alert rules |
| P6 | Push the Swift freeze | `go-migration/phase-0-contract` | n/a | The moving-target rule |
| P7 | `scripts/swift-test-coverage.py` | n/a | n/a | Final gate |
| P8 | `internal/hermesprofile` | `Services/HermesProfileService.swift` (171), reconciler (183 + 75) | 5 + 4 / 0 | 7B, 8A, 8B, `HermesProfileReconcilerService` |
| P9 | `internal/auth` minting and hashing | `Auth/AuthService.swift` token section, `Auth/PasswordHasher.swift`, `Auth/JWT/*` | 25 / 0 (JWTKeyLoader 10, SessionToken 3, SessionTokenScope 5, JWTAuthenticatorE2E 7) | 8A, 8B, 8C |
| P10 | `internal/ratelimit` IP policies and store | `Middleware/RateLimitMiddleware.swift`, `RateLimitStorageFactory.swift`, `ValkeyPersistDriver.swift` | 25 / 0 | 8A, 8B, 8C |
| PA | Audit only | `Crypto/SecretBox` 12 against `internal/secretbox`, `HermesEndpointResolver` 4 against `internal/hermesresolve`, `IdempotencyMiddleware` 5 against `internal/idempotency` | 0 / 21 | Already used code with no citing test |

### P1: job runner

`internal/jobs`, started from `cmd/App/main.go`. A job has a name, a schedule (interval
with jitter, or a daily UTC hour) and a `Run(ctx)`. The runner:

- starts a job only if `config.JobEnabled(name)` is true;
- recovers panics, and stops cleanly on shutdown;
- writes `lv_job_last_success_timestamp_seconds{job}` and a run counter, so the final
  gate has something to check;
- logs one line per run with the job name.

Tests use a fake clock. Names are the Swift type names, so `LV_GO_JOBS` reads like
`jobs.md`. Scheduling doesn't take a lock: exactly-one is enforced by the single value
of `LV_GO_JOBS` on a single-replica Deployment, and by the infra move rule. Add a
startup log listing enabled jobs and a boot failure for unknown names, because a typo in
`LV_GO_JOBS` would otherwise silently disable a job.

### P2: mirror transports

`FilesystemHermesTransport`, `GatewayOnlyMirror`, `MirrorTransportFallback`, the
dashboard client and probe. Note the production gateway kind is sealed
(`HERMES_GATEWAY_KIND`, default filesystem) and the value isn't readable here. **[owner]**
Read it before picking which transport the G2 smoke exercises.

### P4: container layer

Shape depends on O1. In every option it ends with one Go interface for container
operations (`EnsureRunning`, `Exec`, `Remove`) and a default implementation whose errors
match Swift's wire in a pod with no docker. Parity's compose has no docker either, so
that path gets a real 0-diff comparison.

### P5: metric names

Emit from Go, through the existing OTel meter, every name an alert or dashboard uses:
`luminavault.ingestion.*` (claimed, completed, failed, retried, deduplicated,
apns.failures, queue_latency), `luminavault.llm.chat.*` and `.stream.*`,
`luminavault.llm.tool_failure`, `luminavault.voice.transcribe.*`,
`luminavault.workflow.*`, and the `luminavault.<labelPrefix>.requests/failures` family
(`Services/Observability.swift`). `hb_requests_total` is Hummingbird's own name
(`MetricsMiddleware`, `App+build.swift:340`). Rewrite those rules to the `otelhttp` name
in the infra PR at D3 rather than faking a Hummingbird metric in Go.

Both servers share `OTEL_SERVICE_NAME=luminavault-api-production` (inherited plain env).
**[inferred]** They'll share the same Prometheus `job` label, so check that the label
pair stays unique, or split series with the instance label before relying on the G3
comparison.

### P7: test coverage script

Every Swift test file must be named in a Go test, in a parity scenario, or in a waiver
list in `docs/migration/test-waivers.md` with a reason and the owner's sign-off. The
script prints uncited files and exits non-zero. At `cda9718` it would print 213 files
that contain tests.
The gate reads Go test files and parity scenarios only, never plans, because a plan
naming a test doesn't port it.

### P9: minting

See "Auth". Land this with its cross-server tests before any 8A route.

### P10: rate limits

Today Go has an in-memory store and per-user policies only. Add the auth per-IP policies
and the client IP extraction from `Middleware/RateLimitMiddleware.swift:228`
(`x-forwarded-for`), matching which hop it trusts. Add the storage switch only if
**[owner]** `RATE_LIMIT_STORAGE_KIND` in `api-env-production` is `redis`. The value is
sealed and wasn't read.

If it is `redis`, the pairing records also live in Valkey, because they are stored in
the rate-limit driver (`App+build.swift:660-669`). In-memory then becomes a behaviour
change on restart, and Go needs the driver.

## Group details

### 6A settings + capabilities

- **Routes:** `GET`, `PUT`, `DELETE`, `POST test` on `/v1/settings/hermes`
  (`Settings/HermesConfigController.swift:86-89`, mounted at `App+build.swift:3385-3390`), and `/v1/me/hermes/capabilities`
  (`App+build.swift:3396`).
- **Swift tests:** `HermesConfigController` 8, `UserHermesConfig` 2,
  `HermesCapabilitiesParse` 5. `SSRFGuard` 30 is already ported to `internal/ssrfguard`.
  `LLMPreferencesController` 11 goes with W5 `me-providers`.
- **Rules from D-settings-wave6:** PUT and test call capabilities with force, PUT nudges
  the mirror worker, and the probe uses the SSRF guard with no redirects.
- **Coupling:** `HermesMirrorNudging` (`Hermes/Mirror/HermesMirrorRefreshWorker.swift:38,62-66,92-96`,
  wired at `App+build.swift:3386`) is in-process. Go's settings can't nudge Swift's worker.
  Until the worker moves (it has no Swift flag, so at replicas 0 under O4), a newly linked
  Hermes waits for the next 15-minute tick instead of refreshing on connect. That delay is
  the visible cost of O4. If the owner rejects it, the worker needs a Swift flag so it can
  move with this flip.
- **Job unlocked:** `MultimodalIngestionWorker`, per `wave-4-plan.md`.
- **Reuse:** `internal/ssrfguard`, `internal/secretbox`, `internal/hermesresolve`,
  `entitlement.PGCredentials`.

### 6B hermes mirror

- **Routes (26):** status, sync, skills GET, skills/:name PUT, jobs GET and POST,
  jobs/install-compile, jobs/:id PUT and DELETE, jobs/:id/{runs,pause,resume,trigger,collect},
  vault/{import,create,import-sessions}, artifacts 2, workspace 4 (status, files, file,
  diff), webhook GET and POST, public `POST .../webhook/:token`.
- **Swift tests:** `HermesArtifactExtractor` 6, `HermesArtifactsFilter` 5,
  `HermesBundledSkills` 2, `HermesJobRunModel` 4, `HermesMirrorController` 8,
  `HermesMirrorRefreshWorker` 5, `HermesMirrorService` 27,
  `HermesMirrorWebhookController` 5. Total 62.
- **Coupling:** `MemoryCompileProgressPublisher` is driven from mirror
  (`jobs/install-compile`), so **W5** `kb-compile` and `/v1/ws` must be on one server.
  Skills mirror writes under `HERMES_DATA_ROOT=/app/data/hermes` on the shared volume.
- **Job:** `HermesMirrorRefreshWorker` stays on Swift until replicas 0 (O4). It reads and
  writes the same tables, so routes on Go work while it runs. It is must-never-both and has
  no Swift flag. See "Job move rule".
- **Symlink handling:** D-wave4-prep left the vault symlink question open. The mirror's
  vault import goes through the same path. Port Swift's behaviour and don't decide this
  here.

### 6C hermes runs

- **Routes (6):** `Hermes/Runs/HermesRunsController.swift:34-39`. Middleware: jwt, byo,
  hermesProfile, conversation rate limit, `Entitlement .chat` (`App+build.swift:3412-3421`).
- **Swift tests:** `HermesRunConversationPersistence` 5, `HermesRunEvent` 15,
  `HermesRunPersistence` 8, `HermesRunPushNotifier` 4, `HermesRunsClient` 14,
  `HermesRunsController` 11, `HermesRunsListFilter` 4, `HermesRunsService` 17. Total 78.
- **Coupling:** B12. `HermesRunsService` re-attaches watchers at boot for runs younger
  than 300 s (`Hermes/Runs/HermesRunsService.swift:295-299`). `hermes_run_event` degrades
  to a 5 s DB poll across processes (`coupling.md`), so SSE survives a split, but two
  processes must never both watch.
- **Recommendation:** port 6C during W5, ahead of the chat flip. Move routes, watcher
  and chat escalation in one infra commit. The W5 plan must name this.
- **Job:** `HermesRunsService`, whose only gate is `LV_SECRET_MASTER_KEY`, which also
  mounts the routes. So Swift can't stop the watcher alone. After the flip, a run started
  on Go is watched only by Go. The exception is Swift's boot re-attach: a Swift restart
  within 300 s of a Go-started run adds a second watcher, with duplicate push and events
  **[inferred]**. So don't restart Swift for those windows, or add the flag. Record it as
  an accepted risk in `decisions.md`.

### 6D me-gateways

- **Routes (16):** order matters: literals before `:id`, and `apply` routes are
  registered after `:id` routes. Add a parity scenario for the shadowing cases.
- **Swift tests:** `HermesGatewaysController` 6, `WhatsAppPairParser` 8,
  `SidecarTokenMiddleware` 5. Total 19.
- **Unconfigured in production:** `photon.sidecarUrl` and `photon.sidecarToken` aren't in
  the sealed key list, so `POST /v1/gateways/photon/inbound` answers 503
  (`Me/HermesGatewaysController.swift:463-464`).
- **Channels:** gateway-apply SSE, WhatsApp-pair SSE and Photon-setup SSE are in-memory.
  All live in this group, so they stay together.
- **O1 dependent:** apply, WhatsApp pair, Photon delivery all use `containerManager`
  (`App+build.swift:1064-1110`).

### 6E integrations

- **Routes (8):** Nous 4, xAI 4.
- **Swift tests:** `LiveXaiOAuthBackend` 9, `NousOAuthService` 5, `XaiOAuthController` 4,
  `XaiOAuthService` 5. Total 23.
- **Process-local:** `XaiOAuthSessionStore` + `XaiOAuthProcessRegistry`,
  `NousOAuthSessionStore` + `NousOAuthProcessRegistry`, the xAI one holding a live
  `docker exec` handle. Both route pairs flip together. A login in flight at flip time
  fails and the user restarts it.

### 6F plugins

- **Routes (9):** `PluginController` 7, `HermesHubSkillsController` 2 (conditional).
- **Swift tests:** `HermesHubSkillsService` 2, `MemoryProviderPlugin` 4,
  `PluginConnectorsSlice2` 8, `PluginLogic` 8, `SkillPluginCatalog` 3. Total 25.
  `MarketplaceSafety`, `PluginMarketplace` and `NewsTickerPlugin` are done in Go modules.
- **Dependencies:** `PluginService` takes `ImportService` (`App+build.swift:2706-2712`),
  so 4F import must be on `main`.
- **External:** `PLUGIN_RUNNER_URL`. `plugin-runner` isn't deployed in infra. See D6.

### 7A billing

See "Billing". Tests to port: `RevenueCatWebhook` 11, `SubscriptionCatalog` 6,
`TierOverrideAllowlist` 5, `LapseArchiver` 7, `CostLedgerService` 5, `UsageMeterBudget` 5,
`UsageMetrics` 2. Total 41. Audit against `internal/entitlement` and
`internal/usagemeter`: `BYOEntitlementMiddleware` 15, `BYOEntitlementPolicy` 7,
`Enforcement` 11, `EntitlementChecker` 27, `StorageQuotaService` 7. Total 67.

If the W5 plan claims `CostLedgerService` or `UsageMeterBudget`, move them there. Per the
memory note, `checkBudget` has no callers, so don't invent a live budget check while
porting.

### 7B admin rest

- **Routes (12):** `/internal/metrics`, hermes-profiles 3, `/v1/admin/health` 2,
  `/v1/admin/memory` 5, `/v1/admin/usage-events/prune`.
- **Swift tests:** `MetricsSecretMiddleware` 5, `UsageEventsRetention` 6, `MemoryScoring` 5,
  `MemoryPruningService` 4, `MemoryKeepGate` 2, `HealthCorrelationService` 6. Total 28.
  `HermesProfileReconciler` 4 and `HermesProfileService` 5 count under P8.
- **`/internal/metrics`** returns JSON (`MetricsResponse(users:)`,
  `Admin/InternalMetricsController.swift:19-25`), behind `METRICS_SECRET`. It is not a
  Prometheus endpoint.
- **Flip:** repoint `apps/isis/values.yaml:42` and add Go to
  `allow-metrics-from-isis-api`. Otherwise Isis fails at Swift replicas 0.
- **`chunks/backfill`:** conditional. Check `chunkBackfill` construction before writing
  the parity scenario.

### 7C system

- **Routes (7):** `System/HermesUpdateController.swift:17-23`. JWT + admin token.
- **Tests:** `HermesUpdateService` 3.
- **Channels:** update SSE, `HermesContainerManager` image override. Both moved here or
  retired with O1.

### 8D, 8A, 8B, 8C

See "Auth". Handlers for `/v1/auth/me*` are in `App+build.swift` (`meHandler` :4106,
`updatePrivacyHandler` :4126, `meBillingHandler` :4168-4200, `meUsageHandler` :4203), so they
have no controller to read. 8A and 8B both need P8, because register and OAuth create call
`hermesProfileService.ensureSoft`.

### 8E app shell

- **Routes (6):** `GET /` returns `Hello!` (`App+build.swift:398`).
  `/.well-known/apple-app-site-association` returns exact JSON (`:382`). `/health` returns
  `ok` (`:370`). `/health/deep` uses `gatewayProbe` (`:3244`). `POST /v1/kb-compile` is a 308
  with no auth (`:2537`). `GET /v1/kb-compile/pending` (`:2544`).
- **Swift tests:** `AppTests` 2, `ClientCapabilities` 6. `KB/LegacyCompileAlias` 2 is counted
  with `kb-compile` (wave 4).
- **Flip:** `/health`, `/.well-known` and the aliases at D1 step 1. `GET /` and unknown paths
  at D1 step 2. No flip before that, since the blackbox probe and every readiness check read
  `/health`, and a wrong answer there pages.

## Modules still on swift

| Module | Routes | When | What it waits on |
|---|---|---|---|
| `reminders` | 5 | Next flip batch that holds apple and devices | Owner accepts D-reminders-detect, holding `/v1/reminders/detect` on Swift. Better: wire `ReminderIntentClassifier` through `internal/llm` (on `main`) in a small PR, and no hold is needed. `/v1/devices/command` stays held until end of W5. `ReminderScheduler` is J1 |
| `dashboard` | 16 | Next flip batch | Hold `/v1/dashboard/home` per `flip-blockers.md`, or drop the hold: `DashboardCronPreview.swift:11-23` catches errors. If B19 holds, Swift answers `cronJobs: []` as Go does **[inferred]**. Verify with one production call as an owner check. `AnalyticsMaintenanceService` is J1 |
| `community` | 17 | After `internal/llm` wiring in W5 | Moderation on arrival. Hold `POST /v1/admin/community/daily/run`. Swift keeps the moderation sweep until J2. `CommunityController` 7, `CommunityUnit` 22 and `DailyPostWorker` 5 aren't named in any Go test: audit before the flip |
| `me` | parts | With `PeriodRecapThemesWorker` (J2) | `/v1/me/recap/period` held, per `flip-blockers.md` |

Also on swift and belonging to wave 4F or W5, not planned here: `import`, `kb-compile`,
`knowledge-reason`, `memory-agent`, and every W5 module.

## Billing

### Webhook (Swift behaviour, port exactly)

From `Billing/RevenueCatWebhookController.swift`:

- Empty `REVENUECAT_WEBHOOK_SECRET` returns 500 (`:27-30`). Body cap 1 MiB (`:32`).
- Authentication, in order (`:35-55`). Remove `Bearer ` from `Authorization` with a plain
  string replace, then compare constant-time against the secret. Otherwise take
  `X-RevenueCat-Signature`, hex HMAC-SHA256 of the raw body with the same secret. Failure
  returns 401.
- Decode failure returns 400.
- Idempotency: look up `billing_event_logs.event_id` first and return 200 on a hit. Then
  `processEvent`, then insert the log row. These are **not** in one transaction. The
  unique index is the backstop (`Migrations/M32_CreateBillingEventLog.swift`).
- User lookup: `revenuecat_user_id = appUserId` or `id = UUID(appUserId)`. Unknown user
  writes the log row and returns 200, which suppresses retries. Unknown product throws
  `UnknownProductError`: 500, no log row, RevenueCat retries.
- Events:
  - `INITIAL_PURCHASE`, `PRODUCT_CHANGE`, `RENEWAL`, `UNCANCELLATION`: apply tier and
    `tierExpiresAt` from `expiration_at_ms`.
  - `CANCELLATION`: lapsed only if `is_refund` or past expiry.
  - `EXPIRATION`: lapsed.
  - `BILLING_ISSUE`: hold 16 days, extending `tierExpiresAt`.
  - `SUBSCRIBER_ALIAS` and default: log.
  - Every path saves `revenuecatUserID`.
- Products: `pro_monthly_14_99`, `pro_yearly_149_99`, `ultimate_monthly_29_99`,
  `ultimate_yearly_299_99` (`Billing/SubscriptionCatalog.swift`). `RevenueCatClient.swift`
  has no callers; don't port it.

Don't wrap the effect and the log in one transaction during the port. The wire is the
same, but it's a behaviour change, and it belongs after the flip if the owner wants it.

### When both servers receive one

Traefik sends each delivery to exactly one server, so a duplicate only happens when
RevenueCat retries across the flip.

| Case | Result |
|---|---|
| Swift processed it, Go gets the retry | Go finds the log row, 200, no effect |
| Swift crashed after the effect, before the log | Go repeats the effect. Effects are idempotent, except that `BILLING_ISSUE` recomputes the 16-day hold, so its timestamp can shift |
| Concurrent duplicates on both servers | Both pass the check. The loser violates the unique index and returns 500. RevenueCat retries **[inferred from its docs, not read here]**, finds the row, gets 200. **The Go port must return 500 here, not swallow the error** |

The URL stays `POST https://api.luminavault.fyi/v1/billing/revenuecat-webhook`
(`LuminaVaultClient/docs/revenuecat-appstore-setup.md`), so nothing changes in the
RevenueCat dashboard. The secret comes from the inherited `api-env-production`. Go must
start reading `REVENUECAT_WEBHOOK_SECRET`; it doesn't today.

### Testing without real charges

1. **Unit and parity, always.** Signed fixtures against both servers with a test secret
   in the parity compose: bearer, HMAC, wrong secret, empty secret (500), 1 MiB + 1,
   invalid JSON, duplicate `event_id`, unknown product (500, no row), unknown user (200,
   row), each event type, `is_refund` both ways.
2. **Realistic payloads.** `billing_event_logs` keeps only `id`, `event_id`, `event_type`,
   `user_id`, `processed_at`, so nothing can be replayed from it. Build fixtures from the
   RevenueCat docs samples, or **[owner]** copy them from the dashboard's event history.
3. **Staging.** `api-go-staging` has no Ingress, so RevenueCat can't reach it.
   `kubectl port-forward`, then curl signed fixtures with the staging secret. Check the
   `users` row and the log row.
4. **RevenueCat "send test event"** in the dashboard, after the flip. **[inferred]** It uses
   a fake user, so it should hit the unknown-user path and prove auth and routing.
5. **Sandbox purchase [owner].** App Store sandbox tester per
   `LuminaVaultClient/docs/revenuecat-appstore-setup.md:54-68`, on a test account, since the
   webhook changes that user's tier in the production database. **[inferred]** Sandbox
   renewals are accelerated, so `INITIAL_PURCHASE`, `RENEWAL` and `EXPIRATION` arrive within
   hours. No money moves.

### Lapse job

`LapseArchiverJob` (239 lines, 03:00 UTC, `App+build.swift:277`):

- trial past expiry → free;
- pro or ultimate past expiry with no override → lapsed;
- lapsed past 90 days → move `tenantRoot` to `coldStorageRoot/<UUID upper>`, then archived;
- archived past 365 days → delete cold storage, `TenantDataEraser.erase`, force delete.

**The last two steps are irreversible.** It is on the must-never-both list. Go runs it in
a dry-run mode first:

1. Port with the 7 tests on temp dirs and a test DB, plus a `LV_LAPSE_DRY_RUN=true`
   default that logs every would-be action and changes nothing.
2. Run Go in dry run alongside Swift for 7 days. Each day's Go log must match what Swift
   did that day, by user ID and action. Note Swift runs at 03:00 and acts first, so Go
   should run at 02:30.
3. Move it in the jobs commit (below), then turn dry run off in a separate commit within
   24 hours, so nobody keeps an expired plan for long.

`BILLING_COLD_STORAGE_PATH` is sealed. Default is `<vault.rootPath>/cold-storage`.
**[owner]** Confirm it is on the shared volume before the job moves.

### Flip

Billing flips alone, with one exact path. Watch 72 hours and at least one real `RENEWAL`
or a sandbox renewal. A 5xx is retried by RevenueCat **[inferred]**, so an error is
recoverable inside its retry window. Roll back by reverting the infra commit.

## Auth

Go has verified tokens since phase 1. Minting moves last.

### Token formats (must match byte for byte where a client or Swift reads them)

- **Access:** JWT HS256, 1 hour. Claims `sub` (UUID uppercase), `exp`, optional `iat`,
  `jti` (UUID uppercase), optional `hpid`, optional `scp` (`Auth/JWT/SessionToken.swift`).
  `hpid` is set only when the profile status is `ready`. Key from `JWT_HMAC_SECRETS`
  (`kid:secret` CSV, first is active, minimum 32 characters) or legacy
  `JWT_HMAC_SECRET` + `JWT_KID` (`App+build.swift:120,131`).
- **Numeric dates:** Swift writes fractional `iat`/`exp` (D-jwt-numeric-dates). Go must
  emit what Swift's verifier accepts. Cover it with the cross-server scenario, both ways.
- **`scp`:** `JWTAuthenticator` rejects non-nil `scp`. `AudioJWTAuthenticator` accepts nil
  or `audio`, and checks `iat` against `hermesAudioTokenEpoch`. Audio tokens live 90 days
  in tenant containers (`HermesAudioTokenService`), so **W5** `/v1/audio` needs that
  verifier and P4 needs the minter.
- **Refresh:** 32 random bytes base64url unpadded, 30 days, stored as `sha256Hex` in
  `refresh_tokens.token_hash` (`Auth/AuthService.swift`). Rotation sets `revokedAt`, then
  issues. Not transactional. No reuse detection. Keep it that way.
- **Password:** bcrypt cost 12 (`Auth/PasswordHasher.swift`). Go's `x/crypto` writes `$2a$`
  and Swift may write `$2b$`. Test both directions in both servers.
- **Lockouts:**
  - Login: 5 failures → 15 minutes.
  - Reset: code 15 minutes, 5 failures → 30 minutes.
  - Email verification: 24 hours, 5 failures → 30 minutes.
  - MFA: email OTP, 5 minutes, triggered by header `mfa-auth-v1: true`, with empty-tokens
    pending response.
- **Rate limits:** per-IP at `Auth/AuthController.swift:22-31`. `logout` and
  `oauth/:provider/exchange` are unlimited (`:43-44`).

### Analytics that move with auth

- `$set is_internal`: `Auth/AuthService.swift:416`, inside `issueTokens`, so on **every**
  session issued, including refresh, OAuth, pairing and MFA. Distinct ID is the uppercase
  UUID.
- `user_registered` (`Auth/AuthController.swift:53`) and `user_logged_in` (`:68`), with
  `auth_method`, from the password routes only. `user_logged_in` fires even when MFA is
  pending.
- `applyTierOverrideAllowlist` also runs inside `issueTokens` (`BILLING_TIER_OVERRIDE_EMAILS`).

Go's `internal/posthog` has only been tested against a stub (D-wave4-prep). Verify on the
real project after the 8A flip with a PostHog query, as in the final gate.

### Reset, OAuth, Apple

- **Reset** revokes all refresh tokens and issues new ones. It doesn't invalidate
  existing access tokens. Forgot and resend answer identically either way (no
  enumeration).
- **OAuth exchange:** provider map, 404 unsupported. `catch is OAuthError` → 401
  `invalid id_token`. Everything else → 500.
- **Apple:** JWKS from `https://appleid.apple.com/auth/keys`, cached 12 hours, **no refresh
  on unknown kid**. Checks issuer and audience set (`OAUTH_APPLE_CLIENT_ID`, CSV
  `com.lumina.fernando,com.lumina.fernando.web`). Requires email and `email_verified`
  as Bool or string. **No nonce check.** Keep all of this at parity. The JWKS URL must be
  injectable so tests use a stub.
- **Google:** same shape, `GoogleOAuthProvider` 12 tests.
- **User upsert precedence:** identity match, then email link, then create with a random
  password hash, username `oauth-<8hex>`, trial, personal vault (M90: vault ID equals
  user ID), `ensureSoft`.
- **X:** `GET /2/users/me` with the access token, placeholder email
  `<id>@x.luminavault.local`.
- **Phone and magic link:** `PreAuthChallengeStore`, in memory, 5 minutes, 5 attempts.
  Fixed-OTP config for tests. `SMS_KIND` and `TWILIO_*` are sealed.
- **Pairing:** 120 s TTL in the rate-limit driver (P10). `start` is rate-limited by IP,
  `poll` is unlimited, `approve` needs a JWT.
- **WebAuthn:** challenges in memory, 300 s, one manager per origin. RP ID
  `luminavault.fyi`, origins `https://app.luminavault.fyi,https://luminavault.fyi`.
  `swift-webauthn` is pinned to `1.0.0-beta.1` (`Package.swift:33-34`). All 8 routes run
  `guard enabled`, default false. **[owner]** read `WEBAUTHN_ENABLED` and
  `SELECT count(*) FROM webauthn_credentials`. If zero, this group is low risk.
- **SOUL:** `SOULCoreMigrationJob` is awaited before serving. See the jobs table.
  `compose` uses LLM, so wire through `internal/llm` and use a stub server in tests.
  Haiku 5.5 adapters send no temperature, top_p, top_k or prefill: don't add them.

### How sessions survive the cutover

Nothing is re-issued and clients change nothing:

1. **Same keys.** Go inherits `envFromSecret: api-env-production`, including
   `JWT_HMAC_SECRETS`. Don't reseal or rotate during waves 6 to 8.
2. **Same table.** `refresh_tokens` is shared, so a token issued by one server refreshes
   on the other.
3. **Same claims.** A token minted by Go must pass Swift's verifier, because a rollback
   needs it. Add a parity scenario with a Swift binary and a Go binary on one database:
   - register on Swift, call Go; refresh on Go, call Swift; refresh on Swift, call Go;
   - login on Go, call Swift; reset on Go, old refresh token rejected on Swift;
   - bcrypt written by one, verified by the other; MFA started on one, finished on the
     other (DB state);
   - assert unknown `kid`, no `kid` and rejected `scp` behave the same on both.
4. **Clients don't decode tokens.** A grep of iOS and web found no JWT decoding.
5. **What's lost at the flip:** in-flight in-memory challenges. Phone, magic link,
   WebAuthn (5 minutes), pairing (120 s), xAI and Nous OAuth. Those users retry. Flip at a
   low-traffic hour.

### Order inside wave 8

Each step is one infra commit, each with its own G2 and at least 72 hours G3.

1. **8D soul.** No tokens involved.
2. **8A read paths:** `/v1/auth/me`, privacy, billing, usage.
3. **8A logout and refresh.** Refresh mints, so this is the first real test of P9 with
   real clients. If it fails, every session is at risk, so watch the 401 rate on
   `/v1/auth/refresh` against the previous 7 days.
4. **8A remainder:** login, register, mfa, forgot, reset, email. Same flip, since MFA and
   reset cross routes through DB state only, but a single commit keeps metrics readable.
5. **8B** together. Each start and finish route must flip in one commit.
6. **8C** last, O6 decided.
7. **8E** at decommission. See D1.

### Moves that can't be undone

| Move | Why | Rule |
|---|---|---|
| Rotating JWT keys or `LV_SECRET_MASTER_KEY` during migration | A wrong rotation logs everyone out, or makes stored credentials and BYO secrets unreadable on both servers | Don't, until after the archive window |
| Changing WebAuthn RP ID or origins | Every registered passkey stops working | Don't |
| Changing the password hash format or cost | Swift can't verify on rollback if the format changes | Keep cost 12, verify `$2a$` and `$2b$` on both |
| Emails and SMS sent | Can't be recalled | Stub senders in tests; `EMAIL_KIND` in staging must not reach real users |
| Lapse archiver hard deletes | Data gone | Dry run (see Billing) |
| Goose migrations after adoption | Swift doesn't know them | Expand-only until the archive window ends |
| Deleting the Swift Ingress, TLS secret, or PVC | Outage or data loss | Never delete them. See D1, D2 |

Reversible and allowed: new users and rows (same schema), refresh rotation (same table),
flipping prefixes back (revert the infra commit).

### Auth test without real accounts

Parity for everything. Stub JWKS, stub X, stub Twilio, stub Resend. No real API keys, per
the shared rules. Real provider checks are owner steps after each flip: Sign in with
Apple and Google on a TestFlight build, a real email reset, a real SMS if `SMS_KIND`
isn't off. Run `security-audit` over the 8A and 8B diffs before the flip, since it is the
one place a wire-identical port can still be insecure.

## Jobs

### Job move rule

The roadmap says the infra PR moves a job in one commit, with `LV_GO_JOBS` on and the
Swift flag off, and two servers never both run a job unless a lock makes it safe. 8 of
the 10 must-never-both jobs have no Swift flag.

**Proposal, owner's call (O4):** don't add flags in Swift. That would be a Swift deploy
for a server on its way out. For the flagless must-never-both jobs, turn on Go's copy in
the same commit that scales Swift to 0, in this order:

1. Commit A: `replicaCount: 0` on `api-production`, once `/` has been handled (D1).
2. Wait for the pod to terminate: `kubectl -n horus get pods -l app.kubernetes.io/instance=api-production`
   shows none.
3. Commit B: `LV_GO_JOBS` adds those jobs.

Rollback gets larger, since it has to reverse both commits, and the jobs pause for the
length of a merge. That costs nothing: all of them catch up on the next tick. The
**[owner]** alternative is a Swift flag per job, moved one by one.

Jobs with a flag, or safe in two processes, can move earlier, one at a time, as each
port reaches G2.

### Every boot job (the 40 `jobs.md` rows)

Lines are `App+build.swift`. "Move" says when the job can switch:

- **Any:** safe under two processes, moves as soon as the port is at G2.
- **Flag:** has a Swift flag, one commit.
- **Zero:** moves at Swift replicas 0 per O4.
- **n/a:** nothing to move.

| Job | Line | Runs after Swift | Owner | Move |
|---|---|---|---|---|
| Fluent driver + boot migrations | :235, :85-96 | `internal/db` pool. Goose via `App migrate` in PreSync. See D4 | D4 | n/a |
| `PostHogHTTPClient` | :245 | `internal/posthog` (done). Audit 9 tests | D-tests | n/a |
| `SOULCoreMigrationJob` | :258-260 | One-shot. Port to a `backfill` subcommand or goose data step, not a boot blocker. **[owner]** Run and confirm it reports nothing to do, before Swift's last boot | 8D | Zero |
| `LapseArchiverService` | :277 | `internal/jobs`, dry run first | 7A | Zero |
| `DeletedDataPurgeService` | :283 | `internal/jobs`. Needs the volume | J1 | Any |
| OTel exporters | :299-302 | `internal/telemetry` (done) | Done, P5 for names | n/a |
| `DiskSpaceMonitorService` | :440 | `internal/jobs` wrapping `internal/diskguard` (it measures at check only, so a sampler is the missing part). Must keep the log line | J1 | Any |
| `AchievementsWorker` | :506 | `internal/achievementsink` (done). Audit `apns.Notify*` on a real device | Done | Moves with last producer |
| `RetrievalTelemetryWorker` | :525 | Go inserts directly (`wave-4-plan.md`). Check W5 chat does too | 4F, W5 | Moves with last producer |
| `ValkeyPersistDriver` | :586 | P10, only if `redis` | P10 | n/a |
| `MemoryHitCountUpdateQueue` | :595 | `internal/memorystore` (done) | Done | Moves with search |
| `HermesContainerReaperService` | :945 | O1: port or retire. Also must not run where no docker host is visible, per `jobs.md` | P4 | Zero |
| `ProviderRegistry` | :1354 | `internal/llm` registry. `run()` only waits for shutdown. Nothing to run | W5 | n/a |
| `SelfImprovementScheduler` | :1567 | `internal/jobs`, `SKIP LOCKED` | W5 | Any |
| `TranscribeProviderRegistry` | :1729 | W5. Whisper in the cluster, no paid API | W5 | n/a |
| `VisionEmbedProviderRegistry` | :1824 | `internal/embedding`, already on Go for vision | Done | n/a |
| `MeTodayCache` | :1851 | `me` module, DB check per `flip-blockers.md` | Done | n/a |
| `PeriodRecapThemesWorker` | :1907 | `internal/jobs`, plus claim row | J2 | Any |
| `MultimodalIngestionWorker` | :2681 | `internal/jobs`, `SKIP LOCKED` + lease, volume mounted | After 6A (J1 slot) | Any |
| `HermesMirrorRefreshWorker` | :2825 | `internal/jobs`, plus nudge channel | 6B | Zero, or Swift flag |
| `AnalyticsMaintenanceService` | :2903 | `internal/jobs` | J1 | Any |
| `CalendarSyncWorker` | :3013 | `internal/jobs`, must be single-process | J1 | Zero |
| `SynthesisWorker` | :3057 | `internal/jobs`. Off in prod (default false, key absent) | J2 | Flag |
| `RetrievalLeakReportWorker` | :3069 | `internal/jobs`, unique key on table | J1 | Any |
| `GraphLayoutWorker` | :3083 | `internal/jobs` | J1 | Flag |
| `EmbeddingReembedService` | :3101 | `internal/jobs`. See "Heal pass" | J2 | Flag |
| `MemoryIndexWorker` | :3123 | `internal/jobs`, `SKIP LOCKED`, after chat | W5 | Any |
| `KnowledgeExtractionWorker` | :3144 | `internal/jobs`, `SKIP LOCKED` | J2 | Flag |
| `HermesProfileReconcilerService` | :3282 | `internal/jobs`, 04:00 UTC | P8 | Zero |
| `HermesRunsService` | :3410 | Boot re-attach in Go | 6C | Moves with 6C flip, see 6C |
| `Task { skillRunner.startEventSubscriptions }` | :3612 | Log-only today. Drop in Go (per wave 4 for `vault_file_created`) | W5 | n/a |
| `CronScheduler` | :3624 | `internal/jobs` | W5 | Zero |
| `ReminderScheduler` | :3634 | `internal/jobs` | J1 | Zero |
| `LearnedDigestScheduler` | :3642 | `internal/jobs`, claim row | J2 | Any |
| `MeetingBriefScheduler` | :3664 | `internal/jobs`, claim row | J2 | Any |
| `WorkflowEngine` | :3770 | `internal/jobs`, `SKIP LOCKED` + lease | W5 | Any (two engines are safe) |
| `WorkflowScheduler` | :3784 | `internal/jobs`, unique key | W5 | Any |
| `WorkflowMaintenanceService` | :3789 | `internal/jobs` | W5 | Any |
| `LegacyJobWorkflowMigrator` | :3794 | `internal/jobs`, or retire after a query shows no legacy jobs. **[owner]** | W5 | Zero |
| `DailyPostWorker` | :3927 | `internal/jobs`, claim row | J2 | Any |

### Jobs with no ServiceGroup entry

Absent from the `jobs.md` table. They need a home or a recorded decision.

| Job | Evidence | After Swift | Owner |
|---|---|---|---|
| `MemoryPruningJob`, `MemoryScoringService`, `MemoryPruningService` | `Memory/MemoryPruningJob.swift` (61), `Memory/MemoryScoringService.swift` (108), `Memory/MemoryPruningService.swift` (115); comment at `App+build.swift:3306-3322` says host cron | Admin routes in 7B. Schedule: O3 | 7B |
| `HealthCorrelationJob` | `POST /v1/admin/health/correlate`, 78 lines | Admin route in 7B. Schedule: O3 | 7B |
| `UsageEventsRetentionService` | `App+build.swift:3342-3356`, disabled unless `USAGE_EVENTS_RETENTION_DAYS` | Admin route in 7B. Schedule: O3 | 7B |
| `ChunkBackfillService` | `MemoryAdminController.swift:36` | Admin route in 7B. On demand | 7B |
| `reconcile`, `reap-orphans` admin routes | hermes-profiles group | Admin route in 7B. On demand | 7B, P4 |
| `MemoryKeepJudge` | 96 lines, used by pruning | With pruning | 7B |
| `System/CentralHermesManager.swift:125` health-poll loop | Request-started | O1 | 7C |

Request-started tasks. Each is lost on restart in Swift too, so parity is a goroutine with
a detached context, or a row:

| Task | File:line | Owner |
|---|---|---|
| `LLM/Routing/ProviderFailoverLogger.swift:33` | Log | W5 |
| `RoutedLLMTransport.swift:194,211,252` (meter, trace) | Metering | W5 |
| `TranscribeService.swift:70` | Metering | W5 |
| `VisionEmbedService.swift:65` | Metering | Done in vision: audit |
| `RoutedTTSTransport.swift:51` | Metering | W5 |
| `SkillRunner.swift:536` | Skill run | W5 |
| `LLMController.swift:56` | Side-effect push | W5 |
| `Me/ProvidersController.swift:354` | Probe | W5 |
| `Vault/VaultController.swift:330` | `captureFiling` suggestion | 4B: audit |
| `Calendar/CalendarTokenStore.swift:108` | In-flight refresh dedupe | Done in calendar: audit |
| `Apple/DeviceCommandBroker.swift:44` | Timeout | W5 end |
| `HermesGateways/WhatsAppPairingService.swift:69,200` | Pump | 6D |
| `HermesGateways/PhotonProvisioningService.swift:114,290` | Poller | 6D |
| `HermesGateways/HermesGatewayApplyService.swift:81` | Apply | 6D |
| `System/HermesUpdateService.swift:81,109` | Update | 7C |
| `Capture/LinkCaptureService.swift:152` | Link enrichment | Capture: audit |
| `Hermes/Runs/HermesRunsService.swift:295` | Watcher | 6C |
| `App+build.swift:3852` | WS cleanup | `/v1/ws` |

`Vault/VaultNoteMemoryUpsertQueue` is never constructed. `AutoKBCompileJob` doesn't exist.

### Startup tasks in `App+build.swift` and `App.swift`

| Task | Line | After Swift | Owner |
|---|---|---|---|
| `registerMigrations`, `MigrationGate.migrateOnce`, `StartupRetry` | :85-96 | Removed. PreSync `App migrate`. Go's waits are in `waitForDatabase`. Audit `StartupRetry` 4: the retry loop is for pool startup | D4 |
| `loadJWTKeys` | :120,:131 | `internal/auth.LoadKeys` (done) | Done |
| `PostHogAnalytics.install` | :245 | `internal/posthog` | Done |
| `SOULCoreMigrationJob.run` | :260 | See table | 8D |
| OTel bootstrap, `OTelLatch` | :299-302, :4063 | `internal/telemetry` | Done |
| Skills builtin scan (`skills.builtinScan.enabled`) | `Skills/SkillCatalog.swift:52` | P3 + W5 | W5 |
| CLI `migrate` (with `--revert`) | `App.swift` | `App migrate`. Go has no revert. Rollback is restore-from-dump, see D4 | D4 |
| CLI `bootstrap-admin` | `App.swift` | Port (3 tests) or waive (O5) | D4 |
| CLI `backfill-hermes-profiles`, `backfill-vault-space-folders` | `App.swift` | One-shot. Retire if **[owner]** queries show nothing left. The image contract can drop them | D4 |

### Heal pass

`EmbeddingReembedService` (556 lines, 7 tests, plus 3 heal tests in
`EmbeddingDegradationTests`) heals rows saved with no vector. In production it is
**currently not running**: heal defaults to true only when
`embeddingRegistry.activeKind != .deterministic` (`App+build.swift:3101`), and production's
provider is deterministic per `wave-4-plan.md`. D-wave4-prep names this a decommission
blocker. It stays one: the day real embeddings are enabled, Go must heal.

J2 ports it behind the same conditions. Verification: `SELECT count(*) FROM memories WHERE
embedding IS NULL` **[owner]**, and a test run on a stub provider. Moving is by flag.

### J1, J2: job ports not owned by a wave

| Group | Jobs | Swift lines | Tests to port | Tests to audit |
|---|---|---|---|---|
| J1 | `DeletedDataPurgeService`, `DiskSpaceMonitorService`, `AnalyticsMaintenanceService`, `GraphLayoutWorker`, `RetrievalLeakReportWorker`, `ReminderScheduler`, `CalendarSyncWorker`, `MultimodalIngestionWorker` | 291 + 255 + 46 + 81 + 217 + 146 + 79 + 35 (worker only; its service is 911, read what 4E already ported) | `RetrievalLeakReportWorker` 3, `RetrievalTelemetrySample` 3, `RetrievalTelemetryWorker` 2, `SpaceTopology` 2, `ReminderScheduler` 3 = 13 | `DeletedDataPurgeJob` 6, `GraphLayoutService` 6 = 12 |
| J2 | `KnowledgeExtractionWorker`, `SynthesisWorker`, `EmbeddingReembedService`, `PeriodRecapThemesWorker`, `LearnedDigestScheduler`, `MeetingBriefScheduler`, `DailyPostWorker`, community moderation sweep | 825 + 474 + 556 + 421 + 213 + 251 + 516 | `Insight` 15 (provisional), `LearnedDigestScheduler` 9, `MeetingBrief` 19, `EmbeddingReembedService` 7 = 50 | `KnowledgeExtractionWorker` 9, `DailyPostWorker` 5 = 14 |

J1 can start when P1 lands. J2 needs `internal/llm` wired, and for `DailyPostWorker` the
community flip. All are parallel, each its own branch. Each job's PR includes a metric for
the gate and a matching Swift flag state in the PR body. `LLM` jobs use stub servers, never
real keys.

`wave-4-plan.md` says `KnowledgeExtractionWorker` and `GraphLayoutWorker` are movable "any
time" or after `internal/llm`. This keeps that and just gives them a group.

### Who else claims jobs

Roadmap, `decisions.md`, `flip-blockers.md` and `wave-4-plan.md` were searched per job.
The roadmap's wave table names a few by wave (achievements; `CalendarSyncWorker`, community
daily post; memory index, knowledge, graph layout, synthesis, re-embed; `MemoryKeepJudge`,
self-improvement, workflow engine; reconciler, cron bridge, mirror sync; lapse archiver,
usage-events retention). **No document gave these an owner before this plan** (at most
they're mentioned as staying on Swift): `HermesContainerReaperService`,
`HermesRunsService`, `RetrievalLeakReportWorker`, `LearnedDigestScheduler`,
`MeetingBriefScheduler`, `DeletedDataPurgeService`, `AnalyticsMaintenanceService`,
`ReminderScheduler`, `CronScheduler`, `DiskSpaceMonitorService`,
`LegacyJobWorkflowMigrator`, `SOULCoreMigrationJob`, `HealthCorrelationJob`,
`MemoryPruningJob`, `MemoryScoringService`, `WorkflowScheduler`,
`WorkflowMaintenanceService`.

The W5 plan must confirm: `SelfImprovementScheduler`, `MemoryIndexWorker`, `CronScheduler`,
`WorkflowEngine`, `WorkflowScheduler`, `WorkflowMaintenanceService`,
`LegacyJobWorkflowMigrator`, and the skill event subscription.

## Channels (coupling.md), final disposition

| Channel | Disposition | Owner |
|---|---|---|
| EventBus `memory_upserted`, `achievement_unlocked` | `InvalidateToday` / DB check | Done |
| EventBus `vault_file_created`, `health_event_synced` | Log only. Dropped | Done |
| EventBus `hermes_run_event` | Go store publish + SSE, DB poll fallback | 6C |
| `ConnectionManager` | One Go hub. `/v1/ws` split flips with `kb-compile` | W5 |
| `DeviceCommandBroker` | Waiters move with `/v1/ws`, MCP, Skills | W5 |
| `MemoryCompileProgressPublisher` | With `/v1/ws` | W5 |
| Workflow run events | Already DB | W5 |
| Gateway-apply SSE, WhatsApp, Photon | One group | 6D |
| Self-update SSE | One group | 7C |
| Vault activity SSE | Moved | Done |
| Ingestion SSE | Already DB | Done |
| `MeTodayCache` | Go module | Done |
| `HermesMirrorNudging` | In-process channel in Go, 6A + 6B flip together | 6A, 6B |
| `AchievementsWorker` inbox | Go sink | Done |
| `RetrievalTelemetryWorker` inbox | Direct insert | 4F, W5 |
| `PeriodRecapThemesWorker` inbox | Claim row; held route | J2 |
| `MemoryHitCountUpdateQueue` | Go queue | Done |
| `HermesContainerManager` image override | O1 | 7C |
| `UserCredentialStore` cache (600 s) | Go port, with invalidate on upsert and delete, in one process | W5 |
| `AgentRoomRunRegistry` | Go, in process | W5 |
| Rate-limit counters, `InFlightLimitStore` | P10. Split across servers is harmless during migration | P10 |
| `CalendarOAuthSessionStore` | Moved with calendar in the same flip | Done |
| `XaiOAuthSessionStore`, `NousOAuthSessionStore` + registries | Pairs flip together | 6E |
| `WebAuthnChallengeStore` | Group flips together | 8C |
| `PreAuthChallengeStore` | Group flips together | 8B |

No Valkey bridge is needed anywhere, as long as those flips stay atomic.

## Test accounting

Script: every `.swift` under `Tests/AppTests`, count of `@Test` and `func test*`, search
for the base name in `internal/**/*_test.go`, `parity`, `tools`, `cmd`. The assignment below
was checked by running a name-to-group map over the file list.

Whole suite: 343 files, 2218 tests. Named in no Go test at `cda9718`: 213 files with tests,
1383 tests.

| Group | Files | Tests | Swift test files |
|---|---|---|---|
| P2 | 9 | 68 | `FilesystemHermesGateway`, `HermesDataPathService`, `FilesystemHermesTransport`, `MirrorTransportFallback`, `GatewayOnlyMirror`, `HermesWorkspacePath`, `JSONValueFoundationBridge`, `HermesDashboardClient`, `HermesDashboardProbe` |
| P4 | 4 | 25 | `HermesContainerManager`, `HermesContainerReaperService`, `HermesTenantConfigSeed`, `HermesAudioProxySeed` |
| P8 | 2 | 9 | `HermesProfileReconciler`, `HermesProfileService` |
| P9 | 4 | 25 | `JWTKeyLoader`, `SessionToken`, `SessionTokenScope`, `JWTAuthenticatorE2E` |
| P10 | 3 | 25 | `RateLimitMiddleware`, `RateLimitStorageFactory`, `ValkeyPersistDriverIntegration` |
| PA (audit) | 3 | 21 | `SecretBox`, `HermesEndpointResolver`, `IdempotencyMiddleware` |
| 6A | 3 | 15 | `HermesConfigController`, `UserHermesConfig`, `HermesCapabilitiesParse` |
| 6B | 8 | 62 | See group |
| 6C | 8 | 78 | See group |
| 6D | 3 | 19 | See group |
| 6E | 4 | 23 | See group |
| 6F | 5 | 25 | See group |
| 7A | 7 | 41 | See group |
| 7A (audit) | 5 | 67 | See group. `BYOEntitlementMiddleware` and `StorageQuotaService` are already named in Go tests |
| 7B | 6 | 28 | See group |
| 7C | 1 | 3 | `HermesUpdateService` |
| 8A | 7 | 38 | `AuthFlow`, `EmailVerification`, `HermesProvisioningLifecycle`, `ResendEmailOTPSender`, `PasswordHasher`, `PrivacyToggle`, `UsernamePolicy` |
| 8B | 7 | 52 | `GoogleOAuthProvider`, `XOAuth`, `EmailMagicLinkFlow`, `PhoneAuthFlow`, `PreAuthChallengeStore`, `SMSSender`, `PairingFlow` |
| 8C | 3 | 14 | `WebAuthnAntiEnumeration`, `WebAuthnEnrolmentAuth`, `WebAuthnOrigins` |
| 8D | 5 | 46 | `SOULInit`, `SOULService`, `SoulControllerCompose`, `SOULComposer`, `SOULCore` |
| 8E | 2 | 8 | `AppTests` (already named), `ClientCapabilities` |
| `/v1/ws` | 1 | 9 | `WebSocketBroadcastGuard` |
| J1 | 5 | 13 | `RetrievalLeakReportWorker`, `RetrievalTelemetrySample`, `RetrievalTelemetryWorker`, `SpaceTopology`, `ReminderScheduler` |
| J1 (audit) | 2 | 12 | `DeletedDataPurgeJob`, `GraphLayoutService` |
| J2 | 4 | 50 | `Insight` (provisional: check whether dashboard's module already covers it), `LearnedDigestScheduler`, `MeetingBrief`, `EmbeddingReembedService` |
| J2 (audit) | 2 | 14 | `KnowledgeExtractionWorker`, `DailyPostWorker` |
| D-tests | 9 | 27 | `M00EnableExtensions`, `M114HermesMirror`, `M39HnswAndTsvector`, `M41HermesTenantContainers`, `MigrateCommand`, `BootstrapAdminCommand`, `StartupRetry`, `TenantIsolation`, `UsabilityEndpoints` |
| D-tests (audit) | 5 | 52 | `APNSNotificationService`, `APNSPushRouting` against `internal/apns`; `PostHogHTTPClient` against `internal/posthog`; `CommunityController`, `CommunityUnit` before the community flip |
| Support, waive | 2 | 3 | `PrimeBaseDatabase`, `TestPostgres` |

That leaves 535 tests this plan doesn't own. They need a claim in another plan:

- **Wave 4, 68 tests, not named in a Go test yet.** Check each against 4A to 4F:
  `KB/*` 24 (`kb-compile`), `Indexing/Wikilink` 20, `Import/ImportLogic` 13,
  `Ingestion/HermesIngestionTransport` 2, `Ingestion/MultimodalIngestionCapability` 3,
  `Capture/CaptureFilingService` 3, `Knowledge/KnowledgeReasoningPath` 3.
- **W5, 467 tests:** `Agents` 23, `Chat` 49, `Grok` 6, `LLM` 56, `LLM/OpenAIAudio` 15,
  `LLM/Routing` 56, `LLM/Transcribe` 36, `MCP` 42, `SelfImprovement` 13, `Skills` 93 (all
  but `ReminderScheduler`), `TTS` 5, `Workflows` 16, `Settings/LLMPreferencesController` 11,
  `Weather/WeatherForecastTool` 10, `Services/CronPush` 1, `Apple/DeviceCommandBroker` 2,
  `FollowUpGenerator` 9, `HermesMemoryService` 8, `MemoControllerTitle` 4, `MemoGenerator` 4,
  `QueryStream` 5, `URLSessionHermesChatTransport` 3.

The D-tests group runs last and also takes whatever still fails P7. `TenantIsolation` matters
most: it asserts isolation across modules, so make it a parity scenario, not a unit test.
Migrations and CLI tests run against goose as schema checks, or get a recorded waiver.

## What depends on wave 5

| Item here | Needs from W5 |
|---|---|
| 6C runs | Chat escalation: the same service instance (B12). Order the two together |
| `/v1/me/hermes/cron` | Port in W5 against P4's interface, or move to wave 6 (B13). Needed by O8 |
| 8D `soul/compose`, community moderation, `/v1/reminders/detect`, J2 | `internal/llm` wired for non-chat callers |
| `/v1/ws`, `kb-compile`, `/v1/devices/command`, `mcp`, `skills` | Flip together at the end of W5. The roadmap names `coder/websocket`, not yet in `go.mod` |
| 6B `jobs/install-compile` | `MemoryCompileProgressPublisher` on the same server |
| Audio tokens | `AudioJWTAuthenticator` in Go before `/v1/audio` flips. Minter in P4 |
| P3 | W5 skills reads the same embed |
| Jobs | W5 claims every job marked W5 in the jobs table |

## Decommission (phase 12)

The roadmap's Phase 12 text is stale on one point: it says move `go/*` to root, but Go
already lives in its own repo. Its other steps hold and are expanded here.

### D1: routing and replicas

The principle is **no renames**. Per `CLAUDE.md`, and because it is the safest option,
the `api-production` release stays, with the same Service, Ingress, TLS secret and PVC,
and only its image changes. That keeps Isis, Alloy, Traefik labels, NetworkPolicies and
the PVC owner without touching them. So the `api-*` Deployments aren't removed: `api-go-*`
are. The alternative, deleting `api-*` and keeping `api-go-*`, would take the Ingress, TLS
secret and PVC with it (B3, B4) and force a rename in each of those consumers.

Steps, each an infra PR the owner merges:

1. **All prefixes on Go.** Every path in `routes.json` except `/` is in `ingress.extraPaths`.
   `/health`, `/.well-known` and `/v1/ws` included. `/` then serves only `GET /` and unknown
   paths on Swift.
2. **Small chart change:** add `ingress.defaultService`, defaulting to the release name,
   so `/` can point at `api-go-production`. `extraPaths` can't express it, since the chart
   always adds its own `/` (`charts/app/templates/ingress.yaml`). Without this, Swift at 0
   turns unknown paths into 503, where today they are Swift's 404.
3. **Soak with Swift still up**, `/` retargeted, for 24 hours. Swift gets only direct
   in-cluster traffic. The Traefik counter for `horus-api-production-http@kubernetes` goes
   to zero. The 7-day traffic check before this comes from Swift's own counter (Z7).
4. **Swift to 0**: `replicaCount: 0` on `api-production`. Runs the job move (O4). Final
   Z gate first. `migrate.enabled` must be `false` for this soak, or a sync runs Swift's
   migrate with no reason to. Don't delete the migrate-job template.
5. **Soak with Swift at 0.** See the A gate for the length.
6. **Adopt.** See D4.
7. **Image swap.** `apps/api/values-common.yaml` `image.repository` →
   `ghcr.io/luminavault/luminavaultserver-go`, tag from Go's pin, `replicaCount: 1`,
   `migrate.enabled: true`. Leave `LV_GO_JOBS` empty on this release for now, so two Go
   servers are up and only `api-go-production` runs jobs. Both on one RWO volume on one
   node, as today.
8. **Jobs move.** Commit: `api-go-production` `LV_GO_JOBS: ""`. Wait for it to apply.
   Commit: `api-production` gets the full list. Two commits, so there's never a moment with
   two runners. A gap is fine.
9. **Remove `extraPaths` and `defaultService`.** All traffic is on `api-production`, which
   is Go.
10. **Delete `api-go-production`, `api-go-staging`** and their `allow-web-ingress-go-*`,
    netpol entries (`horus.yaml:77-113` and the `api-go` lines in `allow-postgres`,
    `allow-hermes-production`, `allow-whisper`, `allow-feeds`). They use
    `persistence.existingClaim`, so deleting them doesn't touch the PVC. Check that before
    merging: `helm template` shows no PVC for that release.

Run steps 1 to 10 on staging first, in full, including adoption. Staging differs: no Ingress,
own volume `api-staging-data`, no Hermes.

`image.repository` lives in `apps/api/values-common.yaml`, which both environments share. For
staging-first, set the override in `apps/api/values-staging.yaml` and move it to common when
production goes. Until production has swapped, **don't run the promote workflow for `api`**: it
copies the staging tag, so production would get a Go tag on the Swift repository and fail to
pull.

### D2: shared volume

1. **Before step 1 of D1, [owner]:** set the PV to `Retain`.
   ```sh
   PV=$(kubectl --kubeconfig ~/.kube/maat.yaml -n horus get pvc api-production-data -o jsonpath='{.spec.volumeName}')
   kubectl --kubeconfig ~/.kube/maat.yaml patch pv "$PV" -p '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'
   ```
2. **Never** delete the `api-production` Argo app or set `persistence.enabled: false`.
   `helm.sh/resource-policy: keep` is on the PVC, but Argo's handling of it wasn't verified,
   and the app has `prune: true`. Treat it as unsafe.
3. Restore-test the newest `api-data-backup` (04:15 CronJob, excludes `_deleted_*`) before
   Swift goes to 0. Nothing else has a copy of tenant vault files.
4. UID stays 999 (D3). `DeletedDataPurgeService` and `LapseArchiverService` act on this
   volume, and only Go runs them after the move. Confirm the file-ownership case with a
   write from Go on a directory Swift created.

### D3: observability

- Alert rules: rewrite `hb_requests_total` to `http_server_request_duration_seconds_count`
  **[inferred]** (read the name from a live scrape first). `luminavault_*` rules stay,
  thanks to P5.
- `observability/dashboards/luminavault.json`, `apps-overview.json:378,389`,
  `observability/scripts/build_dashboards.py:292,293,416,419,450`: pod and Loki selectors
  still match `api-production` after the image swap. During the `api-go-production` period
  they don't, so extend the selectors to both at step 1 of D1, then trim at step 10.
- `cluster/alloy/values.yaml:236`: blackbox probe stays.
- Sentry: `SENTRY_*` are sealed, but no App source reads them; only `ci.yml` refers to
  them. Nothing is lost.
- `docs/llm-haiku-trial.md:67` greps `deploy/api-production` logs for `anthropic_usage`.
  Go must log the same key if the trial is still running.
- Runbooks to rewrite: `docs/runbook-loci-telegram.md:265`, `apps/loci/data/hermes-egress.yaml:26`,
  `docs/deployment.md`, `docs/hosted-projects-namespaces.md`, `docs/machine-migration.md:130,139`,
  `docs/runbook-cutover.md`, gethomepage annotations "Swift API — production".
  Replace the distroless case with `kubectl debug` or an ephemeral pod.

### D4: database migrations after Swift

Who: **Go, `App migrate`, in the same PreSync hook**, with the same pg_dump initContainer
(`charts/app/templates/migrate-job.yaml`). `values-common` already has `migrate.enabled: true`.
The hook reads `args: ["migrate"]`, so the image swap is enough. The Go command refuses a
Fluent-managed database (`ErrFluentManaged`) until adopted, and a failed PreSync **blocks the
whole sync**. So adoption comes first.

Steps:

1. Freeze check (P6): no new Fluent migrations. Compare `_fluent_migrations` with
   `internal/migrations/sql/fluent-manifest.txt`. `AdoptFluent` refuses on any difference
   (`internal/migrations/migrations.go:95-102`).
2. Staging first.
3. **[owner]** in production, after a fresh dump:
   `kubectl -n horus exec deploy/api-go-production -- /app/App migrate --adopt-fluent`.
   It is a binary exec, so distroless works.
4. `SELECT count(*) FROM goose_db_version` shows baseline + mirrors.
5. From now on, new migrations are goose only and **expand-only** until the archive window
   ends, so a Swift rollback can still run against this schema. `_fluent_migrations` stays
   untouched. **[inferred]** Swift's boot migrate sees everything applied and does nothing,
   which is what makes this safe under B15. Prove it on staging by booting Swift after
   adoption.

Rollback of a migration: Go has no `--revert`. Restore from the hook's pg_dump
(`pre-migrate-<ns>-<ts>.dump`) or write a forward fix.

Subcommands (O5): `bootstrap-admin` should be ported, because there's no other way to create
the first admin on a restored database. **[inferred]** Nothing in infra calls any of the three.

### D5: Swift CI and release

Order matters (B9).

1. Before the image swap: delete or disable Swift `release.yml`, or set it to fail on
   `main`, in the same window as the swift-freeze. Merge to Swift `main` then can't write to
   `apps/api/values-staging.yaml`.
2. Before the image swap: point the Go release at `apps/api/values-staging.yaml` and promotion
   at `apps/api/values-production.yaml`. `promote.yml` already handles `apps/api`, so it needs
   no change at that step. Remove any `apps/api-go` copy at step 10.
3. Swift `ci.yml` stays on, but isn't required.

### D6: everything the Swift repo owns that isn't the server

Each needs a new home before archive. An archived repo's scheduled workflows stop, and its
images stop building.

| Thing | Today | Move to | Why it can't wait |
|---|---|---|---|
| `.github/workflows/hermes-image.yml`, `docker/hermes.Dockerfile`, `docker/hermes-cont-init.sh`, `Sources/App/Resources/HermesSkills/**` | Publishes `ghcr.io/luminavault/hermes` | `LuminaVaultHermesAgent`, which has `luminavault-image.yml`. **[owner]** Confirm which one built the digest in `apps/hermes/hermes.yaml` (`sha256:9892ad09`). Memory says the fork did | Production Hermes image pipeline. Skills must be kept in sync with Go's embed |
| `.github/workflows/restore-drill.yml` (`0 6 1 1,4,7,10 *`), `scripts/backup*.sh`, `scripts/restore.sh`, `docker/backup*` | Quarterly drill | Infra repo or Go repo | Silent loss of the only restore test. Next run 2027-01-01 |
| `plugin-runner/`, `docker/plugin-runner.Dockerfile` | Rust, wasmtime. Not in infra | Own repo or Go repo. **[owner]** Is it deployed anywhere? `PLUGIN_RUNNER_URL` is sealed | Marketplace and plugins |
| `docker/photon-sidecar` | Not configured in production | Own repo, or retire with O1 | 6D |
| `Sources/AppAPI/openapi.yaml` | Web `gen:api` reads it by relative path (B8). Spec drift known: see `capture-safari-spec-drift` | Go repo. Update `LuminaVaultWebApp/package.json:19` | Web can't regenerate types |
| `scripts/generate-bruno.sh`, `make bruno-regen` | `LuminaVaultCollection` | Go repo | Tooling |
| `Sources/App/Resources/Skills` | `Bundle.module` | P3 | Runtime |
| `docs/jobs.md`, `docs/production-env-wiring.md`, `docs/superpowers/plans/`, `AGENTS.md` | Roadmap and rules | Copy what's still true to Go repo `docs/` | Reference |
| `go-migration/phase-0-contract` branch | Local only | Push (P6) | Lost if the checkout goes |
| `docker-compose.production.yml`, `web/`, `Caddyfile` | VPS era | Nothing. **[inferred]** Apex pages are served by `LuminaVaultWebApp`. Verify: `curl -sI https://luminavault.fyi/privacy` | None if verified |
| `tools/golden` and `Makefile swift-transcripts` in Go | Needs a Swift binary | Keep, pinned image (D7) | Parity oracle |
| `LuminaVaultShared` server role | Roadmap says swap for spec + contract test | **[owner]** Is it worth doing now? iOS keeps importing Shared either way; nothing here blocks archive | Optional |

### D7: parity oracle after archive

Go CI pulls `ghcr.io/luminavault/luminavaultserver:fdbed9b0…` (`parity/SWIFT_IMAGE`). GHCR
packages aren't removed by archiving a repo **[inferred]**, and read access for CI must
survive. Keep the image. Check for any retention policy that would prune it. Parity keeps
running on any Go PR until the owner removes it, which is a separate decision.

### D8: infra to remove (only after the A gate)

- `apps/api-go/`, `argocd/apps/api-go-*.yaml` (D1 step 10).
- `allow-web-ingress-go-*`, `api-go` entries in netpols.
- `ingress.extraPaths` values, `ingress.defaultService`.
- `FLUENT_AUTOMIGRATE` from plain env and sealed; `SWIFT_BACKTRACE`, `SENTRY_*`, `HTTP_HOST`,
  `HTTP_PORT`. These are dead for Go. See "Secrets".
- `HERMES_PER_TENANT_*`, `DOCKER_BINARY_PATH` if O1 retires docker.

**Don't remove:** `api-production` release, Service, Ingress, `api-luminavault-fyi-tls`,
PVC, `api-env-production`, `api-apns-key`, `api-staging`.

### D9: secrets

`secrets/horus/api-env-production.yaml` has 119 sealed keys. Go reads 41 today. All keep
working after the swap, because the secret name and namespace don't change. **A blob is
bound to both, and sealing for the wrong name decrypts to nothing with no error, so this
plan doesn't rename either.** Removing a dead key needs `kubeseal --merge-into` with the
same scope; verify the rest of the keys still appear in the Secret afterward.

| Keys not read by Go today | Who starts reading |
|---|---|
| `REVENUECAT_WEBHOOK_SECRET`, `BILLING_COLD_STORAGE_PATH` | 7A |
| `USAGE_DEGRADE_MODEL`, `USAGE_FREE_MTOK_DAILY`, `USAGE_PER_SKILL_MTOK_DAILY` | W5 LLM metering, or 7A if W5 doesn't claim them |
| `METRICS_SECRET` | 7B |
| `OAUTH_APPLE_CLIENT_ID`, `OAUTH_GOOGLE_CLIENT_ID`, `OAUTH_X_CLIENT_ID`, `SMS_KIND`, `TWILIO_*` | 8B (`OAUTH_APPLE_CLIENT_ID` is also plain env) |
| `WEBAUTHN_*` | 8C |
| `RATE_LIMIT_STORAGE_KIND` | P10 |
| `HERMES_GATEWAY_KIND`, `HERMES_API_KEY`, `HERMES_PER_TENANT_*`, `DOCKER_BINARY_PATH` | P2, P4 or retired with O1 |
| `INGESTION_PUBLIC_BASE_URL` | J1, with `MultimodalIngestionWorker` (`Ingestion/MultimodalIngestionService.swift:440-447`). Also W5 MCP (`App+build.swift:2421-2422`) |
| `KNOWLEDGE_GRAPH_*` | J2 |
| `CERBERUS_*`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `LLM_PROVIDER_*` (see D-wave4-prep on `_APIKEY` naming), `TRANSCRIBE_PROVIDER`, `TTS_*`, `LOCAL_EXECUTION_TOOL_BROKER_ENABLED`, `VISION_EMBED_PROVIDER_COHERE_APIKEY`, `..._BASEURL` | W5 |
| `OTEL_EXPORTER_OTLP_*`, `OTEL_METRICS_EXPORTER`, `OTEL_TRACES_EXPORTER` | P5 (read by the OTel SDK env directly **[inferred]**) |
| `FLUENT_AUTOMIGRATE`, `HTTP_HOST`, `HTTP_PORT`, `SWIFT_BACKTRACE`, `SENTRY_*` | Nobody. Dead |

P7-style check: a script comparing the sealed key list against every env name Go reads. Any
key neither read nor listed as dead fails the gate. Note `ANTHROPIC_FIRST` and the
`haiku-trial` key flow are plain env: read `platform/infra/docs/llm-haiku-trial.md` first.

No paid speech API: `TRANSCRIBE_PROVIDER` goes to the in-cluster Whisper
(`http://whisper.horus.svc.cluster.local:8000/v1`). `allow-whisper` already names `api-go`
at `horus.yaml:562-569`, and `api-production` keeps its entry.

### D10: clients that hard-code something

| Client | Hard-coded | Impact |
|---|---|---|
| iOS `Utilities/Config.swift:13`, `Features/Settings/WebSignIn/WebSignInApprovalView.swift:112` | `https://api.luminavault.fyi` | None, host doesn't change |
| iOS `API/Core/CertificatePinning.swift`, `scripts/verify-cert-pins.sh` | CA SPKI pins, not the leaf | None so long as host and issuer don't change. Never touch the cert or ingress TLS |
| iOS `KBCompileWebSocketClient`, `AppState.swift:157` | `/v1/ws`, device commands | Flip at end of W5. Test a real device first |
| iOS entitlement `$(WEBAUTHN_RP_ID)`, AASA `84X9WYBF36.com.lumina.fernando` (`App+build.swift:382`) | Exact JSON | 8E parity; byte compare. Apex AASA is separate (`LuminaVaultWebApp/docs/apex-well-known.md`) |
| Web `wrangler.jsonc`, `deploy.yml` | API host | None |
| Web `src/lib/types/generated/api.d.ts:5447,5470` | `/v1/kb-compile*` | Keep aliases. Check whether `/options` aliases have live callers before any removal. None is planned |
| Web `package.json:19` | Swift checkout path | B8 |
| RevenueCat dashboard | Webhook URL | None |
| Isis | `api-production` Service | B5. Same name after swap, but needs Go during the soak |
| Alloy | Blackbox `https://api.luminavault.fyi/health` labelled `api-production` | None |
| Hermes tenants | `POST /v1/hermes/mirror/webhook/:token`, base URL in tenant config | Host unchanged. Audit `HermesTenantConfigSeed` for an in-cluster name |
| Installed iOS versions | Any route | Why no route gets removed |
| Probes | `Server: ObsidianClaudeBrainServer` | D-router-parity used it as a flip probe. No client depends on it |

### D11: what "archive" must do, and how to reverse it

Archive is a GitHub setting plus a guarantee that the old world can come back.

1. Tag: `git tag swift-final-<sha>` on the last deployed commit (`fdbed9b0` unless it moves).
   Push the tag. Push `go-migration/phase-0-contract`.
2. Keep `ghcr.io/luminavault/luminavaultserver:<sha>` and keep read access. Record the digest
   in `docs/migration/decisions.md`.
3. Keep the pre-swap pg_dump and a volume backup in the backup location for the archive
   window.
4. Infra git history has the Swift manifests. Rollback after step 10 of D1 is a revert of
   the image swap commit, plus restoring `extraPaths` only if `api-go` is also back. One
   `git revert` on a known commit list. Record those SHAs in `decisions.md` as they merge.
5. Keep the secret keys Swift reads (D8's dead list) until the window ends. Removing them
   breaks a rollback.
6. Expand-only goose migrations until the window ends (D4).
7. `gh repo archive LuminaVault/LuminaVaultServer --yes`. Reverse:
   `gh api -X PATCH repos/LuminaVault/LuminaVaultServer -F archived=false`. Don't delete the
   repo, branches or packages.

Rollback cost grows at each step. Before Swift at 0: revert flips. After Swift at 0, before
the swap: scale up, then revert job commit. After the swap: revert swap and jobs. After a
non-additive goose migration: not available, so that's forbidden during the window.

## Final gate

Everything below must be true. Each is a command or query. **[owner]** marks those agents
can't run. Failing any stops the sequence.

### Z: before Swift's replicas go to 0

| # | Check | Verify |
|---|---|---|
| Z1 | Swift frozen: nothing since the parity image | `git -C LuminaVaultServer log fdbed9b0..origin/main --oneline -- Sources Tests` is empty, or each listed commit is ported with parity. `parity/SWIFT_IMAGE` is the production Swift SHA |
| Z2 | Every module is production | `grep -E '^\s+status:' modules.yaml \| sort \| uniq -c` shows only `production` or `done`. `heldPaths` are all empty |
| Z3 | Parity is clean | `make parity-selftest`, then `make parity` for every module, 0 diffs on main at the deployed Go SHA |
| Z4 | Test accounting is clean | `scripts/swift-test-coverage.py` exits 0. Every waiver in `test-waivers.md` has owner sign-off |
| Z5 | Every flip passed G3 | Each flip batch has its 72 h to 7 d entry in `GO_MIGRATION_STATUS.md`, Go 5xx ≤ Swift, p95 ≤ 1.2× |
| Z6 | No traffic on Swift through Traefik, 24 h after the `/` retarget **[owner]** | `sum(increase(traefik_service_requests_total{service="horus-api-production-http@kubernetes"}[24h]))` is zero or close to it. Check the metric name on a live scrape |
| Z7 | Nothing calls Swift, by route, 7 days. This catches in-cluster callers that bypass Traefik (Isis, Hermes, Loci) **[owner]** | Only Swift emits `hb_requests_total`, so `sum by (http_route) (increase(hb_requests_total{job="luminavault-api-production"}[7d]))` shows only kubelet's `/health`. Read the route label's name on a live series first. Any other route is a missed flip |
| Z8 | Isis reads Go | `/apps` board shows LuminaVault user counts. `kubectl -n horus get netpol allow-metrics-from-isis-api -o yaml` names Go |
| Z9 | Every Go job is ported and enabled | Compare the jobs table to `kubectl -n horus get deploy api-go-production -o jsonpath='{..env[?(@.name=="LV_GO_JOBS")].value}'`. Each "Any" and "Flag" job present. Each "Zero" job ready in the next commit |
| Z10 | No job doubles | For each flagged Swift job moved early, the flag is off: `kubectl -n horus exec deploy/api-production -- printenv GRAPH_LAYOUT_WORKER_ENABLED KNOWLEDGE_GRAPH_WORKER_ENABLED SYNTHESIS_WORKER_ENABLED EMBEDDING_REEMBED_ENABLED`. Name the keys: don't dump `env`, it holds secrets |
| Z11 | Each job ran on Go | Metric `lv_job_last_success_timestamp_seconds{job}` is fresh per schedule. Weekly jobs (`RetrievalLeakReportWorker`, `LearnedDigestScheduler`) seen once. DB cross-check **[owner]**: `max(created_at)` on `retrieval_leak_reports`, `max(sent_at)` on `learned_digest_log` and `meeting_briefs`, `max(updated_at)` on `community_daily_posts` |
| Z12 | Lapse dry run agrees **[owner]** | 7 days of Go dry-run log vs Swift actions, by user and action. Zero differences |
| Z13 | Alerts have Go data **[owner]** | Each expression in `observability/alerts/platform_rules.py` that names LuminaVault returns a series from Go in the last 24 h. Fire one test alert |
| Z14 | Real integrations proved from Go **[owner]** | APNs: push received on device (achievement, reminder, run). PostHog query shows `$set is_internal`, `user_registered`, `user_logged_in` from Go after 8A. RevenueCat: real or sandbox renewal handled. Email reset arrived. SMS if on |
| Z15 | Real sign-in **[owner]** | On the App Store build and TestFlight: Sign in with Apple, Google, password, refresh after 1 h, WebAuthn if enabled |
| Z16 | Sessions survive rollback | Cross-server parity scenario passes (see Auth). On staging, log in on Go, flip to Swift, session still works |
| Z17 | Volume safe **[owner]** | PV is `Retain` (D2 command, `jsonpath='{.spec.persistentVolumeReclaimPolicy}'`). Newest `api-data-backup` restored to a scratch dir and sampled |
| Z18 | DB safe **[owner]** | Fresh `pg_dump` restored, row count compared on `users`, `memories`, `refresh_tokens` |
| Z19 | Dress rehearsal | All D1 steps done on staging, including Swift at 0 and adoption, with G2 smoke after each. Rollback tried once |
| Z20 | Docker question closed | O1 recorded in `decisions.md`, with the query results |
| Z21 | Default route retargeted | `curl -s -o /dev/null -w '%{http_code} [%header{server}]\n' https://api.luminavault.fyi/no-such-path` gives 404 with no `ObsidianClaudeBrainServer` |
| Z22 | Env complete | Key-comparison script (D9) has zero unknown keys. `kubectl -n horus rollout status deploy/api-go-production` is healthy, with the full key list in place |

Then scale to 0, then the "Zero" jobs commit.

### A: before the repo is archived

| # | Check | Verify |
|---|---|---|
| A1 | Soak at 0 | Minimum 7 days at Swift 0 (G3's upper bound), with every weekly job seen. Daily jobs each ran 7 times. Owner decides whether to extend to a full billing month |
| A2 | Replicas still 0, no restarts needed | `kubectl -n horus get deploy api-production -o jsonpath='{.spec.replicas}'` during soak. No revert in infra `git log` since |
| A3 | Adoption done | **[owner]** `SELECT count(*) FROM goose_db_version` equals baseline + mirrors + later migrations. Logs of the last `api-production-migrate` Job, if kept, show goose |
| A4 | Image swapped | `kubectl -n horus get deploy api-production -o jsonpath='{..image}'` is `luminavaultserver-go`. `curl -sI https://api.luminavault.fyi/health` has no `ObsidianClaudeBrainServer` |
| A5 | Migrate hook works on Go | One no-op sync and one real expand-only migration, on staging then production, through PreSync |
| A6 | `extraPaths` gone, `api-go-*` gone | `grep -rn 'extraPaths' apps/api` and `ls apps/api-go argocd/apps/api-go*` both empty |
| A7 | Release chain | Go release writes `apps/api/values-staging.yaml`. Promote works. A Go change reaches staging and production through it. Swift `release.yml` disabled: `gh workflow list -R LuminaVault/LuminaVaultServer` shows it disabled |
| A8 | Nothing else depends on the repo | Each row in D6 moved. `grep -rn 'LuminaVaultServer/' LuminaVaultWebApp/package.json LuminaVaultClient/scripts LuminaVaultClient/.github LuminaVaultShared/.github platform/infra/.github` is empty, or only points to history |
| A9 | Hermes image | A rebuild from the new home produces a working image, tried on staging. Workflow runs in the new repo |
| A10 | Restore drill | Drill ran from its new home: `gh run list -R <new-home> -w restore-drill` one success |
| A11 | Parity oracle stays | `docker pull "$(cat parity/SWIFT_IMAGE)"` works from CI after a package retention check |
| A12 | Observability ported | `grep -rn 'hb_requests' platform/infra/observability` empty, dashboards render with Go data |
| A13 | Runbooks | `grep -rln 'Swift\|exec deploy/api-production -- bash' platform/infra/docs apps` handled |
| A14 | Secrets classified | D9 list; dead keys kept until the window ends |
| A15 | Tag, digest, SHAs recorded | `git ls-remote --tags LuminaVault/LuminaVaultServer 'swift-final-*'`. `decisions.md` has digest and rollback commit list |
| A16 | Rollback is still real | On staging, revert the swap commit, Swift boots on the adopted schema, login works. Then revert the revert |
| A17 | In-flight Swift work resolved | `git -C LuminaVaultServer branch -a --no-merged origin/main`: each branch ported, abandoned or saved. `gh pr list -R LuminaVault/LuminaVaultServer` empty. Specifically `fix/chat-source-citations` and `feat/quick-assistant*` |
| A18 | Owner sign-off | Written in `GO_MIGRATION_STATUS.md` |

Then `gh repo archive`, then watch for a week, then remove D8's items.

## Decisions for the owner

| # | Decision | Recommendation | Why it blocks |
|---|---|---|---|
| O1 | Docker-backed routes (`xAI`, `Nous`, gateway apply, WhatsApp pair, hub skill install, system update, reaper, grok, cron bridge): port fully or retire? Run: `kubectl -n horus exec deploy/api-production -- ls -l /usr/bin/docker /var/run/docker.sock`, `SELECT count(*) FROM hermes_tenant_containers`, `hermes_gateway_apply_jobs`, `hermes_update_jobs`, each by `created_at`, and `count(xai_connected_at)`, `count(nous_connected_at)` on the first, since the cluster move | If no docker and no rows since the move: port the routes and the wire, with a default implementation that answers Swift's docker-unavailable errors, and sign a waiver for tests that exercise only docker success. That's parity with what production does. No route is removed. If rows exist, port fully. Don't re-architect either way | Decides P4, 6D, 6E, 7C, and about 3200 Swift lines (container manager, reaper, gateway apply, WhatsApp, Photon, update, central manager, cron bridge) |
| O2 | How 6B and 6C pass G2 with no staging Hermes | Add a staging Hermes in infra, on its own volume. Never point staging at production: agent runs execute tools. If not, the G2 smoke runs against a stub Hermes and the flip's G3 is the first real check. Owner should accept that explicitly | G2 can't be done otherwise |
| O3 | Pruning, scoring, retention, correlation have not run since the VPS **[inferred]** | Port routes. Add jobs behind `LV_GO_JOBS`, off. Turning on pruning deletes data, so it's a separate owner decision after review | Otherwise "archive" silently loses the intent, or starts deleting |
| O4 | Add Swift flags for 8 jobs, or move them at replicas 0 | Move at 0 (also in `wave-4-plan.md`) | Larger rollback vs Swift deploys |
| O5 | Keep `bootstrap-admin` and the two backfills | Port `bootstrap-admin`. Retire backfills after queries show nothing left | Image contract |
| O6 | WebAuthn library | `go-webauthn`, with fixture credentials from Swift. If `webauthn_credentials` is empty and `WEBAUTHN_ENABLED` false, port wire only and leave disabled | 8C |
| O7 | Soak at Swift 0 | 7 days minimum, every weekly job seen. Longer adds confidence, not gates | Time to archive |
| O8 | `/v1/dashboard/home` hold | Check Swift's production answer. If `cronJobs` is always `[]`, flip without the hold | Dashboard flip |
| O9 | D-reminders-detect | Wire classifier through `internal/llm` and skip the hold | Reminders flip |
| O10 | Who merges the chart change, retarget, swap | Owner, as with all infra | n/a |

## Order

Parallel wherever a line lists several. "Infra" lines are owner merges.

1. **Now:** P6 (push freeze), P7, P1. Answer O1 and O2, since they shape everything in wave 6.
2. **Alongside W5:** P2, P3, P4, P5, PA. J1 starts.
3. **Interleave with flip batches:** reminders (O9), dashboard (O8).
4. **W5 first half:** `internal/llm` wiring. 6C ports here, because chat needs it (B12). Community flips after moderation lands. J2 starts.
5. **W5 end:** `/v1/ws`, `kb-compile`, `/v1/devices/command`, `mcp`, `skills` flip together, with 6C if chat is in the same batch.
6. **Wave 6:** 6A + 6B (flip together, mirror worker with them). 6D, 6E, 6F in parallel, ports independent, one flip batch each.
7. **Wave 7:** P8. 7A (flip alone; lapse dry run starts), 7B (with Isis repoint), 7C.
8. **Wave 8:** P9, P10. 8D. 8A in the four steps. 8B. 8C.
9. **Decommission:** D2 volume, D3 selectors, D6 moves and D-tests in parallel from step 1 on. Then D1 on staging, Z gate, D1 on production to Swift 0, jobs commit, soak, adoption (D4), swap, jobs, remove paths, delete `api-go-*`. D5 alongside the swap. A gate. Archive. D8.

Minimum time is bounded by G3: each flip batch needs 72 hours to 7 days, the soak at Swift 0
needs 7 days, and the lapse dry run 7 days. Ports can run in parallel, and flip batches
that don't interact (for example 6E and 7B) can share a G3 window to go faster, without
weakening a gate. Whichever gate's period is longest wins for that window.

## Recipe

Same as `wave-4-plan.md`: worktree per group in `.worktrees/`, branch from `main`, plan
the routes with `routes.json`, one error and one success scenario per route in parity,
port every Swift assertion with a comment naming the Swift file, `make parity MODULE=x` 0
diffs, PR on green CI. Status moves to `parity`. Flip is an infra PR the owner merges, with
every new Deployment named in each NetworkPolicy.

For jobs: PR adds to `internal/jobs`, default off through `LV_GO_JOBS`. A second infra PR
turns it on.

## Not verified

- Production Postgres row counts and sealed values (agents don't read them):
  `RATE_LIMIT_STORAGE_KIND`, `HERMES_GATEWAY_KIND`, `WEBAUTHN_ENABLED`,
  `BILLING_COLD_STORAGE_PATH`, `DOCKER_BINARY_PATH`.
- Inside the running Swift pod: whether docker exists (B19).
- The host crontab from the VPS era (B6).
- Traefik's precedence between `/` and longer prefixes, beyond what `wave-4-plan.md` relied on.
- `swift-configuration`'s encoder for `fluent.autoMigrate` (B15). Its source wasn't read.
- Argo CD's handling of `helm.sh/resource-policy: keep`.
- Whether old or unknown-route clients exist beyond those grepped.
- The W5 plan: its claims on jobs and on `/v1/me/hermes/cron` are assumed, not read.
