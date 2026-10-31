# Wave 3 plan

Written 2026-10-31 from the roadmap's per-module recipe (the same one wave 2
used: `wave-2-plan.md`, "Per-module tasks"). Wave 3 is ported to `parity`
ahead of the pilot (decisions.md D-wave3-early), on branches based on
`go-migration/wave-3-prep`, which is stacked on `go-migration/wave-2`.

## Modules (95 routes)

Me's gateway and provider routes moved to waves 6 and 5 (decisions.md D-me-split).

| Group (one branch each) | Modules | Routes | Swift tests | Flip blockers (coupling.md / jobs.md) |
|---|---|---|---|---|
| `wave-3-teams` | teams | 19 | none (`Tests/AppTests` has no Teams dir; read the controller) | Vault activity SSE (`GET /v1/vaults/{vaultID}/activity/stream`) carries in-process publishes only. Once both servers write, each stream misses the other's events, so Spaces, Kanban and Teams need a cross-process bridge (Postgres `LISTEN/NOTIFY` on `vault_activity_events`, added to Swift too) before any of them flips. |
| `wave-3-me` | me | 21 | `Tests/AppTests/Me` | `MeTodayCache` is invalidated by EventBus `memory_upserted`/`achievement_unlocked` (in-process). Across processes this falls back to the 300 s TTL. `PeriodRecapThemesWorker` (LLM) stays in Swift; the route only reads and claims `recap_themes`. |
| `wave-3-dashboard` | dashboard | 16 | `Tests/AppTests/Dashboard` | `AnalyticsMaintenanceService` and `SynthesisWorker` stay in Swift (jobs.md). |
| `wave-3-social` | community, marketplace | 17 + 19 | `Tests/AppTests/Community` | `DailyPostWorker` stays in Swift (claim row). |
| `wave-3-calendar` | calendar, mail | 8 + 3 | `Tests/AppTests/Calendar`, `Mail` | `CalendarOAuthSessionStore` is in memory: `connect` and the unauthenticated `oauth/callback` must hit the same process, so both flip together and `/v1/calendar/oauth` stays on whichever server ran `connect` (or the store moves to Postgres). `CalendarSyncWorker` and `MeetingBriefScheduler` stay in Swift. Google is absent in parity: match the disabled and error paths. |
| `wave-3-account` | account, onboarding, health | 1 + 2 + 3 | `Tests/AppTests/Account`, `Onboarding`, `Health` | `DELETE /v1/account` touches every module's tables and the vault folder. Port the exact delete set from Swift and snapshot every table it changes. `DeletedDataPurgeService` stays in Swift. EventBus `health_event_synced` is log-only. |

## Recipe

Unchanged from wave 2 (`wave-2-plan.md`). Shared code from wave 2: `auth.Identify`/`auth.Require`,
`internal/vaultaccess`, `internal/vaultactivity`, `internal/vaultpaths`,
`internal/ratelimit`, `internal/swiftstr`.

Steps 6–7 (staging, production) wait for the pilot and for the flip blockers above.
