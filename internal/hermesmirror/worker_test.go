package hermesmirror

import (
	"context"
	"log/slog"
	"lumina/internal/dbtest"
	"lumina/internal/hermesdash"
	"lumina/internal/migrations"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// perTenantTransports is HermesMirrorRefreshWorkerTests.PerTenantTransports: one
// fake per tenant, recording which tenants the service asked for.
type perTenantTransports struct {
	mu        sync.Mutex
	fakes     map[uuid.UUID]*fakeTransport
	requested []uuid.UUID
}

func newPerTenant() *perTenantTransports {
	return &perTenantTransports{fakes: map[uuid.UUID]*fakeTransport{}}
}

func (p *perTenantTransports) fake(tenant uuid.UUID) *fakeTransport {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f, ok := p.fakes[tenant]; ok {
		return f
	}
	f := newFake(hermesdash.KindRemote)
	p.fakes[tenant] = f
	return f
}

func (p *perTenantTransports) Kind(context.Context, uuid.UUID) hermesdash.TransportKind {
	return hermesdash.KindRemote
}

func (p *perTenantTransports) Transport(_ context.Context, tenant uuid.UUID) (hermesdash.Transport, error) {
	f := p.fake(tenant)
	p.mu.Lock()
	p.requested = append(p.requested, tenant)
	p.mu.Unlock()
	return f, nil
}

func (p *perTenantTransports) requestedTenants() map[uuid.UUID]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[uuid.UUID]bool{}
	for _, id := range p.requested {
		out[id] = true
	}
	return out
}

// seedWorkerTenant is HermesMirrorRefreshWorkerTests.seedTenant.
func seedWorkerTenant(t *testing.T, pool *pgxpool.Pool, dashboard bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	slug := "w" + id.String()[:6]
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', false, 'trial', now(), now())`, id, slug+"@test.luminavault", slug)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'Personal', now(), now())`, id)
	require.NoError(t, err)
	if dashboard {
		_, err = pool.Exec(ctx, `INSERT INTO user_hermes_config (id, tenant_id, base_url, cron_dashboard_url,
			cron_dashboard_token_ciphertext, cron_dashboard_token_nonce, created_at, updated_at)
			VALUES ($1, $2, '', 'http://127.0.0.1:9119', '\x010203', '\x040506', now(), now())`, uuid.New(), id)
		require.NoError(t, err)
	}
	return id
}

func workerService(t *testing.T, pool *pgxpool.Pool, transports *perTenantTransports) *Service {
	t.Helper()
	h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
	h.service.Transports = transports
	h.service.Compile = nil
	h.service.Now = nil
	return h.service
}

var quietLog = slog.New(slog.DiscardHandler)

// TestRefreshWorker ports HermesMirrorRefreshWorkerTests, one subtest per Swift test.
func TestRefreshWorker(t *testing.T) {
	// Its own database: testPool's container is bound to the first test that
	// asked for it and closes when that test ends.
	if testing.Short() {
		t.Skip("integration test (needs Docker); skipped with -short")
	}
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))

	t.Run("enqueue records a tenant once for the connect-time drain", func(t *testing.T) {
		worker := &Worker{Service: workerService(t, pool, newPerTenant()), Log: quietLog}
		tenant := uuid.New()
		worker.Enqueue(tenant)
		worker.Enqueue(tenant)
		assert.Equal(t, []uuid.UUID{tenant}, worker.PendingTenantIDs())
	})

	t.Run("tick syncs only tenants with a dashboard, pages by id, and isolates failures", func(t *testing.T) {
		configuredA := seedWorkerTenant(t, pool, true)
		configuredB := seedWorkerTenant(t, pool, true)
		configuredC := seedWorkerTenant(t, pool, true)
		unconfigured := seedWorkerTenant(t, pool, false)

		transports := newPerTenant()
		transports.fake(configuredA).setSkills(hermesdash.Skill{Name: "a", Enabled: true, Source: "custom"})
		transports.fake(configuredB).fail("listSkills", hermesdash.DashboardUnreachable("down"))
		transports.fake(configuredB).fail("listJobs", hermesdash.DashboardUnreachable("down"))
		service := workerService(t, pool, transports)
		worker := &Worker{
			Service:         service,
			PageSize:        2, // three configured tenants → at least two pages
			MaxConcurrent:   2,
			PerTenantBudget: 30 * time.Second,
			Log:             quietLog,
		}

		summary, err := worker.Tick(ctx)
		require.NoError(t, err)
		requested := transports.requestedTenants()
		assert.True(t, requested[configuredA] && requested[configuredB] && requested[configuredC])
		assert.False(t, requested[unconfigured])
		assert.GreaterOrEqual(t, summary.Processed, 3)
		assert.GreaterOrEqual(t, summary.Failed, 1)
		assert.Equal(t, 0, summary.TimedOut)

		stateA, err := service.Status(ctx, configuredA)
		require.NoError(t, err)
		assert.Equal(t, "ok", stateA.LastStatus)
		assert.EqualValues(t, 1, stateA.SkillsCount)
		stateB, err := service.Status(ctx, configuredB)
		require.NoError(t, err)
		assert.Equal(t, "failed", stateB.LastStatus)
		st, err := service.loadState(ctx, unconfigured)
		require.NoError(t, err)
		assert.Nil(t, st)

		// Keyset pagination: the second page starts strictly after the first page's last row.
		first, err := worker.ConfiguredTenants(ctx, nil)
		require.NoError(t, err)
		require.Len(t, first, 2)
		second, err := worker.ConfiguredTenants(ctx, &first[len(first)-1].RowID)
		require.NoError(t, err)
		require.NotEmpty(t, second)
		for _, a := range first {
			for _, b := range second {
				assert.NotEqual(t, a.TenantID, b.TenantID)
			}
		}
	})

	t.Run("a tenant exceeding its budget is reported as timed out without blocking the tick", func(t *testing.T) {
		slow := seedWorkerTenant(t, pool, true)
		service := workerService(t, pool, newPerTenant())
		assert.Equal(t, OutcomeTimedOut, Refresh(ctx, slow, service, 0, quietLog))
	})

	t.Run("a refresh collects each mirrored job's finished runs into the brain", func(t *testing.T) {
		tenant := seedWorkerTenant(t, pool, true)
		transports := newPerTenant()
		service := workerService(t, pool, transports)
		fake := transports.fake(tenant)
		fake.setJobs(job("digest", "Digest", false))
		started := time.Unix(1_756_800_000, 0).UTC()
		finished := started.Add(60 * time.Second)
		fake.setRuns("digest", hermesdash.JobRun{Key: "cron_digest_1", Status: hermesdash.RunOK, StartedAt: started, FinishedAt: &finished})
		fake.setRunOutput("digest", "cron_digest_1", "# Nightly\n")

		// One refresh: the jobs sync discovers the job, then collect files it.
		require.Equal(t, OutcomeOK, Refresh(ctx, tenant, service, 30*time.Second, quietLog))
		rows, err := pool.Query(ctx, `SELECT hermes_run_key, vault_file_id FROM hermes_job_runs WHERE tenant_id = $1`, tenant)
		require.NoError(t, err)
		var keys []string
		var fileIDs []*uuid.UUID
		for rows.Next() {
			var k string
			var f *uuid.UUID
			require.NoError(t, rows.Scan(&k, &f))
			keys, fileIDs = append(keys, k), append(fileIDs, f)
		}
		rows.Close()
		require.Equal(t, []string{"cron_digest_1"}, keys)
		assert.NotNil(t, fileIDs[0])

		// The next refresh takes the high-water fast path: no second insert.
		_ = Refresh(ctx, tenant, service, 30*time.Second, quietLog)
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM hermes_job_runs WHERE tenant_id = $1`, tenant).Scan(&n))
		assert.Equal(t, 1, n)
	})
}

// HermesMirrorRefreshWorkerTests: jitter stays within the bound.
func TestJitterStaysWithinTheBound(t *testing.T) {
	for range 20 {
		v := Jitter(2 * time.Second)
		assert.GreaterOrEqual(t, v, time.Duration(0))
		assert.LessOrEqual(t, v, 2*time.Second)
	}
	assert.Equal(t, time.Duration(0), Jitter(0))
}
