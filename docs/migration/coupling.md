# Swift in-process channels: what breaks when a route moves to another process

These are the channels through which one component of the Swift server pushes events
to clients, or to other components, inside the same process. If a channel is not
cross-process, its publisher and its subscriber must be served by the same server.
Otherwise events published by Swift never reach a client connected to Go, and the
reverse.

Source: the `go-p0` worktree, `Sources/App`. Paths are relative to `Sources/App`.

How cross-process transport was checked: the only Valkey user in the codebase is
`Middleware/ValkeyPersistDriver.swift` (the rate-limit `PersistDriver`). It is the
only file that imports `Valkey`, and it has no publish or subscribe. Nothing uses
Postgres `LISTEN`/`NOTIFY` (`pg_notify`, `LISTEN `, `PostgresNotification`: 0 hits).
So "yes" below always means **Postgres table polling**.

`EventBus` (`Skills/EventBus.swift:122`) is a lock-protected in-memory dictionary of
`AsyncStream` subscribers (`:143-183`). It is built once at `App+build.swift:483`,
and its own doc calls it single-process (`:102-105`). Its five event types are split
into separate rows.

## Channels

| Channel | File:line | Publishers (type, Sources/App dir) | Subscribers/consumers (type, dir, route if an endpoint) | Cross-process today? |
|---|---|---|---|---|
| EventBus `memory_upserted` | Skills/EventBus.swift:10,143,167 | `MemoryController` (Memory) Memory/MemoryController.swift:257; `HermesMemoryService` (Memory) Memory/HermesMemoryService.swift:502 | `MeTodayCache` (Me) Me/MeTodayCache.swift:71 → invalidates `GET /v1/me/today`; `SkillRunner` (Skills) Skills/SkillRunner.swift:121, log only | no (in-memory, EventBus.swift:143-183) |
| EventBus `achievement_unlocked` | Skills/EventBus.swift:14 | `AchievementsService` (Achievements) Achievements/AchievementsService.swift:105, which runs inside the `AchievementsWorker` drain | `MeTodayCache` (Me) Me/MeTodayCache.swift:78; `SkillRunner` (Skills), log only | no |
| EventBus `vault_file_created` | Skills/EventBus.swift:8 | `LinkCaptureService` (Capture) Capture/LinkCaptureService.swift:149; `VaultController` (Vault) Vault/VaultController.swift:276 | `SkillRunner` (Skills) Skills/SkillRunner.swift:121-131, log only. On-event skill dispatch is TODO (`:112-114`) | no |
| EventBus `health_event_synced` | Skills/EventBus.swift:9 | `HealthIngestController` (Health) Health/HealthIngestController.swift:291 | `SkillRunner` (Skills), log only | no |
| EventBus `hermes_run_event` | Skills/EventBus.swift:18 | `HermesRunStore.publish` (Hermes/Runs) Hermes/Runs/HermesRunStore.swift:121, called from `append` (`:86`) and `HermesRunsService.swift:185,202` | `HermesRunsController.feed` (Hermes/Runs) Hermes/Runs/HermesRunsController.swift:143 → `GET /v1/hermes/runs/:id/events` (SSE); `SkillRunner` (Skills), log only | partly. Live wake is in-process. The SSE also re-reads `hermes_run_events` from Postgres every 5 s (HermesRunsController.swift:24-27,150-156), so events from the other process arrive up to 5 s late |
| `ConnectionManager` (per-tenant WebSocket fan-out) | WebSockets/ConnectionManager.swift:5-6 (`static shared`), broadcast :56-70 | WS clients relaying their own text frames (route closure, App+build.swift:3862-3866); `WebSocketMemoryCompileProgressPublisher` (KB); `DeviceCommandBroker` (Apple) | The tenant's open sockets on `GET /v1/ws` (App+build.swift:3836-3846) | no. The connection map is per process (ConnectionManager.swift:57) |
| `DeviceCommandBroker` (server→device RPC) | Apple/DeviceCommandBroker.swift:16-67 | `PersonalDataTools` (Apple) Apple/PersonalDataTools.swift:199,233, called from `MCPService` (MCP) MCP/MCPService.swift:64 and `SkillRunner` (Skills) Skills/SkillRunner.swift:683 | Device over `ConnectionManager` (WebSockets). The result comes back on `POST /v1/devices/command/:id/result` → `DeviceCommandController` (Apple) Apple/DeviceCommandController.swift:15 → `resolve` (DeviceCommandBroker.swift:55) | no. The continuations live in memory (`:22,50`). The device socket and the result POST must both hit the process that sent the command, or it times out after 12 s (`:27`). The offline fallback `DeviceCommandQueue` is DB-backed (Apple/DeviceCommandQueue.swift, M134) |
| `MemoryCompileProgressPublisher` → WS | KB/MemoryCompileProgressPublisher.swift:24-60; wired App+build.swift:2491-2494 | `MemoryCompileService` (KB) KB/MemoryCompileService.swift:183,295,368; `MemoryCompileController` (KB) KB/MemoryCompileController.swift:62-109. The controller is also driven from Hermes Mirror (App+build.swift:2814) and `SkillsController` (Skills, App+build.swift:3683) | `/v1/ws` clients via `ConnectionManager` (WebSockets) | no |
| Workflow run events → SSE | Workflows/WorkflowEventStore.swift:17-45 (append), :47-77 (list); Workflows/WorkflowEventsSSEResponse.swift:21-38 | `WorkflowEngine`, `WorkflowService` (Workflows) via the shared `WorkflowEventStore` (App+build.swift:3727) | `WorkflowEventsSSEResponse` (Workflows) ← `GET /v1/workflows/runs/:runID/events/stream` (Workflows/WorkflowController.swift:29,112) | **yes**: Postgres `workflow_run_events` polled every 1 s (WorkflowEventsSSEResponse.swift:22,37) |
| Hermes gateway-apply progress → SSE | HermesGateways/HermesGatewayApplyService.swift:39 (subscribers), :81 (job task), :114-136 (subscribe), :156-170 (fan-out) | The apply job task owned by `HermesGatewayApplyService` (HermesGateways) | `HermesGatewayApplySSEResponse` (HTTP) HTTP/HermesGatewayApplySSEResponse.swift:11-30 ← `HermesGatewaysController` (Me) `GET /v1/me/hermes-gateways/apply/:jobID/stream` (Me/HermesGatewaysController.swift:89,323) | partly. The snapshot is replayed from `hermes_gateway_apply_jobs` (`:117-127`), but live steps are in-process only. A stream opened on the other process registers a subscriber for a job it isn't running (`:128`) and never gets `.done` |
| Hermes self-update progress → SSE | System/HermesUpdateService.swift:29-31, :171-192 | The update job task owned by `HermesUpdateService` (System) | `HermesUpdateSSEResponse` (HTTP) ← `HermesUpdateController` (System) `GET /v1/system/hermes/update/:jobID/stream` (System/HermesUpdateController.swift:21,78) | partly. Same shape as gateway-apply: snapshot from `hermes_update_jobs`, live events in-process only |
| WhatsApp pairing → SSE | HermesGateways/WhatsAppPairingService.swift:36-38 (sessions), :69 (pump), :83-87 (subscribe) | The CLI stdout pump task (HermesGateways) | `WhatsAppPairSSEResponse` (HTTP) ← `HermesGatewaysController` (Me) `GET /v1/me/hermes-gateways/whatsapp/pair/:sessionID/stream` (Me/HermesGatewaysController.swift:71,345) | no. Ephemeral, nothing persisted (`:3-6`) |
| Photon setup → SSE | HermesGateways/PhotonProvisioningService.swift:67-69 (sessions), :114 (poller), :157-163 (subscribe) | The poller task (HermesGateways) | `PhotonSetupSSEResponse` (HTTP) ← `HermesGatewaysController` (Me) `GET /v1/me/hermes-gateways/photon/setup/:sessionID/stream` (Me/HermesGatewaysController.swift:79,455). `POST photon/setup/:sessionID/phone` (`:78`) must also hit the same process | no. Session map in memory (`:67-69`) |
| Vault activity → SSE | Teams/VaultActivityPublisher.swift:8-33 (publisher), :35-65 (`VaultActivityRecorder`) | `VaultActivityRecorder` (Teams), used by `SpacesController` (Spaces, App+build.swift:2557), `KanbanController` (Kanban, App+build.swift:3939) and `TeamController` (Teams) Teams/TeamController.swift:522 | `VaultActivitySSEResponse` ← `TeamController` (Teams) `GET /v1/vaults/:vaultID/activity/stream` (Teams/TeamController.swift:88,497) | no. Rows persist to `vault_activity_events` (VaultActivityPublisher.swift:52), but the stream carries only in-process publishes and doesn't replay |
| Ingestion batch events → SSE | HTTP/IngestionSSEResponse.swift:14-35; Ingestion/MultimodalIngestionService.swift:344-356 | `MultimodalIngestionService` / `MultimodalIngestionWorker` (Ingestion); `KnowledgeExtractionWorker` (Workers) inserts `ingestion_events` (Workers/KnowledgeExtractionWorker.swift:331-334) | `IngestionSSEResponse` (HTTP) ← `MultimodalIngestionController` (Ingestion) `GET /v1/ingestions/:batchID/events` (Ingestion/MultimodalIngestionController.swift:17,31) | **yes**: Postgres `ingestion_events` polled every 2 s (IngestionSSEResponse.swift:25) |
| `MeTodayCache` (cache invalidated by other modules) | Me/MeTodayCache.swift:17-87 | Invalidated by EventBus `memory_upserted` (Memory) and `achievement_unlocked` (Achievements) | `MeTodayController` (Me) `GET /v1/me/today` (App+build.swift:1860-1870) | no. A write handled by the other process is visible only after the 300 s TTL (App+build.swift:1850) |
| `HermesMirrorNudging` (connect-time refresh nudge) | Hermes/Mirror/HermesMirrorRefreshWorker.swift:38 (pending), :62-66 (enqueue), :92-96 (wake) | `HermesConfigController` (Settings) Settings/HermesConfigController.swift:160-161, wired App+build.swift:3386, route `/v1/settings/hermes` | `HermesMirrorRefreshWorker` (Hermes/Mirror) | no. A nudge to a process whose worker is off is lost until the next 15-min tick somewhere |
| `AchievementsWorker` inbox | Achievements/AchievementsWorker.swift:15-40 | `LinkCaptureService` (Capture), `LLMController` (LLM), `MemoryController` and `QueryController` (Memory), `MemoryCompileController` (KB), `SoulController` (Auth), `VaultController` (Vault) | `AchievementsWorker` → `AchievementsService` (Achievements) → APNS plus EventBus `achievement_unlocked` | no (bounded `AsyncStream`). The sink is Postgres, so a Go producer can write progress itself |
| `RetrievalTelemetryWorker` inbox | Telemetry/RetrievalTelemetryWorker.swift:59-75 | `HermesMemoryService`, `QueryController` (Memory); `ConversationController` (Chat) | `RetrievalTelemetryWorker` (Telemetry) → `retrieval_telemetry_events` | no (in-process). Insert-only sink, so a Go producer can insert directly |
| `PeriodRecapThemesWorker` inbox | Me/PeriodRecapThemes.swift:355-385 | `PeriodRecapService` (Me) | `PeriodRecapThemesWorker` (Me) | no (in-process). The claim row in `recap_themes` lets another process retry after the cutoff (`:113-126`) |
| `MemoryHitCountUpdateQueue` | Repositories/MemoryHitCountUpdateQueue.swift:21-35 | `MemoryRepository.semanticSearch` (Repositories), shared by every search path (App+build.swift:600-604) | The same queue → `UPDATE memories SET query_hit_count = query_hit_count + 1` (Repositories/MemoryRepository.swift:386) | no. Harmless: relative increments |
| `HermesContainerManager` image override | Hermes/HermesContainerManager.swift:98-101 | `HermesUpdateService` (System) `setImage`, System/HermesUpdateService.swift:295,340,367 | `HermesContainerManager.dockerRun` / `reprovisionAll` (Hermes) | no. After a self-update, only the process that ran it spawns tenants on the new image |
| `UserCredentialStore` cache | LLM/Routing/UserCredentialStore.swift:10-12, :51 (cache), :60 (TTL) | Self-invalidated by its own `upsert` / `delete` (`:148-287`), called from `ProvidersController` (Me) `/v1/me/providers` (Me/ProvidersController.swift:105,207,244) | Routing adapters and `CerberusRouterService` (LLM/Routing) | no. A credential changed through the other process stays stale for up to 600 s |
| `AgentRoomRunRegistry` (one chain per room) | Agents/AgentRoomOrchestrator.swift:349-363, used :190-196 | `AgentRoomOrchestrator` (Agents) ← `POST /v1/agents/rooms/{room}/messages` (Agents/AgentRoomsController.swift:29) | Same | no. The guard is per process, so two processes can run two chains in one room. Stop is DB-backed (`stopRequestedAt`, Agents/AgentRoomsController.swift:169-170) and works across processes |
| Rate-limit counters (`MemoryPersistDriver`) and `InFlightLimitStore` | Middleware/RateLimitStorageFactory.swift:46-51; Middleware/RateLimitMiddleware.swift:143 | Every `RateLimitMiddleware` (Middleware) | Every route group | no with `RATE_LIMIT_STORAGE_KIND=memory` (the default, App+build.swift:184); **yes** via Valkey keys with `redis` (RateLimitStorageFactory.swift:52-74). `InFlightLimitStore` is always per process |

That makes 25 rows.

### Process-local session state (route pairs that must move together)

These are not push channels, but the same rule applies. Each one keeps state in
memory between a *start* request and a *finish* request, so both requests must reach
the same process.

| Store | File:line | Start → finish |
|---|---|---|
| `CalendarOAuthSessionStore` | Calendar/CalendarOAuthSessionStore.swift:11 | Google Calendar OAuth start → unauthenticated callback (`state`) |
| `XaiOAuthSessionStore` and `XaiOAuthProcessRegistry` | Integrations/XaiOAuthSessionStore.swift:16; Integrations/XaiOAuthProcessRegistry.swift:13 | xAI authorize → callback submit (holds a live `docker exec` handle) |
| `NousOAuthSessionStore` and `NousOAuthProcessRegistry` | Integrations/NousOAuthSessionStore.swift:8; Integrations/NousOAuthProcessRegistry.swift:14 | Nous device-code start → await completion |
| `WebAuthnChallengeStore` | Auth/WebAuthnService.swift:75 | WebAuthn begin → finish (registration and authentication) |
| `PreAuthChallengeStore` | Auth/Phone/PhoneAuthController.swift:33 | Phone OTP issue → verify. Its doc says it must move to a shared `PersistDriver` for multi-replica |

Left out on purpose: request-scoped LLM output streams (`AsyncThrowingStream` in
LLM/Routing, Chat, Memory/QueryController, Knowledge, Agents/AgentRoomsController.swift:188),
where producer and consumer are the same request. Also left out: caches nobody else
invalidates (`NewsTickerCache`, `JWKSCache`), and `Vault/VaultNoteMemoryUpsertQueue.swift`,
which is never constructed.

## Channels spanning more than one module dir

These constrain migration order. Publisher and subscriber dirs must move together, or
the channel must first be replaced by something cross-process: a Postgres table plus
polling, as workflows and ingestion already do.

1. **EventBus `memory_upserted`**: Memory → Me (`MeTodayCache`), Skills (log).
2. **EventBus `achievement_unlocked`**: Achievements → Me (`MeTodayCache`), Skills (log).
3. **EventBus `vault_file_created`**: Capture, Vault → Skills (log only, so not yet load-bearing).
4. **EventBus `health_event_synced`**: Health → Skills (log only, so not yet load-bearing).
5. **EventBus `hermes_run_event`**: Hermes/Runs → Hermes/Runs (SSE), Skills (log). It
   degrades to a 5 s DB poll across processes, so this one is not blocking.
6. **ConnectionManager**: WebSockets ← KB (compile progress), Apple (device RPC), plus
   the `/v1/ws` route in App+build.swift.
7. **DeviceCommandBroker**: Apple ← MCP, Skills; delivered over WebSockets. The result
   endpoint (Apple) must share a process with the caller.
8. **MemoryCompileProgressPublisher**: KB → WebSockets; driven from Hermes/Mirror and Skills.
9. **Gateway-apply, WhatsApp-pair and Photon-setup SSE**: HermesGateways → HTTP
   (SSE responses) and Me (`HermesGatewaysController`).
10. **Hermes self-update SSE**: System → HTTP (`HermesUpdateSSEResponse`).
11. **Vault activity SSE**: Spaces, Kanban, Teams → Teams.
12. **Ingestion events SSE**: Ingestion, Workers → HTTP / Ingestion. Already
    cross-process via polling, so it doesn't constrain order.
13. **MeTodayCache invalidation**: Memory, Achievements (via EventBus) → Me.
14. **Hermes Mirror nudge**: Settings → Hermes/Mirror.
15. **AchievementsWorker inbox**: Capture, LLM, Memory, KB, Auth, Vault → Achievements.
16. **RetrievalTelemetryWorker inbox**: Memory, Chat → Telemetry.
17. **HermesContainerManager image override**: System → Hermes.
18. **UserCredentialStore cache**: Me (`ProvidersController`) → LLM/Routing.

`WorkflowEventStore` (Workflows only, and cross-process anyway),
`PeriodRecapThemesWorker` (Me only), `MemoryHitCountUpdateQueue` (Repositories only)
and `AgentRoomRunRegistry` (Agents only) each stay inside one dir.
