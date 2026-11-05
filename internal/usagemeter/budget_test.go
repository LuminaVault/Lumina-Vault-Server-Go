package usagemeter

import (
	"context"
	"encoding/json"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Swift's UsageMeterBudgetTests.service: one million tokens a day, so the soft cap is
// 800_000.
func testBudgets() Budgets {
	return Budgets{FreeMtokDaily: 1.0, PerSkillMtokDaily: 0.5, DegradeModel: "cheap/model"}
}

func newTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@test", id.String())
	require.NoError(t, err)
	return id
}

// Swift UsageMeterBudgetTests: the four against a real database. The fifth is the
// chat route's 429; its envelope is TestCapExceededError, the route comes with 5A.
func TestBudgets(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	meter := NewWithBudgets(pool, testBudgets())

	t.Run("a trial tenant under the cap is allowed", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		meter.Record(ctx, tenant, "a", 1000, 1000)
		assert.Equal(t, BudgetDecision{Action: BudgetAllow}, meter.CheckBudget(ctx, tenant, "trial"))
	})
	t.Run("a trial tenant over the soft cap is degraded", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		meter.Record(ctx, tenant, "a", 500_000, 350_000)
		assert.Equal(t, BudgetDecision{Action: BudgetDegrade, Model: "cheap/model"}, meter.CheckBudget(ctx, tenant, "trial"))
	})
	t.Run("a trial tenant over the hard cap is denied", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		// Spread across two models: the cap is per tenant, not per model.
		meter.Record(ctx, tenant, "a", 600_000, 0)
		meter.Record(ctx, tenant, "b", 0, 600_000)
		decision := meter.CheckBudget(ctx, tenant, "trial")
		assert.Equal(t, BudgetDeny, decision.Action)
		assert.Positive(t, decision.RetryAfter)
	})
	t.Run("a skill over its own cap is denied", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		meter.Record(ctx, tenant, "skill:digest/x", 400_000, 200_000)
		assert.Equal(t, BudgetDeny, meter.CheckSkillBudget(ctx, tenant, "digest").Action)
	})

	t.Run("the boundaries are inclusive", func(t *testing.T) {
		soft, hard := newTenant(ctx, t, pool), newTenant(ctx, t, pool)
		meter.Record(ctx, soft, "a", 799_999, 0)
		assert.Equal(t, BudgetAllow, meter.CheckBudget(ctx, soft, "trial").Action)
		meter.Record(ctx, soft, "a", 1, 0)
		assert.Equal(t, BudgetDegrade, meter.CheckBudget(ctx, soft, "trial").Action)
		meter.Record(ctx, hard, "a", 999_999, 0)
		assert.Equal(t, BudgetDegrade, meter.CheckBudget(ctx, hard, "trial").Action)
		meter.Record(ctx, hard, "a", 1, 0)
		assert.Equal(t, BudgetDeny, meter.CheckBudget(ctx, hard, "trial").Action)
	})
	t.Run("tiers", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		meter.Record(ctx, tenant, "a", 5_000_000, 0)
		for _, tier := range []string{"pro", "ultimate", "free", "lapsed"} {
			assert.Equal(t, BudgetAllow, meter.CheckBudget(ctx, tenant, tier).Action, tier)
		}
		for _, tier := range []string{"trial", "no-such-tier", ""} {
			assert.Equal(t, BudgetDeny, meter.CheckBudget(ctx, tenant, tier).Action, "%q counts as trial", tier)
		}
		clean := newTenant(ctx, t, pool)
		assert.Equal(t, BudgetDeny, meter.CheckBudget(ctx, clean, "archived").Action, "archived has no allowance")
	})
	t.Run("the check counts only today", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		_, err := pool.Exec(ctx, `INSERT INTO usage_meter (tenant_id, day, model, mtok_in, mtok_out)
			VALUES ($1, CURRENT_DATE - 1, 'a', 5000000, 0), ($1, CURRENT_DATE - 1, 'skill:s/m', 5000000, 0)`, tenant)
		require.NoError(t, err)
		assert.Equal(t, BudgetAllow, meter.CheckBudget(ctx, tenant, "trial").Action)
		assert.Equal(t, BudgetAllow, meter.CheckSkillBudget(ctx, tenant, "s").Action)
	})
	t.Run("a skill's budget counts only its own tag", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		meter.Record(ctx, tenant, "skill:other/x", 900_000, 0)
		meter.Record(ctx, tenant, "skill:digest", 900_000, 0)
		meter.Record(ctx, tenant, "skill:digest-long/x", 900_000, 0)
		meter.Record(ctx, tenant, "skill:digest/x", 100, 0)
		assert.Equal(t, BudgetAllow, meter.CheckSkillBudget(ctx, tenant, "digest").Action)
		assert.Equal(t, BudgetDeny, meter.CheckSkillBudget(ctx, tenant, "d%").Action, "LIKE wildcards pass through, as in Swift")
	})
	t.Run("a per-skill cap of zero means none", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		m := NewWithBudgets(pool, Budgets{FreeMtokDaily: 1, PerSkillMtokDaily: 0})
		meter.Record(ctx, tenant, "skill:s/x", 100_000_000, 0)
		assert.Equal(t, BudgetAllow, m.CheckSkillBudget(ctx, tenant, "s").Action)
	})
	t.Run("a failed query allows", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		tenant := newTenant(ctx, t, pool)
		assert.Equal(t, BudgetAllow, meter.CheckBudget(cancelled, tenant, "trial").Action)
		assert.Equal(t, BudgetAllow, meter.CheckSkillBudget(cancelled, tenant, "s").Action)
	})
	t.Run("a nil meter allows", func(t *testing.T) {
		var none *Meter
		assert.Equal(t, BudgetAllow, none.CheckBudget(ctx, uuid.New(), "archived").Action)
		assert.Equal(t, BudgetAllow, none.CheckSkillBudget(ctx, uuid.New(), "s").Action)
	})

	t.Run("characters", func(t *testing.T) {
		tenant := newTenant(ctx, t, pool)
		meter.Record(ctx, tenant, "tts", 5, 7)
		meter.RecordCharacters(ctx, tenant, "tts", 100)
		meter.RecordCharacters(ctx, tenant, "tts", 23)
		meter.RecordCharacters(ctx, tenant, "none", 0)
		var in, out, chars int64
		require.NoError(t, pool.QueryRow(ctx, `SELECT mtok_in, mtok_out, chars_out FROM usage_meter
			WHERE tenant_id = $1 AND model = 'tts' AND day = CURRENT_DATE`, tenant).Scan(&in, &out, &chars))
		assert.Equal(t, []int64{5, 7, 123}, []int64{in, out, chars})
		var rows int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM usage_meter WHERE tenant_id = $1 AND model = 'none'`, tenant).Scan(&rows))
		assert.Zero(t, rows)
		require.NotPanics(t, func() {
			meter.RecordCharacters(ctx, uuid.New(), "fk-violation", 1)
			var none *Meter
			none.RecordCharacters(ctx, tenant, "m", 1)
		})
	})
}

func TestDefaultBudgets(t *testing.T) {
	m := New(nil)
	assert.Equal(t, int64(1_000_000), m.freeCapTokens)
	assert.Equal(t, int64(800_000), m.freeSoftCap)
	assert.Equal(t, int64(200_000), m.perSkillCap)
	assert.Equal(t, "hermes-3-small", m.degradeModel)
}

func TestUntilUTCMidnight(t *testing.T) {
	m := New(nil)
	m.now = func() time.Time { return time.Date(2026, 11, 5, 23, 0, 0, 0, time.FixedZone("x", 3600)) } // 22:00 UTC
	assert.Equal(t, 2*time.Hour, m.untilUTCMidnight())
	m.now = func() time.Time { return time.Date(2026, 11, 5, 23, 59, 59, 900_000_000, time.UTC) }
	assert.Equal(t, time.Second, m.untilUTCMidnight(), "at least a second")
}

// The envelope of Swift's "chat over the trial cap answers 429 with a usable message".
func TestCapExceededError(t *testing.T) {
	e := &CapExceededError{RetryAfter: 90*time.Second + 500*time.Millisecond}
	assert.Equal(t, http.StatusTooManyRequests, e.HTTPStatus())
	assert.Equal(t, 91, e.RetryAfterSeconds())
	assert.Equal(t, 1, (&CapExceededError{}).RetryAfterSeconds())
	assert.Equal(t, `{"error":{"code":"usage_cap_exceeded","cta":["upgrade","add_key"],`+
		`"message":"You've reached today's usage limit. Upgrade for more, or add your own API key in Settings to keep going now.",`+
		`"retryAfterSeconds":91}}`, string(e.Body()))
	var decoded map[string]map[string]any
	require.NoError(t, json.Unmarshal(e.Body(), &decoded))
	assert.Equal(t, "usage_cap_exceeded", decoded["error"]["code"])
	assert.NotEmpty(t, decoded["error"]["message"])
	assert.Contains(t, decoded["error"]["cta"], "upgrade")
}
