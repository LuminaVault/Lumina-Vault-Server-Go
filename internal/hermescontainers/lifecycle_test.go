package hermescontainers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/migrations"
	"lumina/internal/secretbox"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

type invocation struct {
	kind      string
	args      []string
	container string
}

// stubDocker is StubDockerExec: it records every call, tracks which containers
// run, and answers queued results by argument prefix.
type stubDocker struct {
	mu          sync.Mutex
	invocations []invocation
	running     map[string]bool
	queued      []queuedResult
	networkErr  error
}

type queuedResult struct {
	prefix []string
	result Result
}

func newStubDocker() *stubDocker { return &stubDocker{running: map[string]bool{}} }

func (d *stubDocker) enqueue(r Result, prefix ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queued = append(d.queued, queuedResult{prefix, r})
}

func (d *stubDocker) setRunning(name string, up bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.running[name] = up
}

func (d *stubDocker) match(args []string) Result {
	for _, q := range d.queued {
		if len(args) >= len(q.prefix) && slices.Equal(args[:len(q.prefix)], q.prefix) {
			return q.result
		}
	}
	return Result{}
}

func (d *stubDocker) Run(_ context.Context, args []string) (Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.invocations = append(d.invocations, invocation{kind: "run", args: args})
	if len(args) > 0 && args[0] == "run" {
		if i := slices.Index(args, "--name"); i >= 0 && i+1 < len(args) {
			d.running[args[i+1]] = true
		}
	}
	if len(args) > 0 && args[0] == "rm" {
		delete(d.running, args[len(args)-1])
	}
	return d.match(args), nil
}

func (d *stubDocker) Exec(_ context.Context, container string, command []string, _ []byte) (Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.invocations = append(d.invocations, invocation{kind: "exec", args: command, container: container})
	return d.match(command), nil
}

func (d *stubDocker) ExecStreaming(context.Context, string, []string) (Streaming, error) {
	return nil, errors.New("not used")
}

func (d *stubDocker) IsRunning(_ context.Context, container string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.invocations = append(d.invocations, invocation{kind: "isRunning", container: container})
	return d.running[container], nil
}

func (d *stubDocker) EnsureNetworkExists(_ context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.invocations = append(d.invocations, invocation{kind: "ensureNetwork", args: []string{name}})
	return d.networkErr
}

func (d *stubDocker) dockerRuns() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out [][]string
	for _, inv := range d.invocations {
		if inv.kind == "run" && len(inv.args) > 0 && inv.args[0] == "run" {
			out = append(out, inv.args)
		}
	}
	return out
}

type fakeAudio struct {
	mints, revokes int
}

func (f *fakeAudio) Mint(context.Context, uuid.UUID) (string, error) {
	f.mints++
	return "audio-token-" + string(rune('0'+f.mints)), nil
}

func (f *fakeAudio) Revoke(context.Context, uuid.UUID) error {
	f.revokes++
	return nil
}

type lifecycleEnv struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
	root string
}

func newLifecycleEnv(t *testing.T) lifecycleEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	require.NoError(t, err)
	return lifecycleEnv{pool: pool, box: box, root: t.TempDir()}
}

func (e lifecycleEnv) manager(d Docker, portStart, portEnd int, memory, cpu string) *Lifecycle {
	return &Lifecycle{
		Manager: New(e.pool, e.box),
		Docker:  d,
		Config: Config{
			Image: "hermes:test", Network: "lvtest", DataRootBase: e.root,
			PortRangeStart: portStart, PortRangeEnd: portEnd, IdleTTLSeconds: 60,
			DefaultModel: "hermes-3", MnemosyneDefault: true, MemoryLimit: memory, CPULimit: cpu,
		},
	}
}

func (e lifecycleEnv) user(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO users (id, email, username, password_hash) VALUES ($1, $2, $3, 'x')`,
		id, id.String()+"@test.luminavault", "her240a-"+strings.ReplaceAll(id.String(), "-", "")[:16])
	require.NoError(t, err)
	return id
}

func (e lifecycleEnv) age(t *testing.T, tenant uuid.UUID) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(), `UPDATE hermes_tenant_containers SET last_used_at = now() - interval '120 seconds' WHERE tenant_id = $1`, tenant)
	require.NoError(t, err)
}

func (e lifecycleEnv) tenants(t *testing.T) []uuid.UUID {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT tenant_id FROM hermes_tenant_containers`)
	require.NoError(t, err)
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		out = append(out, id)
	}
	return out
}

// Ported from HermesContainerManagerTests: ensureRunning spawns on first call and is idempotent on second.
func TestEnsureRunningSpawnsOnceThenReuses(t *testing.T) {
	e := newLifecycleEnv(t)
	ctx := context.Background()
	tenant := e.user(t)
	d := newStubDocker()
	m := e.manager(d, 9000, 9100, "", "")

	first, err := m.EnsureRunning(ctx, tenant)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(first.ContainerName, "hermes-tenant-"))
	require.GreaterOrEqual(t, first.Port, 9000)
	require.Less(t, first.Port, 9100)
	require.Len(t, d.dockerRuns(), 1)

	second, err := m.EnsureRunning(ctx, tenant)
	require.NoError(t, err)
	require.Equal(t, first.ContainerName, second.ContainerName)
	require.Equal(t, first.Port, second.Port)
	require.Equal(t, first.APIServerKey, second.APIServerKey)
	require.Len(t, d.dockerRuns(), 1, "second call must not spawn a new container")

	h, err := m.Handle(ctx, tenant)
	require.NoError(t, err)
	require.Equal(t, first.ContainerName, h.ContainerName)
	require.Nil(t, h.XaiConnectedAt)

	// The seed lands at the volume root before docker run.
	env, err := os.ReadFile(e.root + "/" + strings.ToLower(tenant.String()) + "/.env")
	require.NoError(t, err)
	require.Contains(t, string(env), "API_SERVER_KEY="+first.APIServerKey)
}

// Ported from HermesContainerManagerTests: configured resource limits reach docker run.
func TestResourceLimitsReachDockerRun(t *testing.T) {
	e := newLifecycleEnv(t)
	d := newStubDocker()
	_, err := e.manager(d, 9000, 9100, "512m", "1.5").EnsureRunning(context.Background(), e.user(t))
	require.NoError(t, err)
	args := d.dockerRuns()[0]
	image := slices.Index(args, "hermes:test")
	require.Contains(t, args, "512m")
	require.Contains(t, args, "1.5")
	require.Less(t, slices.Index(args, "--memory"), image)
	require.Less(t, slices.Index(args, "--cpus"), image)
	require.GreaterOrEqual(t, slices.Index(args, "--memory"), 0)
	require.GreaterOrEqual(t, slices.Index(args, "--cpus"), 0)
}

// Ported from HermesContainerManagerTests: unset resource limits emit no flags.
func TestUnsetResourceLimitsEmitNoFlags(t *testing.T) {
	e := newLifecycleEnv(t)
	d := newStubDocker()
	_, err := e.manager(d, 9000, 9100, "", "").EnsureRunning(context.Background(), e.user(t))
	require.NoError(t, err)
	args := d.dockerRuns()[0]
	require.NotContains(t, args, "--memory")
	require.NotContains(t, args, "--cpus")
}

// Ported from HermesContainerManagerTests: ensureRunning restarts stopped container without reallocating port.
func TestEnsureRunningRestartsStopped(t *testing.T) {
	e := newLifecycleEnv(t)
	ctx := context.Background()
	tenant := e.user(t)
	d := newStubDocker()
	m := e.manager(d, 9000, 9100, "", "")
	first, err := m.EnsureRunning(ctx, tenant)
	require.NoError(t, err)
	d.setRunning(first.ContainerName, false)
	second, err := m.EnsureRunning(ctx, tenant)
	require.NoError(t, err)
	require.Equal(t, first.Port, second.Port)
	require.Equal(t, first.ContainerName, second.ContainerName)
	require.Len(t, d.dockerRuns(), 2)
}

// Ported from HermesContainerManagerTests: evictIdle removes only stale containers without xai connection.
func TestEvictIdleRemovesOnlyStaleUnconnected(t *testing.T) {
	e := newLifecycleEnv(t)
	ctx := context.Background()
	idle, active, connected := e.user(t), e.user(t), e.user(t)
	m := e.manager(newStubDocker(), 9000, 9100, "", "")
	for _, id := range []uuid.UUID{idle, active, connected} {
		_, err := m.EnsureRunning(ctx, id)
		require.NoError(t, err)
	}
	now := time.Now()
	require.NoError(t, m.RecordXaiConnected(ctx, connected, &now))
	e.age(t, idle)

	n, err := m.EvictIdle(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	left := e.tenants(t)
	require.Contains(t, left, active)
	require.Contains(t, left, connected)
	require.NotContains(t, left, idle)
}

// Ported from HermesContainerManagerTests: evictIdle never reaps a stale Nous-connected container.
func TestEvictIdleKeepsNousConnected(t *testing.T) {
	e := newLifecycleEnv(t)
	ctx := context.Background()
	tenant := e.user(t)
	m := e.manager(newStubDocker(), 9000, 9100, "", "")
	_, err := m.EnsureRunning(ctx, tenant)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, m.RecordNousConnected(ctx, tenant, &now))
	e.age(t, tenant)

	n, err := m.EvictIdle(ctx)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Contains(t, e.tenants(t), tenant)
}

// Ported from HermesContainerManagerTests: port range exhaustion throws.
func TestPortRangeExhaustion(t *testing.T) {
	e := newLifecycleEnv(t)
	ctx := context.Background()
	m := e.manager(newStubDocker(), 9000, 9001, "", "")
	_, err := m.EnsureRunning(ctx, e.user(t))
	require.NoError(t, err)
	_, err = m.EnsureRunning(ctx, e.user(t))
	require.ErrorIs(t, err, ErrPortRangeExhausted)
}

// New: a failed docker run rolls the row back; "already in use" falls back to start.
func TestDockerRunFailure(t *testing.T) {
	e := newLifecycleEnv(t)
	ctx := context.Background()
	tenant := e.user(t)
	d := newStubDocker()
	d.enqueue(Result{Stderr: "boom", ExitCode: 125}, "run")
	m := e.manager(d, 9000, 9100, "", "")
	_, err := m.EnsureRunning(ctx, tenant)
	var failed *DockerRunFailedError
	require.ErrorAs(t, err, &failed)
	require.Equal(t, 125, failed.ExitCode)
	require.NotContains(t, e.tenants(t), tenant, "the row is rolled back")

	d2 := newStubDocker()
	d2.enqueue(Result{Stderr: `Conflict. The container name "/x" is already in use`, ExitCode: 125}, "run")
	_, err = e.manager(d2, 9000, 9100, "", "").EnsureRunning(ctx, tenant)
	require.NoError(t, err)
	require.Equal(t, "start", d2.invocations[len(d2.invocations)-1].args[0])
}

// New: no docker in the pod fails at the network check, as in Swift.
func TestNoDockerFailsAtSpawn(t *testing.T) {
	e := newLifecycleEnv(t)
	m := e.manager(ProcessDocker{BinaryPath: "/nonexistent/docker"}, 9000, 9100, "", "")
	_, err := m.EnsureRunning(context.Background(), e.user(t))
	var spawn *SpawnError
	require.ErrorAs(t, err, &spawn)
	require.Empty(t, e.tenants(t))
}

// New: seedGatewayConfig and applyGatewayConfig seed the gateway env-vars and
// recreate the container, rotating the audio credential.
func TestGatewaySeedAndApply(t *testing.T) {
	e := newLifecycleEnv(t)
	ctx := context.Background()
	tenant := e.user(t)
	d := newStubDocker()
	m := e.manager(d, 9000, 9100, "", "")
	audio := &fakeAudio{}
	m.Audio = audio
	m.Config.AudioProxyBaseURL = "https://api.example/v1"

	n, err := m.SeedGatewayConfig(ctx, tenant)
	require.NoError(t, err)
	require.Nil(t, n, "no container yet")

	h, err := m.EnsureRunning(ctx, tenant)
	require.NoError(t, err)
	require.Equal(t, 1, audio.mints)

	cfg, _ := json.Marshal(map[string]string{"bot_token": "tok", "allowed_users": "1"})
	sealed, err := e.box.Seal(string(cfg), tenant)
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `INSERT INTO user_hermes_gateways (id, tenant_id, gateway_id, config_ciphertext, config_nonce, status)
		VALUES ($1, $2, 'telegram', $3, $4, 'configured'), ($5, $2, 'signal', $3, $4, 'configured')`,
		uuid.New(), tenant, sealed.Ciphertext, sealed.Nonce, uuid.New())
	require.NoError(t, err)

	n, err = m.SeedGatewayConfig(ctx, tenant)
	require.NoError(t, err)
	require.Equal(t, 2, *n)
	require.Equal(t, 1, audio.mints, "the token on disk is reused")
	envPath := e.root + "/" + strings.ToLower(tenant.String()) + "/.env"
	env, err := os.ReadFile(envPath)
	require.NoError(t, err)
	require.Contains(t, string(env), "TELEGRAM_BOT_TOKEN='tok'")
	require.Contains(t, string(env), "VOICE_TOOLS_OPENAI_KEY='audio-token-1'")

	require.NoError(t, m.ApplyGatewayConfig(ctx, tenant))
	require.Equal(t, 1, audio.revokes)
	require.Equal(t, 2, audio.mints, "apply rotates the audio credential")
	require.Len(t, d.dockerRuns(), 2)
	after, err := m.Handle(ctx, tenant)
	require.NoError(t, err)
	require.Equal(t, h.Port, after.Port)
}

// New: reprovisionAll recreates every container on the current image.
func TestReprovisionAll(t *testing.T) {
	e := newLifecycleEnv(t)
	ctx := context.Background()
	d := newStubDocker()
	m := e.manager(d, 9000, 9100, "", "")
	for range 2 {
		_, err := m.EnsureRunning(ctx, e.user(t))
		require.NoError(t, err)
	}
	m.SetImage("hermes:new")
	n, err := m.ReprovisionAll(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	runs := d.dockerRuns()
	require.Len(t, runs, 4)
	require.Contains(t, runs[3], "hermes:new")
	require.Equal(t, "hermes:new", m.CurrentImage())
}
