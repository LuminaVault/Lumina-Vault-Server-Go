package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/swiftstr"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// EnrichedMetadata is what an enricher found out about a URL. A nil field wasn't
// found.
type EnrichedMetadata struct {
	Title       *string
	Description *string
	ImageURL    *string
	Author      *string
	URL         string
	Transcript  *string
	// Body is full-article markdown from the Jina post-processor, capped at 1 MiB.
	Body *string
	// ReadingTimeMinutes comes from the reading-time capture hook, when installed.
	ReadingTimeMinutes *int
}

// Enricher is URLEnricher.
type Enricher interface {
	CanHandle(u *URL) bool
	Enrich(ctx context.Context, u *URL) (EnrichedMetadata, error)
}

const (
	enrichTimeout = 10 * time.Second
	browserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	// maxFetchBytes bounds what a fetched page can cost in memory. Swift reads a
	// response whole and has no bound (URLEnricherGuard.maxBodyBytes is unused).
	maxFetchBytes = 32 * 1024 * 1024
)

// fetch is URLSession.data(for:) with timeoutInterval: it follows redirects and
// doesn't look at the status. The timeout here covers the whole request, where
// URLSession's is an idle timeout.
func fetch(ctx context.Context, client *http.Client, rawURL string, timeout time.Duration, headers map[string]string, limit int64) (int, []byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req) //nolint:gosec // the point of enrichment; see IsPublic for what guards it
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// decodeEntities is the entity list the three scraping enrichers share, in
// Swift's order, so "&amp;lt;" ends as "&lt;" and then "<".
var htmlEntities = strings.NewReplacer("&#39;", "'")

func decodeHTMLEntities(text string) string {
	text = htmlEntities.Replace(text)
	text = strings.ReplaceAll(text, "&quot;", `"`)
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	return swiftstr.TrimWhitespacesAndNewlines(text)
}

// latin1 is String(data:encoding:.isoLatin1).
func latin1(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

// GenericOGEnricher is the catch-all: Open Graph tags, then <title> and the
// description meta tag. Any fetch failure gives an empty result, not an error.
type GenericOGEnricher struct{ Client *http.Client }

func (GenericOGEnricher) CanHandle(*URL) bool { return true }

func (e GenericOGEnricher) Enrich(ctx context.Context, u *URL) (EnrichedMetadata, error) {
	metadata := EnrichedMetadata{URL: u.Absolute}
	_, data, err := fetch(ctx, e.Client, u.Absolute, enrichTimeout, map[string]string{"User-Agent": browserAgent}, maxFetchBytes)
	if err != nil {
		return metadata, nil //nolint:nilerr // Swift ignores errors for the fallback enricher
	}
	html := string(data)
	if !utf8.Valid(data) {
		html = latin1(data)
	}
	metadata.Title = firstNonNil(metaContent("property", "og:title", html), titleTag(html))
	metadata.Description = firstNonNil(metaContent("property", "og:description", html), metaContent("name", "description", html))
	metadata.ImageURL = metaContent("property", "og:image", html)
	metadata.Author = metaContent("property", "og:site_name", html)
	return metadata, nil
}

func firstNonNil(values ...*string) *string {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

// metaRes holds GenericOGEnricher's patterns, one per attribute and value. Both
// attribute orders are alternatives, so the content is in group 1 or 2.
var metaRes = func() map[string]*regexp.Regexp {
	out := map[string]*regexp.Regexp{}
	for _, a := range [][2]string{{"property", "og:title"}, {"property", "og:description"}, {"property", "og:image"}, {"property", "og:site_name"}, {"name", "description"}} {
		attr, value := a[0], a[1]
		out[attr+"="+value] = regexp.MustCompile(`(?i)<meta\s+(?:[^>]*?\s+)?` + attr + `=["']` + value + `["']\s+(?:[^>]*?\s+)?content=["']([^"']*)["'][^>]*>|<meta\s+(?:[^>]*?\s+)?content=["']([^"']*)["']\s+(?:[^>]*?\s+)?` + attr + `=["']` + value + `["'][^>]*>`)
	}
	return out
}()

var titleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func metaContent(attr, value, html string) *string {
	m := metaRes[attr+"="+value].FindStringSubmatchIndex(html)
	if m == nil {
		return nil
	}
	for _, group := range []int{1, 2} {
		if m[2*group] >= 0 {
			s := decodeHTMLEntities(html[m[2*group]:m[2*group+1]])
			return &s
		}
	}
	return nil
}

func titleTag(html string) *string {
	m := titleRe.FindStringSubmatch(html)
	if m == nil {
		return nil
	}
	s := decodeHTMLEntities(m[1])
	return &s
}

// stringField is `json[key] as? String`.
func stringField(doc map[string]any, key string) *string {
	if s, ok := doc[key].(string); ok {
		return &s
	}
	return nil
}

// XEnricher reads a tweet's oEmbed. QUIRK, as in Swift: "contains x.com" also
// matches netflix.com and the like, which then get no metadata at all.
type XEnricher struct {
	Client *http.Client
	// OEmbedURL defaults to Twitter's. Tests point it at a stub.
	OEmbedURL string
}

func (XEnricher) CanHandle(u *URL) bool { return hostContains(u, "twitter.com", "x.com") }

var (
	tweetParagraphRe = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
	anyTagRe         = regexp.MustCompile(`<[^>]+>`)
)

func (e XEnricher) Enrich(ctx context.Context, u *URL) (EnrichedMetadata, error) {
	metadata := EnrichedMetadata{URL: u.Absolute}
	endpoint := e.OEmbedURL
	if endpoint == "" {
		endpoint = "https://publish.twitter.com/oembed"
	}
	_, data, err := fetch(ctx, e.Client, endpoint+"?url="+u.Absolute, enrichTimeout, nil, maxFetchBytes)
	if err != nil {
		return metadata, nil //nolint:nilerr // Swift falls through with what it has
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil {
		return metadata, nil //nolint:nilerr // `try?`: a body that isn't JSON yields nothing
	}
	metadata.Author = stringField(doc, "author_name")
	if html, ok := doc["html"].(string); ok {
		if m := tweetParagraphRe.FindStringSubmatch(html); m != nil {
			text := decodeHTMLEntities(anyTagRe.ReplaceAllString(m[1], ""))
			metadata.Description = &text
		}
	}
	return metadata, nil
}

// YouTubeEnricher reads a video's oEmbed, then scrapes the page for a caption
// track. Each step is best effort.
type YouTubeEnricher struct {
	Client *http.Client
	// OEmbedURL defaults to YouTube's. Tests point it at a stub.
	OEmbedURL string
}

func (YouTubeEnricher) CanHandle(u *URL) bool { return hostContains(u, "youtube.com", "youtu.be") }

var captionTrackRe = regexp.MustCompile(`"captionTracks":\s*\[\s*\{\s*"baseUrl":\s*"([^"]+)"`)

func (e YouTubeEnricher) Enrich(ctx context.Context, u *URL) (EnrichedMetadata, error) {
	metadata := EnrichedMetadata{URL: u.Absolute}
	endpoint := e.OEmbedURL
	if endpoint == "" {
		endpoint = "https://www.youtube.com/oembed"
	}
	if _, data, err := fetch(ctx, e.Client, endpoint+"?url="+u.Absolute+"&format=json", enrichTimeout, nil, maxFetchBytes); err == nil {
		var doc map[string]any
		if json.Unmarshal(data, &doc) == nil {
			metadata.Title = stringField(doc, "title")
			metadata.Author = stringField(doc, "author_name")
			metadata.ImageURL = stringField(doc, "thumbnail_url")
		}
	}

	_, page, err := fetch(ctx, e.Client, u.Absolute, enrichTimeout, map[string]string{"User-Agent": browserAgent}, maxFetchBytes)
	if err != nil || !utf8.Valid(page) {
		return metadata, nil //nolint:nilerr // transcript scraping is best effort
	}
	m := captionTrackRe.FindSubmatch(page)
	if m == nil {
		return metadata, nil
	}
	captionURL := strings.ReplaceAll(string(m[1]), `&`, "&")
	parsed, ok := ParseURL(captionURL)
	if !ok {
		return metadata, nil
	}
	_, xml, err := fetch(ctx, e.Client, parsed.Absolute, enrichTimeout, nil, maxFetchBytes)
	if err != nil || !utf8.Valid(xml) {
		return metadata, nil //nolint:nilerr // transcript scraping is best effort
	}
	stripped := anyTagRe.ReplaceAllString(string(xml), " ")
	stripped = strings.ReplaceAll(stripped, "&#39;", "'")
	stripped = strings.ReplaceAll(stripped, "&quot;", `"`)
	stripped = strings.ReplaceAll(stripped, "&amp;", "&")
	transcript := swiftstr.TrimWhitespacesAndNewlines(stripped)
	metadata.Transcript = &transcript
	return metadata, nil
}

// JinaEnricher is the tier-2 post-processor: full-page markdown from jina.ai's
// reader, which works without a key. It retries once on a 429. Other failures
// are returned, and the caller keeps what the primary enricher found.
type JinaEnricher struct {
	Client *http.Client
	APIKey string
	// BaseURL defaults to Jina's, and RetryDelay to Swift's 2 s. Tests set both.
	BaseURL    string
	RetryDelay time.Duration
}

const (
	jinaBodyCap = 1024 * 1024
	jinaTimeout = 15 * time.Second
)

// Jina's failures, as JinaEnricherError.
var (
	ErrJinaRateLimited = errors.New("jina: rate limited")
)

// JinaUpstreamError is JinaEnricherError.upstreamStatus.
type JinaUpstreamError struct{ Status int }

func (e *JinaUpstreamError) Error() string { return fmt.Sprintf("jina: upstream status %d", e.Status) }

func (JinaEnricher) CanHandle(*URL) bool { return true }

func (e JinaEnricher) Enrich(ctx context.Context, u *URL) (EnrichedMetadata, error) {
	metadata, err := e.dispatch(ctx, u)
	if !errors.Is(err, ErrJinaRateLimited) {
		return metadata, err
	}
	delay := e.RetryDelay
	if delay == 0 {
		delay = 2 * time.Second
	}
	slog.WarnContext(ctx, "jina rate-limited; retrying once", "delay", delay.String())
	select {
	case <-ctx.Done():
		return EnrichedMetadata{}, ctx.Err()
	case <-time.After(delay):
	}
	return e.dispatch(ctx, u)
}

func (e JinaEnricher) dispatch(ctx context.Context, u *URL) (EnrichedMetadata, error) {
	base := e.BaseURL
	if base == "" {
		base = "https://r.jina.ai/"
	}
	headers := map[string]string{"Accept": "text/markdown", "X-Return-Format": "markdown"}
	if e.APIKey != "" {
		headers["Authorization"] = "Bearer " + e.APIKey
	}
	status, data, err := fetch(ctx, e.Client, base+encodeComponentStrict(u.Absolute, isURLPathAllowed), jinaTimeout, headers, jinaBodyCap)
	if err != nil {
		return EnrichedMetadata{}, err
	}
	if status == http.StatusTooManyRequests {
		return EnrichedMetadata{}, ErrJinaRateLimited
	}
	if status < 200 || status >= 300 {
		return EnrichedMetadata{}, &JinaUpstreamError{Status: status}
	}
	// QUIRK, as in Swift: a cap landing inside a multi-byte character makes the
	// bytes invalid UTF-8, and the whole body becomes empty.
	body := ""
	if utf8.Valid(data) {
		body = string(data)
	}
	return EnrichedMetadata{URL: u.Absolute, Body: &body}, nil
}

// encodeComponentStrict is addingPercentEncoding(withAllowedCharacters:): unlike
// encodeComponent it also encodes "%", so an escape in the URL is escaped again.
func encodeComponentStrict(s string, allowed func(byte) bool) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		if allowed(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// isURLPathAllowed is CharacterSet.urlPathAllowed, measured on Foundation:
// !$&'()*+,-./0-9:;=@A-Z_a-z~. So "?" and "#" get encoded, and Jina is asked for
// a page whose query is part of its path.
func isURLPathAllowed(c byte) bool {
	return isUnreserved(c) || strings.IndexByte("!$&'()*+,;=:@/", c) >= 0
}
