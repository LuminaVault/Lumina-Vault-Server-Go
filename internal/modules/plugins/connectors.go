package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/modules/news"
	"lumina/internal/swiftstr"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Connector is PluginConnector: source URLs to stage from a decrypted config.
type Connector interface {
	Binding() string
	FetchURLs(ctx context.Context, config map[string]string, tenant uuid.UUID) ([]string, error)
}

// ConnectorError is ConnectorError.
type ConnectorError struct {
	Kind   string // missingConfig, invalidConfig, unauthorized, upstreamFailure
	Key    string
	Status int
}

func (e *ConnectorError) Error() string {
	return fmt.Sprintf("connector %s %s %d", e.Kind, e.Key, e.Status)
}

// HTTPGetter is ConnectorHTTPClient.
type HTTPGetter interface {
	Get(ctx context.Context, rawURL string, headers map[string]string) (int, []byte, error)
}

// DefaultHTTP is URLSessionConnectorHTTPClient: URLSession.shared follows
// redirects and times a request out after 60 s.
type DefaultHTTP struct{ Client *http.Client }

func (d DefaultHTTP) Get(ctx context.Context, rawURL string, headers map[string]string) (int, []byte, error) {
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// Registry is ConnectorRegistry.
type Registry map[string]Connector

func NewRegistry(connectors ...Connector) Registry {
	r := Registry{}
	for _, c := range connectors {
		r[c.Binding()] = c
	}
	return r
}

func token(config map[string]string) (string, error) {
	t := swiftstr.TrimWhitespacesAndNewlines(config["access_token"])
	if t == "" {
		return "", &ConnectorError{Kind: "missingConfig", Key: "access_token"}
	}
	return t, nil
}

func statusErr(status int) error {
	switch status {
	case 200:
		return nil
	case 401, 403:
		return &ConnectorError{Kind: "unauthorized"}
	default:
		return &ConnectorError{Kind: "upstreamFailure", Status: status}
	}
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// ReadwiseConnector is ReadwiseConnector: the v2 export API's source_url values,
// paginated by nextPageCursor, at most MaxPages pages.
type ReadwiseConnector struct {
	HTTP     HTTPGetter
	BaseURL  string
	MaxPages int
}

func (ReadwiseConnector) Binding() string { return "readwise" }

func (r ReadwiseConnector) FetchURLs(ctx context.Context, config map[string]string, tenant uuid.UUID) ([]string, error) {
	tok, err := token(config)
	if err != nil {
		return nil, err
	}
	base, maxPages := r.BaseURL, r.MaxPages
	if base == "" {
		base = "https://readwise.io"
	}
	if maxPages == 0 {
		maxPages = 10
	}
	headers := map[string]string{"Authorization": "Token " + tok}
	urls := []string{}
	seen := map[string]bool{}
	cursor := ""
	for range maxPages {
		u := strings.TrimSuffix(base, "/") + "/api/v2/export/"
		if cursor != "" {
			u += "?pageCursor=" + url.QueryEscape(cursor)
		}
		status, body, err := r.HTTP.Get(ctx, u, headers)
		if err != nil {
			return nil, err
		}
		if err := statusErr(status); err != nil {
			return nil, err
		}
		var page struct {
			NextPageCursor *string `json:"nextPageCursor"`
			Results        *[]struct {
				SourceURL *string `json:"source_url"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		if page.Results == nil {
			return nil, errors.New("readwise: results missing")
		}
		for _, res := range *page.Results {
			if res.SourceURL == nil {
				continue
			}
			raw := swiftstr.TrimWhitespacesAndNewlines(*res.SourceURL)
			if isHTTPURL(raw) && !seen[raw] {
				seen[raw] = true
				urls = append(urls, raw)
			}
		}
		if page.NextPageCursor == nil || *page.NextPageCursor == "" {
			break
		}
		cursor = *page.NextPageCursor
	}
	slog.InfoContext(ctx, "readwise connector", "tenant", tenant, "urls", len(urls))
	return urls, nil
}

// RaindropConnector is RaindropConnector: collection 0, 50 a page, at most MaxPages.
type RaindropConnector struct {
	HTTP     HTTPGetter
	BaseURL  string
	MaxPages int
}

func (RaindropConnector) Binding() string { return "raindrop" }

func (r RaindropConnector) FetchURLs(ctx context.Context, config map[string]string, tenant uuid.UUID) ([]string, error) {
	tok, err := token(config)
	if err != nil {
		return nil, err
	}
	base, maxPages := r.BaseURL, r.MaxPages
	if base == "" {
		base = "https://api.raindrop.io"
	}
	if maxPages == 0 {
		maxPages = 20
	}
	const perPage = 50
	headers := map[string]string{"Authorization": "Bearer " + tok}
	urls := []string{}
	seen := map[string]bool{}
	for page := range maxPages {
		u := strings.TrimSuffix(base, "/") + "/rest/v1/raindrops/0?perpage=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		status, body, err := r.HTTP.Get(ctx, u, headers)
		if err != nil {
			return nil, err
		}
		if err := statusErr(status); err != nil {
			return nil, err
		}
		var decoded struct {
			Items *[]struct {
				Link *string `json:"link"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			return nil, err
		}
		if decoded.Items == nil {
			return nil, errors.New("raindrop: items missing")
		}
		items := *decoded.Items
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			if it.Link == nil {
				continue
			}
			raw := swiftstr.TrimWhitespacesAndNewlines(*it.Link)
			if isHTTPURL(raw) && !seen[raw] {
				seen[raw] = true
				urls = append(urls, raw)
			}
		}
		if len(items) < perPage {
			break
		}
	}
	slog.InfoContext(ctx, "raindrop connector", "tenant", tenant, "urls", len(urls))
	return urls, nil
}

// RSSConnector is RSSConnector: a user-supplied feed, text-guarded by
// URLEnricherGuard.isPublic (no DNS, as in Swift).
type RSSConnector struct{ HTTP HTTPGetter }

func (RSSConnector) Binding() string { return "rss" }

func (r RSSConnector) FetchURLs(ctx context.Context, config map[string]string, tenant uuid.UUID) ([]string, error) {
	raw := swiftstr.TrimWhitespacesAndNewlines(config["feed_url"])
	if raw == "" {
		return nil, &ConnectorError{Kind: "missingConfig", Key: "feed_url"}
	}
	u, err := url.Parse(raw)
	if err != nil || !news.IsPublic(u) {
		return nil, &ConnectorError{Kind: "invalidConfig", Key: "feed_url"}
	}
	status, body, err := r.HTTP.Get(ctx, raw, map[string]string{"Accept": "application/rss+xml, application/atom+xml, application/xml, text/xml"})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, &ConnectorError{Kind: "upstreamFailure", Status: status}
	}
	urls := ExtractLinks(strings.ToValidUTF8(string(body), "�"))
	slog.InfoContext(ctx, "rss connector", "tenant", tenant, "urls", len(urls))
	return urls, nil
}

var linkPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)<link>\s*(https?://[^<\s]+)\s*</link>`),
	regexp.MustCompile(`(?i)<link\b[^>]*\bhref\s*=\s*["'](https?://[^"']+)["']`),
}

// ExtractLinks is RSSConnector.extractLinks: RSS <link>URL</link> first, then Atom
// <link href="URL"/>, deduplicated in order.
func ExtractLinks(xml string) []string {
	urls := []string{}
	seen := map[string]bool{}
	for _, re := range linkPatterns {
		for _, m := range re.FindAllStringSubmatch(xml, -1) {
			u := swiftstr.TrimWhitespacesAndNewlines(m[1])
			if isHTTPURL(u) && !seen[u] {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// NewsTickerConnector is NewsTickerConnector: a sync stages nothing.
type NewsTickerConnector struct{}

func (NewsTickerConnector) Binding() string { return "news-ticker" }

func (NewsTickerConnector) FetchURLs(context.Context, map[string]string, uuid.UUID) ([]string, error) {
	return []string{}, nil
}
