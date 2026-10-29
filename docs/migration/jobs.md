# Swift background jobs: inventory for running two servers side by side

Every long-running service, worker, scheduler and loop that the Swift server
(`LuminaVaultServer`, Hummingbird 2) starts. The source is the `go-p0` worktree,
`Sources/App`. Paths are relative to `Sources/App`. `App+build.swift` line numbers
point at the `managedServices.append` / `appServices.append` call, or at the `Task {}`.

During the strangler period both servers share one Postgres, so the question for
each row is whether a second process running the same job is harmless.

Columns:

- **Service**: the Swift type (or the expression, if there is no type).
- **File:line (registration)**: where `App+build.swift` adds it to the ServiceGroup.
- **What it does**: at most 12 words.
- **Claims work how**: how the job stops two processes from doing the same unit of
  work. The citation points at the claim itself, from code that was read.
  `none` means nothing stops it.
- **Gate**: what has to be true for Swift to start it. Config keys are turned into
  env names with the D-keys rule in `decisions.md`: split on `.`, camelCase
  boundaries become `_`, uppercase. Two Swift strings break that rule:
  `App+build.swift:3061` writes `RETRIEVAL_LEAKREPORT_ENABLED` and `:741` writes
  `CORS_ALLOWEDORIGINS`. Check both against the swift-configuration encoder before
  relying on them. Two gates show up almost everywhere: `fluent.enabled`
  (`FLUENT_ENABLED`, default `true`, read at `App+build.swift:337`) and
  `lv.environment` (`LV_ENVIRONMENT`, default `dev`, read at `:733`). Neither can
  switch off a job in production, because `FLUENT_ENABLED=false` removes the
  database and `LV_ENVIRONMENT=test` changes unrelated behaviour.
- **Module dir**: the directory under `Sources/App` that holds the type.
- **Safe in two processes?**: whether Swift and Go can both run it against the same
  database.

## Boot-time services (ServiceGroup)

| Service | File:line (registration) | What it does (≤12 words) | Claims work how | Gate (config key → ENV_NAME, or condition) | Module dir | Safe in two processes? |
|---|---|---|---|---|---|---|
| `fluent` (Fluent/Postgres driver) + boot migrations | App+build.swift:235 (migrate at :85-96) | Postgres pool; runs Fluent migrations once at boot | n/a (driver). The migrator is memoised per process only (`MigrationGate.swift:1-20`). M00 alone takes `pg_advisory_xact_lock(33500)` (`Migrations/M00_EnableExtensions.swift:15`) | `fluent.enabled` → `FLUENT_ENABLED`; migrations: `fluent.autoMigrate` → `FLUENT_AUTO_MIGRATE` (default true) | (root) / Migrations | yes for the pool. Swift stays the only process that runs schema migrations |
| `PostHogHTTPClient` | App+build.swift:245 | Batches product-analytics events, flushes every 10 s on a timer | n/a (telemetry); per-process buffer (`Telemetry/PostHogHTTPClient.swift:83-104,158`) | `posthog.projectToken` → `POSTHOG_PROJECT_TOKEN` and `posthog.host` → `POSTHOG_HOST`, both non-empty | Telemetry | yes. Each process flushes its own events |
| `SOULCoreMigrationJob(...).run()` (one-shot, awaited before serving) | App+build.swift:258 | Backfills SOULCore covenant into every tenant's SOUL.md | none. Idempotent skip via `needsCoreMigration` (`Auth/SOULCoreMigrationJob.swift:15-17`) | `fluent.enabled` → `FLUENT_ENABLED` | Auth | yes. Idempotent per its doc, and Go doesn't need it while Swift still boots |
| `LapseArchiverService` | App+build.swift:277 | Lapses expired plans, archives vaults to cold storage, hard-deletes | none. Loads all users, saves the tier, moves dirs (`Billing/LapseArchiverJob.swift:130-142,165-179`). Daily at 03:00 UTC (`:232`) | `fluent.enabled` → `FLUENT_ENABLED` only | Billing | **no**: both fire at 03:00 UTC and race `moveItem` (`:178`) and the hard-delete transaction (`:145-163`) |
| `DeletedDataPurgeService` | App+build.swift:283 | `rm -r` soft-deleted tenant dirs past the retention window | none. Filesystem only, no DB (`Account/DeletedDataPurgeJob.swift:124,230-240`). Daily at 04:00 UTC (`:280-284`) | `fluent.enabled` → `FLUENT_ENABLED`; retention `account.deletion.purgeAfterDays` → `ACCOUNT_DELETION_PURGE_AFTER_DAYS` | Account | yes. The end state is idempotent, and the loser only logs ENOENT from `rmdir`/`unlink` (`:238,240`). Needs the same volume |
| OTel metrics / tracer / logs exporters | App+build.swift:299-302 | OTLP exporters for metrics, traces, logs | n/a (telemetry) | `otel.enabled` → `OTEL_ENABLED` (default false, `App+build.swift:45`) | (root) App+build.swift `OTelServices` | yes |
| `DiskSpaceMonitorService` | App+build.swift:440 | Samples vault free space every minute, logs when low | n/a (read-only monitor, `Vault/DiskSpaceGuard.swift:239-252`) | always | Vault | yes |
| `AchievementsWorker` | App+build.swift:506 | Drains the in-process achievement inbox into progress rows plus APNS | none (in-process). AsyncStream inbox (`Achievements/AchievementsWorker.swift:15-40`). Increments aren't transactional (`Achievements/AchievementsService.swift:19-26`) | `fluent.enabled` → `FLUENT_ENABLED` | Achievements | yes. Each process drains only its own inbox. The counter race already exists inside one process |
| `RetrievalTelemetryWorker` | App+build.swift:525 | Inserts per-retrieval telemetry rows from an in-process inbox | none (in-process, insert-only, `Telemetry/RetrievalTelemetryWorker.swift:59-75`) | `fluent.enabled` and `retrieval.telemetry.enabled` → `RETRIEVAL_TELEMETRY_ENABLED` (default true) | Telemetry | yes |
| `ValkeyPersistDriver` (rate-limit storage) | App+build.swift:586 | Shared Valkey rate-limit counters; readiness probe gates boot | n/a (driver). Keys shared in Valkey (`Middleware/RateLimitStorageFactory.swift:44-75`) | `rateLimit.storageKind=redis` → `RATE_LIMIT_STORAGE_KIND` plus `redis.url` → `REDIS_URL`. The default `memory` registers no service (`:48-51`) | Middleware | yes on `redis`. On `memory`, each process keeps its own counters, so the effective limit doubles |
| `MemoryHitCountUpdateQueue` | App+build.swift:595 | Coalesces `query_hit_count + 1` updates from semantic search | none (in-process queue, `Repositories/MemoryHitCountUpdateQueue.swift:21-35`). Relative increment (`Repositories/MemoryRepository.swift:386`) | `fluent.enabled` → `FLUENT_ENABLED` | Repositories | yes |
| `HermesContainerReaperService` | App+build.swift:945 | `docker rm -f` idle per-tenant Hermes containers | none. DB `last_used_at` query plus idempotent `rm -f` (`Hermes/HermesContainerManager.swift:319-328`) | `lv.secretMasterKey` → `LV_SECRET_MASTER_KEY` non-empty (SecretBox block, `App+build.swift:850-853`) and `LV_ENVIRONMENT != test` | Hermes | yes, if both processes use the same Docker host. A process without the Docker socket must not run it |
| `ProviderRegistry` | App+build.swift:1354 | Holds LLM provider adapters; `run()` only awaits shutdown | n/a (`LLM/Routing/ProviderRegistry.swift:111-113`) | always | LLM/Routing | yes |
| `SelfImprovementScheduler` | App+build.swift:1567 | Every 15 s: enqueue due weekly reviews, run one queued improvement | `FOR UPDATE SKIP LOCKED` plus `lease_until` on `self_improvement_settings` (`SelfImprovement/SelfImprovementService.swift:368-383`) and on `self_improvement_runs` (`:398-408`) | `FLUENT_ENABLED` and `LV_ENVIRONMENT != test`. `selfImprovement.enabled` → `SELF_IMPROVEMENT_ENABLED` stops only the enqueue (`:357`), not run claiming (`:320`) | SelfImprovement | yes |
| `TranscribeProviderRegistry` | App+build.swift:1729 | Holds transcription adapters; `run()` only awaits shutdown | n/a (`LLM/Transcribe/TranscribeProviderRegistry.swift:161-163`) | always | LLM/Transcribe | yes |
| `VisionEmbedProviderRegistry` | App+build.swift:1824 | Holds vision-embed adapters; `run()` only awaits shutdown | n/a (`Vision/VisionEmbedProviderRegistry.swift:82-84`) | always | Vision | yes |
| `MeTodayCache` | App+build.swift:1851 | Per-tenant `/v1/me/today` cache, invalidated from the in-process EventBus | n/a (cache, `Me/MeTodayCache.swift:31,64-87`) | always | Me | yes, but invalidation is per process. A write on the other server leaves it stale until the 300 s TTL (`App+build.swift:1850`) |
| `PeriodRecapThemesWorker` | App+build.swift:1907 | Writes the model-generated year/month recap card off the request path | Claim row: upsert on `recap_themes (user_id, period_key)` with an in-flight cutoff (`Me/PeriodRecapThemes.swift:113-126`). Inbox in-process (`:355-385`) | `recap.themesEnabled` → `RECAP_THEMES_ENABLED` (default true) | Me | yes |
| `MultimodalIngestionWorker` | App+build.swift:2681 | Claims queued ingestion items and extracts/processes them | `FOR UPDATE SKIP LOCKED` plus `lease_expires_at` on `ingestion_items` (`Ingestion/MultimodalIngestionService.swift:301-317`). Expired leases are requeued (`:766-772`) | `FLUENT_ENABLED` and `LV_ENVIRONMENT != test` | Ingestion | yes. Both need the same vault volume |
| `HermesMirrorRefreshWorker` | App+build.swift:2825 | Every 15 min syncs each linked Hermes: skills, jobs, vault import | none. Start jitter only (`Hermes/Mirror/HermesMirrorRefreshWorker.swift:23,75`); every process walks every tenant (`:115-127`) | `LV_SECRET_MASTER_KEY` set (endpoint resolver), `FLUENT_ENABLED`, and `LV_ENVIRONMENT != test` (`App+build.swift:2794,2818`) | Hermes/Mirror | **no**: duplicate upstream traffic and concurrent vault-import continuation. Whether the import is idempotent is unclear (`Hermes/Mirror/HermesMirrorService.swift`) |
| `AnalyticsMaintenanceService` | App+build.swift:2903 | Daily DELETE of analytics and telemetry rows past retention | none. Idempotent DELETEs (`Dashboard/AnalyticsMaintenanceService.swift:38-45`) | `FLUENT_ENABLED` and `LV_ENVIRONMENT != test` | Dashboard | yes |
| `CalendarSyncWorker` | App+build.swift:3013 | Every 15 min incremental Google Calendar sync per connected account | none. It calls itself single-replica (`Workers/CalendarSyncWorker.swift:16-17`) | `LV_SECRET_MASTER_KEY` set, and `oauth.googleCalendar.clientId` / `.clientSecret` / `.redirectUri` → `OAUTH_GOOGLE_CALENDAR_CLIENT_ID` / `_CLIENT_SECRET` / `_REDIRECT_URI` all non-empty (`App+build.swift:2955-2957`). Not gated on fluent/test | Workers | **no**: it double-polls, by its own doc (`:16-17`). Concurrent `syncToken` and token-refresh writes are unclear (`Calendar/CalendarSyncService`) |
| `SynthesisWorker` | App+build.swift:3057 | Weekly synthesis, daily patterns, contradiction insights (LLM) | none. Check-then-insert on `insights` (`Workers/SynthesisWorker.swift:115-120`); it calls itself single-replica (`:30-32`) | `synthesis.workerEnabled` → `SYNTHESIS_WORKER_ENABLED` (default **false**) | Workers | **no**: double LLM calls and duplicate `Insight` rows |
| `RetrievalLeakReportWorker` | App+build.swift:3069 | Weekly per-tenant retrieval leak roll-up, Sunday 05:00 UTC | Unique constraint on `retrieval_leak_reports (tenant_id, period_start, period_end)` (`Migrations/M108_CreateRetrievalTelemetry.swift:44`). The loser's insert throws and is swallowed (`Telemetry/RetrievalLeakReportWorker.swift:127-134`) | `retrieval.leakReport.enabled` → `RETRIEVAL_LEAK_REPORT_ENABLED` (default true), `FLUENT_ENABLED`, and not test | Telemetry | yes. The optional Insight is written only by the winner (`:143-155`) |
| `GraphLayoutWorker` | App+build.swift:3083 | Hourly per-tenant PCA 3D graph layout recompute | none. Idempotent overwrite (`Workers/GraphLayoutWorker.swift:18-20`) | `graphLayout.workerEnabled` → `GRAPH_LAYOUT_WORKER_ENABLED` (default true), `FLUENT_ENABLED`, and not test | Workers | yes. The work is wasted twice (`:18-19`) but the result is correct |
| `EmbeddingReembedService` | App+build.swift:3101 | Re-embeds stored vectors; heals rows saved without a vector | none. A shared cursor row in `embedding_reembed_progress` (read `Services/Embedding/EmbeddingReembedService.swift:336-338`, write `:529`), no per-row claim | `embedding.reembed.enabled` → `EMBEDDING_REEMBED_ENABLED` (default false) or `embedding.heal.enabled` → `EMBEDDING_HEAL_ENABLED` (default true, only with a non-deterministic provider), plus `FLUENT_ENABLED` and not test | Services/Embedding | **no**: both walk the same batches, which means duplicate paid embed calls. Data stays correct thanks to the text-equality guard (`:23-25`) |
| `MemoryIndexWorker` | App+build.swift:3123 | Embeds final assistant answers from the `memory_index_jobs` outbox | `FOR UPDATE SKIP LOCKED` on `memory_index_jobs` (`Workers/MemoryIndexWorker.swift:65-79`) | `FLUENT_ENABLED` and `LV_ENVIRONMENT != test`. `memory.keepGate.enabled` only toggles the judge | Workers | yes |
| `KnowledgeExtractionWorker` | App+build.swift:3144 | Extracts claims, entities and events into the knowledge graph | `FOR UPDATE SKIP LOCKED` on `knowledge_extraction_jobs` (`Workers/KnowledgeExtractionWorker.swift:153-170`). Enqueue uses `ON CONFLICT DO NOTHING` (`:149`). Ingestion finalize uses `UPDATE … WHERE state='analyzing' RETURNING` (`:320-327`) | `knowledgeGraph.workerEnabled` → `KNOWLEDGE_GRAPH_WORKER_ENABLED` (default true), `FLUENT_ENABLED`, and not test | Workers | yes |
| `HermesProfileReconcilerService` | App+build.swift:3282 | Daily 04:00 UTC: re-ensure every user's Hermes profile | none. Read-then-save (`Services/HermesProfileService.swift:52-74,156-169`). `UNIQUE(tenant_id)` (`Migrations/M08_CreateHermesProfile.swift:14`) stops only duplicate rows | `FLUENT_ENABLED` and `LV_ENVIRONMENT != test` | Admin | unclear, treat as **no**. Both processes call `gateway.provisionProfile` for the same tenant at 04:00 (`Services/HermesProfileService.swift:62`) |
| `HermesRunsService` | App+build.swift:3410 | On boot re-attaches watchers to active Hermes runs; owns them | none. In-memory watcher map (`Hermes/Runs/HermesRunsService.swift:67,117-131`). Event append races on `last_seq` (`Hermes/Runs/HermesRunStore.swift:79-86`) against `UNIQUE (run_id, seq)` (`Migrations/M118_CreateHermesRunEvents.swift:18`) | `LV_SECRET_MASTER_KEY` set (`App+build.swift:2232`), `FLUENT_ENABLED`, and not test | Hermes/Runs | **no**: both processes re-attach the same active runs. Duplicate watchers race `seq` and both push approvals |
| `Task { await skillRunner.startEventSubscriptions() }` (unmanaged, not in the ServiceGroup) | App+build.swift:3612 | Subscribes to every EventBus type; logs only (dispatch is TODO) | none (in-process; `Skills/SkillRunner.swift:115-135`) | always | Skills | yes (log only) |
| `CronScheduler` | App+build.swift:3624 | Per-minute dispatch of tenants' cron-scheduled skills | none. Reads `last_run_at`, dispatches, then saves (`Skills/CronScheduler.swift:97-109,290-298`). It calls itself single-replica (`:14-16`) | `FLUENT_ENABLED` and `LV_ENVIRONMENT != test` only | Skills | **no**: double-fire |
| `ReminderScheduler` | App+build.swift:3634 | Per-minute APNS reminder firing | none. Selects `firedAt IS NULL`, pushes, then saves (`Reminders/ReminderScheduler.swift:61-65,97-115`). It calls itself single-replica (`:12-13`) | `FLUENT_ENABLED` and `LV_ENVIRONMENT != test` only | Reminders | **no**: duplicate pushes |
| `LearnedDigestScheduler` | App+build.swift:3642 | Sunday 18:00 local "what I learned this week" push | Claim row: `INSERT … ON CONFLICT (tenant_id, week_start) DO NOTHING RETURNING` on `learned_digest_log` (`Services/LearnedDigestScheduler.swift:185-190`), plus the `proactive_push_log` budget (`Services/ProactivePushBudget.swift:31-33`) | `learnedDigest.enabled` → `LEARNED_DIGEST_ENABLED` (default true), `FLUENT_ENABLED`, and not test | Services | yes |
| `MeetingBriefScheduler` | App+build.swift:3664 | Opt-in pre-meeting brief push, once per calendar event | Claim row: `INSERT … ON CONFLICT (tenant_id, event_id) DO NOTHING RETURNING` on `meeting_briefs` (`Calendar/MeetingBriefScheduler.swift:217-224`) | `meetingBrief.enabled` → `MEETING_BRIEF_ENABLED` (default true), `FLUENT_ENABLED`, and not test | Calendar | yes |
| `WorkflowEngine` | App+build.swift:3770 | Executes workflow runs (N workers) under a heartbeated lease | `FOR UPDATE SKIP LOCKED` plus `lease_owner` / `lease_expires_at` on `workflow_runs` (`Workflows/WorkflowEngine.swift:59-77`). The heartbeat is guarded by `lease_owner` (`:208-213`) | `cerberus.studio.enabled` → `CERBERUS_STUDIO_ENABLED` (default true; it also unmounts `/v1/workflows`, `App+build.swift:3725-3726`), `FLUENT_ENABLED`, and not test. Workers: `cerberus.studio.workerCount` → `CERBERUS_STUDIO_WORKER_COUNT` | Workflows | yes |
| `WorkflowScheduler` | App+build.swift:3784 | Each minute enqueues due cron or one-shot workflow runs | Unique constraint on `workflow_runs (workflow_id, dedupe_key)` (`Migrations/M93_HardenWorkflowAutomation.swift:12-13`). Pre-check and a swallowed loser (`Workflows/WorkflowScheduler.swift:57-61,78-81`) | same as WorkflowEngine | Workflows | yes |
| `WorkflowMaintenanceService` | App+build.swift:3789 | Hourly: expire approvals, prune events and snapshots older than 30 days | none. Idempotent updates and deletes (`Workflows/WorkflowMaintenanceService.swift:28-48`) | same as WorkflowEngine | Workflows | yes |
| `LegacyJobWorkflowMigrator` | App+build.swift:3794 | Every 5 min converts legacy scheduled skill Jobs into workflows | none. Check-then-create (`Workflows/LegacyJobWorkflowMigrator.swift:46-48,65`); no unique index on `workflows.legacy_skill_name` (`Migrations/M92_CreateWorkflows.swift:12`) | same as WorkflowEngine | Workflows | **no**: a concurrent pass can create duplicate workflows, which then double-schedule |
| `DailyPostWorker` | App+build.swift:3927 | Community AI Daily Post, once a day at the configured UTC hour | Claim row: `INSERT … ON CONFLICT (day) DO NOTHING` on `community_daily_posts`, plus stale/failed reclaim (`Community/DailyPostWorker.swift:275-296`) | `community.enabled` → `COMMUNITY_ENABLED` (default **false**), `FLUENT_ENABLED`, and not test. Posting: `community.dailyPost.enabled` → `COMMUNITY_DAILY_POST_ENABLED` (`Community/DailyPostWorker.swift:123`) | Community | yes |

These 40 rows cover every `appServices.append` and `managedServices.append` in
`App+build.swift`, plus the boot one-shot and the unmanaged EventBus `Task`. Swift's
`docs/jobs.md` (row 1) says an `AutoKBCompileJob` shipped. No such type exists in
`Sources/App`.

### Background tasks started by requests (not at boot)

These are tasks that requests spawn in memory, outside the ServiceGroup. They are
not boot jobs, but they disappear with the process, and their progress usually
streams out through an in-process channel (see `coupling.md`).

| Task | File:line | What it does | Survives the process? |
|---|---|---|---|
| Link enrichment `Task.detached` | Capture/LinkCaptureService.swift:152 | oEmbed/OG enrich and rewrite a captured link note | no. Lost if the process stops mid-task |
| Gateway-apply job task | HermesGateways/HermesGatewayApplyService.swift:81 | Write the tenant `.env`, restart the container, health-check | Row state persists in `hermes_gateway_apply_jobs`; the task does not |
| Hermes self-update task | System/HermesUpdateService.swift:81,109 | Blue-green central Hermes update, then reprovision tenants | Row state persists in `hermes_update_jobs`; the task does not |
| WhatsApp pairing stdout pump | HermesGateways/WhatsAppPairingService.swift:69 | Stream the QR and status from the pairing CLI | no. Ephemeral by design (`:3-6`) |
| Photon device-code poller | HermesGateways/PhotonProvisioningService.swift:114 | Poll Photon approval, then provision the iMessage line | no (session map at `:67-69`) |
| Hermes run watcher | Hermes/Runs/HermesRunsService.swift:295-299 | Poll one Hermes run, persist events, push approvals | Re-attached at boot if younger than 300 s (`:117-131`) |

## Must never run in both servers

The rows marked **no** above. Exactly one process may run each of these at a time.
Turn it off in one server before turning it on in the other.

1. `LapseArchiverService` (Billing). No claim; filesystem move and hard-delete at 03:00 UTC.
2. `HermesMirrorRefreshWorker` (Hermes/Mirror). No claim, only jitter.
3. `CalendarSyncWorker` (Workers). Calls itself single-replica.
4. `SynthesisWorker` (Workers). Check-then-insert; double LLM spend.
5. `EmbeddingReembedService` (Services/Embedding). Shared cursor; double paid embed calls.
6. `HermesProfileReconcilerService` (Admin). Unclear; treated as no.
7. `HermesRunsService` (Hermes/Runs). Boot re-attach in both processes duplicates watchers.
8. `CronScheduler` (Skills). Double-fire.
9. `ReminderScheduler` (Reminders). Duplicate pushes.
10. `LegacyJobWorkflowMigrator` (Workflows). Can create duplicate workflows.

One more is conditional: `HermesContainerReaperService` (Hermes) is harmless twice on
the same Docker host. It must not run in a process that cannot see that Docker host.

## Jobs with no config flag to switch them off

`FLUENT_ENABLED=false` and `LV_ENVIRONMENT=test` don't count, for the reasons in the
header. Each of these needs a dedicated flag in Swift before its module moves to Go,
so that exactly one server runs it.

From the must-never list (add these flags first):

- `LapseArchiverService`
- `HermesMirrorRefreshWorker`
- `CalendarSyncWorker`. The only way to stop it today is to unset the Google
  Calendar OAuth credentials or `LV_SECRET_MASTER_KEY`, and that also breaks the
  calendar and BYO routes.
- `HermesProfileReconcilerService`
- `HermesRunsService`. Its gate is `LV_SECRET_MASTER_KEY`, which also mounts the
  `/v1/hermes/runs` routes.
- `CronScheduler`
- `ReminderScheduler`
- `LegacyJobWorkflowMigrator`. `CERBERUS_STUDIO_ENABLED=false` stops it, but also
  unmounts `/v1/workflows`, so there is no job-only switch.

Safe in two processes, but still without a switch (needed if the module is to be run
by Go *instead of* Swift):

- `DeletedDataPurgeService`
- `HermesContainerReaperService`. Its only gates are `LV_SECRET_MASTER_KEY` and
  `LV_ENVIRONMENT`.
- `SelfImprovementScheduler`. `SELF_IMPROVEMENT_ENABLED` is only partial: queued
  runs are still claimed.
- `MultimodalIngestionWorker`
- `AnalyticsMaintenanceService`
- `MemoryIndexWorker`
- `AchievementsWorker` (in-process inbox; moves with its producers)
- `MemoryHitCountUpdateQueue` (in-process; moves with `MemoryRepository`)
- `WorkflowEngine`, `WorkflowScheduler`, `WorkflowMaintenanceService`. Only
  `CERBERUS_STUDIO_ENABLED`, which also removes the routes.
- `SOULCoreMigrationJob` (boot one-shot)
- SkillRunner EventBus subscriptions (log only)

These don't need a flag: `DiskSpaceMonitorService`, the three provider registries,
`MeTodayCache`, the OTel exporters and the Fluent driver. None of them touches
shared work.
