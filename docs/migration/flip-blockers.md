# Flip blockers

Written 2026-11-04. Checked against Go `main` (`c17c3f6`), Swift at `fdbed9b0` (production's image),
the web and iOS clients, and `LuminaVaultInfra` `origin/main` (`4049f74`). The cluster was not touched,
so sealed values and live pods are unverified, and each such point says so. Marked **[inferred]** where
not verified in code.

## Bottom line

**Most recorded blockers don't hold today.** All 14 modules below can reach production with no Swift
change and no Swift deploy. Eight need no behaviour change at all. Five need one or two ingress entries that
keep a specific route on Swift. One needs the owner to accept a visible delay. The real work left is small:
smoke tags for G2, a manifest field, and optionally one query.

| Can flip | Modules | Named gap |
|---|---|---|
| Now, no gap | spaces, kanban, teams, account, onboarding, marketplace | None |
| Now, no gap in production | calendar, mail | PostHog events only. Staging differs on embeddings (below) |
| Now, route held on Swift | apple, devices, reminders, dashboard | PostHog events (apple) only. Held routes behave as today |
| Now, route held, small gap | me | Today can lag a Swift write by up to 300 s |
| Only if the owner accepts | community | Posts stay `pending` 5 to 20 min instead of publishing at once |

"Can flip" means no technical blocker. G2 and the pilot gate below still apply to all of them.

## What still gates every flip

These are gates, not blockers. Nothing here proposes skipping them.

1. **Pilot gate.** D-wave2-early and D-wave3-early say no wave 2 or 3 module passes `parity` until `projects`
   is `done` and `pilot-retro.md` exists. The 7-day mark is 2026-11-07 12:15Z. The `go-migration/flip-batch-1`
   branch already sets five modules to `staging`, which those decisions forbid unless the owner has waived the
   gate. No waiver is recorded. **The owner should confirm and record it, or hold batch 1 until then.**
2. **G2 needs a smoke scenario per module.** `smoke: true` exists on `main` only for achievements, apple, devices,
   kanban, me, news, projects, reminders, sessions and spaces. Batch 1 adds profiles and health. Missing:
   teams, account, onboarding, marketplace, calendar, mail, community, dashboard. Tagging them is the main work below.
3. **G3 clocks run independently.** The earliest archive date is the last flip plus its watch, so early flips cost
   little. The critical path is still waves 5 to 8, not this list.

## Order

Ranked by modules unlocked per unit of work. None needs a Swift change.

| # | Fix | Unlocks | Work | Blocker real today? |
|---|---|---|---|---|
| 1 | Flip spaces, kanban and teams in one infra PR | 3 | Smoke tag for teams. Check Resend on staging | **No.** Moving all publishers together removes it |
| 2 | Flip account, onboarding, marketplace | 3 | Smoke tags | **No.** All three stale |
| 3 | Flip calendar and mail together | 2 | Smoke tags. One owner note on embeddings | **No** in production. Yes on staging |
| 4 | Flip apple, devices, reminders, holding two prefixes on Swift | 3 | 2 ingress entries, `heldPaths`, owner accepts D-reminders-detect | **Yes**, both, but ingress avoids them |
| 5 | Flip me, holding `/v1/me/recap/period` on Swift | 1 | 1 ingress entry. Optional Go query | **Yes**, partly |
| 6 | Merge dashboard, flip it, holding `/v1/dashboard/home` on Swift | 1 | Merge, 1 ingress entry, smoke tag | **Yes**, one route |
| 7 | Community: flip with `/v1/admin/community` held, or wait for `internal/llm` | 1 | Owner decision | **Yes** |

Fixes that later remove the held routes and gaps (Go only unless stated; no module waits on them):

| Fix | Removes | Where |
|---|---|---|
| `internal/embedding` | The staging difference; lets production turn on real embeddings | wave-4-plan.md P1 |
| `internal/posthog` | Lost server-side analytics events | wave-4-plan.md P4 |
| `internal/llm` | Holds on `/v1/reminders/detect`, `/v1/me/recap/period`, `/v1/admin/community`; moderation delay | wave-4-plan.md P6 |
| `/v1/ws` plus broker, with MCP and Skills | Hold on `/v1/devices/command` | End of wave 5 |
| Cron bridge | Hold on `/v1/dashboard/home` | Wave 6: ported as `skills-cron` (`internal/modules/skillscron`), which also fills the preview |

## Module by module

### spaces, kanban, teams: vault activity SSE

- **Recorded.** The stream is in-process, so Spaces, Kanban and Teams need a `pg_notify` bridge in both servers.
- **Real today?** No, provided all three flip at once. Swift has exactly three writers of activity rows:
  `SpacesController` and `KanbanController` through the one `VaultActivityRecorder` (`App+build.swift`:2557, 3939),
  and `TeamController` (:514-522). The stream route is in Teams. All of them move, so publisher and subscriber
  share the Go process (one replica, `replicaCount: 1`).
- **Even if one flipped alone, the gap is small.** The only client of the stream is the web page
  `routes/(app)/teams/activity/+page.svelte`, and it also reloads `/activity` every 5 s. iOS calls only the plain
  list (`TeamEndpoints.swift`:128).
- **Smallest fix.** One infra PR with all their prefixes. No code. Drop the `pg_notify` proposal.
- **Swift change.** None. **Unlocks.** 3 modules.
- **Before G2.** Add a teams smoke scenario. The cutover log also asks for the Resend path to be checked on staging,
  since parity can't reach it.

### account: volume mount

- **Recorded.** `apps/api-go` must mount `/app/data`, or deleted users' folders stay under live names and are
  never purged.
- **Real today?** No, stale. `argocd/apps/api-go-production.yaml` layers `apps/api/values-common.yaml`
  (`persistence.enabled: true`, `mountPath: /app/data`) and `apps/api/values-production.yaml` (`VAULT_ROOT_PATH`,
  `HERMES_DATA_ROOT`) under `apps/api-go/values-production.yaml` (`existingClaim: api-production-data`).
  `charts/app/templates/deployment.yaml`:121 uses `existingClaim` as the claim name. `DeletedDataPurgeService` stays
  in Swift on the same volume.
- **Not checked live.** The mount in the running pod. `kubectl -n horus get pod -l app.kubernetes.io/name=api-go-production -o jsonpath='{.items[0].spec.volumes}'`
  settles it (the image is distroless, so no `exec`).
- **Smallest fix.** None. Add a smoke scenario (a throwaway user, since it deletes the account).
- **Swift change.** None. **Unlocks.** 1 module.

### onboarding: latches

- **Recorded.** `OnboardingLatches` not ported.
- **Real today?** No, not for this module. The latches are Swift's vault, capture, compile and chat paths writing
  `onboarding_state`, and Go reads that row on every `GET`. No cache sits between them. While those paths are on
  Swift, flags keep latching and Go shows them. The requirement belongs to whoever moves those paths
  (wave-4-plan.md P4), not to `/v1/onboarding`.
- **Smallest fix.** None. Add a smoke scenario.
- **Swift change.** None. **Unlocks.** 1 module.

### marketplace: `PLUGIN_*`, artifact root, `marketplace_versions`

- **`PLUGIN_*` reaches Go?** Yes, stale. `PLUGIN_ARTIFACT_ROOT`, `PLUGIN_ARTIFACT_SIGNING_KEY`, `PLUGIN_RUNNER_TOKEN`
  and `PLUGIN_RUNNER_URL` are keys in `secrets/horus/api-env-production.yaml`, and `api-go-production` inherits
  `envFromSecret: api-env-production`.
- **Same volume?** **[inferred]** Yes. The root's value is sealed, but the chart documents plugin artifacts as living on
  the app-data volume. Production's Go image `e948c69` contains `marketplace.CheckConfig`, so a bad signing key would
  stop the pod **[inferred: pod state not checked]**.
- **`marketplace_versions`.** Doesn't block. Go reproduces Swift's failing insert behind `versionInserts`, false in
  production, so the same counts give the same answers. The row count only matters to the later decision to fix it.
- **Runner reachability.** `cluster/network-policies/horus.yaml` has no runner policy naming `api-production`, so
  nothing there needs a Go entry. Where `PLUGIN_RUNNER_URL` points is sealed. **Worth one check by a human.**
- **Smallest fix.** None. Add a smoke scenario.
- **Swift change.** None. **Unlocks.** 1 module.

### calendar, mail: OAuth store, brief embedding

- **OAuth store.** Not a blocker with one replica. `connect` and `callback` both move, and `flipsWith` already covers
  mail. Production's `api-go` has `replicaCount: 1` (chart default, not overridden). A consent in flight at the
  flip gets `session_not_found` and the user retries: at most 600 s of exposure, once.
- **Brief embedding.** Not real in production. `EMBEDDING_PROVIDER` is in neither
  `apps/api/values-production.yaml` nor the key list of `api-env-production`, and only staging sets it (infra `530d739`,
  `values-staging.yaml`:11). So production runs Swift's default `deterministic`, which Go implements, and Swift's BYOK
  wrapper falls through to it (`App+build.swift`: `.deterministic` builds no keyed service). Swift's own
  `docs/production-env-wiring.md` §3 says the same.
- **Staging does differ.** `api-go-staging` inherits `openrouter`, so a brief with no stored match answers 500 there. If
  `internal/embedding` lands first, smoke the brief. If not, smoke the other routes and record that the brief's coverage
  comes from parity on the provider production actually runs.
- **Must not happen.** Setting a real `EMBEDDING_PROVIDER` in production while Go lacks the chain would turn those
  routes into 500s. **Land P1 first.**
- **Named gap.** `calendar_connected`, `calendar_event_created`, `apple_calendar_synced`, `gmail_connect_started` stop
  (PostHog, below).
- **Not checked live.** The sealed key list is authoritative for names, but confirm with
  `kubectl -n horus get secret api-env-production -o json | jq '.data | keys' | grep EMBEDDING`.
- **Swift change.** None. **Unlocks.** 2 modules.

### apple, devices, reminders: broker, detect, photos

- **`DeviceCommandBroker`: real.** Continuations live in Swift's memory, and MCP and Skills call from Swift. If the result
  route reaches Go, device RPCs time out after 12 s and queued writes run twice.
  **Smallest fix:** hold `/v1/devices/command` on Swift. `/v1/ws` already stays there. `/v1/devices/commands/pending` is
  DB-only and goes to Go. It moves with MCP and Skills at the end of wave 5.
- **`/v1/reminders/detect`: real.** Go has `reminders.UnavailableTransport` and would silently answer
  `{"isReminder":false}`. **Smallest fix:** hold `/v1/reminders/detect` on Swift. The proposal in D-reminders-detect
  needs the owner's acceptance, and then `reminders` can go to `parity` with the route listed under `heldPaths`.
- **`/v1/photos/index`: stale for production.** It uses the text embedder (`PhotoIndexController`:27,62), and production is
  on `deterministic`, as above. No hold needed. Same staging caveat as calendar: 500 there, and it isn't in the smoke
  scenarios (`apple/consent.yaml` is).
- **Named gap.** `apple_reminders_synced`, `apple_data_consent_updated`, `photos_indexed` stop (PostHog, below).
- **Swift change.** None. **Unlocks.** 3 modules. Needs a small `modules.yaml` and `internal/manifest` change for
  `heldPaths`.

### me: `MeTodayCache`, `PeriodRecapThemesWorker`

- **Themes worker: real.** `GET /v1/me/recap/period` on Go claims the row, and nothing generates it. The card shows
  `pending`, then the fallback sentence; it's a degraded card, not an error. **Smallest fix:** hold
  `/v1/me/recap/period` on Swift, where the claim and the worker still share a process. The longer entry beats
  `/v1/me/recap`. No gap. It moves when `internal/llm` can run the worker.
- **`MeTodayCache`: real, small.** Swift's EventBus can't reach Go's cache. A memory upserted or an achievement unlocked
  on Swift shows on `/v1/me/today` after at most 300 s (`today.go`:22), instead of at once.
  **Accept it,** or use this Go-only fix: on a cache hit, compare
  `max(memories.updated_at)` and `max(achievement_progress.unlocked_at)` for the tenant against the values stored in the
  entry, and treat a newer value as a miss. One query per request, roughly 20 lines **[inferred: index use not checked]**.
  It also covers every Go publisher, so nothing has to call `InvalidateToday`.
- **Named gap, if accepted.** Today lags a Swift write by up to 300 s. The ETag means the client sees nothing new until then.
- **Swift change.** None. **Unlocks.** 1 module.

### dashboard: specific routes stay on Swift

- **Real today?** One route. `cronJobs` on `GET /v1/dashboard/home` comes from `CronBridgeService` (`docker exec` or a BYO
  dashboard). Go answered `[]` until `skills-cron` was ported; it now fills the preview from the same bridge, so the hold
  can go when `skills-cron` flips with `dashboard`. `AnalyticsMaintenanceService` and `SynthesisWorker` staying in Swift
  block nothing (`SYNTHESIS_WORKER_ENABLED` isn't set in production).
- **Smallest fix.** Merge `go-migration/wave-3-dashboard`, whose code isn't on `main`, and hold `/v1/dashboard/home` on
  Swift. Alternatively flip it too, and accept an empty cron preview for tenants with cron jobs. Holding has no gap, so hold.
- **Swift change.** None. **Unlocks.** 1 module.

### community: specific routes stay on Swift

- **Real today?** Yes. `COMMUNITY_ENABLED` and `COMMUNITY_POSTING_ENABLED` are on in production. Swift moderates a post on
  arrival using the LLM; Go keeps it `pending` for Swift's sweep (15-minute tick, 5-minute minimum age).
  `POST /v1/admin/community/daily/run` on Go can only record failures, and three of them cost that day's post.
- **Smallest fix.** Hold `/v1/admin/community` on Swift, which removes the daily-run risk.
- **Named gap, if flipped.** New posts appear after roughly 5 to 20 minutes instead of at once. `POST /v1/community/posts`
  shares a path with the feed, so ingress can't hold only that write.
- **Recommendation.** Wait for `internal/llm` unless it slips past about a week. Flipping earlier starts a G3 clock but
  doesn't move the archive date, because Swift has to outlive wave 5 regardless. Owner's call.
- **Swift change.** None. **Unlocks.** 1 module.

### me-gateways, integrations: in-process sessions (6D, 6E)

- **In-memory state: real, by design.** The gateway-apply SSE subscribers, WhatsApp pairing sessions,
  Photon setup sessions, xAI and Nous OAuth sessions and the parked `hermes auth` processes live in the
  serving process. Start and complete (or stream) must reach the same server, so each module flips as
  one, with no `heldPaths`. Apply jobs themselves persist in `hermes_gateway_apply_jobs`.
- **Containers.** Go ports the container lifecycle in full (D-6de-waivers). Production has no docker in
  the pod, so both servers fail at the first docker call. `ContainerReaperService` stays in Swift until
  the wave 6 jobs move.
- **Photon sidecar.** Ported client; production sets no `PHOTON_SIDECAR_URL`, so inbound answers 503 in
  both. `docker/photon-sidecar` is retired with the Swift archive.
- **Swift change.** None. **Unlocks.** 2 modules.

### admin: Isis reads `/internal/metrics` from the Swift Service (B5)

- **Real today?** Yes. Isis calls `http://api-production.horus.svc.cluster.local:8080/internal/metrics` directly
  (infra `apps/isis/values.yaml:42`, `LUMINAVAULT_METRICS_URL`), not through the Ingress, so `extraPaths` can't move it.
  `allow-metrics-from-isis-api` (`cluster/network-policies/horus.yaml:736`) admits Isis only to pods labelled
  `api-production`. At Swift replicas 0 the LuminaVault card goes "unavailable".
- **Fix, in the flip's infra PR.** Repoint `apps/isis/values.yaml:42` at the Go Service
  (`api-go-production.horus.svc.cluster.local:8080`), and add Go's pod label (`api-go-production`) to
  `allow-metrics-from-isis-api`. Go reads `METRICS_SECRET` (wave 7B) from the same sealed env; check it's set there, or
  Go answers 404 "metrics disabled".
- **Also.** `/v1/admin/users` (tier-override) sits under the admin prefix but is served by Go's billing module (7A).
  Flipping the `admin` prefixes moves it too, so flip `admin` with or after `billing`.
- **Swift change.** None. **Unlocks.** 1 module.

## Gap found that wasn't recorded: server-side PostHog events

Go has no PostHog client, and `POSTHOG_PROJECT_TOKEN` is in production's sealed env. Each flip silently ends that module's
server-side events: `health_events_synced` (health, in batch 1), the calendar, mail and apple events above, and from wave 4
`vault_file_uploaded`, `vault_file_deleted`, `link_captured`. `user_registered` and `user_logged_in` go at wave 8. The cutover
log mentions some of this per module, but nowhere as a whole. The fix is small (`internal/posthog`, wave-4-plan.md P4) and
needs no Swift change. **Owner's call:** flip without them, or wait for that package. If any funnel or strangers count reads
these events, wait; I couldn't check the PostHog dashboards from here.

## Ingress entries

Each batch is one infra PR, which a human merges. The chart takes `{path, service, pathType}`, and Traefik gives the longer
rule priority **[inferred: default rule-length priority, not probed]**.

```yaml
ingress:
  extraPaths:
    # Batch: spaces, kanban, teams
    - {path: /v1/spaces, service: api-go-production}
    - {path: /v1/boards, service: api-go-production}
    - {path: /v1/cards, service: api-go-production}
    - {path: /v1/teams, service: api-go-production}
    - {path: /v1/team-invitations, service: api-go-production}
    - {path: /v1/vaults, service: api-go-production}
    # Batch: account, onboarding, marketplace
    - {path: /v1/account, service: api-go-production}
    - {path: /v1/onboarding, service: api-go-production}
    - {path: /v1/marketplace, service: api-go-production}
    # Batch: calendar, mail
    - {path: /v1/calendar, service: api-go-production}
    - {path: /v1/mail, service: api-go-production}
    # Batch: apple, devices, reminders
    - {path: /v1/apple, service: api-go-production}
    - {path: /v1/photos, service: api-go-production}
    - {path: /v1/devices, service: api-go-production}
    - {path: /v1/devices/commands/pending, service: api-go-production}
    - {path: /v1/devices/command, service: api-production}       # held: broker
    - {path: /v1/reminders, service: api-go-production}
    - {path: /v1/reminders/detect, service: api-production}      # held: LLM
    # Batch: me
    - {path: /v1/me/today, service: api-go-production}
    - {path: /v1/me/brief, service: api-go-production}
    - {path: /v1/me/recap, service: api-go-production}
    - {path: /v1/me/recap/period, service: api-production}       # held: themes worker
    - {path: /v1/me/resurface, service: api-go-production}
    - {path: /v1/me/learned, service: api-go-production}
    - {path: /v1/me/commitments, service: api-go-production}
    - {path: /v1/me/location, service: api-go-production}
    - {path: /v1/me/apns-categories, service: api-go-production}
    # Batch: dashboard
    - {path: /v1/analytics, service: api-go-production}
    - {path: /v1/insights, service: api-go-production}
    - {path: /v1/tasks, service: api-go-production}
    - {path: /v1/dashboard, service: api-go-production}
    - {path: /v1/dashboard/home, service: api-production}        # held: cron bridge
```

Check after the first batch with a held route: `curl -sI https://api.luminavault.fyi/v1/reminders/detect`. Swift's responses
carry `Server: ObsidianClaudeBrainServer` and Go's don't (D-router-parity). Rollback is always reverting the PR.

## Recorded blockers that are stale

- Account's volume mount: resolved by `existingClaim` and inherited persistence.
- `pg_notify` for vault activity: unnecessary if the three modules flip together.
- `PLUGIN_*` reaching Go: inherited through the sealed env.
- `/v1/photos/index` and the calendar brief in production: the provider is `deterministic` there.
- Onboarding latches as a flip blocker for `/v1/onboarding`: a requirement on wave 4 and 5 writers instead.
- The `marketplace_versions` count: informs a later fix, not the flip.

## Decisions for the owner

1. Record a waiver of the pilot gate for batch 1 and later flips, or hold them until 2026-11-07 12:15Z plus the retro.
2. Accept D-reminders-detect's hold, so that `reminders` can go to `parity` and the apple group can flip.
3. Accept the `MeTodayCache` 300 s gap, or take the Go query.
4. Community: flip with the moderation delay, or wait for `internal/llm` (recommended).
5. Flip modules with lost PostHog events now, or wait for `internal/posthog`.
6. When production gets real embeddings. Not before `internal/embedding` is in the production Go image.
