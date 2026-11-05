# Wave 5 plan

Written 2026-11-05 from the roadmap's per-module recipe, in the format of `wave-4-plan.md`. Swift was
read at `fdbed9b0`, the image production and staging run. Go was read at `cda9718`. Infra was read
through `gh` at `301dcfce`, without changing anything. Nothing here has been built or run. Claims
marked **[inferred]** were not verified in code; everything else cites what was read. `App+build`
means `Sources/App/App+build.swift`.

## Modules (124 routes)

Method and path were checked against each controller's `addRoutes`. `routes.json` agrees on all 124.

| Module | Routes | Controllers (line of the route table) |
|---|---|---|
| agents | 10 | `AgentsController`:19-21, `AgentRoomsController`:24-30 |
| chat | 15 | `ConversationController`:119-130, `ChatExperienceController`:23-33 |
| grok | 4 | `GrokController`:34-37 |
| llm | 22 | `LLMController`:41, `TranscribeController`:41, `OpenAIAudioController`:64,68, `LLMHealthController`:20, `RouterController`:26-34, `ParallelController`:22-29 |
| mcp | 6 | `MCPController`:24, `AgentConnectionsController`:14-18 |
| me-providers | 14 | `ProvidersController`:58-67 (9), `LLMPreferencesController`:90-91 (2), `ConnectionsController`:23-25 (3) |
| self-improvement | 12 | `SelfImprovementController`:19-30 |
| skills | 15 | `SkillsController`:39-43, `SkillOutputsController`:39, `JobsController`:26-27, `CronBridgeController`:18-24 |
| tts | 1 | `TTSController`:28 |
| workflows | 25 | `WorkflowController` (21, plus templates 3 at :187-189), `WorkflowWebhookController`:19 |

`GET /v1/ws` (module `app`, wave 8) joins this wave, for 125 in all. See group 5J.
`POST /v1/devices/command/{id}/result` is already ported in `apple` and held on Swift; it gets its
broker in 5J and flips with it.

## Where `routes.json` and `modules.yaml` mislead

The inventory is right about paths. It has no conditions and no middleware, and those matter here.

| Route | What the inventory doesn't say | Source |
|---|---|---|
| `POST /v1/tts` | Mounted only when `llm.provider.openai.apiKey` is non-empty. That key encodes to `LLM_PROVIDER_OPENAI_API_KEY`. The sealed env has only `LLM_PROVIDER_OPENAI_APIKEY` (legacy spelling, ignored per D-wave4-prep), and no values file sets it. **[inferred]** So it answers 404 in production today. A `curl` settles it | `App+build`:1934-1964 |
| `POST .../messages/prepare`, `/commit`, `DELETE .../local-executions/{id}` | Only if `hybridExecution.enabled` (default `"true"`, sealed `HYBRID_EXECUTION_ENABLED`) | `ConversationController` `addRoutes`, `App+build`:2262 |
| `POST .../local-executions/{id}/tools` | Also needs `localExecution.toolBroker.enabled` (sealed) | `ConversationController`:130 |
| `/v1/me/preferences/hybrid-execution` | Only with hybrid execution | `App+build`:2306 |
| All 25 workflow routes | Only if `cerberus.studio.enabled` (default true, sealed `CERBERUS_STUDIO_ENABLED`) | `App+build`:3725-3726 |
| `/v1/me/providers/*` | Only if `userCredentialStore` exists, so only with a master key | `App+build`:3433 |
| `/v1/me/hermes/cron/*` | Only if `cronBridgeService` exists | `App+build`:2788-2791 |
| `/v1/grok/*` | Only if `grokController` exists (master-key block) | `App+build`:3545 |
| `POST /v1/grok/tts` | Always 501 `tts_coming_soon`; both branches throw | `GrokController` `tts` |
| `POST /v1/audio/speech` | Always 501, on purpose, so Hermes can't fall back to Edge TTS. The exact message is the contract | `OpenAIAudioController` `speech`, `HermesTenantConfigTemplate`:181 |
| `/v1/router/parallel/*`, `/v1/router/synthesis-presets` | Mounted, but 403 `router_parallel_requires_ultimate` unless enabled and Ultimate | `ParallelController`:194-195 |

Module placement that has to change (P6 does it):

- **`/v1/me/hermes/cron` is not a skills route in practice.** The managed path runs `docker exec`
  (`CronBridgeService`:296,324), which is wave 6 machinery. It becomes pseudo-module `skills-cron`, wave 6.
- **`/v1/ws` is in `app`, wave 8,** but wave 4 committed to flipping it at the end of wave 5. It becomes
  pseudo-module `ws`, wave 5.
- **`/v1/skills/outputs` shares a prefix with `/v1/skills` but not a rate limit:** `skillOutputsByUser`
  against `skillRunByUser` (`App+build`:3695-3698).
- **`llm` is four unrelated surfaces** with different auth and limits (table in P2). It becomes `llm-router`,
  `llm-voice`, `llm-chat`, and `llm-admin`, which needs a narrower prefix than `/admin`.

Middleware per group, all in `App+build`, for P2 and the module `Mount` functions:

| Surface | Middleware | Lines |
|---|---|---|
| `/v1/llm/chat` | jwt, byoHermes, Entitlement `.chat` with `hasUsableCredential`, `chatByUser`, InFlightLimit (`http.maxConcurrentStreamsPerUser`, default 3), contextRouter | 1653-1669 |
| `/v1/transcribe` | jwt, Entitlement `.chat` platform-funded, `transcribeByUserPerMinute`, `transcribeByUserDaily` | 1740-1745 |
| `/v1/audio/*` | `OpenAIErrorEnvelopeMiddleware` outermost, `AudioJWTAuthenticator`, Entitlement `.chat`, InFlight 2 on transcriptions, `audio*ByUser*` | 1767-1783 |
| `/v1/tts` | platform-funded, `ttsByUser*` | 1958-1963 |
| `/v1/conversations` | jwt, byoHermes, `hermesProfileMiddleware`, `conversationByUser`, Entitlement `.memoryQuery` | 2282-2290 |
| `/v1/chat/inbox` | `conversationByUser` | 2298 |
| `/v1/mcp` | `MCPOriginGuard`, `mcpAnonymousByIP`, `MCPAuthenticator`, `mcpByUser` | 2435-2439 |
| agent-connections, agents, providers, preferences, router, connections | `settingsByUser` | 2464, 3165 and others |
| `/admin/llm/health` | `AdminTokenMiddleware` | 3265 |
| `/v1/grok` | jwt, `PremiumGuardMiddleware` | 3545 |
| `/v1/jobs` | `jobsByUser`, Entitlement `.chat` | 3704-3711 |
| `/v1/workflows` | `workflowsByUser`, Entitlement `.workflowAutomation`. Templates: jwt only. Hook: HMAC only | 3753-3760 |
| `/v1/me/improvement`, `/v1/me/hermes/cron` | jwt only | 1572, 2789 |

## Findings that shape the split

1. **The LLM stack exists; its request-path callers don't.** `internal/llm` has the adapters, Cerberus, the
   free lane, the pool, profiles, the catalog, the trial and metering. It has no `RoutedHermesLLMService`,
   `RoutedHermesLLMStreamService`, `HermesToolErrorClassifier` or `ChatStreamCompletionPolicy`, and `NewStack`
   isn't wired into `cmd/App/main.go`. Group 4F is writing part of this now, uncommitted, in
   `.worktrees/wave-4-agent/internal/llm/streamservice.go` (456 lines). Wave 5 builds on that and must not repeat it.
2. **Chat is the only route with a coupling that can't be held by ingress.** Both real clients declare
   `chat.hermes_run` (`chat-view-model.svelte.ts`:354, `ConversationsEndpoints.swift`:204), so escalated turns are
   live. An escalated turn starts a run through `HermesRunsService`, which is wave 6 and whose watcher lives in
   process. See "Hermes coupling".
3. **`HERMES_BASE_URL` is the wrong name.** Neither server reads it (0 hits in `Sources/App`). Both read
   `hermes.gatewayUrl`, so `HERMES_GATEWAY_URL` (`App+build`:146,723; `internal/config/config.go`:137). That key
   is sealed in both `api-env-production` and `api-env-staging`. The `HERMES_BASE_URL` in
   `apps/api/values-production.yaml` is inert, and setting it on staging would do nothing. Wave 4's plan
   (flip blocker for 4F) and the "Staging has no Hermes" note use the wrong name.
4. **Most jobs can stay in Swift while their routes flip.** Workflows and self-improvement routes only read and
   write rows that Swift's workers poll (`WorkflowEngine`:52 sleeps on `pollInterval`, no in-process wake from
   the controller). Only `CronScheduler`, `LegacyJobWorkflowMigrator` and `HermesRunsService` are unsafe in two
   servers, and none of them has a Swift flag.
5. **Grok needs no Docker at request time.** `containerManager.handle` is a DB read plus a decrypt
   (`HermesContainerManager`:273-276,384-403), and the proxy is an HTTP POST (`HermesGrokProxy`:115-135). So grok
   stays in wave 5. The cron bridge does need Docker, so it doesn't.
6. **The Swift image doesn't appear to ship Docker.** The `Dockerfile` installs no docker client, and
   `apps/api/values-common.yaml` mounts no socket, while `docker.binaryPath` defaults to `/usr/bin/docker`. **[inferred]**
   So no new tenant container can be created in the cluster, `/v1/grok/*` answers 402 or 409 for everyone
   once a request passes validation, and the managed cron path fails. A row count on `hermes_tenant_containers` settles it. Wave 6 should know.

## Groups (one branch each)

Module names marked "new" are pseudo-modules, split as in D-me-split, so each passes G1 on its own routes and no
gate is relaxed. Test counts are `@Test` in the wave's `Tests/AppTests` directories (721 in all) and exclude
"Related tests", listed separately below.

| Group (branch `go-migration/wave-5-*`) | Modules | Routes | Swift tests | Flip blockers |
|---|---|---|---|---|
| `wave-5-prep` | none | 0 | 365 (342 audit, 23 stream) | Must merge before the groups start, except where marked |
| `wave-5-router` (5A) | me-providers 14, new `llm-router` 17, new `llm-admin` 1 | 32 | 33 | Flips with chat: credential cache |
| `wave-5-voice` (5B) | new `llm-voice` 3, tts 1 | 4 | 56 | `/v1/tts` decision; audio token proof |
| `wave-5-chat` (5C) | chat 15, new `llm-chat` 1 | 16 | 61 | Escalation decision; staging stub for G2 |
| `wave-5-agents` (5D) | agents 10 | 10 | 23 | Staging stub for G2 |
| `wave-5-skills` (5E) | skills 8 | 8 | 106 | Flips with `ws` |
| `wave-5-mcp` (5F) | mcp 6 | 6 | 42 | Flips with `ws` |
| `wave-5-workflows` (5G) | workflows 25 | 25 | 16 | Engine stays in Swift, migrator unsafe twice (only if moved) |
| `wave-5-improvement` (5H) | self-improvement 12 | 12 | 13 | None beyond G1, G2 |
| `wave-5-grok` (5I) | grok 4 | 4 | 6 | None beyond G1, G2 |
| `wave-5-ws` (5J) | new `ws` 1 | 1 (+ held result route) | 0 in wave dirs (11 related) | Bundle: `ws`, device result, `kb-compile`, skills, mcp |
| deferred to wave 6 | new `skills-cron` 7 | 7 | 0 | Docker exec, container manager |

Routes: 32 + 4 + 16 + 10 + 8 + 6 + 25 + 12 + 4 + 7 = 124, plus `/v1/ws` = 125.
Tests: 365 + 33 + 56 + 61 + 23 + 106 + 42 + 16 + 13 + 6 = 721.

The 342 audit tests cover code that's already merged in `internal/llm` (78 `Test` functions today). They're a
coverage check, not a port. Grouping by file name is the only thing behind the split of those 342: **[inferred]**
which cases Go already covers needs the audit in P1.

## Land first

P0, P2, P3, P4 and P6 don't depend on each other. P1 needs 4F's `streamservice.go` on `main`, or takes it over if 4F
stalls. P5 needs P2's WebSocket dependency approved.

| # | Package | Swift source | Used by |
|---|---|---|---|
| P0 | Parity harness and stubs | n/a | all groups |
| P1 | `internal/llm`: wire `NewStack`, routed services, classifier, completion policy, test audit | `LLM/Routed*`, `HermesToolErrorClassifier`, `ChatStreamCompletionPolicy` | 5A, 5C, 5D, 5E, 5F, 4F |
| P2 | Request-path helpers: rate limits, in-flight limit, client capabilities, push-stream SSE, OpenAI error envelope, audio JWT | `Middleware/RateLimitMiddleware`, `HTTP/ClientCapabilities`, `HTTP/SSEResponse`, `Auth/Middleware/AudioJWTAuthenticator` | all groups |
| P3 | `internal/soul` (read, strip, write), vault navigation and status extracted from `modules/vault`, `internal/hermescontainers` (read-only handle) | `Soul*`, `VaultNavigationService`, `VaultIndexStatusService`, `HermesContainerManager.handle` | 5C, 5F, 5H, 5I |
| P4 | `internal/skills`: catalog, manifest, slash parser, context router; `usagemeter` budgets; `apns` additions; self-improvement activity hook | `Skills/SkillCatalog`, `Resources/Skills`, `Billing/UsageMeterService` | 5C, 5E, 5F, 5G, 5H |
| P5 | `internal/ws` and `internal/devicerpc`: connections, broadcast guard, broker | `ConnectionManager`, `WebSockets/`, `Apple/DeviceCommandBroker` | 5E, 5F, 5J, 4F's `kb-compile` |
| P6 | Manifest splits in `scripts/gen-modules.py`, `heldPaths`, ingress prefix changes | n/a | every group's G1 |

### P0: harness and stubs

- **New harness features.** A token-coalesce option on `stream` steps, because chunk boundaries aren't contract
  (see "Streaming"). A `ws` step, or an explicit decision that WS frames are proven in Go tests only. A way to run
  scenarios with a declared `X-LV-Client-Caps`: the existing `headers` field probably suffices **[inferred]**.
- **Stubs.** See "External providers in `parity/stubs`".
- **Hermes stub image.** Build `parity/stubs/llm` as a deployable image. It's needed for staging (see "G2 for chat").
  Pushing the image is CI; the Deployment is an infra PR.

### P1: `internal/llm` finish

- Wire `llm.NewStack` in `cmd/App/main.go` from the env it already reads. Coordinate with 4F, which needs the same thing
  for `/v1/query/stream`; whichever lands first does it.
- Port `RoutedHermesLLMService`, and `RoutedHermesLLMStreamService` (`LLM`:416, wiring at `App+build` ~2185) in Swift's
  decision order: parallel strategy, then BYOK pinned, then managed `managedAutoRequest`, then free lane
  (`free_lane_unroutable` fails closed), then the gateway fallback (`HERMES_STREAM_IDLE_TIMEOUT`, 60 s).
- Port `HermesToolErrorClassifier.sanitize` and `ChatStreamCompletionPolicy`. Its `emptyResponseMessage` is a wire string.
- Audit the 342 tests below against `internal/llm/*_test.go`, and port the cases that have no Go counterpart.
- **Haiku trial rule stands.** Chat builds `temperature: 0.4`, follow-ups 0.5, and others set their own. All pass through
  `internal/llm/anthropic_translate.go`, which drops them (line 125). Don't add them back. No test uses a real key.
- Leave `ParallelExecutor` as a hook unless the owner says otherwise (decision 7).

### P2: request-path helpers

- **`internal/ratelimit` has 10 policies. Add, with Swift's numbers** (`RateLimitMiddleware`): `chatByUser` 30/60,
  `conversationByUser` 30/60, `skillRunByUser` 10/60, `skillOutputsByUser` 60/60, `jobsByUser` 20/60, `workflowsByUser` 60/60,
  `mcpAnonymousByIP` 600/60 (by IP), `mcpByUser` 120/60, `transcribeByUserPerMinute` 10/60, `transcribeByUserDaily` 200/86400,
  `audioTranscriptionsByUserPerMinute` 20/60, `audioTranscriptionsByUserDaily` 200/86400 (voice tier scale),
  `audioSpeechByUserPerMinute` 30/60, `audioSpeechByUserDaily` 1000/86400 (voice tier scale), `ttsByUserPerMinute` 30/60,
  `ttsByUserDaily` 1000/86400. Check tier scales against each definition; only some have one.
- **`InFlightLimit`**: nothing in Go. Streams per user. Must release on disconnect.
- **`ClientCapabilities`**: `x-lv-client-caps`, comma-separated, lowercased, unknown tokens ignored, absent means nothing
  (`HTTP/ClientCapabilities.swift`). It fails closed, which protects old iOS builds.
- **Push-stream SSE.** `internal/sse` only serves a polling `Source`. Chat, rooms and parallel need a writer for an event
  channel: `SSEStreamResponse` frames (`data:` only, `type` in the JSON, ISO 8601 dates, a final `.error` frame, then
  close) and `EncodableSSEStreamResponse(eventName:)`. Register through `Router.Streaming`.
- **`OpenAIErrorEnvelopeMiddleware`**, **`AudioJWTAuthenticator`**: accepts `scp == nil` or `audio`, refuses any other scope,
  checks `hermesAudioTokenEpoch`. Go's `internal/auth` already refuses every scoped token (`middleware.go`:121), so the
  asymmetry the Swift comment calls "the whole security model" holds. Keep a test proving it from both sides.
- **`PremiumGuardMiddleware`**: 402 with an exact JSON body. **`MCPOriginGuard`, `MCPAuthenticator`** stay in 5F.
- **`hermesProfileMiddleware`**: sets the session key to `<hermesProfileID>:<slug>` or `pending-<tenant>:<slug>`, and creates
  the default persona if missing (`HermesProfileMiddleware`:66-85). `modules/profiles` mentions it; check before writing.

### P3: shared extractions

- `internal/modules/vault/soul.go` only renders the default. Add read, strip and write. Chat, MCP's `status`, and
  self-improvement use them; `/v1/soul` itself stays wave 8.
- MCP needs `VaultNavigationService`, `VaultIndexStatusService` and `VaultLinkRepository`, which live inside `modules/vault`
  today. Move them out with no behaviour change, in one PR, before 5F starts. `ChunkBackfillService` has no Go form.
- `internal/hermescontainers`: `Handle(ctx, tenant)` reads the row and decrypts `api_server_key` with `secretbox`. It writes
  nothing, starts nothing, and has no Docker. 5I uses it. Wave 6 extends it.

### P4: skills and metering

- **`internal/skills`.** The catalog merges built-ins from `Resources/Skills` (32 directories, 4.7 MB) with
  `<vaultRoot>/tenants/<id>/skills/<name>/SKILL.md`. Embed with `go:embed` or copy in the Dockerfile; bundled skills must
  leave the Swift repo in the end (see "Archive"). Includes the manifest parser, slash parser, and context router selector, whose
  Swift tests are counted under 5E.
- **`usagemeter`** has `Record` and vision only. Add `CheckBudget` (`UsageMeterService`:185, called only by `LLMController`:98),
  `CheckSkillBudget` (:271, `SkillRunner`:188) and characters. `CostLedgerService.checkBudget` has no callers, so don't port a
  caller for it (see the note "USD spend cap is inert").
- **`apns`** lacks `notifyLLMReply`, `notifyDigest`, `notifyNudge` and proactive delivery. `notifyWorkflow` stays with the engine.
- **`internal/selfimprove.Activity`**: `noteActivity` (updates `last_activity_at`) and `triggerComplexSession` (≥5 tool calls or a
  created workflow, cooldown, enqueue) (`SelfImprovementService`:293-311). Chat and workflows call these, so they can't wait for 5H.

### P5: WebSocket and broker

- **New dependency.** Go has no WebSocket library in `go.mod`. `coder/websocket` is the proposal, subject to D-deps vetting.
- `ConnectionManager` is keyed by tenant with several sockets per tenant **[inferred]**. `WebSocketBroadcastGuard` (9 tests)
  relays client text frames only if they pass size, JSON and `type` checks (`App+build`:3836-3880).
- `DeviceCommandBroker` timeouts are 12 s (`:27`). On timeout, only `.reminderCreate` and `.calendarCreate` queue, with a visible push.
  Reads fail. `commandResult` in `internal/modules/apple/module.go`:280 gets the continuation resume.
- 4F's compile progress publisher must call this. Until it exists, 4F publishes through an interface.

### P6: manifest

Add to `SPLITS`/`SPLIT_PATHS`: `llm-router` (`/v1/router`), `llm-voice` (`/v1/audio`, `/v1/transcribe`), `llm-chat` (`/v1/llm`),
`llm-admin` (`/admin/llm/health`), `skills-cron` (`/v1/me/hermes/cron`, wave 6), `ws` (`/v1/ws`, wave 5). Keep
`/v1/memory-compile`, `/v1/devices/command`, `/v1/reminders/detect`, `/v1/me/recap/period` and `/v1/admin/community` under `heldPaths`,
each `releasedBy` the batch below.

## Group details

### 5A `wave-5-router`: me-providers (14), `llm-router` (17), `llm-admin` (1)

- **Routes.** `/v1/me/providers`: `GET` list and catalog, `PUT` and `DELETE /{provider}`, `POST /{provider}/test`,
  `GET /{provider}/models`, `GET` and `POST /{provider}/pool`, `DELETE /{provider}/pool/{keyID}`. `GET` and `PUT /v1/me/preferences/llm`.
  `GET /v1/me/connections`, `/events` (JSON, not SSE), `POST /test-all`. `/v1/router`: `GET`, `POST`, `PUT` and `DELETE /{id}`, `/catalog`, `/bindings` (3),
  `/dashboard`, `/parallel/executions` (4), `/synthesis-presets` (4). `GET /admin/llm/health`.
- **Swift tests (33).** `RouterPlaceholderRoutes` 7, `RouterPlaceholderSave` 4, `ParallelExecutor` 2, `ProviderCatalog` 6,
  `ProviderModelListing` 5, `ProviderToolProbe` 9. Related: `Settings/LLMPreferencesControllerTests` 11.
- **Reuse.** `llm.NewPreferenceRepository`, `llm` credentials (read side), `RouterProfileRepository`, catalog, `secretbox`, `ssrfguard`.
- **New in the module.** The write side of `UserCredentialStore` (`internal/llm/doc.go` says it moves with wave 5), with cache
  invalidation on write. `ProviderToolProbe` (background). `ParallelExecutionStore` (290 lines; DB only).
- **Traps.** `models` hard-codes `api.anthropic.com/v1/models` (:432) and `generativelanguage.googleapis.com` (:440), so parity
  must refuse those hosts. `/admin/llm/health` calls `healthCheck()` on every adapter, so it dials real providers. Parallel
  streaming answers 403 while off, and Go must match.
- **Order.** After P1, P2. Others read the tables, so nothing waits on it to be written.
- **Flip blockers.** Credential cache (below). Must flip with 5C.

### 5B `wave-5-voice`: `llm-voice` (3), tts (1)

- **Routes.** `POST /v1/transcribe`, `POST /v1/audio/transcriptions`, `POST /v1/audio/speech`, `POST /v1/tts`.
- **Swift tests (56).** `OpenAIAudio/MultipartFormParser` 15. `Transcribe`: `OpenAICompatibleTranscribeAdapter` 7,
  `TranscribeProviderConfig` 9, `TranscribeServiceUsage` 8, `VoiceUsage` 12. `TTS` 5. Related, stays where it is:
  `Dashboard/VoiceUsageSummary` 4, `Hermes/HermesAudioProxySeed` 10 (wave 6).
- **Transcription rule.** One provider type named for the wire format, as Swift's `OpenAICompatibleTranscribeAdapter` and
  Norviq's `OpenAICompatibleTranscriptionProvider` are, defaulting to `http://whisper.horus.svc.cluster.local:8000/v1`, no key,
  model `Systran/faster-whisper-small`. No paid speech provider, no ffmpeg. `allow-whisper` already names `api-go-production` and
  `api-go-staging`, so no netpol change is needed. Swift falls back to the wire-format adapter on an unknown `TRANSCRIBE_PROVIDER`,
  with a warning. Go keeps only that adapter and the test stub. The sealed value wasn't read, so check it isn't a hosted URL.
- **Timeout.** Whisper takes about 7 s plus 0.38x realtime, so the 120 s cap takes about 53 s. Register on `Router.Streaming` or
  otherwise exempt these routes from the short request timeout.
- **`/v1/audio/speech`.** Port the 501 and exact message: "Spoken replies are not available yet. Voice messages you send are still transcribed."
- **`/v1/tts`.** No in-cluster TTS exists, and Swift's only real adapter is `OpenAITTSAdapter`, which is the paid OpenAI `tts-1`.
  **Go doesn't get an OpenAI speech adapter.** Plan: port the route, gated on a new `TTS_BASE_URL` for a self-hosted, OpenAI-shaped
  service; unset, it isn't mounted, which matches today's 404. `TTS_PROVIDER`, `TTS_DEFAULT_MODEL` and `TTS_CHARACTERS_DAILY` are sealed.
  The web's `SpeakButton` (in `ChatMessageBubble`) calls this route, so **[inferred]** it shows an error toast today. Decision 4.
- **New in the module.** Multipart parser, `TranscribeService`, `VoiceUsageEventStore`, `X-Lumina-Channel` default `app`.
- **Memory note.** Hermes calls whisper directly, so voice rows stay zero. That's an accepted decision. Don't repoint it here.
- **Order.** After P2. Independent of the rest.
- **Flip blockers.** `/v1/audio/transcriptions` serves tenant containers with 90-day scoped tokens. After the flip they reach Go through the
  ingress **[inferred]**, so G2 must send one scoped token through. `hermes.audioProxy.baseURL` isn't set per-tenant in the cluster; see Not verified.

### 5C `wave-5-chat`: chat (15), `llm-chat` (1)

- **Routes.** `POST`, `GET /v1/conversations`, `GET`, `DELETE /{id}`, `GET /{id}/traces`, `POST /{id}/messages/stream`, `/prepare`, `/commit`,
  `DELETE` and `POST .../local-executions/{executionID}[/tools]`, `GET /v1/chat/inbox`, `GET` and `PUT /v1/me/chat-preferences`,
  `GET` and `PUT /v1/me/preferences/hybrid-execution`, `POST /v1/llm/chat`.
- **Swift tests (61).** Chat: `ChatAttachmentPrompt` 9, `ChatGrounding` 3, `ChatSoulPrompt` 2, `ConversationE2E` 12, `ConversationEscalationE2E` 4,
  `Conversation` 7, `HermesEscalationPolicy` 9, `LLMPreferencesChatE2E` 3. LLM: `LLMControllerPushSideEffect` 4, `URLExtractor` 8.
  Related: `Trace/AgentTurnTraceRoundTrip` 5, first-query cases in `Onboarding/GuidedStart`, `Usability/UsabilityEndpoints` 4 **[inferred]**.
- **The stream's side effects, in order** (`ConversationController.streamReply`). User message persisted first. Escalation decided before grounding
  (`HermesEscalationPolicy` is pure). Classic path: history, `embedForQuery`, `chunkindex` hybrid limit 5 with `chat.retrieval.maxDistance`, retrieval
  telemetry, pinned memories, `recordMemoryRetrievals`, schedule context, disclosure, prompt, `WithValues`, classifier, completion policy, persist assistant,
  `EnqueueOutput`, `recordTurnTrace`, PostHog `chat_grounded_answer`, self-improvement, `.firstQuery`, link save (`.linkSaved`), follow-ups, `.done`.
  Frame types: source, token, fallback, parallel, linkSaved, followUps, done, hermesRun, error.
- **Reuse.** `internal/llm`, `memorystore`, `chunkindex`, `capture.CaptureLink`, `onboardlatch`, `posthog`, `hermesresolve`, `entitlement`, `embedding`, `vaultaccess`.
- **New in the module.** `ChatGrounding`, escalation policy, prompt builders, hybrid execution (prepare, commit, tools), trace store,
  `ChatURLPreEnricher`, `X-LuminaVault-Degraded`. Follow-ups come from 4F; don't port twice.
- **Stream router.** `messages/stream` registers on `streamRouter ?? router` (`ConversationController` `addRoutes`). Read which middleware that leaves out before mounting it in Go.
- **Moving target.** On a different branch, `fix/chat-source-citations` has staged, uncommitted Swift changes: `hits.map(\.queryHitDTO)` so chunk hits keep
  their citation. When that ships, port it and bump `parity/SWIFT_IMAGE`.
- **Order.** After P1, P2, P3, P4, and 4F's follow-ups. 5A can run at the same time.
- **Flip blockers.** Escalation (decision 1), staging stub (decision 2), flips with 5A.

### 5D `wave-5-agents`: agents (10)

- **Routes.** `GET /v1/agents/instances`, `/sessions`, `/instances/{instance}/sessions/{session}/messages`, `/room-candidates`, `/rooms`, `POST /rooms`,
  `GET` and `DELETE /rooms/{room}`, `POST /rooms/{room}/messages`, `/stop`.
- **Swift tests (23).** `AgentGatewayClient` 8, `AgentRoomOrchestrator` 6, `AgentRoomTurnPolicy` 6, `AgentRoomsController` 2, `AgentsController` 1.
- **Reuse.** `hermesresolve`, `internal/llm`, P2's push SSE (`event: agent.room`).
- **New.** `AgentsService`: the central instance reads `conversations`, BYO goes through `AgentGatewayClient` (`/api/sessions`).
  Orchestrator and turn policy. `LiveAgentRoomSpeaker`: `llm.chat` at :130, gateway client at :143. `AgentRoomRunRegistry`, per process (:349-363), and 409 `room_busy`.
- **Order.** After P1, P2. No dependency on 5C.
- **Flip blockers.** Staging stub for the success path. Whole prefix `/v1/agents`.

### 5E `wave-5-skills`: skills (8)

- **Routes.** `GET /v1/skills`, `GET /v1/skills/outputs`, `POST /v1/skills/slash`, `PATCH /v1/skills/{name}`, `GET /{name}/runs`, `POST /{name}/run`,
  `POST /v1/jobs`, `POST /v1/jobs/detect`.
- **Swift tests (106).** `BeliefEvolution` 3, `ContextRouter` 7, `CronExpression` 10, `DailyRunCap` 4, `EventBus` 9, `JobOrigin` 2, `Jobs` 5,
  `MuseProactiveManifest` 4, `ProjectModel` 2, `ReminderScheduler` 3, `SkillManifest` 16, `SkillOutputsController` 15, `SkillRunCapGuard` 4, `SkillRunnerLifecycle` 4,
  `SkillRunner` 3, `SkillsController` 1, `SlashCommandParser` 5, `UsageSummaryAggregation` 1 (98), plus `Chat/MuseProactiveIntegration` 8.
  Related: `Services/CronPush` 1, `Memory/MemoryKeepGate` 2. `ReminderScheduler` 3 and `CronPush` 1 wait for their jobs.
- **Biggest port in the wave.** `SkillRunner` is 1,249 lines. It takes transport, memories, embeddings, APNS digest and nudge, cap guard, event bus, skill budget,
  `ProactiveChatDelivery`, `gmailInbox`, `BriefComposer` and the weather tool.
- **Reuse.** `SkillsController.list` appends `mirror.mirroredSkills`, which is a table read (`HermesMirrorService`:249), so Go reads the `HermesMirroredSkill` table directly. `/slash` calls
  `memoryCompileController.compile`, from 4F. Hermes skills answer `hermes_skill_runs_on_hermes`.
- **New.** `SkillRunner`, `JobIntentClassifier` (one LLM call per request), `JobAuthoring` (writes the vault and SQL).
- **EventBus.** `skillRunner.startEventSubscriptions` only logs (jobs.md, "dispatch is TODO"). Don't port the subscription.
- **Order.** After P1, P2, P4, P5, and 4F's compile.
- **Flip blockers.** In the `ws` bundle (`SkillRunner.personalData`, `Skills/SkillRunner.swift`:683-686, calls the broker).

### 5F `wave-5-mcp`: mcp (6)

- **Routes.** `POST /v1/mcp`, `GET`, `POST /v1/me/agent-connections`, `GET /preview`, `PATCH` and `DELETE /{id}`.
- **Swift tests (42).** `AgentConnectionsController` 7, `MCPContract` 16, `MCPMemoryTools` 9, `MCPSetup` 10.
- **Contract.** JSON-RPC, protocol `2025-06-18`, server version `0.2.0`, 1 MiB body. Any `Origin` gets 403 `forbidden` (`allowedOrigins` is empty). Tokens: `lv_` plus
  SHA-256 through `agents.authenticate`, which updates `lastUsedAt`, otherwise JWT. `MCP_PUBLIC_BASE_URL` env.
- **Tools (17).** status, search, browse, read, recent, links, context, index, memory_context, memory_save, memory_update, memory_forget, health_query, calendar_query, reminders_list, calendar_create, reminder_create.
- **Reuse.** `chunkindex.NewHybrid`, `memorystore`, P3 extractions, P3 soul, `modules/health` and `modules/calendar` queries. P5 broker for `PersonalDataTools.deviceRead` (:233) and `deviceWrite` (:199), via `MCPService.callPersonal` (`MCP/MCPService.swift`:64).
- **Order.** After P2, P3, P5. Doesn't need P1 unless a tool calls the LLM **[inferred]**.
- **Flip blockers.** In the `ws` bundle.

### 5G `wave-5-workflows`: workflows (25)

- **Routes.** Everything under `/v1/workflows`, `/v1/workflow-templates`, and `POST /v1/workflow-hooks/{hookID}` (table above; `routes.json` is complete).
- **Swift tests (16).** `Lifecycle` 2, `Validator` 12, `WebhookSignature` 2.
- **Reuse.** `internal/sse` with options for the event feed, `secretbox`, P4's activity hook.
- **New.** `WorkflowService`, `WorkflowValidator`, `WorkflowSpendService` (`CERBERUS_STUDIO_GLOBAL_*_USD_MICROS`), template catalog.
- **SSE.** `WorkflowEventsSSEResponse` polls every 1 s, sends `: keep-alive`, ends when terminal and empty, frames are `id:`, `event:`, `data:`, and sets `X-Accel-Buffering: no`, with `ModelDisclosurePolicy.scrub`.
- **Webhook.** `x-lumina-timestamp`, `x-lumina-signature` (HMAC-SHA256 of `"<ts>.<body>"`), `idempotency-key` (≤200), dedupe key `webhook:<key>`, `payload` flattening.
- **Conditional mount.** Go must read `CERBERUS_STUDIO_ENABLED`. Off means no routes.
- **Order.** After P2, P4. The engine isn't touched: routes write rows, and `WorkflowEngine` polls them.
- **Flip blockers.** None for the routes. See Jobs for what the engine still does in Swift.

### 5H `wave-5-improvement`: self-improvement (12)

- **Routes.** `GET`, `PUT /v1/me/improvement`, `/changes` (3), `/curator/runs`, `/resources` (2), `/runs` (3), `/soul/reviews`.
- **Swift tests (13).** `Policy` 6, `Service` 7.
- **Reuse.** P3 soul, P4 catalog, `vaultpaths`, `hermesresolve.IsUserOverride`.
- **New.** Controller, policy, and the service's approve, reject, rollback paths (they write vault files and SOUL, `SelfImprovementService`:234,246,628).
- **Order.** After P3, P4.
- **Flip blockers.** None. `curator/runs` and `soul/reviews` enqueue rows and the scheduler picks them up. Its LLM goes straight to the gateway (`DefaultHermesLLMService`, `App+build` ~1542), not through routing.

### 5I `wave-5-grok`: grok (4)

- **Routes.** `POST /v1/grok/chat`, `/x-search`, `/vision`, `/tts`.
- **Swift tests (6).** `GrokController` 4, `PremiumGuardMiddleware` 2.
- **Contract.** 402 from the guard, then 409 `xai_not_connected`, 400 validation, 502 `grok_upstream_<code>`, 501 `tts_coming_soon`.
- **Paid.** Tenant's own xAI OAuth, not the platform's **[inferred]**. Not in any smoke.
- **Parity.** Seed `hermes_tenant_containers` with a host the stub answers on. `handle.baseURL` shape needs checking.
- **Order.** After P2, P3.
- **Flip blockers.** None beyond G1, G2. If finding 6 holds and no containers exist, the routes still answer as Swift does.

### 5J `wave-5-ws`: `ws` (1), and the device result route

- **Routes.** `GET /v1/ws`, jwt group (`App+build`:3836-3880). `POST /v1/devices/command/{id}/result`.
- **Swift tests.** None in the wave dirs. Related: `WebSockets/WebSocketBroadcastGuard` 9, `Apple/DeviceCommandBroker` 2.
- **Order.** After P5.
- **Flip blockers.** The bundle below.

## Related tests outside the wave's directories

| File | Count | Goes to |
|---|---|---|
| `Settings/LLMPreferencesControllerTests` | 11 | 5A |
| `Trace/AgentTurnTraceRoundTrip` | 5 | 5C |
| `Billing/UsageMeterBudget` | 5 | P4 |
| `Billing/CostLedgerService` | 5 | P1 audit |
| `Memory/URLSessionHermesChatTransport` | 3 | P1 |
| `Memory/FollowUpGenerator` | 9 | 4F |
| `Memory/MemoryKeepGate` | 2 | 5E **[inferred]** |
| `WebSockets/WebSocketBroadcastGuard`, `Apple/DeviceCommandBroker` | 9, 2 | 5J, P5 |
| `Onboarding/GuidedStart` (first-query cases) | part of 8 | 5C |
| `Usability/UsabilityEndpoints` | 4 | 5C **[inferred]** |
| `Services/CronPush` | 1 | With `CronScheduler` |
| `Dashboard/VoiceUsageSummary` | 4 | Dashboard, already out of scope |
| `Hermes/HermesAudioProxySeed`, `Hermes/Runs/*` | 10, 78 | Wave 6, unless decision 1 moves runs |

## Tests in `wave-5-prep`

**Stream (23).** `HermesLLMStreamService` 8, `HermesToolErrorClassifier` 15.

**Audit (342).** From `LLM/`: `HermesEndpointResolver` 4, `HermesGatewayAdapter` 9, `HermesGatewayTimeout` 5, `ModelOrigin` 5, `ProviderKindSpendable` 5, `RouterTaskClassifier` 2,
`RoutingContextMeter` 10, `UpstreamErrorTelemetry` 1 (41). From `LLM/Routing/`: `AnthropicAdapterTranslation` 39, `AnthropicManagedTrialRouter` 20, `BYOHermesRouting` 4, `BYOKConsistency` 2,
`BYOKFailClosed` 6, `ComplexityClassifier` 20, `CostLedgerWiring` 8, `FreeLaneCatalog` 11, `FreeLaneExhaustedError` 4, `FreeLaneGate` 7, `FreeLaneModelUnavailableFailover` 6, `FreeLanePolicy` 14,
`FreeLaneRecoveryActions` 3, `FreeLaneRouting` 12, `FreeLaneStartupWarning` 3, `ModelDisclosurePolicy` 17, `ModelRouter` 11, `NvidiaAdapterRegistration` 4, `NvidiaNIMLive` 2 (live; don't port),
`OpenAICompletionReader` 12, `ProviderCredentialEvidence` 4, `ProviderCredentialWiring` 3, `ProviderErrorClassifier` 13, `ProviderErrorReasonCode` 8, `ProviderRegistry` 8, `ProviderStreamKitStreamFlag` 3,
`ProviderStreamKit` 9, `RoutedLLMTransportFallback` 3, `RoutedLLMTransportStreaming` 8, `RoutedLLMTransport` 12, `RoutedLLMTransportTimeoutEnvelope` 6, `RoutingModelRouter` 9, `StreamErrorMessage` 4, `UpstreamErrorResponse` 6 (301).

## What's reusable, read in code

| Package | Has | Missing for wave 5 |
|---|---|---|
| `internal/llm` | Adapters (Anthropic, OpenAI-compatible, Hermes gateway with `ChatStream`, Gemini, Ollama), Cerberus, free lane, pool, profiles, catalog, trial, disclosure, errors (`StreamErrorMessage`, `UserFacingError`), `PGMeters`, `WithValues`, `NewStack` | Routed services, classifier, completion policy, credential writes, `ParallelExecutor`, `main.go` wiring |
| `internal/sse` | `Serve`, `Ingestion`, `Frame`, `Cursor`, polling `Source` | Push-stream writer |
| `internal/httpserver` | `Router.Streaming(method, path)` | WebSocket upgrade path **[inferred]** |
| `internal/memorystore` | `EnqueueOutput`, `SuppressJob`, `Create*`, `Find*`, `SemanticSearch`, `LexicalSearch`, `SearchDocuments`, `NewHitCountQueue` | Enough for chat and MCP **[inferred]** |
| `internal/chunkindex` | `NewHybrid`, `Search`, `MergeHits`, `NewIndexer`, links, `HybridSearch` | `ChatGrounding`'s hybrid limit wrapper |
| `internal/hermesresolve` | `Resolve`, `Middleware`, `FromContext`, `IsUserOverride` | Nothing |
| `internal/usagemeter` | `Record`, `RecordVisionEmbed` | `CheckBudget`, `CheckSkillBudget`, characters |
| `internal/entitlement` | `PGCredentials`, `Middleware`, `MiddlewareWithHermes`, `PlatformFundedMiddleware` | Nothing noted |
| `internal/ratelimit` | 10 policies, `TieredMiddleware` | 16 policies (P2), in-flight limit |
| `internal/capture` | `CaptureLink`, `ExtractURLs`, `EnrichURL`, `EnrichAndRewrite` | `ChatURLPreEnricher` wrapper |
| `internal/apns` | `Notify`, `NotifyAchievement`, `NotifyIngestion` | LLM reply, digest, nudge |
| `internal/onboardlatch` | `Latch`, `firstQuery` constant | Nothing |
| `internal/posthog` | `Capture` | Event names |
| `internal/secretbox`, `internal/ssrfguard` | As named | Nothing noted |

No Go equivalent was found for: `HermesMemoryService` (4F), `ProviderToolProbe`, `ParallelExecutionStore`, `FollowUpGenerator` (4F), `SkillCatalog`, `SkillRunner`, `ChatGrounding`,
`ProactiveChatDelivery`, `ConnectionManager`, `SelfImprovement*`, `TranscribeService`, multipart parser, `HermesDashboardClient` (wave 6), `RunsClient`, `ChunkBackfillService`.

## Jobs

Nothing moves in wave 5 unless a decision below says so. `LV_GO_JOBS` stays `""`. Parity stays `LV_ENVIRONMENT=test`, so no job runs there.

| Job | During the wave | Two servers safe? | Swift flag | For archive |
|---|---|---|---|---|
| `WorkflowEngine` | Stays in Swift. Routes write, it polls | Yes: `SKIP LOCKED`, lease, heartbeat | `cerberus.studio.enabled` also unmounts routes | Moves with `SkillRunner` in Go; needs all node kinds |
| `WorkflowScheduler` | Stays | Yes: unique `(workflow_id, dedupe_key)` | Same | Moves with the engine |
| `WorkflowMaintenanceService` | Stays | Yes: idempotent | Same | Moves with the engine |
| `LegacyJobWorkflowMigrator` | Stays | **No:** check-then-create, every 5 min | Only the studio flag, which also removes routes | Phase 12 |
| `SelfImprovementScheduler` | Stays | Yes: `SKIP LOCKED`, `lease_until` | `SELF_IMPROVEMENT_ENABLED` stops enqueue only | Moves after LLM direct path ports |
| `CronScheduler` | Stays | **No:** reads `last_run_at`, dispatches, then saves | None | Phase 12, or flag (decision 3) |
| `HermesRunsService` | Stays, wave 6 | **No.** On boot it scans every active row. A run older than 300 s is marked lost, and a younger one gets a second watcher, which appends with `seq = lastSeq + 1` and pushes again (`HermesRunsService`:117-131, `HermesRunStore`:80, `HermesRunWatcher`:228) | None | Wave 6 with a flag |
| `MemoryIndexWorker` | Stays. Go writes the outbox with `EnqueueOutput` | Yes: `SKIP LOCKED` | None needed | Later |
| `KnowledgeExtractionWorker`, `PeriodRecapThemesWorker` | Stay. Can move now that the stack exists | Yes | Exists / claim row | Wave 7 or Phase 12 |
| `ReminderScheduler`, `LearnedDigestScheduler`, `MeetingBriefScheduler`, `DailyPostWorker` | Out of scope (jobs.md) | See jobs.md | | Phase 12 |

**What degrades with `/v1/ws` on Go and these jobs on Swift.**

- `CronScheduler` runs `SkillRunner` in Swift. Only `daily-brief` (`0 7 * * *`) and `lapse-archiver` (`0 3 * * *`) have a non-empty `schedule:`, and only `daily-brief` references device or
  weather tools. Device reads will fail with "device did not respond" after 12 s; weather has a last-known-location fallback. Writes queue, with a push.
- `WorkflowEngine` `skill` nodes (`:437`) use the same runner.
- Mirror-driven compile frames (`App+build`:2814) stop reaching sockets until wave 6.

The recommendation is to accept these, since each is small and named, rather than pay for a Swift deploy now. Owner's call (decision 3).

## Channels (coupling.md), wave 5 disposition

| Channel | Disposition |
|---|---|
| `ConnectionManager`, `/v1/ws` | Moves in 5J. All publishers on Go by then, except the mirror (wave 6) |
| `DeviceCommandBroker` | Moves in P5 and 5J. Callers (MCP, Skills) flip in the same PR |
| `MemoryCompileProgressPublisher` | Flips with `/v1/ws` as wave 4 committed |
| Open sockets at flip time | Stay on Swift until they drop **[inferred]**. During that window Go's broker can't reach that device. Restart Swift's Deployment right after the flip so clients reconnect through the new route. iOS reconnection not verified |
| `EventBus` to `SkillRunner` | Log-only. Dropped |
| `EventBus` `hermes_run_event` | Swift stays the publisher. Feed polls Postgres every 5 s anyway (`HermesRunsController`:24-27) |
| `UserCredentialStore`, preference and profile caches (600 s, `internal/llm/preference.go`:20) | **Real.** Swift and Go each cache in process. A key deleted on Go still works on Swift for up to 600 s. So 5A and 5C flip together; Swift's remaining callers (engine, workers, 4F routes if not flipped) carry a named gap |
| `AgentRoomRunRegistry` | Per process, and rooms flip whole. A turn running on Swift at flip time finishes there **[inferred]** |
| `RetrievalTelemetryWorker` | Go inserts directly (4F) |
| `AchievementsWorker` inbox | `achievementsink` |
| Workflow events | Already cross-process through Postgres |
| `MeTodayCache` | Handled as in flip-blockers.md. Chat memory writes are one more source |

No Valkey bridge is needed in this wave.

## Hermes coupling

**What Swift does.** `streamReply` checks `hermesRuns` and `req.clientCapabilities.supports(.chatHermesRun)`, then `HermesEscalationPolicy.decide`, then `startAgentRun`:
`HermesRunsService.start(tenantID: actorID, request(prompt, conversationID), sessionKey: memoryTenantID.uuidString)`. It emits `.hermesRun(ref)` and `.done`. If that fails it falls back to the
classic path. `memoryTenantID` is the vault ID (`ConversationController`:143).

`HermesRunsService` is 1,757 lines with 78 tests, is planned for wave 6, and has a watcher that can't safely run twice (Jobs). Options for Go-served chat:

| Option | How | Cost | Gap |
|---|---|---|---|
| **A. Bridge** (recommended) | On an escalation decision, Go calls Swift's `POST /v1/hermes/runs` with the user's JWT. Same fallback to classic | ~60 lines, removed in wave 6. Infra: an `api-go-production` to `api-production` netpol entry, since `allow-web-ingress-production` admits only Traefik. No Swift change | The stock route uses `activeHermesSessionKey` (`<hermesProfileID>:<slug>`), not the vault ID. So an escalated run gets a different Hermes session key than Swift's chat gives it **[inferred: effect on Hermes memory not read]**. Also one extra `conversationByUser` count, and `.chat` entitlement rather than `.memoryQuery` |
| B. Move runs to wave 5 | Port the 6 routes, watcher and client. Flip with chat | 1,757 lines, 78 tests. Needs a Swift flag on the service and a Swift deploy: a Swift restart would otherwise mark Go's runs lost | None |
| C. No escalation in Go | Go always answers classic | No code | A behaviour regression for every "do something" turn. Weakens the gate. Not recommended |

**Other Hermes facts.**

- After the flip, Go dials `HERMES_GATEWAY_URL` itself. `allow-hermes-production` already admits `api-go-production` and refuses `api-go-staging` (`cluster/network-policies/horus.yaml`:206-222).
- Paid managed chat streams go through the gateway, not the Haiku trial (`llm-haiku-trial.md`).
- `DefaultHermesLLMService` for self-improvement stays in Swift.
- `/v1/hermes/*` isn't in any `extraPaths`, so all of it stays on Swift, including run approval and stop.

## Streaming

**Routes.** `POST /v1/conversations/{id}/messages/stream`, `POST /v1/agents/rooms/{room}/messages`, `GET /v1/workflows/runs/{runID}/events/stream`, `POST /v1/router/parallel/executions/stream`, and 4F's
`/v1/query/stream`, which shares the writer. `/v1/hermes/runs/{id}/events` stays in Swift.

**The harness.** `stream: true`, `frames: N`, `framesTimeout` (5 s default, 25 s max), `trigger`, `headers`. `parity/scenario/sse.go` normalizes `id` and JSON `data`, compares field names with their colon and space, and
skips comments, so `: keep-alive` isn't compared.

| Can be compared | How |
|---|---|
| Status and headers before the first frame (401, 402, 403, 404, 409, 429) | Plain step |
| Frame field layout, `type` values, order of non-token frames | `frames: N` on both servers, stub answers |
| Prompt sent to the provider | The stub's digest reply: any prompt difference shows as different reply text |
| Token text | New coalesce option. Compare concatenated `token` payloads, not frame count |
| Fallback and error frames | Stub directives `[[stub:error:STATUS]]` |
| `hermesRun` frame | Stub `/v1/runs`, with a declared capability header. Needs the option 1 decision |
| Workflow event frames | Seed `workflow_events` rows and `trigger` an insert. The engine doesn't run in parity |
| Side effects | Follow-up `GET` and DB snapshot steps (message rows, `memory_index_jobs`, trace rows) |

| Can't be compared | Proven how |
|---|---|
| Token chunk boundaries | Not contract. Coalesced |
| Timing, idle timeouts (60 s stream idle, 90 s gateway) | Go tests with a clock or stalled stub |
| Heartbeat count | Comments are skipped. Go test on the interval |
| Mid-stream client disconnect and cancellation | Go test; Swift behaviour read from source |
| In-flight limit under concurrency | Go test. Harness steps run in sequence |
| Real Hermes tool loop | G3 watch only. No stub can stand in for it |
| Run watcher, approvals | Stay on Swift |
| WS frames | New `ws` step (P0), or Go tests plus the G2 manual device check |
| Live workflow execution | Engine stays in Swift, unchanged |
| Push and PostHog | Not observable in harness. Go tests with fakes |
| Parallel success | Off in production. 403 parity only |

## External providers in `parity/stubs`

`parity/stubs/llm/main.go` has `POST /v1/chat/completions`, `POST /v1/messages` (rejects temperature and prefill), `GET /v1/models`, `/health`, `/stub/stats`, and directives `[[stub:reply:TEXT]]`, `[[stub:tool:NAME:ARGS]]`,
`[[stub:error:STATUS]]`. Compose sets `HERMES_GATEWAY_URL` to `http://127.0.0.1:9` by default and refuses `r.jina.ai` via `extra_hosts`.

| Add | For | Notes |
|---|---|---|
| `GET /v1/capabilities`, `POST /v1/runs`, run events, approval, stop | 5C escalation, Hermes runs later | Shape from `HermesRunsClient` |
| `/api/sessions` and friends | 5D BYO, through a seeded `user_hermes_config` | Shape from `AgentGatewayClient` |
| `POST /v1/audio/transcriptions` | 5B | Whisper shape, fixed text, accepts all five formats |
| `POST /v1/embeddings` | Only if `deterministic` won't do | Parity already runs deterministic **[inferred]** |
| Gemini, Ollama (`/api/chat`) | If 5A's `test` and `models` are scenario-tested | |
| Per-tenant Hermes (`/v1/...` grok paths) | 5I | Seeded `hermes_tenant_containers` |
| Extra `extra_hosts` refusals | `api.anthropic.com`, `generativelanguage.googleapis.com`, `api.openai.com`, `openrouter.ai` and each sealed provider host | So a mistake fails the scenario rather than bills |
| Per-provider base URLs in compose | Every `LLM_PROVIDER_*_BASEURL` Swift and Go both read | Adapters with a hard-coded host need care |

Not stubbed: real TTS. There's no scenario with a paid provider key.

## G2 for chat, without pointing staging at production

**Staging's `HERMES_GATEWAY_URL` is sealed; the note says it dials `hermes.staging.svc.cluster.local:8642`, which doesn't exist.** Production Hermes runs tools and `allow-hermes-production` refuses `api-go-staging` on purpose, so that path stays closed.

**Proposal: a `hermes-stub` on staging.** An infra PR, which the owner merges:

1. A Deployment and Service in `horus`, from the `parity/stubs/llm` image: no tool execution, no secrets, no upstream. Its own NetworkPolicy admitting only `api-staging` and `api-go-staging`.
2. `HERMES_GATEWAY_URL` set to it explicitly: `env` on `api-staging`, `extraEnv` on `api-go-staging`. Explicit env beats `envFrom`. Don't use `HERMES_BASE_URL`; nothing reads it.
3. `LLM_PROVIDER_OPENROUTER_BASEURL` also to the stub, and the other provider keys blanked, so routing can't reach a paid provider (see "Paid calls").

Then Swift on staging is the control. Same frames, same stub, same digest. Classic path: no capability header. Escalated path: header plus stub `/v1/runs`, and with option A both servers reach the same Swift run route.
It also ends staging's dead chat, and 4F's `memory-agent` and `knowledge-reason` can use it in place of the error-path-only G2 wave 4 expected.

**G2 with the stub proves** the ingress route, SSE through Traefik (buffering, compression, timeouts), auth, entitlement, DB side effects on real Postgres, and the real volume. **It doesn't prove** the real Hermes or a real tool loop. That's a named gap for G3: watch production errors, `lv.hermes` logs and `failover` rows right after the flip.

**If the owner refuses the stub:** fall back to error-path status comparison and the Swift control on staging, plus success paths from G1, as wave 4 said. This is weaker: SSE through Traefik with real frames goes unproven until production, so chat should then flip in a low-traffic window with an immediate revert ready.

## Paid calls during staging smokes

Staging has real keys for OpenRouter and others in `api-env-staging`, and no `ANTHROPIC_API_KEY`, so the Haiku trial isn't in play there. Whisper is free.

| Call | Triggered by | How to avoid |
|---|---|---|
| OpenRouter embeddings (`EMBEDDING_PROVIDER=openrouter`) | Chat, MCP search, any memory write | `EMBEDDING_PROVIDER=deterministic` on both staging servers during smoke. `EMBEDDING_REEMBED_ENABLED`'s service refuses to run on deterministic, which also stops its background spend |
| Managed routes, Cerberus, free lane | Chat, `/v1/llm/chat`, rooms | Provider base URL to the stub, others blanked (above) |
| `FollowUpGenerator`, `JobIntentClassifier`, `ContextRouter` selector, skill runs | Chat, `/v1/jobs/detect`, `/v1/llm/chat`, `/v1/skills/{name}/run` | Same override. Otherwise leave them out of the smoke |
| Provider `test` and `models` (hard-coded hosts) | 5A | Not in smoke. Parity covers them |
| `/admin/llm/health` | 5A | Not in smoke: it probes every adapter |
| Parallel | Off | 403 only |
| Workflow LLM nodes | `POST /v1/workflows/{id}/runs` (Swift's engine runs them) | Smoke only CRUD, or a template with no `llm` node |
| Self-improvement runs | `curator/runs`, `soul/reviews`, and `SELF_IMPROVEMENT_ENABLED=true` on staging | `dryRun`, or skip the enqueue routes in smoke. Economy model is a `:free` model already |
| Cohere vision | `VISION_EMBED_PROVIDER*` | Not touched here |
| OpenAI TTS | `/v1/tts` | Never. Go has no adapter |
| xAI | Grok | No connected container on staging **[inferred]** |
| PostHog | All | Free, tagged per earlier waves. Not a cost |

Every smoke account remains a throwaway the agent registers itself (see "Staging smoke user"). **Production** probes are paid: a real chat turn after the flip is a Haiku trial call against the $20 cap, with fail-over to the old chain. Keep it to one turn, run by the owner.

## Flip order and what flips together

Each batch is one infra PR, merged by a human. Merging deploys. Each needs G1 and G2, and production flips stay behind D-wave2-early's pilot gate.

| Batch | Contents | `extraPaths` added | Why together |
|---|---|---|---|
| 5-1 | 5G, 5H, 5B | `/v1/workflows`, `/v1/workflow-templates`, `/v1/workflow-hooks`, `/v1/me/improvement`, `/v1/audio`, `/v1/transcribe` | No LLM chain on the request path, except what stays in Swift's engine. Can be separate PRs |
| 5-2 | 5D, 5I | `/v1/agents`, `/v1/grok` | Need P1; low traffic |
| 5-3 | 5A, 5C | `/v1/me/providers`, `/v1/me/preferences/llm`, `/v1/me/connections`, `/v1/router`, `/admin/llm/health`, `/v1/conversations`, `/v1/chat`, `/v1/me/chat-preferences`, `/v1/me/preferences/hybrid-execution`, `/v1/llm` | Credential and preference caches; escalation decided. Recheck parallel flags (`CERBERUS_*`) first, as `GO_MIGRATION_STATUS.md` requires |
| 5-3b | Held routes that were waiting on LLM | Remove the holds on `/v1/reminders/detect` and `/v1/me/recap/period`. Add or release `/v1/admin/community` per the community decision | Each waits on the LLM. Needs their ports (D-reminders-detect) and, for recap, moving the worker or keeping the hold |
| 5-4 | 5J, 5E, 5F, `kb-compile` | Add `/v1/ws`, `/v1/mcp`, `/v1/me/agent-connections`, `/v1/skills`, `/v1/jobs`. Remove the `api-production` holds on `/v1/devices/command` and `/v1/memory-compile` | Wave 4's commitment. Broker's callers and sockets must be on one server. iOS review sheet (`SyncAndLearnViewModel`:174,197) |

**Ingress holds and traps.**

- `/v1/conversations` flips whole. A stream can't be held separately from CRUD by parameter.
- `/v1/me/hermes/cron` gets no entry, so it stays on Swift. Same for all of `/v1/hermes`, `/v1/me/hermes/capabilities`, and `/v1/kb-compile`.
- `/v1/tts` gets no entry until decision 4. It's 404 on both servers unless configured.
- `/admin/llm/health`, not `/admin`.
- `/v1/memory` catches `/v1/memory-compile` by string prefix (`stringPrefixCatches` in `modules.yaml`), so its hold must be an explicit entry to `api-production` until 5-4. Traefik's longer rule wins **[inferred]**.
- `/v1/dashboard/home` stays held for the cron bridge until wave 6.
- After each batch with a hold, check with `curl -sI`: Swift's responses carry `Server: ObsidianClaudeBrainServer` and Go's don't (D-router-parity).
- `/v1/skills` and `/v1/jobs` have nothing nested on Swift.

**Rollback.** Remove the `extraPaths` entries. Writes are in shared Postgres and the shared volume. For 5-4, rollback drops sockets to Swift, and they reconnect. Hermes-started `hermes_runs` stay on Swift throughout, so nothing is stranded under option A.

## Everything that blocks Swift's archive beyond this wave

**Modules.** Waves 6 to 8 (`hermes`, `integrations`, `me-gateways`, `plugins`, `settings`; `admin`, `billing`, `system`; `app`, `auth`), and `skills-cron`. See `wave-6-8-plan.md`, being written separately.

**Jobs.**

- The must-never-both jobs in jobs.md, either with Swift flags or moved in Phase 12's commit. This wave adds `LegacyJobWorkflowMigrator` and `HermesRunsService` context to that list.
- `WorkflowEngine` and friends need a Go `SkillRunner` (5E delivers it) and all node kinds.
- `SelfImprovementScheduler` and its gateway LLM.
- `EmbeddingReembedService` heal pass.

**Things that live in the Swift repo and aren't routes.**

- **Schema ownership.** `migrate --adopt-fluent` and the Fluent manifest; Swift still owns migrations.
- **`Sources/AppAPI/openapi.yaml` and `LuminaVaultShared`.** The spec is the source of truth for clients and for Go's models.
- **`docker/hermes.Dockerfile`, `hermes-image.yml`.** The Hermes image chain.
- **`plugin-runner`, `docker/photon-sidecar`.** Dependabot commits show they're still live in that repo.
- **`Resources/Skills`.** 32 bundled skills, needed by P4.
- **`scripts/route-inventory.py`, `swift-freeze`, the unpushed phase-0 branch.**
- **CLI subcommands.** The image contract includes `migrate`, `bootstrap-admin`, `backfill-hermes-profiles`, `backfill-vault-space-folders`.

**Auth and containers.**

- JWT minting (wave 8, D-jwt-numeric-dates).
- Container manager and the `docker` path, including finding 6: is it even alive in the cluster?
- Audio token minting (`HermesAudioTokenService`, needs `hermes_audio_token_epoch`).

**Open items.**

- G3 clocks on every flip.
- 4F unfinished, including `/v1/memory/search` and the held `kb-compile`.
- Pending Swift citation fix on `fix/chat-source-citations`, which is to be ported or dropped.
- Open owner decisions in flip-blockers.md (symlink security, `marketplace_versions`, community hold, PostHog gap).
- `LuminaVaultApp/Services/TranscriptionService.swift`:9 calls `api.openai.com/v1/audio/transcriptions` with an API key, so a client-side paid speech call seems to exist. Not a server matter, and whether that target ships wasn't checked. Flagged for the speech rule.

## Owner decisions

1. **Escalation.** A (bridge, recommended), B (move runs in, Swift flag and deploy), or C (no escalation). Blocks batch 5-3.
2. **`hermes-stub` on staging.** Infra PR with the overrides in "G2 for chat". Recommended. Without it, chat's G2 is error paths only.
3. **Jobs.** Accept named degradations and move `CronScheduler`, `LegacyJobWorkflowMigrator` and `HermesRunsService` at Phase 12 (recommended, matching the wave 4 proposal), or add Swift flags now.
4. **`/v1/tts`.** Recommended: mounted only for a self-hosted `TTS_BASE_URL`, no OpenAI adapter. First `curl` production to confirm it's 404. If it's actually live and paid, the flip ends paid TTS, and the web's speak button needs its own decision. Either hide that button or stand up a self-hosted TTS.
5. **`skills-cron` to wave 6.** Recommended. Needs a `modules.yaml` change and leaves 7 routes on Swift until then.
6. **`/v1/ws` bundle in 5-4, with a Swift restart right after.** Recommended, as wave 4 committed. The alternative, a Valkey bridge, is more code to delete later.
7. **`ParallelExecutor`.** Recommended: leave as a hook with 403 parity while production has it off. Port it if the owner wants to turn it on before Swift goes. If it's on at flip time, the flip stops.
8. **Dependency.** `coder/websocket` under D-deps.

## Order

1. Merge 4F (or take over `streamservice.go`), with P1's stack wiring first in line.
2. Decide 1, 2, 3, 5 and 8. None blocks prep, but 2 is needed before any chat G2, and 8 before P5.
3. Run prep: P0, P2, P3, P4, P6 in parallel, plus P1 after 4F's stream service. P5 after decision 8.
4. Port in parallel: 5B, 5G (need P2, P4), then 5H, 5I (need P3, P4), 5A, 5D (need P1, P2). 5C after 4F's follow-ups. 5F and 5J after P5. 5E last, after P5 and 4F's compile.
5. Flip batches 5-1, 5-2, 5-3, 5-3b, 5-4, with each batch's G1 and G2. Start each G3 clock.

## Recipe

Unchanged from wave 4, with these changes:

- Reuse prep packages. Don't add rate limit, SSE, soul or skill helpers inside a module.
- Tests never use a real API key. Stub servers only. No `temperature`, `top_p`, `top_k` or prefill to Anthropic.
- Streaming scenarios use `frames` with a coalesced token option; list what wasn't compared in the group's PR.
- A conditionally mounted route needs a scenario with the switch on and off (`scripts/parity-switches.sh`).
- Workers named under "Jobs" stay in Swift: port the routes only.
- Shared-file edits stay minimal (`cmd/App/main.go`, `sqlc.yaml`, `modules.yaml`, `cutover-log.md`).

## Not verified

- **[inferred]** `/v1/tts` answers 404 in production. Only key names were read; `curl` settles it.
- **[inferred]** No Docker in the Swift image and none usable in the cluster, so no tenant containers. Read from `Dockerfile` in the working tree and from `apps/api/values-common.yaml`; a row count settles it.
- **[inferred]** Traefik string-prefix and longest-prefix behaviour, as in wave 4.
- Sealed values were not read: `HERMES_GATEWAY_URL` on staging, `TRANSCRIBE_PROVIDER`, `HYBRID_EXECUTION_ENABLED`, `LOCAL_EXECUTION_TOOL_BROKER_ENABLED`, `CERBERUS_*`, `TTS_*`.
- `hermes.audioProxy.baseURL` isn't set in the values files read, and whether tenant containers exist depends on finding 6, so whether `/v1/audio/*` has any caller today wasn't settled. iOS and web use `/v1/transcribe` **[inferred from web endpoint table and client grep]**.
- **[inferred]** Which of the 342 audit tests already have Go counterparts.
- **[inferred]** Test placement where marked, from file names.
- iOS `/v1/ws` reconnect behaviour after a server drop, and whether web uses `/v1/ws` (wave 4 says it doesn't).
- Whether blanking provider keys by explicit env fully disables each adapter on staging, on both servers. Prove it on staging by watching `/stub/stats` and provider dashboards before the first smoke.
- Effect of a different session key on an escalated run's Hermes memory under option A.
