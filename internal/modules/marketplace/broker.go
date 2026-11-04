package marketplace

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/modules/marketplace/db"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultpaths"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CapabilityRequest is MarketplaceCapabilityRequest.
type CapabilityRequest struct {
	ID        string            `json:"id"`
	Operation string            `json:"operation"`
	Arguments map[string]string `json:"arguments"`
}

// CapabilityResult is MarketplaceCapabilityResult.
type CapabilityResult struct {
	ID     string            `json:"id"`
	OK     bool              `json:"ok"`
	Values map[string]string `json:"values"`
	Error  *string           `json:"error,omitempty"`
}

// Broker is MarketplaceCapabilityBrokering.
type Broker interface {
	Execute(ctx context.Context, requests []CapabilityRequest, tenant uuid.UUID, slug string, permissions []PluginPermission, hosts []string) []CapabilityResult
}

const (
	maxCapRequests = 16
	maxValueBytes  = 1_048_576
	maxMemoryBytes = 65536
	maxTags        = 32
	maxTagBytes    = 64
	maxPathBytes   = 512
)

// brokerError is the BrokerError enum: failures the module may see by name.
// Anything else is capability_failed.
type brokerError string

func (e brokerError) Error() string { return string(e) }

const (
	errPermissionDenied brokerError = "permission_denied"
	errInvalidArguments brokerError = "invalid_arguments"
	errUnsupported      brokerError = "operation_unsupported"
	errResourceNotFound brokerError = "resource_not_found"
	errResponseTooLarge brokerError = "response_too_large"
	errHostNotAllowed   brokerError = "host_not_allowed"
	errUpstreamFailure  brokerError = "upstream_failure"
)

var contentTypeRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.+-]*/[a-zA-Z0-9][a-zA-Z0-9.+-]*$`)

// CapabilityBroker is MarketplaceCapabilityBroker.
type CapabilityBroker struct {
	q      *db.Queries
	paths  vaultpaths.Paths
	guard  ssrfGuard
	client *http.Client
	now    func() time.Time
}

// NewBroker builds the production broker. Its HTTP client never follows
// redirects and ignores proxy settings (BYOHTTP.httpClient).
func NewBroker(pool *pgxpool.Pool, paths vaultpaths.Paths) *CapabilityBroker {
	return &CapabilityBroker{
		q: db.New(pool), paths: paths, guard: ssrfGuard{resolve: systemResolve},
		client: &http.Client{
			Transport: &http.Transport{TLSHandshakeTimeout: 10 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}
}

// SetNetwork replaces the resolver and HTTP client (tests).
func (b *CapabilityBroker) SetNetwork(resolve func(ctx context.Context, host string) ([]string, error), client *http.Client) {
	b.guard.resolve = resolve
	b.client = client
}

func (b *CapabilityBroker) Execute(ctx context.Context, requests []CapabilityRequest, tenant uuid.UUID, slug string, permissions []PluginPermission, hosts []string) []CapabilityResult {
	results := make([]CapabilityResult, len(requests))
	if len(requests) > maxCapRequests {
		for i, r := range requests {
			results[i] = failed(r.ID, "too_many_capability_requests")
		}
		return results
	}
	for i, r := range requests {
		values, err := b.executeOne(ctx, r, tenant, slug, permissions, hosts)
		if err == nil {
			results[i] = CapabilityResult{ID: r.ID, OK: true, Values: values}
			continue
		}
		var named brokerError
		if errors.As(err, &named) {
			results[i] = failed(r.ID, string(named))
			continue
		}
		slog.WarnContext(ctx, "marketplace capability failed", "operation", r.Operation, "plugin", slug, "error", err)
		results[i] = failed(r.ID, "capability_failed")
	}
	return results
}

func failed(id, code string) CapabilityResult {
	return CapabilityResult{ID: id, Values: map[string]string{}, Error: &code}
}

func (b *CapabilityBroker) executeOne(ctx context.Context, r CapabilityRequest, tenant uuid.UUID, slug string, permissions []PluginPermission, hosts []string) (map[string]string, error) {
	operation := PluginPermission(r.Operation)
	if !operation.Valid() {
		return nil, errUnsupported
	}
	if !slices.Contains(permissions, operation) {
		return nil, errPermissionDenied
	}
	switch operation {
	case MemoryRead:
		return b.readMemory(ctx, r.Arguments, tenant)
	case MemoryWrite:
		return b.writeMemory(ctx, r.Arguments, tenant, slug)
	case VaultRead:
		return b.readVault(ctx, r.Arguments, tenant)
	case VaultWrite:
		return b.writeVault(ctx, r.Arguments, tenant)
	case NetworkFetch:
		return b.fetchNetwork(ctx, r.Arguments, hosts)
	default: // output.emit
		return r.Arguments, nil
	}
}

func (b *CapabilityBroker) readMemory(ctx context.Context, args map[string]string, tenant uuid.UUID) (map[string]string, error) {
	raw, ok := args["id"]
	if !ok || len(raw) != 36 {
		return nil, errInvalidArguments
	}
	id, err := ids.Parse(raw)
	if err != nil {
		return nil, errInvalidArguments
	}
	row, err := b.q.ReadMemory(ctx, db.ReadMemoryParams{TenantID: tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errResourceNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(row.Content) > maxValueBytes {
		return nil, errResponseTooLarge
	}
	return map[string]string{"id": ids.Upper(id), "content": row.Content, "tags": strings.Join(row.Tags, ",")}, nil
}

func isSwiftWhitespace(r rune) bool { return r == '\t' || unicode.Is(unicode.Zs, r) }

func (b *CapabilityBroker) writeMemory(ctx context.Context, args map[string]string, tenant uuid.UUID, slug string) (map[string]string, error) {
	raw, ok := args["content"]
	content := swiftstr.TrimWhitespacesAndNewlines(raw)
	if !ok || content == "" || len(content) > maxMemoryBytes {
		return nil, errInvalidArguments
	}
	var tags []string
	if rawTags, ok := args["tags"]; ok {
		tags = []string{}
		for t := range strings.SplitSeq(rawTags, ",") {
			if t != "" { // Swift's split drops empty pieces before trimming
				tags = append(tags, strings.TrimFunc(t, isSwiftWhitespace))
			}
		}
		if len(tags) > maxTags {
			return nil, errInvalidArguments
		}
		for _, t := range tags {
			if t == "" || len(t) > maxTagBytes {
				return nil, errInvalidArguments
			}
		}
	}
	id := uuid.New()
	source := "marketplace:" + slug
	now := b.now()
	if err := b.q.InsertMemory(ctx, db.InsertMemoryParams{
		ID: id, TenantID: tenant, Content: content, Tags: tags, OriginSourceID: &source, At: &now,
	}); err != nil {
		return nil, err
	}
	return map[string]string{"id": ids.Upper(id)}, nil
}

func (b *CapabilityBroker) readVault(ctx context.Context, args map[string]string, tenant uuid.UUID) (map[string]string, error) {
	path, err := safeRelativePath(args)
	if err != nil {
		return nil, err
	}
	row, err := b.q.VaultFileByPath(ctx, db.VaultFileByPathParams{TenantID: tenant, Path: path})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errResourceNotFound
	}
	if err != nil {
		return nil, err
	}
	target, err := b.resolveVaultPath(path, tenant)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(target) //nolint:gosec // resolveVaultPath confines it to the tenant's raw directory
	if err != nil {
		return nil, err
	}
	if len(data) > maxValueBytes {
		return nil, errResponseTooLarge
	}
	return map[string]string{
		"path": path, "contentType": row.ContentType,
		"bytesBase64": base64.StdEncoding.EncodeToString(data), "sha256": row.Sha256,
	}, nil
}

func (b *CapabilityBroker) writeVault(ctx context.Context, args map[string]string, tenant uuid.UUID) (map[string]string, error) {
	path, err := safeRelativePath(args)
	if err != nil {
		return nil, err
	}
	var data []byte
	if encoded, ok := args["bytesBase64"]; ok {
		if decoded, ok := decodeBase64(encoded); ok {
			data = decoded
		} else if text, ok := args["text"]; ok {
			data = []byte(text)
		} else {
			return nil, errInvalidArguments
		}
	} else if text, ok := args["text"]; ok {
		data = []byte(text)
	} else {
		return nil, errInvalidArguments
	}
	if len(data) == 0 || len(data) > maxValueBytes {
		return nil, errInvalidArguments
	}
	if err := b.paths.EnsureTenantDirectories(tenant); err != nil {
		return nil, err
	}
	target, err := b.resolveVaultPath(path, tenant)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // Foundation's default, as vaultpaths
		return nil, err
	}
	if err := writeAtomic(target, data); err != nil {
		return nil, err
	}
	digest := sha256Hex(data)
	contentType, ok := args["contentType"]
	if !ok {
		contentType = "application/octet-stream"
	}
	// Swift checks the content type after the write, so a bad one leaves the
	// file on disk with no row.
	if len(contentType) > 255 || !contentTypeRe.MatchString(contentType) {
		return nil, errInvalidArguments
	}
	now := b.now()
	row, err := b.q.VaultFileByPath(ctx, db.VaultFileByPathParams{TenantID: tenant, Path: path})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		err = b.q.InsertVaultFile(ctx, db.InsertVaultFileParams{
			ID: uuid.New(), TenantID: tenant, Path: path, ContentType: contentType, SizeBytes: int64(len(data)), Sha256: digest, At: &now,
		})
	case err == nil:
		err = b.q.UpdateVaultFile(ctx, db.UpdateVaultFileParams{
			ContentType: contentType, SizeBytes: int64(len(data)), Sha256: digest, At: &now, ID: row.ID,
		})
	}
	if err != nil {
		return nil, err
	}
	return map[string]string{"path": path, "sha256": digest, "sizeBytes": strconv.Itoa(len(data))}, nil
}

func (b *CapabilityBroker) fetchNetwork(ctx context.Context, args map[string]string, allowed []string) (map[string]string, error) {
	raw, ok := args["url"]
	if !ok {
		return nil, errInvalidArguments
	}
	u, err := b.guard.validate(ctx, raw)
	if err != nil {
		return nil, err
	}
	if u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return nil, errInvalidArguments
	}
	if !slices.Contains(allowed, strings.ToLower(u.Hostname())) {
		return nil, errHostNotAllowed
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain;q=0.9, */*;q=0.1")
	resp, err := b.client.Do(req) //nolint:gosec // validated by the SSRF guard and the version's host allowlist
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errUpstreamFailure
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxValueBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxValueBytes {
		return nil, errors.New("capability: response over the limit")
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return map[string]string{
		"status": strconv.Itoa(resp.StatusCode), "contentType": contentType,
		"bytesBase64": base64.StdEncoding.EncodeToString(data),
	}, nil
}

func safeRelativePath(args map[string]string) (string, error) {
	raw, ok := args["path"]
	if !ok {
		return "", errInvalidArguments
	}
	normalized := strings.Trim(strings.ReplaceAll(raw, `\`, "/"), "/")
	if normalized == "" || len(normalized) > maxPathBytes {
		return "", errInvalidArguments
	}
	for s := range strings.SplitSeq(normalized, "/") {
		if s == "" || s == "." || s == ".." {
			return "", errInvalidArguments
		}
	}
	if strings.ContainsFunc(normalized, func(r rune) bool { return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) }) {
		return "", errInvalidArguments
	}
	return normalized, nil
}

// resolveVaultPath confines path to the tenant's raw directory, following
// symlinks in whatever part of it exists.
func (b *CapabilityBroker) resolveVaultPath(path string, tenant uuid.UUID) (string, error) {
	root := resolveSymlinks(b.paths.Raw(tenant))
	target := filepath.Join(root, path)
	if !strings.HasPrefix(resolveSymlinks(target), root+string(filepath.Separator)) {
		return "", errInvalidArguments
	}
	return target, nil
}

// resolveSymlinks is URL.resolvingSymlinksInPath(): a path that does not exist
// yet keeps its missing tail.
func resolveSymlinks(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(resolveSymlinks(parent), filepath.Base(path))
}

// MARK: - SSRF guard (Sources/App/Settings/SSRFGuard.swift)

// ssrfGuard is SSRFGuard(allowPrivateRanges: false, requireHTTPS: true) with the
// default tailnet carve-out, the only configuration the broker uses. Settings
// (wave 6) ports the configurable guard; move this to internal/ then.
type ssrfGuard struct {
	resolve func(ctx context.Context, host string) ([]string, error)
}

var errRejected = errors.New("ssrf: rejected")

func (g ssrfGuard) validate(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(swiftstr.TrimWhitespacesAndNewlines(raw))
	if err != nil {
		return nil, err
	}
	httpPendingTailnet := false
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		httpPendingTailnet = true
	default:
		return nil, errRejected
	}
	host := u.Hostname()
	if host == "" || isLoopbackHost(host) {
		return nil, errRejected
	}
	addresses, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 || (httpPendingTailnet && !slices.ContainsFunc(addresses, isTailscale)) ||
		(httpPendingTailnet && slices.ContainsFunc(addresses, func(a string) bool { return !isTailscale(a) })) {
		return nil, errRejected
	}
	for _, ip := range addresses {
		if isLoopback(ip) || isLinkLocal(ip) {
			return nil, errRejected
		}
		if !isTailscale(ip) && isPrivate(ip) {
			return nil, errRejected
		}
	}
	return u, nil
}

func systemResolve(ctx context.Context, host string) ([]string, error) {
	if parseIPv4(host) != nil || strings.Contains(host, ":") {
		return []string{host}, nil
	}
	return net.DefaultResolver.LookupHost(ctx, host)
}

func isLoopbackHost(host string) bool {
	lower := strings.ToLower(host)
	if lower == "localhost" || lower == "ip6-localhost" || lower == "ip6-loopback" {
		return true
	}
	return isLoopback(strings.Trim(lower, "[]"))
}

func isLoopback(ip string) bool {
	if ip == "::1" || ip == "0:0:0:0:0:0:0:1" {
		return true
	}
	v4 := parseIPv4(ip)
	return v4 != nil && v4[0] == 127
}

func isLinkLocal(ip string) bool {
	if v4 := parseIPv4(ip); v4 != nil && v4[0] == 169 && v4[1] == 254 {
		return true
	}
	return strings.HasPrefix(strings.ToLower(ip), "fe80:")
}

func isTailscale(ip string) bool {
	if v4 := parseIPv4(ip); v4 != nil {
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
	}
	return strings.HasPrefix(strings.ToLower(ip), "fd7a:115c:a1e0:")
}

func isPrivate(ip string) bool {
	if v4 := parseIPv4(ip); v4 != nil {
		switch {
		case v4[0] == 10, v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31, v4[0] == 192 && v4[1] == 168, v4[0] == 0:
			return true
		}
	}
	lower := strings.ToLower(ip)
	if strings.HasPrefix(lower, "fc") || strings.HasPrefix(lower, "fd") {
		return true
	}
	if tail, ok := strings.CutPrefix(lower, "::ffff:"); ok {
		return isPrivate(tail) || isLoopback(tail) || isLinkLocal(tail)
	}
	return false
}

func parseIPv4(ip string) []int {
	parts := strings.FieldsFunc(ip, func(r rune) bool { return r == '.' })
	if len(parts) != 4 {
		return nil
	}
	out := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return nil
		}
		out[i] = n
	}
	return out
}
