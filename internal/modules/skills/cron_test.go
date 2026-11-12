package skills

import (
	"context"
	"io"
	"log/slog"
	"lumina/internal/skills"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func writeCronSkill(t *testing.T, v dbEnv, tenant uuid.UUID, name, schedule string) {
	t.Helper()
	path := filepath.Join(v.paths.TenantRoot(tenant), "skills", name, "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	meta := "metadata:\n  capability: low\n"
	if schedule != "" {
		meta += "  schedule: \"" + schedule + "\"\n"
	}
	require.NoError(t, os.WriteFile(path,
		[]byte("---\nname: "+name+"\ndescription: cron test\n"+meta+"---\nSay hi.\n"), 0o644))
}

func cronModule(v dbEnv, transport *scriptedTransport, push *pushRecorder) *Module {
	d := runnerDeps{transport: transport}
	if push != nil {
		d.push = push
	}
	return New(v.pool, Options{Catalog: skills.NewVaultCatalog(v.paths), Runner: v.runner(d)})
}

type cronState struct {
	enabled            bool
	lastRunAt          *time.Time
	lastStatus, lastEr *string
}

func readCronState(t *testing.T, v dbEnv, tenant uuid.UUID, name string) (cronState, bool) {
	t.Helper()
	var s cronState
	err := v.pool.QueryRow(context.Background(), `SELECT enabled, last_run_at, last_status, last_error
		FROM skills_state WHERE tenant_id = $1 AND source = 'vault' AND name = $2`, tenant, name).
		Scan(&s.enabled, &s.lastRunAt, &s.lastStatus, &s.lastEr)
	if err != nil {
		return s, false
	}
	return s, true
}

func testCronScheduler(t *testing.T, v dbEnv) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 07:00 in Lisbon on a winter day is 07:00 UTC; New York is 02:00.
	at := time.Date(2026, 1, 12, 7, 0, 20, 0, time.UTC)

	t.Run("a matching cron in the user's timezone runs once per minute with trigger cron", func(t *testing.T) {
		tenant := v.user()
		_, err := v.pool.Exec(ctx, `UPDATE users SET timezone = 'Europe/Lisbon' WHERE id = $1`, tenant)
		require.NoError(t, err)
		writeCronSkill(t, v, tenant, "morning", "0 7 * * *")
		writeCronSkill(t, v, tenant, "evening", "0 19 * * *")
		writeCronSkill(t, v, tenant, "manual-only", "")
		tr := &scriptedTransport{steps: []string{contentStep("hi")}}
		m := cronModule(v, tr, nil)

		n, err := m.CronTick(ctx, at, quiet)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.Len(t, tr.payloads(), 1)
		require.Contains(t, string(tr.payloads()[0]), `Trigger: cron`)
		s, ok := readCronState(t, v, tenant, "morning")
		require.True(t, ok)
		require.True(t, s.enabled)
		require.Equal(t, "ok", *s.lastStatus)
		require.True(t, s.lastRunAt.Equal(at))

		// Same wall-clock minute: no second run.
		n, err = m.CronTick(ctx, at.Add(30*time.Second), quiet)
		require.NoError(t, err)
		require.Zero(t, n)
	})

	t.Run("the timezone decides the minute", func(t *testing.T) {
		tenant := v.user()
		_, err := v.pool.Exec(ctx, `UPDATE users SET timezone = 'America/New_York' WHERE id = $1`, tenant)
		require.NoError(t, err)
		writeCronSkill(t, v, tenant, "ny-morning", "0 7 * * *")
		m := cronModule(v, &scriptedTransport{}, nil)
		n, err := m.CronTick(ctx, at, quiet)
		require.NoError(t, err)
		_, ran := readCronState(t, v, tenant, "ny-morning")
		require.False(t, ran)
		_ = n
	})

	t.Run("a failed run still advances last_run_at and records the error", func(t *testing.T) {
		tenant := v.user()
		writeCronSkill(t, v, tenant, "broken", "* * * * *")
		m := cronModule(v, &scriptedTransport{}, nil) // script exhausted: the agent call fails
		_, err := m.CronTick(ctx, at, quiet)
		require.NoError(t, err)
		s, ok := readCronState(t, v, tenant, "broken")
		require.True(t, ok)
		require.True(t, s.lastRunAt.Equal(at))
		require.NotNil(t, s.lastStatus)
	})

	t.Run("a one-shot fires once when due, then is disabled", func(t *testing.T) {
		tenant := v.user()
		writeCronSkill(t, v, tenant, "once", "")
		_, err := v.pool.Exec(ctx, `INSERT INTO skills_state (tenant_id, source, name, enabled, run_at, apns_category)
			VALUES ($1, 'vault', 'once', true, $2, 'job')`, tenant, at.Add(-time.Hour))
		require.NoError(t, err)
		push := &pushRecorder{}
		_, err = v.pool.Exec(ctx, `INSERT INTO device_tokens (id, tenant_id, token, platform, bundle_id, created_at, updated_at)
			VALUES ($1, $2, $3, 'ios', 'com.luminavault.test', now(), now())`, uuid.New(), tenant, strings.Repeat("ab", 32))
		require.NoError(t, err)
		m := cronModule(v, &scriptedTransport{steps: []string{contentStep("done")}}, push)

		n, err := m.CronTick(ctx, at, quiet)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		s, _ := readCronState(t, v, tenant, "once")
		require.False(t, s.enabled)
		require.Equal(t, "ok", *s.lastStatus)
		require.NotEmpty(t, push.categories(), "a successful run with an APNs category pushes notifyCron")

		// Disabled now, so a later tick leaves it alone (other tenants' skills may run).
		_, err = m.CronTick(ctx, at.Add(time.Minute), quiet)
		require.NoError(t, err)
		s, _ = readCronState(t, v, tenant, "once")
		require.True(t, s.lastRunAt.Equal(at))
	})

	t.Run("a job linked to a workflow is left to WorkflowScheduler", func(t *testing.T) {
		tenant := v.user()
		writeCronSkill(t, v, tenant, "linked", "* * * * *")
		workflow := uuid.New()
		_, err := v.pool.Exec(ctx, `INSERT INTO workflows (id, tenant_id, name, draft_definition)
			VALUES ($1, $2, 'Job · linked', '{}')`, workflow, tenant)
		require.NoError(t, err)
		_, err = v.pool.Exec(ctx, `INSERT INTO skills_state (tenant_id, source, name, enabled, workflow_id)
			VALUES ($1, 'vault', 'linked', true, $2)`, tenant, workflow)
		require.NoError(t, err)
		tr := &scriptedTransport{}
		_, err = cronModule(v, tr, nil).CronTick(ctx, at, quiet)
		require.NoError(t, err)
		s, _ := readCronState(t, v, tenant, "linked")
		require.Nil(t, s.lastRunAt)
	})
}

func testRunWorkflowSkill(t *testing.T, v dbEnv) {
	ctx := context.Background()
	tenant := v.user()
	writeCronSkill(t, v, tenant, "brief", "")
	tr := &scriptedTransport{steps: []string{contentStep("the brief")}}
	m := cronModule(v, tr, nil)

	markdown, _, found, err := m.RunWorkflowSkill(ctx, tenant, "pro", "u", "brief", "topic=go")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "the brief", markdown)
	require.Len(t, tr.payloads(), 1)
	require.Contains(t, string(tr.payloads()[0]), `Trigger: event:workflow payload:[:]`)
	require.Contains(t, string(tr.payloads()[0]), `topic=go`)

	_, _, found, err = m.RunWorkflowSkill(ctx, tenant, "pro", "u", "missing", "")
	require.NoError(t, err)
	require.False(t, found)
}
