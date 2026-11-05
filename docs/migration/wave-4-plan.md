# Wave 4 plan

Written 2026-11-04 from the roadmap's per-module recipe. Swift was read at
`fdbed9b0`, the image production runs. Nothing here has been built or run. Claims
marked **[inferred]** were not verified in code; everything else cites what was read.

## Modules (62 routes)

`routes.json` agrees with the brief: todos 4, vault 12, memory 18, knowledge 6, kb 2,
capture 1, import 7, ingestion 10, vision 2. Total 62. `Indexing` and `Repositories`
register no routes. They become shared packages (see "Land first").

## Three findings that shape the split

1. **Nine routes call the LLM transport, and it is a wave 5 package.** They are
   `POST /v1/memory/search` (it runs the agent loop, `HermesMemoryService.search`:345),
   `POST /v1/query`, `POST /v1/query/stream`, `POST /v1/memos`, `POST /v1/knowledge/reason`,
   `POST /v1/knowledge/reason/stream`, `POST /v1/memory-compile`,
   `POST /v1/import/{id}/categorize` and `POST /v1/import/{id}/approve` (it calls
   `compileExistingVaultFiles`, `ImportService.swift`:222). Ingress can't split
   `/v1/memos` by method or `/v1/import/{id}/*` around a parameter, so 6 more routes
   travel with them. That leaves 47 routes that can flip without LLM (groups 4A to 4E)
   and 15 that can't (group 4F).
2. **The core of the LLM package should be built now, not after wave 4.** Group 4F needs
   it. So do three recorded blockers: `/v1/reminders/detect`, community moderation and
   `PeriodRecapThemesWorker`. `LLM/Routing` is 10,602 lines in 48 files; see P6 for the
   subset.
3. **No route here needs a job to move, and no job moves here.** Every wave 4 job either
   claims with `SKIP LOCKED`, finds its work by scanning tables, or is switched off in
   production. So wave 4 needs no Swift flag and no Swift deploy. See "Jobs".

## Groups (one branch each)

Module names in `code` marked "new" are pseudo-modules, split the way `me` was
(decisions.md D-me-split), so each passes G1 on its own routes and no gate is relaxed.
P5 adds them to `scripts/gen-modules.py` (`SPLITS`, `SPLIT_PATHS`).

| Group (branch `go-migration/wave-4-*`) | Modules | Routes | Swift tests | Flip blockers |
|---|---|---|---|---|
| `wave-4-prep` | none | 0 | Embedding, Indexing, Idempotency, latches (below) | Must merge before 4A to 4E start |
| `wave-4-vault` (4A) | vault | 12 | `Vault/*` | Latch, achievements, explicit `/v1/vaults` ingress entry |
| `wave-4-capture` (4B) | todos, capture | 4 + 1 | `Capture/*`, `DiskSpaceGuardTests` | Latch, achievements, parity race on enrichment |
| `wave-4-memory` (4C) | memory | 13 | most of `Memory/*` | `/v1/memory/search` and `/v1/memory-compile` held on Swift by ingress |
| `wave-4-graph` (4D) | knowledge, vision, kb | 4 + 2 + 1 | `KBCompilePendingTests`, `VisionSearchLimitTests`, `KnowledgeVaultScopeIntegrationTests` | `/v1/knowledge/reason` and `POST /v1/memory-compile` held on Swift |
| `wave-4-ingestion` (4E) | ingestion | 10 | `MultimodalIngestionAPIIntegrationTests` | None beyond G1/G2; the worker stays in Swift |
| `wave-4-agent` (4F) | new `memory-agent` 5, new `knowledge-reason` 2, new `kb-compile` 1, import 7 | 15 | The rest (below) | Needs P6. `kb-compile` flips with `/v1/ws` |

12 + 5 + 13 + 7 + 10 + 15 = 62.

## Land first

All of this goes to `main` before the parallel ports start, so that no two groups write
the same helper. P0, P1, P2, P4 and P5 don't depend on each other and can run as parallel
agents. P3 needs P1. P6 can start at the same time and doesn't block 4A to 4E.

| # | Package | Swift source (lines) | Owner | Used by |
|---|---|---|---|---|
| P0 | Parity harness and shared fixes | n/a | prep agent 1 | every group |
| P1 | `internal/embedding` | `Services/Embedding` minus re-embed (989) | prep agent 2 | all groups, apple, calendar |
| P2 | `internal/vaultfiles`, `internal/diskguard` | `VaultController` helpers, `VaultFileMover` (74), `DiskSpaceGuard` (255) | prep agent 3 | 4A, 4B, 4E, 4F |
| P3 | `internal/memorystore`, `internal/chunkindex` | `MemoryRepository` (881), `MemoryChunkRepository` (318), `VaultLinkRepository` (281), `Indexing` chunker, wikilinks, IDs (941), `HybridMemorySearch` (102) | prep agent 4, after P1 | 4A to 4F |
| P4 | Side effects: `internal/onboardlatch`, `internal/achievementsink`, `internal/apns`, `internal/idempotency`, `internal/posthog`, `internal/sse`, `internal/hermesresolve`, usage row | `OnboardingLatches` (138), `AchievementsService.record`, `APNSNotificationService.notifyAchievement`, `IdempotencyMiddleware` (192), `HermesEndpointResolver` (119) | prep agent 5 | 4A to 4F |
| P5 | Manifest: pseudo-modules, held paths | `scripts/gen-modules.py`, `internal/manifest` | prep agent 1 | flips |
| P6 | `internal/llm` (subset, below) | `LLM/Routing` | 4F's first agent | 4F, then wave 5 |

### P0: harness gaps that would otherwise stop every group

- **Filesystem snapshots.** Parity has no view of the volume (cutover-log, account: "Not
  observable in parity"). Vault, todos, capture and ingestion are mostly file writes. Add
  a step option comparing the tenant tree (relative path, sha256, size), with
  server-generated UUIDs normalised. **[inferred]** The compose file gives each server its
  own vault root; check that before designing this.
- **Raw and binary bodies.** Vault upload takes a raw body with `X-Vault-Metadata` and
  query parameters. Ingestion takes chunked `PUT`. Downloads need byte comparison, and
  `HEAD` needs covering on `/v1/ingestion-sources/{token}`.
- **SSE frames.** The `stream: true` step compares status and headers only. Ingestion
  events and query streams need bounded frame comparison.
- **LLM stub.** `HERMES_GATEWAY_URL` in parity is `http://127.0.0.1:9`, so LLM paths only
  ever fail. Add a deterministic chat stub to `parity/stubs` (needed by 4F, not 4A to 4E).
- **Smoke tags.** Each new module needs a `smoke: true` scenario for G2.
- **Shared fixes from D-wave3-notes, once that branch is merged.** A `wire` helper reading
  raw query values (a literal `+` stays `+`), which `calendar` and `dashboard` each do on
  their own and the vault `path` parameter needs. `wire.Bind` answering 413 over its limit.
  A strict `ids.Parse`. Move the `queryByUser` tier scale out of `calendar` into
  `internal/ratelimit`, and export policies shared between groups: `captureByUser` (capture,
  memory upsert, memory search, knowledge), `meTodayByUser` (`me`, `/v1/me/suggestions`).

### P1: embedding

Port `EmbeddingProviderRegistry`, the OpenAI, OpenRouter and Nomic services, the fallback
chain, the usage tracker, `embedForWrite` and `embedForQuery` (`EmbeddingDegradation`: an
outage stores NULL, it doesn't fail the write), and `TenantAwareEmbeddingService` for
BYOK. Move `internal/modules/apple/embedding.go` into it and repoint `apple` and
`calendar`. Tests: `Services/Embedding` (Registry 6, OpenAI 9, Nomic 3, Fallback 3,
KindAndError 6, LocalHermes 3) and `Memory/EmbeddingDegradationTests` 7, against stub
servers only. `LocalHermesEmbeddingService` can stay a stub answering `.endpointMissing`,
as Swift's wiring does today (`App+build.swift`:1969-1976).

This also closes a staging gap: staging runs `EMBEDDING_PROVIDER=openrouter`, so
`api-go-staging` answers 500 on a live calendar brief or `/v1/photos/index`. Production
runs the deterministic provider (flip-blockers.md).

### P2: vault files

`sanitizePath`, `resolveInside`, `validateContentType`, `upsertVaultFileRow`,
`mergeMetadata`, `VaultFile` plus the `VaultFileMetadata` JSONB codec (dates are seconds
since 2001-01-01, per the porting brief), `VaultFileMover`, the `_deleted_*` soft delete, and
the disk guard with its 507. `internal/vaultpaths` is only 40 lines and stays as it is.
Tests: `Vault/DiskSpaceGuardTests` 11.

### P3: memory and chunks

`MemoryRepository` is shared by 4A to 4F and, in Swift, by wave 5, so port it whole: create,
list, semantic, lexical and document search, updates, lineage, delete, forget, and the hit
count update. `pgvector-go` is already in `go.mod`. `ChunkIDs` hashes must match
byte for byte, or both servers write two copies of each chunk. Snapshot `memory_chunks`
IDs in parity. Tests: `Indexing/MarkdownChunkerTests` 20, `WikilinkTests` 20,
`MemoryChunkSearchTests` 7, `Memory/HybridMemorySearchTests` 11,
`MemoryHitCountUpdateQueueTests` 2.

### P4: side effects

- `internal/onboardlatch`: one `INSERT … ON CONFLICT` per latch, non-throwing, as
  `OnboardingLatches.swift` documents. Called by 4A (`firstCapture`), 4B (`firstCapture`)
  and 4F (`firstMemoryCompile`). Chat and `LLMController` keep calling Swift's until
  wave 5 (`firstQuery`). `/v1/query` does not latch, whatever the name suggests. Tests:
  the 8 unported latch tests in `Onboarding/GuidedStartTests` go with the group that owns
  each call site; the first-query ones wait for wave 5.
- `internal/achievementsink`: the Go half of the `AchievementsWorker` inbox. Producers in
  wave 4 are `VaultController` and `LinkCaptureService` (`vaultUploaded`), `MemoryController`
  (`memoryUpserted`), `QueryController` (`queryRan`) and `MemoryCompileController`
  (`kbCompiled`). `internal/modules/achievements/catalog.go` already holds the catalog.
  Handlers must not wait on the DB: keep a bounded in-process queue, as Swift does.
- `internal/apns`: an unlock pushes through APNS, and Go has no APNS client. Port only
  `notifyAchievement`, device token lookup and category prefs. `APNS_*` and the key file are
  inherited from the Swift values. Without this, unlock pushes from Go-served writes are
  silently lost. A new dependency needs the usual vetting (`D-deps`); the roadmap names
  `sideshow/apns2`.
- `MeTodayCache`: see flip-blockers.md. If `me` flips first with the DB check proposed
  there, no publisher has to call it. If not, 4C and the sink must call
  `me.Module.InvalidateToday`.
- `internal/idempotency`: on the `/v1/vault/files` group, the `/v1/vault/create` and `/status`
  group, and `POST /v1/memory-compile`. Keep Swift's request hash format, so a
  retry that spans a flip doesn't get a 409. Tests: `Middleware/IdempotencyMiddlewareTests` 5.
- `internal/posthog`: Go has no client, and `POSTHOG_PROJECT_TOKEN` is in production's
  sealed env. Wave 4 captures `vault_file_uploaded`, `vault_file_deleted`, `link_captured`.
- `internal/sse`: `IngestionSSEResponse` polling, `Last-Event-ID`, used by 4E and 4F.
- `internal/hermesresolve`: `HermesResolutionMiddleware` sits on `/v1/memory/upsert` as well
  as on the LLM routes, and a resolution failure there is a 502 `hermes_unreachable` even
  though upsert never dials Hermes (`App+build.swift`:2081-2086). So 4C needs it too.
- Usage row: `VisionEmbedService` writes one usage row per embed. Port that insert only;
  `UsageMeterService` (322) stays with billing and LLM.

### P5: manifest

Add the pseudo-modules and a `heldPaths` field listing routes a module leaves on Swift, with
their reason and the group that releases them. Make `internal/manifest`'s prefix check match
string prefixes as well as path elements. Today it assumes element-wise matching. The
chart's `pathType: Prefix` on Traefik 3.7.4 (`cluster/traefik/helmchartconfig.yaml`, with no
`strictPrefixMatching` set) is **[inferred]** to be a plain string prefix. That would make
`/v1/vault` also catch `/v1/vaults`, and `/v1/memory` also catch `/v1/memory-compile`. These
are the only two such pairs in `modules.yaml`. Every flip below names them explicitly, which
is correct under either behaviour.

### P6: `internal/llm`

The subset needed: `RoutedLLMTransport` (940), `AnthropicAdapter` and its translation (1,022),
`OpenAICompatibleAdapter` (307), `HermesGatewayAdapter` (199), `ModelRouter` (354),
`ProviderRegistry` (194), `AnthropicManagedTrialRouter` (85), `UserPreferenceModelRouter` (102),
plus failover classification and streaming. About 3,200 lines **[inferred: exact subset not
traced]**. Cost ledger, usage meter and trace recorder are optional in Swift's transport, but
they are writes production makes today, so each needs a port or a recorded decision to leave
it to wave 5. Read `platform/infra/docs/llm-haiku-trial.md` first: the Anthropic adapter sends
no `temperature`, `top_p`, `top_k` or prefill, while `HermesMemoryService` builds payloads with
`temperature: 0.2`. Stub servers only.

## Group details

### 4A `wave-4-vault`: vault (12)

- **Routes.** `POST /v1/vault/create`, `GET /v1/vault/status`, `POST` and `GET /v1/vault/files`,
  `GET` and `DELETE /v1/vault/files/**`, `POST /v1/vault/files/move`, `GET /v1/vault/export`,
  `GET /v1/vault/index`, `GET /v1/vault/context/{vaultFileID}`,
  `GET /v1/vault/links/{vaultFileID}`, `GET /v1/vault/lint`.
- **Swift tests.** `VaultCRUDTests` 13, `VaultUploadSpaceTests` 6, `VaultInitTests` 3,
  `VaultExportTests` 3, `VaultLargePayloadTests` 2, `Services/TenantVectorIndexServiceTests` 4.
  The index, lint, context and links routes have no direct test (only `MCPContractTests`
  mentions them), so read `VaultIndexStatusService`, `VaultNavigationService` and
  `VaultLintService`.
- **Coupling.** EventBus `vault_file_created` is log-only (coupling.md), so drop it and note
  that. `AchievementsWorker` inbox (`vaultUploaded`). `firstCapture` latch. Vault activity: not
  a publisher.
- **Jobs.** None move. `DiskSpaceMonitorService` keeps running in Swift, and the Go guard
  measures on each check.
- **New code in the module.** `VaultExportService` (636), `VaultInitService`, which needs
  `SOULService.initIfMissing` (write `SOUL.md`, mirror to the Hermes dir; port these calls only,
  `/v1/soul` stays wave 8), `spaces.Module.SeedDefaults` (already in Go) and
  `TenantVectorIndexService.ensureIndex`. That last one is runtime DDL from Go, though Swift
  owns schema. Use the identical statement and exclude the index from snapshots.
- **Order.** After P0 to P4. Independent of 4B to 4E. `CaptureFilingService` needs LLM and is
  off in production (`CAPTURE_AUTOFILE_ENABLED` is in neither the plain env nor the sealed key
  list), so port the off path now and the rest in 4F.
- **Flip blockers.** Ingress needs `/v1/vaults` explicit, if `teams` hasn't flipped.
  `Hermes/Mirror` keeps writing the same tree from Swift; its only lock is a local `flock` in
  `FilesystemHermesTransport`, with nothing guarding uploads in Swift either, so the two
  processes add no new race **[inferred]**. G1 needs the P0 filesystem snapshot.

### 4B `wave-4-capture`: todos (4), capture (1)

- **Routes.** `GET` and `POST /v1/todos`, `PATCH` and `DELETE /v1/todos/{id}`,
  `POST /v1/capture/safari`.
- **Swift tests.** `Capture/URLExtractionServiceTests` 11, `URLEnricherGuardTests` 5,
  `CaptureHookDispatcherTests` 6, `Enrichers/JinaEnricherTests` 7. No Todos directory: read the
  controller, and port the todo and capture cases of `DiskSpaceGuardTests`.
  `CaptureFilingServiceTests` 3 go to 4F.
- **Coupling.** `LinkCaptureService` publishes `vault_file_created` (drop), enqueues
  `vaultUploaded` and latches `firstCapture`. `Task.detached` enrichment is lost if the process
  stops mid-task, in Go as in Swift; port as a detached goroutine. Todos has no latch or
  achievement call.
- **New shared package, owned here.** `internal/capture`: `LinkCaptureService`,
  `URLEnrichmentService`, the enrichers (generic OG, Jina, X, YouTube), hooks and the SSRF guard.
  4F's import and wave 5's chat auto-save call it.
- **Order.** After prep. Must merge before 4F's import starts.
- **Flip blockers.** Enrichment rewrites the row after the response, so a `vault_files` snapshot
  races: exclude enrichment fields, or poll for a terminal state. `POST /v1/capture/safari`
  has known spec drift (returns `{id,path,status}`, 202), so use a hand-written DTO.

### 4C `wave-4-memory`: memory (13)

- **Routes.** `GET /v1/me/suggestions`, `GET /v1/memory`, `/facets`, `/graph`, `/local-sync`,
  `POST /v1/memory/upsert`, and on `/v1/memory/{id}`: `GET`, `DELETE`, `PATCH`, `/lineage`,
  `/provenance`, `/related`, `POST /review`.
- **Swift tests.** `MemoryCRUDTests` 9, `MemoryGeoTests` 3, `MemoryGraphControllerTests` 3,
  `MemoryGraphServiceTests` 8, `MemoryLineageTests` 4, `MemoryRelatedTests` 3,
  `MemorySavedIDTests` 4, `MemorySpaceMoveTests` 3, `MemoryCorrectionTests` 4, `MemoryKindTests` 2,
  `LocalMemorySyncCursorTests` 3, `SuggestionsControllerTests` 2, and the "Memory health queues
  validate their filter" case left over from dashboard. `MemoryScoringTests`, `MemoryPruningServiceTests`
  and `MemoryKeepGateTests` stay with their jobs and the wave 7 admin routes **[inferred from
  file names]**. `GraphLayoutServiceTests` 6 stays with `GraphLayoutWorker`.
- **Coupling.** EventBus `memory_upserted` feeds `MeTodayCache` (P4). `achievements` inbox
  (`memoryUpserted`). `MemoryHitCountUpdateQueue` is harmless (relative increments). Patch calls
  the chunk indexer.
- **Jobs.** None move. `GraphLayoutWorker` writes `graph_x/y/z` that `/v1/memory/graph` reads:
  same table, no in-process link. `KnowledgeExtractionWorker` finds changed memories by scanning
  (`enqueueNextChangedMemory`), so Go's writes are picked up without any call.
- **Switches.** `/v1/memory/local-sync` is mounted only with `hybridExecution.enabled`.
  `HYBRID_EXECUTION_ENABLED` is sealed in production, so compare both states.
- **Order.** After prep. `/v1/me/suggestions` must use the same `meTodayByUser` policy as `me`.
- **Flip blockers.** Ingress: `/v1/memory` and `/v1/me/suggestions` to Go; `/v1/memory/search` and
  `/v1/memory-compile` to Swift. BYO resolver path on upsert (P4).

### 4D `wave-4-graph`: knowledge (4), vision (2), kb (1)

- **Routes.** `GET /v1/knowledge/graph`, `POST /v1/knowledge/connections/explain` (SQL only:
  `KnowledgeGraphService.explain` makes no transport call), `POST /v1/knowledge/edges/{id}/confirm`
  and `/dismiss`, `POST /v1/vision/embed`, `POST /v1/vision/search`, `GET /v1/memory-compile/pending`.
- **Swift tests.** `KnowledgeVaultScopeIntegrationTests` 1, `VisionSearchLimitTests` 7,
  `KBCompilePendingTests` 4, and the vision and knowledge cases of `BYOEntitlementMiddlewareTests`
  not already in `internal/entitlement`.
- **Coupling.** None in process. `KnowledgeExtractionWorker` fills the tables and stays in Swift.
- **New in the module.** `VisionEmbedService`, `VisionEmbedProviderRegistry`, `CohereImageEmbedAdapter`
  and the stub. Production has `VISION_EMBED_PROVIDER` and the Cohere keys sealed, so this is the one
  group calling a paid API in production: stub servers only in tests.
- **Order.** After prep. The smallest group; a good first PR to prove the prep packages.
- **Flip blockers.** `/v1/knowledge/reason` stays on Swift until 4F. `/v1/memory-compile/pending`
  goes to Go only with `/v1/memory-compile` named explicitly for Swift.

### 4E `wave-4-ingestion`: ingestion (10)

- **Routes.** `GET` and `POST /v1/ingestions`, `GET /v1/ingestions/{batchID}`, `/events`,
  `PUT .../items/{itemID}/chunks/{index}`, `POST .../complete`, `POST .../retry`, `DELETE .../items/{itemID}`,
  `GET` and `HEAD /v1/ingestion-sources/{token}` (public, token-gated).
- **Swift tests.** `MultimodalIngestionAPIIntegrationTests` 6. `MultimodalIngestionCapabilityTests` 3 and
  `HermesIngestionTransportTests` 2 test the worker path and stay with it **[inferred from file names]**.
- **Coupling.** Ingestion SSE is already cross-process (Postgres, 2 s poll), so it's a straight port.
- **Jobs.** `MultimodalIngestionWorker` stays in Swift: `SKIP LOCKED` plus a lease, and both need the
  same volume, which `existingClaim` provides. Routes only enqueue rows and write chunks; capability
  checks, LLM calls, link capture and push all happen in `processNext`. So no Hermes dependency
  reaches the routes.
- **New in the module.** `StorageQuotaService` (only ingestion uses it), source token hash, chunk paths
  identical to `chunkDirectory`, and the file mode the Swift worker can read (both run as UID 999, D3).
- **Order.** After prep. No other group depends on it.
- **Flip blockers.** None beyond G1 and G2. `INGESTION_PUBLIC_BASE_URL` keeps pointing at the
  API host, so Hermes fetches sources through the ingress and reaches Go after the flip.

### 4F `wave-4-agent`: memory-agent (5), knowledge-reason (2), kb-compile (1), import (7)

- **Routes.** `POST /v1/memory/search`, `POST /v1/query`, `POST /v1/query/stream`, `GET` and `POST /v1/memos`,
  `POST /v1/knowledge/reason`, `POST /v1/knowledge/reason/stream`, `POST /v1/memory-compile`, and the 7
  under `/v1/import`.
- **Swift tests.** `HermesMemoryServiceTests` 8, `FollowUpGeneratorTests` 9, `QueryStreamTests` 5,
  `MemoGeneratorTests` 4, `MemoControllerTitleTests` 4, `KnowledgeReasoningPathTests` 3,
  `KBCompileControllerTests` 5, `MemoryCompileControllerTests` 6, `MemoryCompileHappyPathTests` 1,
  `MemoryCompileMemoryUpsertTests` 1, `MemoryCompileProgressPublisherTests` 2,
  `MemoryCompileProgressServiceTests` 3, `MemoryCompileSpaceCountersTests` 4, `Import/ImportLogicTests` 13,
  `Capture/CaptureFilingServiceTests` 3, the first-compile latch cases in `GuidedStartTests`, and the
  commitment and kb-compile cases deferred from `me` (7). `LegacyCompileAliasTests` 2 stays with module
  `app` (wave 8).
- **Coupling.** `MemoryCompileProgressPublisher` to `ConnectionManager` to `/v1/ws`. `RetrievalTelemetryWorker`
  inbox: insert directly from Go (insert-only sink). `memory_upserted` from the agent's upsert tool.
  `CommitmentsService.record` (kb-compile).
- **Jobs.** `MemoryIndexWorker` stays: its outbox is written from chat (wave 5).
- **Order.** After P6, 4B (import calls `internal/capture`) and 4C. Runs alongside wave 5's LLM work, and
  can share its first PR.
- **Flip blockers.**
  - **`kb-compile` must stay on the same server as `/v1/ws`.** The iOS review sheet fills only from
    `.memorySaved` WebSocket frames (`SyncAndLearnViewModel.swift`:174,197), not from the HTTP response. A
    Go-served compile with sockets on Swift would still answer 200 and silently skip the review sheet.
    `/v1/ws` can't move until `DeviceCommandBroker`'s callers (MCP, Skills; wave 5) move. So flip
    `/v1/memory-compile`, `/v1/ws`, `/v1/devices/command` and the `mcp` and `skills` modules in one
    infra PR at the end of wave 5. `modules.yaml` puts `/v1/ws` in `app` (wave 8), so that needs a split.
  - `memory-agent`, `knowledge-reason` and import have no such link. **[inferred]** No iOS or web import screen
    subscribes to compile frames (only `KBCompileWebSocketClient` reads them, the web app has no
    `/v1/ws` client), so import can flip before `/v1/ws`.
  - G2: staging has no `HERMES_BASE_URL`, and it must not be pointed at production (agent runs execute
    tools). Expect error-path status comparison plus the Swift control on staging, as flip batch 1 did.
    Success paths rest on the P0 LLM stub in G1.

## Jobs

The roadmap names five jobs for wave 4; `MultimodalIngestionWorker` is the sixth, from jobs.md. None has
to move for a route to flip, and each stays in Swift until a later step.

| Job | Stays in Swift until | Why this is safe | Swift flag |
|---|---|---|---|
| `MultimodalIngestionWorker` | Hermes capabilities move (wave 6) | `SKIP LOCKED` plus lease; same volume | none, not needed: two runners are safe |
| `MemoryIndexWorker` | Chat (wave 5) | `SKIP LOCKED`; outbox only chat writes | none, not needed |
| `KnowledgeExtractionWorker` | `internal/llm` | `SKIP LOCKED`; scans `memories` itself | `KNOWLEDGE_GRAPH_WORKER_ENABLED` |
| `GraphLayoutWorker` | Any time; no LLM | Idempotent, wasteful if doubled | `GRAPH_LAYOUT_WORKER_ENABLED` |
| `SynthesisWorker` | Not running | `SYNTHESIS_WORKER_ENABLED` is in neither plain env nor sealed keys; default false | exists |
| `EmbeddingReembedService` | Real embeddings in production | Refuses to run on the deterministic provider. Must never run in both | `EMBEDDING_REEMBED_ENABLED` |

**Proposal for the owner.** The roadmap's job rule makes each wave plan add a Swift flag before moving a job.
For the eight must-never-both jobs with no flag (jobs.md), that means Swift deploys. Alternative: move them in the
Phase 12 infra commit that removes the Swift Deployment and sets `LV_GO_JOBS`, so that exactly one server runs each,
with no Swift change. The rule's intent holds; the rollback gets larger. Owner's call, and it doesn't affect wave 4.

## Channels (coupling.md), wave 4 disposition

| Channel | Disposition |
|---|---|
| `vault_file_created` | Log-only subscriber. Dropped in Go |
| `memory_upserted`, `achievement_unlocked` to `MeTodayCache` | Handled by the DB check in flip-blockers.md, or `InvalidateToday` |
| `AchievementsWorker` inbox | Go sink plus `internal/apns` (P4) |
| `RetrievalTelemetryWorker` inbox | Go inserts directly (4F) |
| `MemoryHitCountUpdateQueue` | Moves with the repository. Harmless across processes |
| `MemoryCompileProgressPublisher`, `ConnectionManager` | `kb-compile` flips with `/v1/ws` (4F) |
| Ingestion SSE | Already polls Postgres. Straight port |

The roadmap's coupling rule offers a Valkey bridge for channels split across waves. Nothing in wave 4 needs one.

## Order

1. Merge the pending wave 3 work (`wave-3-dashboard`, `wave-3-notes`) once the hold on merges lifts.
2. Run prep: P0, P1, P2, P4, P5 in parallel, then P3. Start P6 at the same time.
3. Port 4A, 4B, 4C, 4D and 4E in parallel, branching from `main` after prep. 4D first if capacity is short.
4. Port 4F once P6, 4B and 4C are in.
5. Flip 4A to 4E as each gets G2, batching infra PRs. No flip order is required between them, because state is in
   Postgres and the shared volume. Flip 4F except `kb-compile` next. Flip `kb-compile` at the end of wave 5.

Production flips stay behind the pilot gate (D-wave2-early) and G2. See flip-blockers.md.

## Recipe

Unchanged from wave 2 (`wave-2-plan.md`) and `.worktrees/brief3.md`, with these changes for the brief:

- Reuse the prep packages. Don't add vault, memory, embedding or latch helpers inside a module.
- Writes must also be proven on the volume with a filesystem snapshot step.
- Workers named under "Jobs" stay in Swift: port the routes only.
- A route under `heldPaths` is out of its group's G1 and in the G1 of the group that releases it.
- Shared-file edits stay minimal (`cmd/App/main.go`, `sqlc.yaml`, `modules.yaml`, `cutover-log.md`).

## Not verified

- **[inferred]** Traefik's string-prefix behaviour for `pathType: Prefix`. A probe after the next flip settles it.
- **[inferred]** Test placement from file names where marked, and the P6 line count.
- **[inferred]** Parity's vault root layout, and whether import screens read compile frames.
- Values of sealed keys (`HYBRID_EXECUTION_ENABLED`, `KNOWLEDGE_GRAPH_WORKER_ENABLED`, `VISION_EMBED_PROVIDER`):
  key names only were read.
