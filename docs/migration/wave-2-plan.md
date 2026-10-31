# Wave 2 plan

Written 2026-10-30 from the roadmap's per-module recipe
(`LuminaVaultServer/docs/superpowers/plans/2026-10-29-go-echo-migration-roadmap.md`).
Wave 2 is ported to `parity` ahead of the pilot's retro (decisions.md D-wave2-early).

## Modules (43 routes)

Settings moved to wave 6 (decisions.md D-settings-wave6).

| Group (one branch each) | Modules | Routes | Swift tests | Flip blockers (coupling.md / jobs.md) |
|---|---|---|---|---|
| `wave-2-apple` | apple, devices, reminders (`flipsWith`) | 6 + 2 + 5 | `Tests/AppTests/Apple`, `Devices` | DeviceCommandBroker is delivered over ConnectionManager (WebSockets, wave 7) and called by MCP/Skills (wave 5): needs a Valkey bridge in Swift before the flip. `ReminderScheduler` (single replica) stays in Swift. |
| `wave-2-kanban` | kanban | 15 | `Tests/AppTests/Kanban` | Vault activity SSE → Teams (wave 3). `promote` writes vault notes (VaultAccessService). |
| `wave-2-spaces` | spaces | 5 | `Tests/AppTests/Spaces` | Vault activity SSE → Teams (wave 3); space folders on the PVC (VaultPathService). |
| `wave-2-profiles` | profiles, sessions | 6 + 1 | `Tests/AppTests/Profiles` | none known |
| `wave-2-reads` | news, achievements | 1 + 2 | `Tests/AppTests/Achievements` | `AchievementsWorker` stays in Swift (its inbox producers are waves 4–5); routes only read. NewsTickerCache is per-process. |

## Per-module tasks (recipe, each ends with a commit)

1. Scenarios in `parity/scenarios/<m>/` covering every route with a success and
   an error case; seed rows in `parity/seed/<m>.sql`. CI's self-test must show
   0 differences.
2. `internal/modules/<m>/oapi-codegen.yaml` (models only, D2), `queries.sql`,
   an `sqlc.yaml` entry, `make gen`.
3. Port every assertion of the module's Swift tests into `internal/modules/<m>/*_test.go`.
4. Implement on Echo v5 until the tests pass; register in `cmd/App/main.go`.
5. CI `parity` at 0 differences; set the status to `parity` in `modules.yaml`
   and record G0/G1 evidence in `cutover-log.md`.

Shared code: `auth.Identify`, `internal/vaultaccess`, `internal/vaultactivity` (decisions.md D-identify).

Steps 6–7 (staging, production) wait for the pilot (D-wave2-early) and for
the flip blockers above.
