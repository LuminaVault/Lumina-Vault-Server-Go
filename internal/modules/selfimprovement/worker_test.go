package selfimprovement_test

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/selfimprovement"
	"lumina/internal/selfimprove"
	"lumina/internal/soul"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// stubReviewLLM answers every Chat with reply (or err) and records the prompts.
type stubReviewLLM struct {
	mu      sync.Mutex
	reply   string
	err     error
	models  []string
	prompts []string
}

func (s *stubReviewLLM) Chat(_ context.Context, _, _, model, prompt string, temperature *float64) (string, llm.CompletionMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = append(s.models, model)
	s.prompts = append(s.prompts, prompt)
	if temperature == nil || *temperature != 0.1 {
		return "", llm.CompletionMessage{}, errors.New("temperature must be 0.1")
	}
	if s.err != nil {
		return "", llm.CompletionMessage{}, s.err
	}
	return "economy-stub", llm.CompletionMessage{Role: "assistant", Content: s.reply}, nil
}

func (v *env) worker(stub *stubReviewLLM) *selfimprovement.Worker {
	return v.m.NewWorker(selfimprovement.WorkerDeps{LLM: stub, MainModel: "main-stub"})
}

type runRow struct {
	status, kind, trigger            string
	report, snapshot, failure, model *string
	applied, skipped                 int
	startedAt, endedAt               *time.Time
}

func (v *env) run(id uuid.UUID) runRow {
	v.t.Helper()
	var r runRow
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT status, kind, trigger, report_markdown, snapshot_json,
		failure_reason, model_used, actions_applied, actions_skipped, started_at, ended_at FROM self_improvement_runs WHERE id = $1`, id).
		Scan(&r.status, &r.kind, &r.trigger, &r.report, &r.snapshot, &r.failure, &r.model, &r.applied, &r.skipped, &r.startedAt, &r.endedAt))
	return r
}

func (v *env) queueRun(tenant uuid.UUID, kind string, dryRun bool) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO self_improvement_runs (id, tenant_id, kind, status, trigger, dry_run, actions_applied, actions_skipped, created_at)
		VALUES ($1, $2, $3, 'queued', 'manual', $4, 0, 0, now())`, id, tenant, kind, dryRun)
	return id
}

// drain ticks until run leaves queued/running (other tests' rows can't exist: each
// test has its own database, but tick claims one run per call).
func (v *env) drain(w *selfimprovement.Worker, run uuid.UUID) runRow {
	v.t.Helper()
	for range 8 {
		w.Tick(context.Background(), time.Now())
		if r := v.run(run); r.status != "queued" && r.status != "running" {
			return r
		}
	}
	return v.run(run)
}

func (v *env) ageSkill(tenant uuid.UUID, name string, days int) {
	v.t.Helper()
	at := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	require.NoError(v.t, os.Chtimes(v.skillPath(tenant, name), at, at))
}

// SelfImprovementServiceTests: "tick dry-run skips pinned skill and applies zero
// mutations".
func TestTickDryRunSkipsPinnedSkillAndAppliesZeroMutations(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	v.writeSkill(tenant, "pinned-skill", "test vault skill")
	v.exec(`INSERT INTO self_improvement_settings (id, tenant_id, consolidate) VALUES (gen_random_uuid(), $1, FALSE)`, tenant)
	r := v.do(tenant, "PATCH", base+"/resources/skill/pinned-skill", `{"pinned":true}`)
	require.Equal(t, 200, r.code, r.body)
	queued := v.queueRun(tenant, "curator", true)

	run := v.drain(v.worker(&stubReviewLLM{}), queued)
	require.Equal(t, "succeeded", run.status)
	require.Contains(t, *run.report, "pinned")
	require.Zero(t, run.applied)
	require.Nil(t, run.snapshot)
	require.NotNil(t, run.startedAt)
	require.NotNil(t, run.endedAt)
}

// A live curator archives a skill idle for 90+ days (directory moved, snapshot
// with archivedPath), marks a 30+ day one stale, leaves a fresh one, mirrors the
// report and stamps the settings. Swift's SkillsState saves by tenant_id, so the
// state updates reach every skills_state row of the tenant.
func TestTickLiveCuratorArchivesAndMarksStale(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	v.writeSkill(tenant, "ancient", "old")
	v.writeSkill(tenant, "dusty", "older")
	v.writeSkill(tenant, "fresh", "new")
	v.ageSkill(tenant, "ancient", 120)
	v.ageSkill(tenant, "dusty", 45)
	v.exec(`INSERT INTO self_improvement_settings (id, tenant_id, consolidate, lease_until) VALUES (gen_random_uuid(), $1, FALSE, now())`, tenant)
	queued := v.queueRun(tenant, "curator", false)

	run := v.drain(v.worker(&stubReviewLLM{}), queued)
	require.Equal(t, "succeeded", run.status, run.failure)
	require.Equal(t, 2, run.applied)
	report := *run.report
	require.Contains(t, report, "# Curator Report\n\n- Run: "+ids.Upper(queued)+"\n- Mode: live\n- Candidates: 3\n")
	require.Contains(t, report, "- Archived `ancient` after 120 idle days.")
	require.Contains(t, report, "- Marked `dusty` stale after 45 idle days.")
	require.Contains(t, report, "- Consolidation skipped — fewer than two eligible skills or disabled.")

	archived := filepath.Join(v.paths.TenantRoot(tenant), "skills", ".archive", "ancient-"+ids.Upper(queued))
	require.FileExists(t, filepath.Join(archived, "SKILL.md"))
	require.NoDirExists(t, filepath.Dir(v.skillPath(tenant, "ancient")))

	var snapshots []map[string]any
	require.NoError(t, json.Unmarshal([]byte(*run.snapshot), &snapshots))
	require.Len(t, snapshots, 2)
	require.Equal(t, "ancient", snapshots[0]["name"])
	require.Equal(t, archived, snapshots[0]["archivedPath"])
	require.Equal(t, "active", snapshots[0]["state"])
	require.Contains(t, *run.snapshot, `\/`, "Foundation escapes slashes")
	require.Equal(t, "dusty", snapshots[1]["name"])
	require.NotContains(t, snapshots[1], "archivedPath")

	states := map[string]string{}
	rows, err := v.pool.Query(context.Background(), `SELECT name, curator_state FROM skills_state WHERE tenant_id = $1`, tenant)
	require.NoError(t, err)
	for rows.Next() {
		var name, state string
		require.NoError(t, rows.Scan(&name, &state))
		states[name] = state
	}
	rows.Close()
	require.Equal(t, map[string]string{"ancient": "archived", "dusty": "stale", "fresh": "active"}, states, "new rows insert one by one")

	mirror, err := os.ReadFile(filepath.Join(v.paths.TenantRoot(tenant), "reports", "self-improvement", ids.Upper(queued), "REPORT.md"))
	require.NoError(t, err)
	require.Equal(t, report, string(mirror))
	var reviewed *time.Time
	var lease *time.Time
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT last_curator_review_at, lease_until FROM self_improvement_settings WHERE tenant_id = $1`, tenant).
		Scan(&reviewed, &lease))
	require.NotNil(t, reviewed)
	require.Nil(t, lease)
}

func replacement(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\nmetadata:\n  capability: low\n---\n\nDo it better.\n"
}

// Consolidation asks the model (economy mode → economy model), applies a safe
// patch, merges one skill into another (archiving the source), and rejects unknown
// and unsafe actions; dry runs only report.
func TestTickConsolidatesSkills(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		v.writeSkill(tenant, name, name)
	}
	v.exec(`INSERT INTO self_improvement_settings (id, tenant_id, consolidate, model_mode) VALUES (gen_random_uuid(), $1, TRUE, 'economy')`, tenant)
	actions := []map[string]any{
		{"action": "patch", "name": "alpha", "reason": "tighten", "replacement_markdown": replacement("alpha", "tight")},
		{"action": "merge", "name": "beta", "target": "gamma", "reason": "duplicate", "replacement_markdown": replacement("gamma", "merged")},
		{"action": "delete", "name": "gamma", "reason": "no"},
		{"action": "patch", "name": "alpha", "reason": "rename", "replacement_markdown": replacement("other", "x")},
		{"action": "keep", "name": "gamma", "reason": "fine"},
	}
	body, err := json.Marshal(map[string]any{"actions": actions})
	require.NoError(t, err)
	stub := &stubReviewLLM{reply: "```json\n" + string(body) + "\n```"}
	w := v.worker(stub)

	dry := v.drain(w, v.queueRun(tenant, "curator", true))
	require.Equal(t, "succeeded", dry.status, dry.failure)
	require.Contains(t, *dry.report, "- Would patch `alpha`: tighten")
	require.Contains(t, *dry.report, "- Would merge `beta` into `gamma`: duplicate")
	require.Equal(t, skillBody("alpha", "alpha"), readFile(t, v.skillPath(tenant, "alpha")))

	queued := v.queueRun(tenant, "curator", false)
	run := v.drain(w, queued)
	require.Equal(t, "succeeded", run.status, run.failure)
	require.Equal(t, "economy-stub", *run.model)
	require.Equal(t, []string{"economy-stub", "economy-stub"}, stub.models)
	require.Contains(t, stub.prompts[0], `<skill name="alpha">`)
	report := *run.report
	require.Contains(t, report, "## Consolidation")
	require.Contains(t, report, "- Patch `alpha`: tighten")
	require.Contains(t, report, "- Merge `beta` into `gamma`: duplicate")
	require.Contains(t, report, "- Rejected unknown action for `gamma`.")
	require.Contains(t, report, "- Rejected unsafe `patch` for `alpha`.")
	require.Contains(t, report, "- Kept `gamma`: fine")
	require.Equal(t, 2, run.applied)
	require.Equal(t, 2, run.skipped)

	require.Equal(t, replacement("alpha", "tight"), readFile(t, v.skillPath(tenant, "alpha")))
	require.Equal(t, replacement("gamma", "merged"), readFile(t, v.skillPath(tenant, "gamma")))
	require.NoDirExists(t, filepath.Dir(v.skillPath(tenant, "beta")))
	var snapshots []map[string]any
	require.NoError(t, json.Unmarshal([]byte(*run.snapshot), &snapshots))
	require.Len(t, snapshots, 3)
	require.Equal(t, selfimprove.SHA256(replacement("alpha", "tight")), snapshots[0]["afterSHA"])
	require.Equal(t, "beta", snapshots[2]["name"])
	require.NotEmpty(t, snapshots[2]["archivedPath"])
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// The SOUL reviewer files a conservative patch as a pending change, reports no
// change when none is needed, and fails disabled or broad reviews with Swift's
// HTTPError description.
func TestTickSoulReview(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	current, err := v.soul.Write(tenant, v.username(tenant), "# SOUL.md\n\n## Identity\n\nStable voice.\n")
	require.NoError(t, err)
	v.exec(`INSERT INTO self_improvement_settings (id, tenant_id, soul_review_enabled, lease_until) VALUES (gen_random_uuid(), $1, TRUE, now())`, tenant)
	v.exec(`INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 't', now(), now())`, uuid.New(), tenant)

	proposed := "# SOUL.md\n\n## Identity\n\nStable voice.\n\n## Note\n\nTiny tweak.\n"
	reply, err := json.Marshal(map[string]any{"needed": true, "summary": "tiny", "proposed_markdown": proposed})
	require.NoError(t, err)
	stub := &stubReviewLLM{reply: string(reply)}
	queued := v.queueRun(tenant, "soul", false)
	run := v.drain(v.worker(stub), queued)
	require.Equal(t, "succeeded", run.status, run.failure)
	require.Equal(t, "# SOUL Review\n\ntiny\n\nA conservative patch is waiting for approval.", *run.report)
	require.Equal(t, []string{"economy-stub"}, stub.models, "model_mode defaults to economy")
	require.Contains(t, stub.prompts[0], "<current_soul>\n"+current+"\n</current_soul>")

	var state, title, patch, proposedMarkdown, baseSHA, changeReport string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT state, title, patch, proposed_markdown, base_sha256, report_markdown
		FROM self_improvement_changes WHERE run_id = $1`, queued).Scan(&state, &title, &patch, &proposedMarkdown, &baseSHA, &changeReport))
	require.Equal(t, "pending", state)
	require.Equal(t, "SOUL.md review", title)
	require.Equal(t, soul.Inject(proposed), proposedMarkdown)
	require.Equal(t, selfimprove.SHA256(current), baseSHA)
	require.True(t, strings.HasPrefix(patch, "--- SOUL.md\n+++ SOUL.md (proposed)\n"))
	require.Contains(t, patch, "+Tiny tweak.")
	require.Equal(t, "# SOUL Review\n\ntiny", changeReport)

	stub.reply = `{"needed":false,"summary":"stable"}`
	run = v.drain(v.worker(stub), v.queueRun(tenant, "soul", false))
	require.Equal(t, "succeeded", run.status, run.failure)
	require.Equal(t, "# SOUL Review\n\nstable\n\nNo SOUL.md change is needed.", *run.report)

	stub.reply = `{"summary":"missing needed"}`
	run = v.drain(v.worker(stub), v.queueRun(tenant, "soul", false))
	require.Equal(t, "failed", run.status)
	require.Equal(t, "HTTPError: Bad Gateway, review_output_invalid_json", *run.failure)

	v.exec(`UPDATE self_improvement_settings SET soul_review_enabled = FALSE WHERE tenant_id = $1`, tenant)
	run = v.drain(v.worker(stub), v.queueRun(tenant, "soul", false))
	require.Equal(t, "failed", run.status)
	require.Equal(t, "HTTPError: Service Unavailable, soul_review_disabled", *run.failure)
}

// A curator for a BYO Hermes tenant fails read-only; an LLM failure fails the run
// with the error's description.
func TestTickFailures(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	v.writeSkill(tenant, "one", "a")
	v.writeSkill(tenant, "two", "b")
	v.exec(`INSERT INTO self_improvement_settings (id, tenant_id, consolidate) VALUES (gen_random_uuid(), $1, TRUE)`, tenant)
	stub := &stubReviewLLM{err: &llm.DirectChatError{Message: "hermes upstream error (500)"}}
	run := v.drain(v.worker(stub), v.queueRun(tenant, "curator", false))
	require.Equal(t, "failed", run.status)
	require.Equal(t, "HTTPError: Bad Gateway, hermes upstream error (500)", *run.failure)

	v.overrides[tenant] = true
	run = v.drain(v.worker(stub), v.queueRun(tenant, "curator", true))
	require.Equal(t, "failed", run.status)
	require.Equal(t, "HTTPError: Conflict, self_improvement_read_only_on_byo_hermes", *run.failure)
}

// enqueueDueReviews seeds settings for every user, then leases due, idle tenants
// and queues their weekly curator and SOUL runs; the lease keeps a second tick from
// queueing again.
func TestTickEnqueuesDueWeeklyReviews(t *testing.T) {
	v := setup(t)
	ctx := context.Background()
	due, busy, fresh := v.user(), v.user(), v.user()
	v.exec(`INSERT INTO self_improvement_settings (id, tenant_id, soul_review_enabled, next_review_at) VALUES (gen_random_uuid(), $1, TRUE, now() - interval '1 hour')`, due)
	v.exec(`INSERT INTO self_improvement_settings (id, tenant_id, next_review_at, last_activity_at, minimum_idle_hours)
		VALUES (gen_random_uuid(), $1, now() - interval '1 hour', now(), 2)`, busy)
	w := v.m.NewWorker(selfimprovement.WorkerDeps{LLM: &stubReviewLLM{err: errors.New("unused")}})
	now := time.Now()
	w.Tick(ctx, now)

	var seeded int
	v.scalar(`SELECT count(*) FROM self_improvement_settings WHERE tenant_id = $1`, &seeded, fresh)
	require.Equal(t, 1, seeded, "a user without settings gets the default row")

	count := func(tenant uuid.UUID) (curator, soulRuns int) {
		rows, err := v.pool.Query(ctx, `SELECT kind FROM self_improvement_runs WHERE tenant_id = $1 AND trigger = 'weekly' AND dry_run = FALSE`, tenant)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var k string
			require.NoError(t, rows.Scan(&k))
			if k == "curator" {
				curator++
			} else {
				soulRuns++
			}
		}
		return curator, soulRuns
	}
	c, s := count(due)
	require.Equal(t, 1, c)
	require.Equal(t, 1, s)
	c, s = count(busy)
	require.Zero(t, c+s, "an active tenant waits for its idle window")

	var next time.Time
	v.scalar(`SELECT next_review_at FROM self_improvement_settings WHERE tenant_id = $1`, &next, due)
	require.WithinDuration(t, now.Add(168*time.Hour), next, 2*time.Second)

	// The next review is a week out, so another tick queues nothing new.
	w.Tick(ctx, now.Add(time.Minute))
	c, s = count(due)
	require.Equal(t, 1, c)
	require.Equal(t, 1, s)
}

// Swift's SkillsState @ID is tenant_id, so saving a loaded row updates every
// skills_state row of the tenant: archiving one existing skill archives them all,
// and the next skills then read as archived and aren't marked stale. Kept for
// parity (security and correctness fixes wait until Swift is archived).
func TestTickCuratorSavesLoadedStateTenantWide(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	for _, name := range []string{"ancient", "dusty"} {
		v.writeSkill(tenant, name, name)
		v.exec(`INSERT INTO skills_state (tenant_id, source, name, enabled, curator_pinned, curator_state) VALUES ($1, 'vault', $2, TRUE, FALSE, 'active')`, tenant, name)
	}
	v.exec(`INSERT INTO skills_state (tenant_id, source, name, enabled) VALUES ($1, 'builtin', 'other', TRUE)`, tenant)
	v.ageSkill(tenant, "ancient", 120)
	v.ageSkill(tenant, "dusty", 45)
	v.exec(`INSERT INTO self_improvement_settings (id, tenant_id, consolidate) VALUES (gen_random_uuid(), $1, FALSE)`, tenant)

	run := v.drain(v.worker(&stubReviewLLM{}), v.queueRun(tenant, "curator", false))
	require.Equal(t, "succeeded", run.status, run.failure)
	require.Equal(t, 1, run.applied)
	require.NotContains(t, *run.report, "stale")
	var notArchived int
	v.scalar(`SELECT count(*) FROM skills_state WHERE tenant_id = $1 AND curator_state <> 'archived'`, &notArchived, tenant)
	require.Zero(t, notArchived)
}
