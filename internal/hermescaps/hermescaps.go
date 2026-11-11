// Package hermescaps is HermesRemoteCapabilitiesService: what a tenant's
// connected Hermes exposes over HTTP, so clients can gate each settings pane.
// Managed tenants report the managed default with no network call. BYO tenants
// get a probe of their remote api_server (/health, /v1/capabilities, /api/jobs),
// plus a dashboard probe (/api/status, /api/skills) when the row carries a
// dashboard URL and sealed token. Results are cached on user_hermes_config.
package hermescaps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/hermesresolve"
	"lumina/internal/ids"
	"lumina/internal/secretbox"
	"lumina/internal/ssrfguard"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Availability is HermesDomainAvailability.
type Availability string

const (
	Live        Availability = "live"
	ReadOnly    Availability = "read_only"
	Unsupported Availability = "unsupported"
	Managed     Availability = "managed"
)

// AuthMode is HermesDashboardAuthMode.
type AuthMode string

const (
	Bearer       AuthMode = "bearer"
	OAuthOnly    AuthMode = "oauth_only"
	Unauthorized AuthMode = "unauthorized"
	Unreachable  AuthMode = "unreachable"
)

// Dashboard is HermesDashboardCapabilitiesDTO.
type Dashboard struct {
	Reachable   bool     `json:"reachable"`
	AuthMode    AuthMode `json:"authMode"`
	SkillsWrite bool     `json:"skillsWrite"`
	Cron        bool     `json:"cron"`
	FS          bool     `json:"fs"`
	Sessions    bool     `json:"sessions"`
	Version     *string  `json:"version,omitempty"`
	KBVaultPath *string  `json:"kbVaultPath,omitempty"`
}

// Capabilities is HermesCapabilities, fields in declaration order so the cached
// JSON is the bytes Swift's JSONEncoder writes.
type Capabilities struct {
	IsUserOverride              bool          `json:"isUserOverride"`
	RemoteVersion               *string       `json:"remoteVersion,omitempty"`
	Chat                        Availability  `json:"chat"`
	Sessions                    Availability  `json:"sessions"`
	Jobs                        Availability  `json:"jobs"`
	Skills                      Availability  `json:"skills"`
	Soul                        Availability  `json:"soul"`
	Gateways                    Availability  `json:"gateways"`
	Memory                      Availability  `json:"memory"`
	Providers                   Availability  `json:"providers"`
	MultimodalIngestion         *Availability `json:"multimodalIngestion,omitempty"`
	IngestionSupportedMimeTypes *[]string     `json:"ingestionSupportedMimeTypes,omitempty"`
	IngestionMaxSourceBytes     *int64        `json:"ingestionMaxSourceBytes,omitempty"`
	IngestionRemoteSourceURL    *bool         `json:"ingestionRemoteSourceURL,omitempty"`
	Dashboard                   *Dashboard    `json:"dashboard,omitempty"`
}

// Response is HermesCapabilitiesResponse.
type Response struct {
	Capabilities Capabilities `json:"capabilities"`
	CheckedAt    *wire.Time   `json:"checkedAt,omitempty"`
}

func ptr[T any](v T) *T { return &v }

// ManagedDefault is HermesCapabilities.managedDefault.
func ManagedDefault() Capabilities {
	return Capabilities{
		Chat: Managed, Sessions: Managed, Jobs: Managed, Skills: Managed,
		Soul: Managed, Gateways: Managed, Memory: Managed, Providers: Managed,
		MultimodalIngestion:         ptr(Managed),
		IngestionSupportedMimeTypes: &[]string{"application/pdf", "image/*", "audio/*", "video/*", "text/html"},
		IngestionMaxSourceBytes:     ptr(int64(2 * 1024 * 1024 * 1024)),
		IngestionRemoteSourceURL:    ptr(false),
	}
}

// Service is the probe and its cache.
type Service struct {
	pool     *pgxpool.Pool
	resolver *hermesresolve.Resolver
	box      *secretbox.Box
	guard    ssrfguard.Guard
	// probe is BYOHTTP.session: no redirects.
	probe *http.Client
	// dashboard is BYOHTTP.httpClient: no redirects, 15 s per request.
	dashboard *http.Client
	ttl       time.Duration
	log       *slog.Logger
	Now       func() time.Time
}

func noRedirect(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// New builds the service. Swift builds it only with a secret box (the resolver
// needs one), so callers pass nil-safe values only when box is set.
func New(pool *pgxpool.Pool, resolver *hermesresolve.Resolver, box *secretbox.Box, guard ssrfguard.Guard) *Service {
	return &Service{
		pool: pool, resolver: resolver, box: box, guard: guard,
		probe: noRedirect(8 * time.Second), dashboard: noRedirect(dashboardTimeout),
		ttl: time.Hour, log: slog.Default(), Now: time.Now,
	}
}

type configRow struct {
	exists              bool
	dashboardURL        *string
	tokenCT, tokenNonce []byte
	cached              *string
	checkedAt           *time.Time
}

func (s *Service) load(ctx context.Context, tenant uuid.UUID) configRow {
	var r configRow
	err := s.pool.QueryRow(ctx, `SELECT cron_dashboard_url, cron_dashboard_token_ciphertext, cron_dashboard_token_nonce,
		capabilities, capabilities_checked_at FROM user_hermes_config WHERE tenant_id = $1 LIMIT 1`, tenant).
		Scan(&r.dashboardURL, &r.tokenCT, &r.tokenNonce, &r.cached, &r.checkedAt)
	r.exists = err == nil
	return r
}

// Capabilities returns the tenant's capabilities, re-probing when the cache is
// stale or force is set. It never fails: an unreachable box degrades the panes.
func (s *Service) Capabilities(ctx context.Context, tenant uuid.UUID, force bool) Response {
	now := s.Now()
	row := s.load(ctx, tenant)
	hasDashboard := row.exists && row.dashboardURL != nil && *row.dashboardURL != ""

	var res *hermesresolve.Resolution
	if s.resolver != nil {
		if r, err := s.resolver.Resolve(ctx, tenant); err == nil {
			res = &r
		}
	}
	isUserOverride := res != nil && res.IsUserOverride
	if !isUserOverride && !hasDashboard {
		return Response{Capabilities: ManagedDefault()}
	}

	if !force && row.exists && row.cached != nil && row.checkedAt != nil && now.Sub(*row.checkedAt) < s.ttl {
		var decoded Capabilities
		if decodeCapabilities([]byte(*row.cached), &decoded) == nil {
			return Response{Capabilities: decoded, CheckedAt: &wire.Time{Time: *row.checkedAt}}
		}
	}

	var dashboard *Dashboard
	if hasDashboard {
		dashboard = s.probeDashboard(ctx, tenant, row)
	}
	probed := ManagedDefault()
	if isUserOverride {
		probed = s.probeRemote(ctx, res)
	}
	probed.Dashboard = dashboard
	if row.exists {
		if encoded, err := wire.MarshalFoundation(probed); err == nil {
			_, _ = s.pool.Exec(ctx, `UPDATE user_hermes_config SET capabilities = $2, capabilities_checked_at = $3, updated_at = NOW()
				WHERE tenant_id = $1`, tenant, string(encoded), now)
		}
	}
	return Response{Capabilities: probed, CheckedAt: &wire.Time{Time: now}}
}

// decodeCapabilities is JSONDecoder's decode of HermesCapabilities: required
// keys must be present and enums known.
func decodeCapabilities(data []byte, out *Capabilities) error {
	if err := wire.DecodeInto(data, out); err != nil {
		return err
	}
	known := func(a Availability) bool { return a == Live || a == ReadOnly || a == Unsupported || a == Managed }
	for _, a := range []Availability{out.Chat, out.Sessions, out.Jobs, out.Skills, out.Soul, out.Gateways, out.Memory, out.Providers} {
		if !known(a) {
			return errors.New("unknown availability")
		}
	}
	if out.MultimodalIngestion != nil && !known(*out.MultimodalIngestion) {
		return errors.New("unknown availability")
	}
	if d := out.Dashboard; d != nil {
		switch d.AuthMode {
		case Bearer, OAuthOnly, Unauthorized, Unreachable:
		default:
			return errors.New("unknown auth mode")
		}
	}
	return nil
}

// IsUserOverride is HermesRemoteCapabilitiesService.isUserOverride.
func (s *Service) IsUserOverride(ctx context.Context, tenant uuid.UUID) bool {
	if s.resolver == nil {
		return false
	}
	r, err := s.resolver.Resolve(ctx, tenant)
	return err == nil && r.IsUserOverride
}

// appendPath is URL.appendingPathComponent.
func appendPath(base *url.URL, component string) string {
	u := *base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + component
	u.RawPath = ""
	return u.String()
}

func (s *Service) get(ctx context.Context, rawURL string, auth *string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return 0, nil, err
	}
	if auth != nil {
		req.Header.Set("Authorization", *auth)
	}
	resp, err := s.probe.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, body, err
}

func ok2xx(status int) bool { return status >= 200 && status < 300 }

// probeRemote is HermesRemoteCapabilitiesService.probe(resolution:).
func (s *Service) probeRemote(ctx context.Context, res *hermesresolve.Resolution) Capabilities {
	var version *string
	if status, body, err := s.get(ctx, appendPath(res.BaseURL, "health"), nil); err == nil && ok2xx(status) {
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			if v, ok := obj["version"].(string); ok {
				version = &v
			}
		}
	}
	var flags *Flags
	if status, body, err := s.get(ctx, appendPath(res.BaseURL, "v1/capabilities"), res.AuthHeader); err == nil && ok2xx(status) {
		flags = ParseFlags(body)
	}
	jobsReachable := false
	if status, _, err := s.get(ctx, appendPath(res.BaseURL, "api/jobs"), res.AuthHeader); err == nil && ok2xx(status) {
		jobsReachable = true
	}
	// flags?.ingestionRemoteSourceURL: absent when /v1/capabilities didn't parse.
	var remoteSourceURL *bool
	if flags != nil {
		remoteSourceURL = ptr(flags.RemoteSourceURL)
	} else {
		flags = &Flags{}
	}
	avail := func(b bool) Availability {
		if b {
			return Live
		}
		return Unsupported
	}
	jobs := avail(flags.Jobs)
	if jobsReachable {
		jobs = Live
	}
	skills := Unsupported
	if flags.Skills {
		skills = ReadOnly
	}
	return Capabilities{
		IsUserOverride: true, RemoteVersion: version,
		Chat: Live, Sessions: avail(flags.Sessions), Jobs: jobs, Skills: skills,
		Soul: Unsupported, Gateways: Unsupported, Memory: Unsupported, Providers: ReadOnly,
		MultimodalIngestion:         ptr(avail(flags.MultimodalIngestion)),
		IngestionSupportedMimeTypes: flags.MimeTypes,
		IngestionMaxSourceBytes:     flags.MaxSourceBytes,
		IngestionRemoteSourceURL:    remoteSourceURL,
	}
}

// Flags is CapabilityFlags.
type Flags struct {
	Sessions, Jobs, Skills, MultimodalIngestion, RemoteSourceURL bool
	MimeTypes                                                    *[]string
	MaxSourceBytes                                               *int64
}

// ParseFlags is parseCapabilities: the "features" map, or the object itself.
func ParseFlags(data []byte) *Flags {
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil
	}
	features, ok := obj["features"].(map[string]any)
	if !ok {
		features = obj
	}
	flag := func(keys ...string) bool {
		for _, k := range keys {
			if b, ok := features[k].(bool); ok && b {
				return true
			}
		}
		return false
	}
	f := &Flags{
		Sessions:            flag("session_resources", "sessions", "session_chat"),
		Jobs:                flag("jobs_admin", "jobs"),
		Skills:              flag("skills_api", "skills"),
		MultimodalIngestion: flag("multimodal_ingestion", "ingestion_api"),
		RemoteSourceURL:     flag("ingestion_remote_source_url", "remote_source_url"),
	}
	if arr, ok := features["ingestion_supported_mime_types"].([]any); ok {
		types := make([]string, 0, len(arr))
		all := true
		for _, v := range arr {
			s, ok := v.(string)
			if !ok {
				all = false
				break
			}
			types = append(types, s)
		}
		if all {
			f.MimeTypes = &types
		}
	}
	if n, ok := features["ingestion_max_source_bytes"].(float64); ok {
		f.MaxSourceBytes = ptr(int64(n))
	}
	return f
}

const (
	dashboardTimeout    = 15 * time.Second
	dashboardMaxRetries = 3
)

// probeDashboard reads the sealed dashboard credentials and probes them. A token
// that does not decrypt reports unauthorized.
func (s *Service) probeDashboard(ctx context.Context, tenant uuid.UUID, row configRow) *Dashboard {
	if s.box == nil || row.dashboardURL == nil || *row.dashboardURL == "" || row.tokenCT == nil || row.tokenNonce == nil {
		return nil
	}
	token, err := s.box.Open(secretbox.Sealed{Ciphertext: row.tokenCT, Nonce: row.tokenNonce}, tenant)
	if err != nil {
		s.log.WarnContext(ctx, "dashboard token decrypt failed", "tenant", ids.Upper(tenant))
		return &Dashboard{AuthMode: Unauthorized}
	}
	return s.probeDashboardWith(ctx, *row.dashboardURL, token)
}

// probeDashboardWith is HermesRemoteCapabilitiesService.probeDashboard(client:).
func (s *Service) probeDashboardWith(ctx context.Context, baseURL, token string) *Dashboard {
	status, body, err := s.dashboardGet(ctx, baseURL, token, "/api/status")
	var obj map[string]any
	if err != nil || !ok2xx(status) || json.Unmarshal(body, &obj) != nil || obj == nil {
		return &Dashboard{AuthMode: Unreachable}
	}
	// Swift's status() also reads /api/fs/default-cwd; the probe ignores it.
	_, _, _ = s.dashboardGet(ctx, baseURL, token, "/api/fs/default-cwd")
	var authRequired *bool
	if b, ok := obj["auth_required"].(bool); ok {
		authRequired = &b
	}
	var version *string
	if v, ok := obj["version"].(string); ok {
		version = &v
	}
	mode := Unreachable
	if st, _, err := s.dashboardGet(ctx, baseURL, token, "/api/skills"); err == nil {
		switch {
		case ok2xx(st):
			mode = Bearer
		case st >= 300 && st < 400:
			mode = OAuthOnly
		case st == 401 || st == 403:
			mode = Unauthorized
			if authRequired != nil && *authRequired {
				mode = OAuthOnly
			}
		}
	}
	usable := mode == Bearer
	return &Dashboard{Reachable: true, AuthMode: mode, SkillsWrite: usable, Cron: usable, FS: usable, Sessions: usable, Version: version}
}

// dashboardGet is HermesDashboardClient.send for a GET: SSRF validation on every
// call, both token headers, and up to three tries on 429.
func (s *Service) dashboardGet(ctx context.Context, baseURL, token, path string) (int, []byte, error) {
	validated, err := s.guard.Validate(ctx, baseURL)
	if err != nil {
		return 0, nil, err
	}
	u := *validated
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath, u.RawQuery, u.Fragment = "", "", ""
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Hermes-Session-Token", token)
		req.Header.Set("Accept", "application/json")
		resp, err := s.dashboard.Do(req)
		if err != nil {
			return 0, nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if err != nil {
			return 0, nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < dashboardMaxRetries {
			if !sleep(ctx, retryDelay(resp.Header.Get("Retry-After"), attempt)) {
				return 0, nil, ctx.Err()
			}
			continue
		}
		return resp.StatusCode, body, nil
	}
}

func retryDelay(retryAfter string, attempt int) time.Duration {
	if seconds, err := strconv.ParseFloat(retryAfter, 64); err == nil && seconds > 0 {
		return time.Duration(min(seconds, 30) * float64(time.Second))
	}
	return time.Duration(attempt) * time.Second
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
