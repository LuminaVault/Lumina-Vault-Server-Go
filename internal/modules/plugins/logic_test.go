package plugins

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Ported from PluginLogicTests, PluginConnectorsSlice2Tests, MemoryProviderPluginTests
// (catalog part), SkillPluginCatalogTests and HermesHubSkillsServiceTests.

func slugs(entries []CatalogEntryDTO) []string {
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Slug)
	}
	return out
}

func TestReadwiseInCatalogWithSecretToken(t *testing.T) {
	e, ok := catalog["readwise"]
	require.True(t, ok)
	require.Equal(t, "readwise", e.binding)
	require.Equal(t, "connector", e.dto.CapabilityKind)
	require.Equal(t, []ConfigField{{Key: "access_token", Label: "Access token", Placeholder: ptr("readwise.io/access_token"), Kind: "secret", IsRequired: true}}, e.dto.ConfigFields)
}

func TestCatalogFiltersByCategoryAndSortsBySlug(t *testing.T) {
	require.Equal(t, []string{"byok-embeddings", "news-ticker", "raindrop", "reading-time", "readwise", "rss"}, slugs(catalogDTOs("", nil, nil)))
	require.Equal(t, []string{"raindrop", "readwise", "rss"}, slugs(catalogDTOs("connector", nil, nil)))
	require.Equal(t, []string{"byok-embeddings"}, slugs(catalogDTOs("memory", nil, nil)))
	yes := true
	require.Equal(t, []string{"news-ticker", "readwise"}, slugs(catalogDTOs("", &yes, nil)))
	require.Equal(t, []string{"byok-embeddings"}, slugs(catalogDTOs("", nil, &yes)))
}

func TestValidate(t *testing.T) {
	require.Equal(t, validation{}, validate("readwise", map[string]string{"access_token": "t"}))
	require.Equal(t, validation{kind: "unknown_plugin"}, validate("nope", map[string]string{}))
	require.Equal(t, validation{kind: "missing", key: "access_token"}, validate("readwise", map[string]string{}))
	require.Equal(t, validation{kind: "missing", key: "access_token"}, validate("readwise", map[string]string{"access_token": "  \n"}))
	require.Equal(t, validation{kind: "unknown_field", key: "x"}, validate("readwise", map[string]string{"access_token": "t", "x": "y"}))
	require.Equal(t, validation{kind: "missing", key: "access_token"}, validate("byok-embeddings", map[string]string{}))
	require.Equal(t, validation{}, validate("news-ticker", map[string]string{}))
	require.True(t, isPremium("byok-embeddings"))
	require.False(t, isPremium("skill-x"))
}

func TestRSSAndRaindropFieldKinds(t *testing.T) {
	require.Equal(t, "url", catalog["rss"].dto.ConfigFields[0].Kind)
	require.Equal(t, "secret", catalog["raindrop"].dto.ConfigFields[0].Kind)
}

type stubHTTP struct {
	pages []stubPage
	calls []string
	auth  []string
}

type stubPage struct {
	status int
	body   string
}

func (s *stubHTTP) Get(_ context.Context, rawURL string, headers map[string]string) (int, []byte, error) {
	s.calls = append(s.calls, rawURL)
	s.auth = append(s.auth, headers["Authorization"])
	p := s.pages[min(len(s.calls)-1, len(s.pages)-1)]
	return p.status, []byte(p.body), nil
}

func kindOf(t *testing.T, err error) string {
	t.Helper()
	var ce *ConnectorError
	require.True(t, errors.As(err, &ce), "%v", err)
	return ce.Kind
}

func TestReadwiseMapsSourceURLs(t *testing.T) {
	h := &stubHTTP{pages: []stubPage{
		{200, `{"nextPageCursor":"c2","results":[{"source_url":"https://example.com/a"},{"source_url":"https://example.com/a"},{"source_url":"ftp://nope"},{"source_url":null}]}`},
		{200, `{"nextPageCursor":null,"results":[{"source_url":" http://example.com/b "}]}`},
	}}
	urls, err := ReadwiseConnector{HTTP: h}.FetchURLs(context.Background(), map[string]string{"access_token": " tok "}, uuid.New())
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.com/a", "http://example.com/b"}, urls)
	require.Equal(t, []string{"https://readwise.io/api/v2/export/", "https://readwise.io/api/v2/export/?pageCursor=c2"}, h.calls)
	require.Equal(t, "Token tok", h.auth[0])
}

func TestReadwiseErrors(t *testing.T) {
	h := &stubHTTP{}
	_, err := ReadwiseConnector{HTTP: h}.FetchURLs(context.Background(), map[string]string{}, uuid.New())
	require.Equal(t, "missingConfig", kindOf(t, err))
	require.Empty(t, h.calls, "no request before the token check")
	_, err = ReadwiseConnector{HTTP: &stubHTTP{pages: []stubPage{{401, ""}}}}.FetchURLs(context.Background(), map[string]string{"access_token": "t"}, uuid.New())
	require.Equal(t, "unauthorized", kindOf(t, err))
	_, err = ReadwiseConnector{HTTP: &stubHTTP{pages: []stubPage{{500, ""}}}}.FetchURLs(context.Background(), map[string]string{"access_token": "t"}, uuid.New())
	require.Equal(t, "upstreamFailure", kindOf(t, err))
}

func TestRaindropMapsItemsAndStopsOnShortPage(t *testing.T) {
	h := &stubHTTP{pages: []stubPage{{200, `{"items":[{"link":"https://example.com/a"},{"link":"https://example.com/a"},{"link":"mailto:x"},{"link":null},{"link":"https://example.com/b"}]}`}}}
	urls, err := RaindropConnector{HTTP: h}.FetchURLs(context.Background(), map[string]string{"access_token": "t"}, uuid.New())
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.com/a", "https://example.com/b"}, urls)
	require.Equal(t, []string{"https://api.raindrop.io/rest/v1/raindrops/0?perpage=50&page=0"}, h.calls)
	require.Equal(t, "Bearer t", h.auth[0])
	_, err = RaindropConnector{HTTP: &stubHTTP{}}.FetchURLs(context.Background(), map[string]string{}, uuid.New())
	require.Equal(t, "missingConfig", kindOf(t, err))
	_, err = RaindropConnector{HTTP: &stubHTTP{pages: []stubPage{{401, ""}}}}.FetchURLs(context.Background(), map[string]string{"access_token": "t"}, uuid.New())
	require.Equal(t, "unauthorized", kindOf(t, err))
}

func TestExtractLinks(t *testing.T) {
	xml := `<rss><channel>
  <link>https://blog.example.com/</link>
  <item><link>https://blog.example.com/a</link></item>
  <item><link>https://blog.example.com/a</link></item>
  <entry><link rel="alternate" href="https://blog.example.com/b"/></entry>
  <item><link>ftp://nope/x</link></item>
</channel></rss>`
	require.Equal(t, []string{"https://blog.example.com/", "https://blog.example.com/a", "https://blog.example.com/b"}, ExtractLinks(xml))
}

func TestRSSFetch(t *testing.T) {
	h := &stubHTTP{pages: []stubPage{{200, "<rss><channel><item><link>https://example.com/a</link></item></channel></rss>"}}}
	urls, err := RSSConnector{HTTP: h}.FetchURLs(context.Background(), map[string]string{"feed_url": "https://example.com/feed.xml"}, uuid.New())
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.com/a"}, urls)
	_, err = RSSConnector{HTTP: h}.FetchURLs(context.Background(), map[string]string{}, uuid.New())
	require.Equal(t, "missingConfig", kindOf(t, err))
	for _, bad := range []string{"http://169.254.169.254/latest/meta-data/", "http://localhost:8080/feed"} {
		_, err = RSSConnector{HTTP: h}.FetchURLs(context.Background(), map[string]string{"feed_url": bad}, uuid.New())
		require.Equal(t, "invalidConfig", kindOf(t, err), bad)
	}
}

func TestSkillSlugs(t *testing.T) {
	e := skillEntry("pattern-detector", "Finds patterns.")
	require.Equal(t, "skill-pattern-detector", e.Slug)
	require.Equal(t, "skill", e.Category)
	require.Equal(t, "skill", e.CapabilityKind)
	require.Empty(t, e.ConfigFields)
	require.Equal(t, "Pattern Detector", e.Name)
	name, ok := lvSkillName("skill-pattern-detector")
	require.True(t, ok)
	require.Equal(t, "pattern-detector", name)
	for _, s := range []string{"readwise", "hermes-foo", "skill-"} {
		_, ok := lvSkillName(s)
		require.False(t, ok, s)
	}
}

func TestTitleizeAndSummarize(t *testing.T) {
	require.Equal(t, "Belief Evolution", titleize("belief_evolution"))
	require.Equal(t, "Kb Compile", titleize("kb-compile"))
	require.Equal(t, "first line", summarize("first line\nsecond"))
	require.Equal(t, strings.Repeat("x", 139)+"…", summarize(strings.Repeat("x", 200)))
}

func TestParseHermesSkills(t *testing.T) {
	obj := parseHermesSkills([]byte(`{"skills":[{"name":"gif-search","description":"Find GIFs"},{"name":"gif-search"}]}`))
	require.Len(t, obj, 1)
	require.Equal(t, "hermes-gif-search", obj[0].Slug)
	require.Equal(t, "Hermes", obj[0].Publisher)
	require.Equal(t, "Find GIFs", obj[0].Description)
	require.Equal(t, []string{"hermes-alpha", "hermes-beta"}, slugs(parseHermesSkills([]byte(`["alpha","beta"]`))))
	require.Equal(t, []string{"hermes-zeta"}, slugs(parseHermesSkills([]byte(`{"data":[{"id":"zeta"}]}`))))
	require.Empty(t, parseHermesSkills([]byte("not json")))
	require.Empty(t, parseHermesSkills([]byte(`{"other":1}`)))
}

func TestValidatedRef(t *testing.T) {
	for in, want := range map[string]string{
		"gif-search": "gif-search", "  amanning3390/hermeshub:gif-search  ": "amanning3390/hermeshub:gif-search",
		"https://example.com/SKILL.md": "https://example.com/SKILL.md",
	} {
		got, err := validatedRef(in)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	for _, bad := range []string{"", "   ", "two words", "has\ttab", "line\nbreak", "--help", "-rf", strings.Repeat("x", 513)} {
		_, err := validatedRef(bad)
		require.Error(t, err, bad)
	}
}
