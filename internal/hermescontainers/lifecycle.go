package hermescontainers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrPortRangeExhausted is HermesContainerManager.Error.portRangeExhausted.
var ErrPortRangeExhausted = errors.New("portRangeExhausted")

// DockerRunFailedError is HermesContainerManager.Error.dockerRunFailed.
type DockerRunFailedError struct {
	Stderr   string
	ExitCode int
}

func (e *DockerRunFailedError) Error() string {
	return fmt.Sprintf("dockerRunFailed(stderr: %q, exitCode: %d)", e.Stderr, e.ExitCode)
}

// Containers is the part of HermesContainerManager the wave 6 routes call.
type Containers interface {
	Handle(ctx context.Context, tenant uuid.UUID) (*Handle, error)
	EnsureRunning(ctx context.Context, tenant uuid.UUID) (*Handle, error)
	RecordXaiConnected(ctx context.Context, tenant uuid.UUID, at *time.Time) error
	RecordNousConnected(ctx context.Context, tenant uuid.UUID, at *time.Time) error
	// SeedGatewayConfig answers the number of gateway variables written, or nil
	// when the tenant has no container yet.
	SeedGatewayConfig(ctx context.Context, tenant uuid.UUID) (*int, error)
	ApplyGatewayConfig(ctx context.Context, tenant uuid.UUID) error
}

// AudioTokens is HermesAudioTokenService: the scoped credential seeded into a
// tenant's .env for the audio proxy.
type AudioTokens interface {
	Mint(ctx context.Context, tenant uuid.UUID) (string, error)
	Revoke(ctx context.Context, tenant uuid.UUID) error
}

// Config is HermesContainerManager.Config.
type Config struct {
	Image          string
	Network        string
	DataRootBase   string
	PortRangeStart int
	PortRangeEnd   int
	IdleTTLSeconds int
	// DefaultModel is rendered into config.yaml.
	DefaultModel string
	// MnemosyneDefault applies when the tenant has no users row.
	MnemosyneDefault bool
	// MemoryLimit and CPULimit become --memory and --cpus; empty leaves them out.
	MemoryLimit string
	CPULimit    string
	// AudioProxyBaseURL includes /v1. Empty disables voice seeding.
	AudioProxyBaseURL string
}

// Lifecycle is HermesContainerManager: Manager plus the docker-backed lifecycle.
// Its methods hold one lock, as Swift's actor serialises them, so two requests
// for one tenant can't spawn twice or take the same port.
//
// Production runs in a pod with no docker: ProcessDocker then fails at spawn on
// the first call (the network check), and every container route answers Swift's
// empty 500.
type Lifecycle struct {
	*Manager
	Docker Docker
	Config Config
	// Audio mints the audio-proxy credential; nil disables voice seeding.
	Audio AudioTokens
	Now   func() time.Time
	Log   *slog.Logger

	mu            sync.Mutex
	imageOverride string
}

var _ Containers = (*Lifecycle)(nil)

func (l *Lifecycle) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Lifecycle) log() *slog.Logger {
	if l.Log != nil {
		return l.Log
	}
	return slog.Default()
}

// SetImage is setImage: future spawns use ref (after a Hermes self-update).
func (l *Lifecycle) SetImage(ref string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.imageOverride = ref
}

// CurrentImage is currentImage.
func (l *Lifecycle) CurrentImage() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.activeImage()
}

func (l *Lifecycle) activeImage() string {
	if l.imageOverride != "" {
		return l.imageOverride
	}
	return l.Config.Image
}

func (l *Lifecycle) volumePath(tenant uuid.UUID) string {
	return l.Config.DataRootBase + "/" + strings.ToLower(tenant.String())
}

type containerRow struct {
	id            uuid.UUID
	containerName string
	port          int
}

func (l *Lifecycle) loadRow(ctx context.Context, tenant uuid.UUID) (*containerRow, error) {
	var r containerRow
	err := l.pool.QueryRow(ctx, `SELECT id, container_name, port FROM hermes_tenant_containers WHERE tenant_id = $1 LIMIT 1`, tenant).
		Scan(&r.id, &r.containerName, &r.port)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // Swift's Optional
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (l *Lifecycle) touch(ctx context.Context, id uuid.UUID) error {
	_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET last_used_at = $2 WHERE id = $1`, id, l.now())
	return err
}

// EnsureRunning is ensureRunning: the tenant's handle, spawning the container
// on first use or restarting a stopped one, and bumping last_used_at.
func (l *Lifecycle) EnsureRunning(ctx context.Context, tenant uuid.UUID) (*Handle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ensureRunning(ctx, tenant)
}

func (l *Lifecycle) ensureRunning(ctx context.Context, tenant uuid.UUID) (*Handle, error) {
	if err := l.Docker.EnsureNetworkExists(ctx, l.Config.Network); err != nil {
		return nil, err
	}
	row, err := l.loadRow(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if row != nil {
		h, err := l.Handle(ctx, tenant)
		if err != nil {
			return nil, err
		}
		if up, err := l.Docker.IsRunning(ctx, row.containerName); err != nil || !up {
			if err := l.dockerRun(ctx, row.containerName, row.port, h.APIServerKey, tenant); err != nil {
				return nil, err
			}
		}
		if err := l.touch(ctx, row.id); err != nil {
			return nil, err
		}
		return h, nil
	}

	port, err := l.allocatePort(ctx)
	if err != nil {
		return nil, err
	}
	key, err := randomAPIKey()
	if err != nil {
		return nil, err
	}
	sealed, err := l.box.Seal(key, tenant)
	if err != nil {
		return nil, err
	}
	name := "hermes-tenant-" + strings.ToLower(tenant.String())
	id := uuid.New()
	now := l.now()
	if _, err := l.pool.Exec(ctx, `INSERT INTO hermes_tenant_containers
		(id, tenant_id, container_name, port, api_server_key_ciphertext, api_server_key_nonce, xai_connected_at, created_at, last_used_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULL, $7, $7)`, id, tenant, name, port, sealed.Ciphertext, sealed.Nonce, now); err != nil {
		return nil, err
	}
	if err := l.dockerRun(ctx, name, port, key, tenant); err != nil {
		// Roll the row back so a retry starts clean.
		_, _ = l.pool.Exec(context.WithoutCancel(ctx), `DELETE FROM hermes_tenant_containers WHERE id = $1`, id)
		return nil, err
	}
	l.log().Info("spawned hermes-tenant container", "tenantID", tenant, "container", name, "port", port)
	return &Handle{TenantID: tenant, ContainerName: name, Port: port, APIServerKey: key}, nil
}

// RecordXaiConnected is recordXaiConnected (at set) and recordXaiDisconnected
// (at nil). A tenant with no row is a no-op.
func (l *Lifecycle) RecordXaiConnected(ctx context.Context, tenant uuid.UUID, at *time.Time) error {
	if at != nil {
		_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET xai_connected_at = $2, last_used_at = $2
			WHERE tenant_id = $1`, tenant, *at)
		return err
	}
	_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET xai_connected_at = NULL WHERE tenant_id = $1`, tenant)
	return err
}

// RecordNousConnected is recordNousConnected / recordNousDisconnected.
func (l *Lifecycle) RecordNousConnected(ctx context.Context, tenant uuid.UUID, at *time.Time) error {
	if at != nil {
		_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET nous_connected_at = $2, last_used_at = $2
			WHERE tenant_id = $1`, tenant, *at)
		return err
	}
	_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET nous_connected_at = NULL WHERE tenant_id = $1`, tenant)
	return err
}

// EvictIdle is evictIdle: removes containers idle past the TTL with neither an
// xAI nor a Nous connection, and answers how many.
func (l *Lifecycle) EvictIdle(ctx context.Context) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.now().Add(-time.Duration(l.Config.IdleTTLSeconds) * time.Second)
	rows, err := l.pool.Query(ctx, `SELECT id, tenant_id, container_name FROM hermes_tenant_containers
		WHERE xai_connected_at IS NULL AND nous_connected_at IS NULL AND last_used_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	type stale struct {
		id, tenant uuid.UUID
		name       string
	}
	var list []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.tenant, &s.name); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, s := range list {
		_, _ = l.Docker.Run(ctx, []string{"rm", "-f", s.name})
		if _, err := l.pool.Exec(ctx, `DELETE FROM hermes_tenant_containers WHERE id = $1`, s.id); err != nil {
			return 0, err
		}
		l.log().Info("evicted idle hermes-tenant container", "tenantID", s.tenant, "container", s.name)
	}
	return len(list), nil
}

// ReprovisionAll is reprovisionAll: recreate every tenant container on the
// current image, re-seeding its config. Answers how many succeeded.
func (l *Lifecycle) ReprovisionAll(ctx context.Context) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.Docker.EnsureNetworkExists(ctx, l.Config.Network); err != nil {
		return 0, err
	}
	rows, err := l.pool.Query(ctx, `SELECT tenant_id FROM hermes_tenant_containers`)
	if err != nil {
		return 0, err
	}
	var tenants []uuid.UUID
	for rows.Next() {
		var t uuid.UUID
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return 0, err
		}
		tenants = append(tenants, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	count := 0
	for _, t := range tenants {
		row, err := l.loadRow(ctx, t)
		if err != nil || row == nil {
			continue
		}
		_, _ = l.Docker.Run(ctx, []string{"rm", "-f", row.containerName})
		err = l.recreate(ctx, row, t)
		if err != nil {
			l.log().Error("reprovision failed for hermes-tenant container", "tenantID", t, "container", row.containerName, "error", err)
			continue
		}
		count++
	}
	l.log().Info("reprovisioned hermes-tenant containers", "count", count)
	return count, nil
}

func (l *Lifecycle) recreate(ctx context.Context, row *containerRow, tenant uuid.UUID) error {
	h, err := l.Handle(ctx, tenant)
	if err != nil {
		return err
	}
	if err := l.dockerRun(ctx, row.containerName, row.port, h.APIServerKey, tenant); err != nil {
		return err
	}
	return l.touch(ctx, row.id)
}

// SeedGatewayConfig is seedGatewayConfig: writes config.yaml and .env from the
// gateway rows without restarting. Nil when the tenant has no container.
func (l *Lifecycle) SeedGatewayConfig(ctx context.Context, tenant uuid.UUID) (*int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	h, err := l.Handle(ctx, tenant)
	if err != nil || h == nil {
		return nil, err
	}
	gateways, err := l.gatewaySeeds(ctx, tenant)
	if err != nil {
		return nil, err
	}
	mnemosyne, err := l.mnemosyneEnabled(ctx, tenant)
	if err != nil {
		return nil, err
	}
	volume := l.volumePath(tenant)
	if err := Seed(volume, h.APIServerKey, l.Config.DefaultModel, gateways, mnemosyne, l.audioProxySeed(ctx, tenant, volume)); err != nil {
		return nil, err
	}
	n := 0
	for _, g := range gateways {
		n += len(GatewayEnvVars(g.GatewayID, g.Config))
	}
	return &n, nil
}

// ApplyGatewayConfig is applyGatewayConfig: recreate the container so a freshly
// seeded .env takes effect, rotating the audio credential on the way. With no
// container yet it spawns one.
func (l *Lifecycle) ApplyGatewayConfig(ctx context.Context, tenant uuid.UUID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.Docker.EnsureNetworkExists(ctx, l.Config.Network); err != nil {
		return err
	}
	row, err := l.loadRow(ctx, tenant)
	if err != nil {
		return err
	}
	if row == nil {
		_, err := l.ensureRunning(ctx, tenant)
		return err
	}
	_, _ = l.Docker.Run(ctx, []string{"rm", "-f", row.containerName})
	l.rotateAudioToken(ctx, tenant, l.volumePath(tenant))
	return l.recreate(ctx, row, tenant)
}

func (l *Lifecycle) allocatePort(ctx context.Context) (int, error) {
	rows, err := l.pool.Query(ctx, `SELECT port FROM hermes_tenant_containers`)
	if err != nil {
		return 0, err
	}
	taken := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return 0, err
		}
		taken[p] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for p := l.Config.PortRangeStart; p < l.Config.PortRangeEnd; p++ {
		if !taken[p] {
			return p, nil
		}
	}
	return 0, ErrPortRangeExhausted
}

func randomAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (l *Lifecycle) mnemosyneEnabled(ctx context.Context, tenant uuid.UUID) (bool, error) {
	var on bool
	err := l.pool.QueryRow(ctx, `SELECT mnemosyne_enabled FROM users WHERE id = $1`, tenant).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return l.Config.MnemosyneDefault, nil
	}
	return on, err
}

// RunArgs is the `docker run` command dockerRun issues.
func (l *Lifecycle) RunArgs(name string, port int, apiKey, volume string) []string {
	args := []string{"run", "--detach", "--restart=unless-stopped", "--name", name, "--network", l.Config.Network}
	if l.Config.MemoryLimit != "" {
		args = append(args, "--memory", l.Config.MemoryLimit)
	}
	if l.Config.CPULimit != "" {
		args = append(args, "--cpus", l.Config.CPULimit)
	}
	return append(args,
		"--publish", strconv.Itoa(port)+":8642",
		"--volume", volume+":/opt/data",
		"--env", "API_SERVER_ENABLED=true",
		"--env", "API_SERVER_HOST=0.0.0.0",
		"--env", "API_SERVER_PORT=8642",
		"--env", "API_SERVER_KEY="+apiKey,
		"--env", "HERMES_HOME=/opt/data",
		"--env", "HOME=/opt/data",
		"--env", "MNEMOSYNE_DATA_DIR=/opt/data/mnemosyne",
		l.activeImage(),
		"gateway", "run",
	)
}

func (l *Lifecycle) dockerRun(ctx context.Context, name string, port int, apiKey string, tenant uuid.UUID) error {
	volume := l.volumePath(tenant)
	gateways, err := l.gatewaySeeds(ctx, tenant)
	if err != nil {
		return err
	}
	mnemosyne, err := l.mnemosyneEnabled(ctx, tenant)
	if err != nil {
		return err
	}
	if err := Seed(volume, apiKey, l.Config.DefaultModel, gateways, mnemosyne, l.audioProxySeed(ctx, tenant, volume)); err != nil {
		return err
	}
	res, err := l.Docker.Run(ctx, l.RunArgs(name, port, apiKey, volume))
	if err != nil {
		return err
	}
	if res.OK() {
		return nil
	}
	if strings.Contains(res.Stderr, "is already in use") {
		start, err := l.Docker.Run(ctx, []string{"start", name})
		if err != nil {
			return err
		}
		if start.OK() {
			return nil
		}
	}
	return &DockerRunFailedError{Stderr: res.Stderr, ExitCode: res.ExitCode}
}

// gatewaySeeds is gatewaySeeds: the tenant's gateway rows, decrypted. Rows that
// won't open or decode, or name an unknown gateway, are skipped.
func (l *Lifecycle) gatewaySeeds(ctx context.Context, tenant uuid.UUID) ([]GatewaySeed, error) {
	rows, err := l.pool.Query(ctx, `SELECT gateway_id, config_ciphertext, config_nonce FROM user_hermes_gateways WHERE tenant_id = $1`, tenant)
	if err != nil {
		return nil, err
	}
	type raw struct {
		id         string
		cipher, nc []byte
	}
	var all []raw
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.id, &r.cipher, &r.nc); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var seeds []GatewaySeed
	for _, r := range all {
		if !knownGateways[r.id] {
			continue
		}
		plain, err := l.box.Open(sealedOf(r.cipher, r.nc), tenant)
		var cfg map[string]string
		if err == nil {
			err = json.Unmarshal([]byte(plain), &cfg)
		}
		if err != nil {
			l.log().Error("skipping undecodable hermes gateway config", "tenantID", tenant, "gateway", r.id, "error", err)
			continue
		}
		seeds = append(seeds, GatewaySeed{GatewayID: r.id, Config: cfg})
	}
	return seeds, nil
}

// audioProxySeed reuses the credential already in the volume's .env so seeding
// stays idempotent; otherwise it mints one. A mint failure seeds no voice.
func (l *Lifecycle) audioProxySeed(ctx context.Context, tenant uuid.UUID, volume string) *AudioProxySeed {
	if l.Config.AudioProxyBaseURL == "" || l.Audio == nil {
		return nil
	}
	if tok := existingAudioToken(volume); tok != "" {
		return NewAudioProxySeed(l.Config.AudioProxyBaseURL, tok)
	}
	tok, err := l.Audio.Mint(ctx, tenant)
	if err != nil {
		l.log().Warn(fmt.Sprintf("audio token mint failed for %s: %v", strings.ToUpper(tenant.String()), err))
		return nil
	}
	return NewAudioProxySeed(l.Config.AudioProxyBaseURL, tok)
}

const voiceKeyPrefix = "VOICE_TOOLS_OPENAI_KEY="

func existingAudioToken(volume string) string {
	data, err := os.ReadFile(volume + "/.env") //nolint:gosec // under the operator's data root
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, voiceKeyPrefix) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, voiceKeyPrefix))
		if len(v) >= 2 && strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
			v = v[1 : len(v)-1]
		}
		return v
	}
	return ""
}

func (l *Lifecycle) rotateAudioToken(ctx context.Context, tenant uuid.UUID, volume string) {
	if l.Config.AudioProxyBaseURL == "" || l.Audio == nil {
		return
	}
	if err := l.Audio.Revoke(ctx, tenant); err != nil {
		l.log().Warn(fmt.Sprintf("audio token rotation failed for %s: %v", strings.ToUpper(tenant.String()), err))
		return
	}
	path := volume + "/.env"
	data, err := os.ReadFile(path) //nolint:gosec // under the operator's data root
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, voiceKeyPrefix) {
			kept = append(kept, line)
		}
	}
	_ = os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644) //nolint:gosec // as Swift writes it
}
