package news

// Ports Tests/AppTests/Plugins/NewsTickerPluginTests.swift (all but the
// PluginCatalog/NewsTickerConnector test, which belongs to the plugins
// module), plus the quirks found while porting.

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/wire"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const fixture = `{"version":"https://jsonfeed.org/version/1.1","title":"Merged timeline","items":[
 {"id":"a","url":"https://bbc.example/a","title":"Ceasefire talks resume","date_published":"2026-09-17T11:00:00Z","_feeds":{"source_name":"BBC News","source_url":"https://www.bbc.co.uk","feed_url":"https://feeds.bbci.co.uk/news/rss.xml"}},
 {"id":"b","url":"https://npr.example/b","title":"Markets open higher","date_published":"2026-09-17T10:30:00.250Z","_feeds":{"source_name":"NPR","feed_url":"https://feeds.npr.org/1001/rss.xml"}}],
 "_feeds":{"warming":[],"stale":["https://feeds.npr.org/1001/rss.xml"],"generated_at":"2026-09-17T12:00:00Z"}}`

type stubInstalls struct{ state *InstallState }

func (s stubInstalls) NewsTickerInstall(context.Context, uuid.UUID) (*InstallState, error) {
	return s.state, nil
}

var enabled = &InstallState{Enabled: true, Config: map[string]string{}}

// aggregator is a stub feed aggregator that counts calls and records the
// last request.
type aggregator struct {
	*httptest.Server
	calls atomic.Int32
	last  atomic.Pointer[http.Request]
}

func newAggregator(t *testing.T, status int, body string) *aggregator {
	t.Helper()
	a := &aggregator{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.calls.Add(1)
		a.last.Store(r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(a.Close)
	return a
}

func svc(base string, state *InstallState) *Service {
	return NewService(base, []string{"https://feeds.bbci.co.uk/news/rss.xml"}, nil, stubInstalls{state})
}

func status(t *testing.T, err error) int {
	t.Helper()
	var we *wire.Error
	require.True(t, errors.As(err, &we), "want *wire.Error, got %v", err)
	return we.Status
}

// Swift "not installed is 404, disabled is 409, unconfigured is 503".
func TestInstallStateErrors(t *testing.T) {
	a := newAggregator(t, 200, fixture)
	ctx := context.Background()
	_, err := svc(a.URL, nil).Ticker(ctx, uuid.New(), 10)
	require.Equal(t, http.StatusNotFound, status(t, err))
	require.EqualError(t, err, "plugin_not_installed")
	_, err = svc(a.URL, &InstallState{Enabled: false}).Ticker(ctx, uuid.New(), 10)
	require.Equal(t, http.StatusConflict, status(t, err))
	require.EqualError(t, err, "plugin_disabled")
	_, err = svc("", enabled).Ticker(ctx, uuid.New(), 10)
	require.Equal(t, http.StatusServiceUnavailable, status(t, err))
	require.EqualError(t, err, "news_ticker_not_configured")
	// Unconfigured wins over the install lookup (the parity env's only path).
	_, err = svc("", nil).Ticker(ctx, uuid.New(), 10)
	require.EqualError(t, err, "news_ticker_not_configured")
	require.Zero(t, a.calls.Load())
}

// Swift "maps aggregator items to DTOs, flags stale, caches per tenant".
func TestMapsItemsStaleAndCaches(t *testing.T) {
	a := newAggregator(t, 200, fixture)
	s := svc(a.URL+"/", enabled) // trailing slash trimmed
	ctx := context.Background()
	tenant := uuid.New()
	first, err := s.Ticker(ctx, tenant, 10)
	require.NoError(t, err)
	require.Len(t, first.Items, 2)
	require.Equal(t, "Ceasefire talks resume", first.Items[0].Title)
	require.Equal(t, "BBC News", first.Items[0].Source)
	require.Equal(t, "https://www.bbc.co.uk", *first.Items[0].SourceUrl)
	require.Nil(t, first.Items[1].SourceUrl)
	require.Equal(t, time.Date(2026, 9, 17, 10, 30, 0, 250_000_000, time.UTC), first.Items[1].PublishedAt.UTC())
	require.True(t, first.Stale)
	require.Equal(t, time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC), first.GeneratedAt.UTC())

	req := a.last.Load()
	require.Equal(t, "/v1/items", req.URL.Path)
	require.Equal(t, "10", req.URL.Query().Get("limit"))
	require.Equal(t, "https://feeds.bbci.co.uk/news/rss.xml", req.URL.Query().Get("feeds"))
	require.Equal(t, "application/feed+json, application/json", req.Header.Get("Accept"))

	_, err = s.Ticker(ctx, tenant, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, a.calls.Load(), "cached for the same tenant, limit and feeds")
	_, err = s.Ticker(ctx, uuid.New(), 10)
	require.NoError(t, err)
	require.EqualValues(t, 2, a.calls.Load(), "cache is per tenant")
	_, err = s.Ticker(ctx, tenant, 11)
	require.NoError(t, err)
	require.EqualValues(t, 3, a.calls.Load(), "and per limit")
}

func TestCacheExpiresAfterTTL(t *testing.T) {
	a := newAggregator(t, 200, fixture)
	s := svc(a.URL, enabled)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	tenant := uuid.New()
	_, _ = s.Ticker(context.Background(), tenant, 5)
	now = now.Add(59 * time.Second)
	_, _ = s.Ticker(context.Background(), tenant, 5)
	require.EqualValues(t, 1, a.calls.Load())
	now = now.Add(time.Second) // expires > now is false at exactly 60 s
	_, _ = s.Ticker(context.Background(), tenant, 5)
	require.EqualValues(t, 2, a.calls.Load())
}

func TestCacheBounded(t *testing.T) {
	c := &cache{entries: map[string]cacheEntry{}}
	now := time.Now()
	for i := range cacheMaxEntries {
		c.set(fmt.Sprint(i), NewsTickerResponse{}, now)
	}
	require.Len(t, c.entries, cacheMaxEntries)
	c.set("next", NewsTickerResponse{}, now)
	require.Len(t, c.entries, 1, "all live and full: cleared, as Swift does")
	c.set("old", NewsTickerResponse{}, now.Add(-2*cacheTTL))
	_, ok := c.get("old", now)
	require.False(t, ok)
}

func TestLimitClampedAndNoExtGivesNowAndNotStale(t *testing.T) {
	a := newAggregator(t, 200, `{"items":[{"id":"x","title":"T","url":null,"date_published":"2026-01-01T00:00:00+02:00","_feeds":{"source_name":"S","feed_url":"f"}}]}`)
	s := svc(a.URL, enabled)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	for in, want := range map[int]string{0: "1", -4: "1", 100: "100", 500: "100"} {
		r, err := s.Ticker(context.Background(), uuid.New(), in)
		require.NoError(t, err)
		require.Equal(t, want, a.last.Load().URL.Query().Get("limit"), in)
		require.False(t, r.Stale)
		require.Equal(t, now, r.GeneratedAt.Time, "no _feeds: generatedAt is now")
		require.Nil(t, r.Items[0].Url)
	}
}

func TestEmptyFeedListSkipsAggregator(t *testing.T) {
	a := newAggregator(t, 200, fixture)
	s := NewService(a.URL, nil, nil, stubInstalls{enabled})
	r, err := s.Ticker(context.Background(), uuid.New(), 10)
	require.NoError(t, err)
	require.NotNil(t, r.Items)
	require.Empty(t, r.Items)
	require.False(t, r.Stale)
	require.Zero(t, a.calls.Load())
}

// Swift "aggregator failure is a 502".
func TestAggregatorNon200Is502(t *testing.T) {
	a := newAggregator(t, 502, "")
	_, err := svc(a.URL, enabled).Ticker(context.Background(), uuid.New(), 5)
	require.Equal(t, http.StatusBadGateway, status(t, err))
	require.EqualError(t, err, "feeds_unavailable")
}

// Transport and decoding failures escape as plain errors (500, no body),
// as Swift's URLSession and DecodingError do.
func TestUnreachableAndUndecodableArePlainErrors(t *testing.T) {
	var we *wire.Error
	_, err := svc("http://127.0.0.1:9", enabled).Ticker(context.Background(), uuid.New(), 5)
	require.Error(t, err)
	require.False(t, errors.As(err, &we))

	for _, body := range []string{
		`not json`,
		`{}`,
		`{"items":[{"id":"x","title":"T","date_published":"2026-01-01T00:00:00Z"}]}`,
		`{"items":[{"id":"x","title":null,"date_published":"2026-01-01T00:00:00Z","_feeds":{"source_name":"S","feed_url":"f"}}]}`,
		`{"items":[{"id":1,"title":"T","date_published":"2026-01-01T00:00:00Z","_feeds":{"source_name":"S","feed_url":"f"}}]}`,
		`{"items":[{"id":"x","title":"T","date_published":"yesterday","_feeds":{"source_name":"S","feed_url":"f"}}]}`,
		`{"items":[],"_feeds":{"stale":[]}}`,
	} {
		a := newAggregator(t, 200, body)
		_, err := svc(a.URL, enabled).Ticker(context.Background(), uuid.New(), 5)
		require.Error(t, err, body)
		require.False(t, errors.As(err, &we), body)
	}
}

// Swift "user feeds are appended after curated, private and malformed ones dropped, capped".
func TestUserFeedsAndFeedList(t *testing.T) {
	user := UserFeeds(map[string]string{"feed_urls": " https://a.example/rss ,http://10.0.0.1/x, not a url\nhttps://b.example/feed.json,https://a.example/rss "}, 10)
	require.Equal(t, []string{"https://a.example/rss", "https://b.example/feed.json"}, user)
	var many []string
	for i := 1; i <= 15; i++ {
		many = append(many, fmt.Sprintf("https://f%d.example/rss", i))
	}
	require.Len(t, UserFeeds(map[string]string{"feed_urls": strings.Join(many, ",")}, 3), 3)
	require.Equal(t,
		[]string{"https://c.example/rss", "https://a.example/rss", "https://b.example/feed.json"},
		FeedList([]string{"https://c.example/rss", "https://a.example/rss"}, user))
	var forty []string
	for i := 1; i <= 40; i++ {
		forty = append(forty, fmt.Sprintf("https://f%d.example/rss", i))
	}
	require.Len(t, FeedList(forty, nil), maxFeedsPerCall)
	require.Empty(t, UserFeeds(map[string]string{}, 10))
}

func TestUserFeedsSwiftQuirks(t *testing.T) {
	// "\r\n" is one Swift Character, so it does not split; the glued piece
	// is not a valid URL and is dropped.
	require.Equal(t, []string{"https://c.example/x"},
		UserFeeds(map[string]string{"feed_urls": "https://a.example/rss\r\nhttps://b.example/rss,https://c.example/x"}, 10))
	require.Equal(t, []string{"https://a.example/rss", "https://b.example/rss"},
		UserFeeds(map[string]string{"feed_urls": "https://a.example/rss\n\n,https://b.example/rss"}, 10))
}

func TestIsPublic(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://example.com/rss":          true,
		"HTTP://Example.com/rss":           true,
		"ftp://example.com/rss":            false,
		"https:///nohost":                  false,
		"http://localhost/rss":             false,
		"http://LOCALHOST:8080/rss":        false,
		"http://metadata.google.internal/": false,
		"http://svc.cluster.internal/":     false,
		"http://printer.local/":            false,
		"http://10.1.2.3/":                 false,
		"http://127.0.0.1/":                false,
		"http://169.254.169.254/":          false,
		"http://172.16.0.1/":               false,
		"http://172.32.0.1/":               true,
		"http://192.168.1.1/":              false,
		"http://0.0.0.0/":                  false,
		"http://100.64.0.1/":               false,
		"http://100.128.0.1/":              true,
		"http://8.8.8.8/":                  true,
		"http://10.0.0.1.nip.io/":          false, // Swift drops non-octet labels
		"http://[::1]/":                    false,
		"http://[fd00::1]/":                false,
		"http://[fe80::1]/":                false,
		"http://[2001:db8::1]/":            true,
		"http://fdic.gov/":                 false, // Swift's "fd" prefix check is on any host
	} {
		u, err := url.Parse(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, IsPublic(u), raw)
	}
}

func TestCuratedFeeds(t *testing.T) {
	require.Equal(t, []string{"https://a/rss", "https://b/rss"}, CuratedFeeds(" https://a/rss, ,https://b/rss,"))
	require.Empty(t, CuratedFeeds(""))
}
