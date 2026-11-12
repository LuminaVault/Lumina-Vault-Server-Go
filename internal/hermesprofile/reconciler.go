package hermesprofile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// deletedPrefix marks a profile directory reapOrphans already moved aside.
const deletedPrefix = "_deleted_"

// ReconcileSummary is HermesProfileReconcileSummary.
type ReconcileSummary struct {
	UsersScanned      int      `json:"usersScanned"`
	ProfilesCreated   int      `json:"profilesCreated"`
	ProfilesRecovered int      `json:"profilesRecovered"`
	ProfilesAlreadyOK int      `json:"profilesAlreadyOK"`
	Failures          []string `json:"failures"`
}

// ReapSummary is HermesProfileReapSummary.
type ReapSummary struct {
	DirsScanned        int      `json:"dirsScanned"`
	OrphansSoftDeleted []string `json:"orphansSoftDeleted"`
	ActiveKept         int      `json:"activeKept"`
}

// Health is HermesProfileHealth. GatewayLatencyMs is left out when nil, as
// Swift's synthesized encoder leaves out a nil optional.
type Health struct {
	TotalUsers           int  `json:"totalUsers"`
	ProfilesReady        int  `json:"profilesReady"`
	ProfilesProvisioning int  `json:"profilesProvisioning"`
	ProfilesError        int  `json:"profilesError"`
	UsersWithoutProfile  int  `json:"usersWithoutProfile"`
	OrphanFilesystemDirs int  `json:"orphanFilesystemDirs"`
	GatewayReachable     bool `json:"gatewayReachable"`
	GatewayLatencyMs     *int `json:"gatewayLatencyMs,omitempty"`
}

// Reconciler is HermesProfileReconciler (Admin/HermesProfileReconciler.swift):
// the admin routes' view of hermes_profiles against the users table and the
// profile directories on the Hermes volume. Its daily job,
// HermesProfileReconcilerService, is cmd/App/profilereconciler.go.
type Reconciler struct {
	pool       *pgxpool.Pool
	service    *Service
	dataRoot   string
	gatewayURL string
	probe      *GatewayProbe
	now        func() time.Time
}

// NewReconciler returns a Reconciler. dataRoot is HERMES_DATA_ROOT, which holds
// profiles/<hermesProfileID>/.
func NewReconciler(pool *pgxpool.Pool, service *Service, dataRoot, gatewayURL string, probe *GatewayProbe) *Reconciler {
	return &Reconciler{pool: pool, service: service, dataRoot: dataRoot, gatewayURL: gatewayURL, probe: probe, now: time.Now}
}

type userRow struct {
	id       uuid.UUID
	username string
}

func (r *Reconciler) users(ctx context.Context) ([]userRow, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, username FROM users`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []userRow
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.id, &u.username); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Reconcile is reconcile(): Ensure for every user, counted by the row's status
// before the call. A failed read of the row before Ensure fails the whole sweep;
// a failed Ensure is a failure line, as in Swift.
func (r *Reconciler) Reconcile(ctx context.Context) (ReconcileSummary, error) {
	users, err := r.users(ctx)
	if err != nil {
		return ReconcileSummary{}, err
	}
	sum := ReconcileSummary{UsersScanned: len(users), Failures: []string{}}
	for _, u := range users {
		existing, err := r.service.Find(ctx, u.id)
		if err != nil {
			return ReconcileSummary{}, err
		}
		if err := r.service.Ensure(ctx, u.id); err != nil {
			sum.Failures = append(sum.Failures, fmt.Sprintf("%s: %v", u.username, err))
			slog.WarnContext(ctx, "reconcile failed", "username", u.username, "error", err)
			continue
		}
		switch {
		case existing == nil:
			sum.ProfilesCreated++
		case existing.Status == statusError || existing.Status == statusProvisioning:
			sum.ProfilesRecovered++
		default:
			sum.ProfilesAlreadyOK++
		}
	}
	slog.InfoContext(ctx, "reconcile", "scanned", len(users), "created", sum.ProfilesCreated,
		"recovered", sum.ProfilesRecovered, "ok", sum.ProfilesAlreadyOK, "failures", len(sum.Failures))
	return sum, nil
}

func (r *Reconciler) profilesDir() string { return filepath.Join(r.dataRoot, "profiles") }

func (r *Reconciler) activeIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := r.pool.Query(ctx, `SELECT hermes_profile_id FROM hermes_profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// entryNames is FileManager.contentsOfDirectory(atPath:): every name, hidden ones too.
func entryNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

// ReapOrphans is reapOrphans(): a profile directory no hermes_profiles row names is
// renamed to _deleted_<unix seconds>_<name>. dirsScanned counts the _deleted_
// entries it skips, as in Swift. A failed rename is logged and skipped.
func (r *Reconciler) ReapOrphans(ctx context.Context) (ReapSummary, error) {
	dir := r.profilesDir()
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return ReapSummary{OrphansSoftDeleted: []string{}}, nil
	}
	names, err := entryNames(dir)
	if err != nil {
		return ReapSummary{}, err
	}
	active, err := r.activeIDs(ctx)
	if err != nil {
		return ReapSummary{}, err
	}
	sum := ReapSummary{DirsScanned: len(names), OrphansSoftDeleted: []string{}}
	for _, name := range names {
		if strings.HasPrefix(name, deletedPrefix) {
			continue
		}
		if active[name] {
			sum.ActiveKept++
			continue
		}
		stamped := deletedPrefix + strconv.FormatInt(r.now().Unix(), 10) + "_" + name
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, stamped)); err != nil {
			slog.WarnContext(ctx, "reap failed", "entry", name, "error", err)
			continue
		}
		sum.OrphansSoftDeleted = append(sum.OrphansSoftDeleted, name)
		slog.InfoContext(ctx, "reaped orphan profile dir", "entry", name)
	}
	return sum, nil
}

// Health is health(): counts by status, users with no row, orphan directories (0
// when the directory can't be read) and the gateway probe.
func (r *Reconciler) Health(ctx context.Context) (Health, error) {
	var h Health
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&h.TotalUsers); err != nil {
		return Health{}, err
	}
	rows, err := r.pool.Query(ctx, `SELECT tenant_id, hermes_profile_id, status FROM hermes_profiles`)
	if err != nil {
		return Health{}, err
	}
	defer rows.Close()
	tenants := map[uuid.UUID]bool{}
	active := map[string]bool{}
	for rows.Next() {
		var tenant uuid.UUID
		var hermesID, status string
		if err := rows.Scan(&tenant, &hermesID, &status); err != nil {
			return Health{}, err
		}
		switch status {
		case statusReady:
			h.ProfilesReady++
		case statusProvisioning:
			h.ProfilesProvisioning++
		case statusError:
			h.ProfilesError++
		}
		tenants[tenant] = true
		active[hermesID] = true
	}
	if err := rows.Err(); err != nil {
		return Health{}, err
	}
	h.UsersWithoutProfile = max(0, h.TotalUsers-len(tenants))
	if names, err := entryNames(r.profilesDir()); err == nil {
		for _, name := range names {
			if !strings.HasPrefix(name, deletedPrefix) && !active[name] {
				h.OrphanFilesystemDirs++
			}
		}
	}
	probe := r.probe.Probe(ctx, r.gatewayURL)
	h.GatewayReachable, h.GatewayLatencyMs = probe.Reachable, probe.LatencyMs
	return h, nil
}

// ProbeResult is HermesGatewayProbe.ProbeResult.
type ProbeResult struct {
	Reachable bool
	LatencyMs *int
	CheckedAt time.Time
}

// GatewayProbe is HermesGatewayProbe: GET <gateway>/v1/models with a 1 s timeout,
// cached per URL for 30 s. Any HTTP response, whatever its status, is reachable.
type GatewayProbe struct {
	client *http.Client
	apiKey string
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]ProbeResult
}

// NewGatewayProbe returns a probe that sends apiKey as a bearer when it isn't empty.
func NewGatewayProbe(apiKey string) *GatewayProbe {
	return &GatewayProbe{
		client: &http.Client{Timeout: time.Second},
		apiKey: apiKey, ttl: 30 * time.Second, now: time.Now,
		cache: map[string]ProbeResult{},
	}
}

// Probe is probe(gatewayURL:). Swift's actor serializes callers, so a cache miss
// probes once while the others wait; the mutex does the same.
func (p *GatewayProbe) Probe(ctx context.Context, gatewayURL string) ProbeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if cached, ok := p.cache[gatewayURL]; ok && now.Sub(cached.CheckedAt) < p.ttl {
		return cached
	}
	result := p.run(ctx, gatewayURL, now)
	p.cache[gatewayURL] = result
	return result
}

func (p *GatewayProbe) run(ctx context.Context, gatewayURL string, now time.Time) ProbeResult {
	down := ProbeResult{CheckedAt: now}
	base, err := url.Parse(gatewayURL)
	if err != nil {
		slog.WarnContext(ctx, "hermes.probe invalid gateway URL", "url", gatewayURL)
		return down
	}
	target := base.JoinPath("v1", "models")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return down
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	start := time.Now()
	resp, err := p.client.Do(req)
	if err != nil {
		slog.DebugContext(ctx, "hermes.probe failed", "url", gatewayURL, "error", err)
		return down
	}
	_ = resp.Body.Close()
	ms := max(0, int(time.Since(start).Milliseconds()))
	return ProbeResult{Reachable: true, LatencyMs: &ms, CheckedAt: now}
}
