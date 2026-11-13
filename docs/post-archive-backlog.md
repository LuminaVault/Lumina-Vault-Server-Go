# Post-archive backlog

LuminaVaultServer (Swift) was archived on 2026-11-13. This server now serves every route and runs every background job
in staging and production (`docs/migration/cutover-log.md`). The parity rule that froze Swift's behaviour in place
(D-security-after-archive in `docs/migration/decisions.md`) is lifted. This file lists the work that was waiting for
the archive, in the order the owner wants it done, plus one item that is deliberately **not** for now.

Read this before you pick up server work. When an item lands, strike it here in the same PR.

## Rules that still apply

- Go PRs merge only when CI is fully green. Infra PRs are merged by the owner, and merging infra deploys.
- Schema changes are goose migrations under `internal/migrations/sql/` and are **expand-only**. The Swift image is
  the rollback target (`ghcr.io/luminavault/luminavaultserver:fdbed9b0069ebaad4ebe500f1777ce50a9f9e611`), and it
  must still boot against the schema.
- No test waivers. Every fix comes with a test, and a fix that changes a `TestSwiftWeakness*` test turns it into a
  test of the fixed behaviour.
- Tests never use real API keys; use stub servers.
- Staging talks to `hermes-stub-staging` and never to production Hermes.

## Next: queued in this order

### 1. Security and data fixes

These were decided on 2026-11-11 (D-owner-2026-11-11 items 5–9, plus `skills_state`) and then held for parity. Each
one is a deliberate change from Swift. Name each in `docs/migration/cutover-log.md` when it lands.

1. **SSRF.** Every outbound fetch goes through `internal/ssrfguard`, BYOK base URLs included.
2. **Vault file paths.** `internal/vaultfiles` refuses symlink escapes and `link/../nope.md` (the `TestSwiftWeakness*`
   cases in `internal/vaultfiles/resolve_test.go`). Snapshot restore paths in self-improvement go through the same
   checks.
3. **Storage quota.** Enforce it.
4. **`/v1/memory/graph`.** Scrub what it discloses.
5. **Share-sheet note deletion.** Fix it.
6. **`skills_state`.** A PATCH updates only its own row, not every row for that skill name.

Found during the port, same batch:

- **JWT unknown `kid`.** A token that names an unknown `kid` is verified with the active key (a JWTKit fallback that
  Go matched in #85). Refuse unknown kids.
- **xAI/Nous OAuth `complete`.** The body is read before any auth check, so a valid sessionID alone can finish a link.
- **Auth weaknesses kept by 8A (#85).** These are listed in that PR body:
  - The login timing oracle, and the 423 sent before the password check.
  - The lockout counter is not reset after expiry, and failure counters are racy.
  - `mfa/resend` enumerates accounts. Register 409s enumerate emails and usernames.
  - Requesting a new code bypasses the per-row reset and verify locks.
  - Codes are compared in non-constant time, and the logging sender writes OTP codes to logs.
  - A password reset does not invalidate access tokens.
  - Refresh has no reuse detection and is not transactional.
- **MCP `browse` `pathPrefix`.** The `path >= p AND path < p || U+FFFF` range matches nothing under en_US.utf8
  collation. Use `starts_with` or `COLLATE "C"`.
- **`usage_meter`.** There is no unique `(tenant_id, day, model)` key, and metering is read-then-write, so concurrent
  calls split rows. Add the key through an expand-only migration, then upsert.

### 2. Hermes upgrade for CVE-2026-10221

`hermes-production` runs the LuminaVaultHermesAgent fork 0.18.2 (image `ghcr.io/luminavault/hermes@sha256:9892ad09…`),
which is affected. The image chain now runs:

1. The fork's `luminavault-image.yml` publishes `ghcr.io/luminavault/luminavault-hermes-agent:sha-*`.
2. In this repo, `docker/hermes.Dockerfile` pins that base by digest.
3. This repo's `.github/workflows/hermes-image.yml` publishes `ghcr.io/luminavault/hermes`. It moved here from
   LuminaVaultServer in #115; GHCR Actions access is granted.
4. LuminaVaultInfra `apps/hermes/hermes.yaml` pins the digest in two containers. That is an owner-merged infra PR.

Steps:

1. Bump the fork to a patched release.
2. Bump the `FROM` digest here, and confirm the Hermes Image run is green.
3. Open the infra PR with the new digest.
4. Smoke production: run a chat completion and an ingestion through Hermes.
5. Rollback is reverting the infra PR.

### 3. Knowledge extraction self-edge

`KnowledgeExtractionWorker` fails about 1 run in 43 in production with a `knowledge_edges_check` violation (`from_node_id
<> to_node_id`). In `inferWithinMemory` (`internal/modules/knowledge/extraction.go`), a memory that repeats a statement
yields two claims with the same node id, and the worker links that node to itself. Swift had the same gap, so this is
not a port regression.

Fix: skip the pair when `left.id == right.id`. Add a test with a memory that repeats a statement.

## Later, not now: finish the move to sqlc

**Status:** the owner decided on 2026-11-13 that this is future work for another agent. Do not start it before the
strangers gate in the owner's rules ("no re-architectures until 10 strangers have used the current version"). It is
recorded here so the shape is agreed when the time comes.

**What exists.** sqlc is already the main data layer:

- `sqlc.yaml` has about 39 packages. Each one is `internal/.../queries.sql`, generated into `<module>/db` with
  `sql_package: pgx/v5`.
- `make gen` runs `sqlc generate`.
- `.golangci.yml` excludes the generated code.

GORM was considered and rejected. The schema came from Fluent and is fixed (goose, expand-only), and the queries rely
on `FOR UPDATE SKIP LOCKED`, `ON CONFLICT … DO UPDATE`, pgvector and jsonb. Several of them reproduce Fluent's SQL on
purpose.

**What is left:**

1. **Hand-written SQL.** About 108 non-test files still call `pool.Query`, `pool.QueryRow` or `pool.Exec` with
   inline SQL. Examples: `internal/hermesmirror`, `internal/llm`, `internal/authservice`, `internal/workers`,
   `internal/modules/skills`, `internal/modules/workflows`, `internal/modules/ingestion/worker.go` and
   `internal/modules/knowledge/extraction.go`.
2. **Schema source.** Every package points `schema:` at the `schema-swift.sql` snapshot, plus individual migrations
   where one was added (`quickassistant`). Point every package at the goose migrations directory instead, so a new
   migration reaches every package.
3. **CI drift check.** CI never runs `sqlc generate`. Add a job that runs it (pinned sqlc version) and fails on
   `git diff --exit-code`.
4. **Config duplication.** The same `overrides` block is repeated about 39 times. Collapse it with a YAML anchor or
   sqlc's top-level overrides.

**How to do it:**

- First do items 2–4 above: schema source, drift check, overrides. They change no runtime behaviour.
- Then move hand-written SQL one package at a time, only when that package is already being changed for another
  reason. Never as a big-bang rewrite.
- Keep each query's SQL text as it is; the move changes how the Go code calls a query, not the query itself.
- The existing tests (testcontainers Postgres) must pass unchanged.
- Leave dynamic SQL hand-written: queries whose columns are chosen at runtime, such as ingestion's
  `save(ctx, item, cols...)`. sqlc cannot express them.
