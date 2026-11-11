package quickassistant

import (
	"context"
	"encoding/json"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *Store
}

func newEnv(t *testing.T) env {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return env{t: t, pool: pool, store: NewStore(pool)}
}

// seed makes a user with a personal vault and one conversation.
func (e env) seed() (tenant, vault, conversation uuid.UUID) {
	e.t.Helper()
	ctx := context.Background()
	tenant = uuid.New()
	short := strings.ToLower(tenant.String()[:8])
	_, err := e.pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'trial', now(), now())`, tenant, "qa-"+short+"@test.luminavault", "qa-"+short)
	require.NoError(e.t, err)
	vault = tenant
	_, err = e.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'test', now(), now())`, vault)
	require.NoError(e.t, err)
	conversation = uuid.New()
	_, err = e.pool.Exec(ctx, `INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 't', now(), now())`, conversation, tenant)
	require.NoError(e.t, err)
	return tenant, vault, conversation
}

func (e env) insert(tenant, vault, conversation uuid.UUID, expires time.Time) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	require.NoError(e.t, e.store.Insert(context.Background(), PendingAction{
		ID: id, TenantID: tenant, VaultID: vault, ConversationID: conversation,
		Tool: "memory_forget", Args: map[string]any{"memoryID": "X"}, Summary: "s", ExpiresAt: expires,
	}))
	return id
}

type rowState struct {
	Status  string
	Args    string
	Summary string
}

func (e env) row(id uuid.UUID) rowState {
	e.t.Helper()
	var r rowState
	require.NoError(e.t, e.pool.QueryRow(context.Background(),
		`SELECT status, args::text, summary FROM assistant_pending_actions WHERE id = $1`, id).Scan(&r.Status, &r.Args, &r.Summary))
	return r
}

func (e env) count(id uuid.UUID) int {
	e.t.Helper()
	var n int
	require.NoError(e.t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM assistant_pending_actions WHERE id = $1`, id).Scan(&n))
	return n
}

func (e env) setExpiry(id uuid.UUID, ago time.Duration) {
	e.t.Helper()
	_, err := e.pool.Exec(context.Background(),
		`UPDATE assistant_pending_actions SET expires_at = now() - make_interval(secs => $2) WHERE id = $1`, id, ago.Seconds())
	require.NoError(e.t, err)
}

func TestClaimReturnsStoredArgsOnceAndClearsTheRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenant, vault, conv := e.seed()
	id := e.insert(tenant, vault, conv, time.Now().Add(10*time.Minute))

	got, err := e.store.Claim(ctx, id, tenant)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "memory_forget", got.Tool)
	require.Equal(t, vault, got.VaultID)
	require.Equal(t, conv, got.ConversationID)
	require.Equal(t, map[string]any{"memoryID": "X"}, got.Args)

	require.Equal(t, rowState{Status: "confirmed", Args: "{}", Summary: ""}, e.row(id))

	again, err := e.store.Claim(ctx, id, tenant)
	require.NoError(t, err)
	require.Nil(t, again)
}

func TestConcurrentClaimsRunOnce(t *testing.T) {
	e := newEnv(t)
	tenant, vault, conv := e.seed()
	id := e.insert(tenant, vault, conv, time.Now().Add(10*time.Minute))

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	var errs []error
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := e.store.Claim(context.Background(), id, tenant)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if got != nil {
				wins++
			}
		}()
	}
	wg.Wait()
	// A claim that errored would hide a second winner or a lost one.
	require.Empty(t, errs)
	require.Equal(t, 1, wins)
}

func TestExpiredActionCannotBeClaimed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenant, vault, conv := e.seed()
	id := e.insert(tenant, vault, conv, time.Now().Add(-time.Second))

	got, err := e.store.Claim(ctx, id, tenant)
	require.NoError(t, err)
	require.Nil(t, got)

	status, found, err := e.store.Status(ctx, id, tenant)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "pending", status)
}

func TestOtherTenantCannotClaimCancelOrSeeStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenant, vault, conv := e.seed()
	other, _, _ := e.seed()
	id := e.insert(tenant, vault, conv, time.Now().Add(10*time.Minute))

	got, err := e.store.Claim(ctx, id, other)
	require.NoError(t, err)
	require.Nil(t, got)

	c, err := e.store.Cancel(ctx, id, other)
	require.NoError(t, err)
	require.Nil(t, c)

	_, found, err := e.store.Status(ctx, id, other)
	require.NoError(t, err)
	require.False(t, found)

	require.Equal(t, "pending", e.row(id).Status)
}

func TestCancelThenClaimDoesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenant, vault, conv := e.seed()
	id := e.insert(tenant, vault, conv, time.Now().Add(10*time.Minute))

	c, err := e.store.Cancel(ctx, id, tenant)
	require.NoError(t, err)
	require.NotNil(t, c)
	require.Equal(t, conv, *c)
	require.Equal(t, rowState{Status: "cancelled", Args: "{}", Summary: ""}, e.row(id))

	got, err := e.store.Claim(ctx, id, tenant)
	require.NoError(t, err)
	require.Nil(t, got)

	c, err = e.store.Cancel(ctx, id, tenant)
	require.NoError(t, err)
	require.Nil(t, c)
}

func TestMarkFailedOnlyMovesConfirmedRows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenant, vault, conv := e.seed()
	pending := e.insert(tenant, vault, conv, time.Now().Add(10*time.Minute))
	confirmed := e.insert(tenant, vault, conv, time.Now().Add(10*time.Minute))
	got, err := e.store.Claim(ctx, confirmed, tenant)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.NoError(t, e.store.MarkFailed(ctx, pending))
	require.NoError(t, e.store.MarkFailed(ctx, confirmed))

	p := e.row(pending)
	require.Equal(t, "pending", p.Status)
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(p.Args), &args))
	require.Equal(t, map[string]any{"memoryID": "X"}, args)
	require.Equal(t, "s", p.Summary)
	require.Equal(t, rowState{Status: "failed", Args: "{}", Summary: ""}, e.row(confirmed))
}

func TestDeleteLongExpiredKeepsRecentAndOtherUsers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenant, vault, conv := e.seed()
	other, otherVault, otherConv := e.seed()
	old := e.insert(tenant, vault, conv, time.Now())
	e.setExpiry(old, 25*time.Hour)
	recent := e.insert(tenant, vault, conv, time.Now())
	e.setExpiry(recent, 23*time.Hour)
	foreign := e.insert(other, otherVault, otherConv, time.Now())
	e.setExpiry(foreign, 25*time.Hour)

	require.NoError(t, e.store.DeleteLongExpired(ctx, tenant))

	require.Equal(t, 0, e.count(old))
	require.Equal(t, 1, e.count(recent))
	require.Equal(t, 1, e.count(foreign))
}

func TestCascadesFromUserVaultAndConversation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tenant, vault, conv := e.seed()
	byConv := e.insert(tenant, vault, conv, time.Now().Add(time.Hour))
	_, err := e.pool.Exec(ctx, `DELETE FROM conversations WHERE id = $1`, conv)
	require.NoError(t, err)
	require.Equal(t, 0, e.count(byConv))

	// A row proposed against another vault goes when that vault does.
	conv2 := uuid.New()
	_, vault2, _ := e.seed()
	_, err = e.pool.Exec(ctx, `INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 't', now(), now())`, conv2, tenant)
	require.NoError(t, err)
	byVault := e.insert(tenant, vault2, conv2, time.Now().Add(time.Hour))
	_, err = e.pool.Exec(ctx, `DELETE FROM vaults WHERE id = $1`, vault2)
	require.NoError(t, err)
	require.Equal(t, 0, e.count(byVault))

	conv3 := uuid.New()
	_, err = e.pool.Exec(ctx, `INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 't', now(), now())`, conv3, tenant)
	require.NoError(t, err)
	byUser := e.insert(tenant, vault, conv3, time.Now().Add(time.Hour))
	_, err = e.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, tenant)
	require.NoError(t, err)
	require.Equal(t, 0, e.count(byUser))
}

func TestReadyReportsTheTable(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ready, err := Ready(ctx, e.pool)
	require.NoError(t, err)
	require.True(t, ready)
	_, err = e.pool.Exec(ctx, `DROP TABLE assistant_pending_actions`)
	require.NoError(t, err)
	ready, err = Ready(ctx, e.pool)
	require.NoError(t, err)
	require.False(t, ready)
}

// A database that cannot answer is an error, not a missing table, so boot can
// say which it was.
func TestReadyReportsAFailedQueryAsAnError(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ready, err := Ready(ctx, e.pool)
	require.Error(t, err)
	require.False(t, ready)
}
