package news

// Port of Sources/App/News/NewsTickerService.swift: curated feeds plus the
// tenant's own, read in one call from the cluster's feed aggregator
// (JSON Feed 1.1 with a `_feeds` extension) and cached per process.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
)

const (
	// maxFeedsPerCall is the aggregator's cap on feeds in one call.
	maxFeedsPerCall = 25
	maxUserFeeds    = 10
	cacheTTL        = 60 * time.Second
	cacheMaxEntries = 512
	// fetchTimeout is URLSession.shared's default request timeout.
	fetchTimeout = 60 * time.Second
)

// Error messages are NewsTickerService.ErrorCode raw values.
var (
	errNotConfigured  = wire.NewError(http.StatusServiceUnavailable, "news_ticker_not_configured")
	errNotInstalled   = wire.NewError(http.StatusNotFound, "plugin_not_installed")
	errDisabled       = wire.NewError(http.StatusConflict, "plugin_disabled")
	errUnavailable    = wire.NewError(http.StatusBadGateway, "feeds_unavailable")
	errInvalidBaseURL = wire.NewError(http.StatusInternalServerError, "invalid feeds base url")
)

// InstallState is Swift's NewsTickerInstallState.
type InstallState struct {
	Enabled bool
	Config  map[string]string
}

// Installs is Swift's NewsTickerInstalls seam. A nil state means the plugin
// is not installed for the tenant.
type Installs interface {
	NewsTickerInstall(ctx context.Context, tenantID uuid.UUID) (*InstallState, error)
}

// Service is Swift's NewsTickerService.
type Service struct {
	baseURL  string
	curated  []string
	http     *http.Client
	installs Installs
	cache    *cache
	now      func() time.Time
}

// NewService trims one trailing "/" from baseURL, as Swift's init does.
func NewService(baseURL string, curated []string, client *http.Client, installs Installs) *Service {
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	return &Service{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		curated:  curated,
		http:     client,
		installs: installs,
		cache:    &cache{entries: map[string]cacheEntry{}},
		now:      time.Now,
	}
}

// CuratedFeeds parses feeds.defaultFeeds as App+build.swift does: split on
// ",", trim, drop empties.
func CuratedFeeds(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = trim(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (s *Service) configured() bool { return s.baseURL != "" }

// Ticker is NewsTickerService.ticker(tenantID:limit:).
func (s *Service) Ticker(ctx context.Context, tenantID uuid.UUID, limit int) (NewsTickerResponse, error) {
	if !s.configured() {
		return NewsTickerResponse{}, errNotConfigured
	}
	install, err := s.installs.NewsTickerInstall(ctx, tenantID)
	if err != nil {
		return NewsTickerResponse{}, err
	}
	if install == nil {
		return NewsTickerResponse{}, errNotInstalled
	}
	if !install.Enabled {
		return NewsTickerResponse{}, errDisabled
	}
	clamped := min(max(limit, 1), 100)
	feeds := FeedList(s.curated, UserFeeds(install.Config, maxUserFeeds))
	now := s.now()
	if len(feeds) == 0 {
		return emptyResponse(now), nil
	}
	key := ids.Upper(tenantID) + "|" + strconv.Itoa(clamped) + "|" + strings.Join(feeds, "\n")
	if cached, ok := s.cache.get(key, now); ok {
		return cached, nil
	}
	resp, err := s.fetch(ctx, feeds, clamped, now)
	if err != nil {
		return NewsTickerResponse{}, err
	}
	s.cache.set(key, resp, now)
	return resp, nil
}

func emptyResponse(now time.Time) NewsTickerResponse {
	return NewsTickerResponse{Items: []NewsTickerItemDTO{}, Stale: false, GeneratedAt: wire.Time{Time: now}}
}

func (s *Service) fetch(ctx context.Context, feeds []string, limit int, now time.Time) (NewsTickerResponse, error) {
	u, err := url.Parse(s.baseURL + "/v1/items")
	if err != nil {
		return NewsTickerResponse{}, errInvalidBaseURL
	}
	u.RawQuery = "feeds=" + queryItemEscape(strings.Join(feeds, ",")) + "&limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return NewsTickerResponse{}, errInvalidBaseURL
	}
	req.Header.Set("Accept", "application/feed+json, application/json")
	res, err := s.http.Do(req)
	if err != nil {
		// Swift lets the URLSession error escape: a 500 with no body.
		return NewsTickerResponse{}, fmt.Errorf("news ticker: aggregator request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return NewsTickerResponse{}, fmt.Errorf("news ticker: aggregator body: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "news ticker: aggregator returned non-200", "status", res.StatusCode)
		return NewsTickerResponse{}, errUnavailable
	}
	doc, err := decodeJSONFeed(body)
	if err != nil {
		// A DecodingError escapes in Swift too: 500, no body.
		return NewsTickerResponse{}, fmt.Errorf("news ticker: decode feed: %w", err)
	}
	out := NewsTickerResponse{Items: make([]NewsTickerItemDTO, 0, len(doc.items)), GeneratedAt: wire.Time{Time: now}}
	for _, it := range doc.items {
		out.Items = append(out.Items, NewsTickerItemDTO{
			Id: it.id, Title: it.title, Url: it.url, Source: it.sourceName,
			SourceUrl: it.sourceURL, PublishedAt: wire.Time{Time: it.published},
		})
	}
	if doc.hasExt {
		out.Stale = len(doc.stale) > 0
		if doc.generatedAt != nil {
			out.GeneratedAt = wire.Time{Time: *doc.generatedAt}
		}
	}
	return out, nil
}

// UserFeeds is NewsTickerService.userFeeds(from:max:): config["feed_urls"]
// split on "," or "\n", trimmed, public http(s) only, deduped, capped.
func UserFeeds(config map[string]string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range splitFeedURLs(config["feed_urls"]) {
		if len(out) == limit {
			break
		}
		t := trim(part)
		if t == "" || seen[t] {
			continue
		}
		u, err := url.Parse(encodeInvalidCharacters(t))
		if err != nil || !IsPublic(u) {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// splitFeedURLs splits on "," and "\n" the way Swift's Character-wise
// split(whereSeparator:) does: "\r\n" is one Character in Swift, not "\n",
// so it does not separate (the glued piece is then kept as one feed URL).
// Empty pieces are dropped (Swift's default).
func splitFeedURLs(raw string) []string {
	var out []string
	start := 0
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != ',' && c != '\n' {
			continue
		}
		if c == '\n' && i > 0 && raw[i-1] == '\r' {
			continue
		}
		if i > start {
			out = append(out, raw[start:i])
		}
		start = i + 1
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

// queryItemEscape percent-encodes a query item value the way Foundation's
// URLComponents.queryItems does (parity run 41196161637): everything outside
// RFC 3986 query characters, plus "&" and "=". "+" stays raw, so a
// form-decoding aggregator reads it as a space, as it does for Swift.
func queryItemEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) || strings.IndexByte("!$'()*+,;:@/?", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// encodeInvalidCharacters mirrors URL(string:) in swift-foundation, which
// percent-encodes characters that are not valid in a URL instead of failing
// (encodingInvalidCharacters: true). Parity showed it: a feed_urls piece with
// an inner "\r\n" or a non-ASCII path is kept. Bytes outside RFC 3986 and a
// "%" not starting an escape are encoded; the rest is left for url.Parse.
func encodeInvalidCharacters(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte(c)
		case isUnreserved(c) || strings.IndexByte(":/?#[]@!$&'()*+,;=", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// FeedList is NewsTickerService.feedList(curated:user:): curated first, then
// the tenant's, trimmed, deduped, capped at one aggregator call.
func FeedList(curated, user []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, f := range append(append([]string{}, curated...), user...) {
		f = trim(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
		if len(out) == maxFeedsPerCall {
			break
		}
	}
	return out
}

// trim is trimmingCharacters(in: .whitespacesAndNewlines).
func trim(s string) string { return strings.TrimFunc(s, unicode.IsSpace) }

// IsPublic is URLEnricherGuard.isPublic (Sources/App/Capture/Enrichers/URLEnricher.swift):
// http(s), a host, not a blocked or .internal/.local name, not a private
// numeric literal. No DNS.
func IsPublic(u *url.URL) bool {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	switch host {
	case "localhost", "ip6-localhost", "ip6-loopback", "metadata.google.internal":
		return false
	}
	if strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return false
	}
	return !isPrivateNumericHost(host)
}

func isPrivateNumericHost(host string) bool {
	stripped := strings.Trim(host, "[]")
	// Swift: split(separator: ".").compactMap { UInt8($0) } — non-octet
	// labels are dropped, so "10.0.0.1.nip.io" counts as 10.0.0.1.
	var parts []int
	for _, p := range strings.Split(stripped, ".") {
		if p == "" {
			continue
		}
		if n, ok := swiftUInt8(p); ok {
			parts = append(parts, n)
		}
	}
	if len(parts) == 4 {
		a, b := parts[0], parts[1]
		switch {
		case a == 10, a == 127, a == 0:
			return true
		case a == 169 && b == 254, a == 192 && b == 168:
			return true
		case a == 172 && b >= 16 && b <= 31:
			return true
		case a == 100 && b >= 64 && b <= 127:
			return true
		}
	}
	lower := strings.ToLower(stripped)
	if lower == "::1" || lower == "::" {
		return true
	}
	return strings.HasPrefix(lower, "fc") || strings.HasPrefix(lower, "fd") || strings.HasPrefix(lower, "fe80")
}

// swiftUInt8 is UInt8(String): optional leading "+", ASCII digits, 0...255.
func swiftUInt8(s string) (int, bool) {
	s = strings.TrimPrefix(s, "+")
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 255 {
			return 0, false
		}
	}
	return n, true
}

// jsonFeed is the decoded JSONFeedDocument.
type jsonFeed struct {
	items       []feedItem
	hasExt      bool
	stale       []string
	generatedAt *time.Time
}

type feedItem struct {
	id, title, sourceName string
	url, sourceURL        *string
	published             time.Time
}

var errFeedShape = errors.New("feed document does not match JSONFeedDocument")

// decodeJSONFeed decodes as Swift's JSONDecoder does for JSONFeedDocument:
// required keys must be present and non-null, optional ones may be null,
// unknown keys are ignored, and dates are internet date-time with or without
// fractional seconds.
func decodeJSONFeed(raw []byte) (jsonFeed, error) {
	var doc struct {
		Items *[]struct {
			ID            *string `json:"id"`
			URL           *string `json:"url"`
			Title         *string `json:"title"`
			DatePublished *string `json:"date_published"`
			Feeds         *struct {
				SourceName *string `json:"source_name"`
				SourceURL  *string `json:"source_url"`
				FeedURL    *string `json:"feed_url"`
			} `json:"_feeds"`
		} `json:"items"`
		Feeds *struct {
			Warming     *[]string `json:"warming"`
			Stale       *[]string `json:"stale"`
			GeneratedAt *string   `json:"generated_at"`
		} `json:"_feeds"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil {
		return jsonFeed{}, err
	}
	if doc.Items == nil {
		return jsonFeed{}, errFeedShape
	}
	var out jsonFeed
	for _, it := range *doc.Items {
		if it.ID == nil || it.Title == nil || it.DatePublished == nil || it.Feeds == nil ||
			it.Feeds.SourceName == nil || it.Feeds.FeedURL == nil {
			return jsonFeed{}, errFeedShape
		}
		published, err := parseFeedDate(*it.DatePublished)
		if err != nil {
			return jsonFeed{}, err
		}
		out.items = append(out.items, feedItem{
			id: *it.ID, title: *it.Title, sourceName: *it.Feeds.SourceName,
			url: it.URL, sourceURL: it.Feeds.SourceURL, published: published,
		})
	}
	if doc.Feeds != nil {
		if doc.Feeds.Warming == nil || doc.Feeds.Stale == nil {
			return jsonFeed{}, errFeedShape
		}
		out.hasExt = true
		out.stale = *doc.Feeds.Stale
		if doc.Feeds.GeneratedAt != nil {
			t, err := parseFeedDate(*doc.Feeds.GeneratedAt)
			if err != nil {
				return jsonFeed{}, err
			}
			out.generatedAt = &t
		}
	}
	return out, nil
}

// parseFeedDate accepts what Swift's two ISO8601DateFormatters accept
// (.withInternetDateTime, with and without .withFractionalSeconds).
func parseFeedDate(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("unparseable date %s", s)
	}
	return t, nil
}

type cacheEntry struct {
	resp    NewsTickerResponse
	expires time.Time
}

// cache is NewsTickerCache: per-process, TTL 60 s, bounded at 512 entries.
type cache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

func (c *cache) get(key string, now time.Time) (NewsTickerResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || !e.expires.After(now) {
		return NewsTickerResponse{}, false
	}
	return e.resp, true
}

func (c *cache) set(key string, resp NewsTickerResponse, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= cacheMaxEntries {
		for k, e := range c.entries {
			if !e.expires.After(now) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= cacheMaxEntries {
			c.entries = map[string]cacheEntry{}
		}
	}
	c.entries[key] = cacheEntry{resp: resp, expires: now.Add(cacheTTL)}
}
