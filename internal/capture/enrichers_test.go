package capture

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func jinaStub(t *testing.T, handler http.HandlerFunc) JinaEnricher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return JinaEnricher{APIKey: "test-key", BaseURL: srv.URL + "/", RetryDelay: time.Millisecond}
}

// JinaEnricherTests.swift.
func TestJinaHandlesAnyURL(t *testing.T) {
	e := JinaEnricher{}
	require.True(t, e.CanHandle(mustURL(t, "https://example.com/article")))
	require.True(t, e.CanHandle(mustURL(t, "https://blog.example.org")))
}

func TestJinaPopulatesBody(t *testing.T) {
	var got *http.Request
	e := jinaStub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte("# Article Title\n\nFull body text from jina."))
	})
	u := mustURL(t, "https://example.com/article")
	m, err := e.Enrich(context.Background(), u)
	require.NoError(t, err)
	require.Equal(t, "Bearer test-key", got.Header.Get("Authorization"))
	require.Equal(t, "text/markdown", got.Header.Get("Accept"))
	require.Equal(t, "markdown", got.Header.Get("X-Return-Format"))
	require.Equal(t, "/https://example.com/article", got.URL.Path)
	require.Equal(t, u.Absolute, m.URL)
	require.Equal(t, "# Article Title\n\nFull body text from jina.", *m.Body)
}

func TestJinaWithoutKeyOmitsAuthorization(t *testing.T) {
	var got *http.Request
	e := jinaStub(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte("body"))
	})
	e.APIKey = ""
	_, err := e.Enrich(context.Background(), mustURL(t, "https://example.com"))
	require.NoError(t, err)
	require.Empty(t, got.Header.Values("Authorization"))
}

func TestJinaCapsBody(t *testing.T) {
	e := jinaStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 2*1024*1024)))
	})
	m, err := e.Enrich(context.Background(), mustURL(t, "https://example.com"))
	require.NoError(t, err)
	require.Len(t, *m.Body, 1024*1024)
}

// QUIRK: the cap splits a character, and the whole body becomes empty.
func TestJinaCapInsideACharacterEmptiesBody(t *testing.T) {
	e := jinaStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 1024*1024-1) + "é"))
	})
	m, err := e.Enrich(context.Background(), mustURL(t, "https://example.com"))
	require.NoError(t, err)
	require.Empty(t, *m.Body)
}

func TestJinaRetriesOnceOn429(t *testing.T) {
	var attempts atomic.Int32
	e := jinaStub(t, func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("recovered"))
	})
	m, err := e.Enrich(context.Background(), mustURL(t, "https://example.com"))
	require.NoError(t, err)
	require.EqualValues(t, 2, attempts.Load())
	require.Equal(t, "recovered", *m.Body)
}

func TestJinaFailsAfterTwo429s(t *testing.T) {
	var attempts atomic.Int32
	e := jinaStub(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := e.Enrich(context.Background(), mustURL(t, "https://example.com"))
	require.ErrorIs(t, err, ErrJinaRateLimited)
	require.EqualValues(t, 2, attempts.Load())
}

func TestJinaUpstreamStatusFails(t *testing.T) {
	e := jinaStub(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	_, err := e.Enrich(context.Background(), mustURL(t, "https://example.com"))
	var upstream *JinaUpstreamError
	require.True(t, errors.As(err, &upstream))
	require.Equal(t, http.StatusBadGateway, upstream.Status)
}

func TestJinaNetworkErrorFails(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	_, err := JinaEnricher{BaseURL: srv.URL + "/"}.Enrich(context.Background(), mustURL(t, "https://example.com"))
	require.Error(t, err)
}

// urlPathAllowed leaves ":" and "/" alone but encodes what a path can't hold.
func TestJinaEncodesQueryIntoThePath(t *testing.T) {
	var uri string
	e := jinaStub(t, func(w http.ResponseWriter, r *http.Request) {
		uri = r.RequestURI
		_, _ = w.Write([]byte("b"))
	})
	_, err := e.Enrich(context.Background(), mustURL(t, "https://example.com/a?q=1#f"))
	require.NoError(t, err)
	require.Equal(t, "/https://example.com/a%3Fq=1%23f", uri)
}

func page(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestGenericOGReadsTags(t *testing.T) {
	raw := page(t, http.StatusOK, `<title>Fallback</title><meta property="og:title" content="A &amp; B">
		<meta content="Desc" property="og:description"><meta property='og:image' content='https://i/a.png'>
		<meta property="og:site_name" content="Site">`)
	m, err := GenericOGEnricher{Client: http.DefaultClient}.Enrich(context.Background(), mustURL(t, raw))
	require.NoError(t, err)
	require.Equal(t, "A & B", *m.Title)
	require.Equal(t, "Desc", *m.Description)
	require.Equal(t, "https://i/a.png", *m.ImageURL)
	require.Equal(t, "Site", *m.Author)
}

func TestGenericOGFallsBackToTitleAndDescriptionMeta(t *testing.T) {
	raw := page(t, http.StatusOK, `<TITLE>Plain</TITLE><meta name="description" content="Meta desc">`)
	m, err := GenericOGEnricher{Client: http.DefaultClient}.Enrich(context.Background(), mustURL(t, raw))
	require.NoError(t, err)
	require.Equal(t, "Plain", *m.Title)
	require.Equal(t, "Meta desc", *m.Description)
	require.Nil(t, m.ImageURL)
}

func TestGenericOGReadsLatin1AndIgnoresStatus(t *testing.T) {
	raw := page(t, http.StatusInternalServerError, "<title>caf\xe9</title>")
	m, err := GenericOGEnricher{Client: http.DefaultClient}.Enrich(context.Background(), mustURL(t, raw))
	require.NoError(t, err)
	require.Equal(t, "café", *m.Title)
}

func TestGenericOGNetworkFailureIsAnEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	raw := srv.URL
	srv.Close()
	m, err := GenericOGEnricher{Client: http.DefaultClient}.Enrich(context.Background(), mustURL(t, raw))
	require.NoError(t, err)
	require.Nil(t, m.Title)
	require.Equal(t, raw, m.URL)
}

func TestXReadsOEmbed(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{
			"author_name": "Jack", "html": `<blockquote><p lang="en">just <a href="x">setting</a> up &amp; ok</p></blockquote>`,
		})
	}))
	defer srv.Close()
	m, err := XEnricher{Client: http.DefaultClient, OEmbedURL: srv.URL}.Enrich(context.Background(), mustURL(t, "https://x.com/jack/status/20"))
	require.NoError(t, err)
	require.Equal(t, "url=https://x.com/jack/status/20", query)
	require.Equal(t, "Jack", *m.Author)
	require.Equal(t, "just setting up & ok", *m.Description)
}

func TestXBadBodyIsAnEmptyResult(t *testing.T) {
	m, err := XEnricher{Client: http.DefaultClient, OEmbedURL: page(t, http.StatusOK, "nope")}.Enrich(context.Background(), mustURL(t, "https://x.com/a"))
	require.NoError(t, err)
	require.Nil(t, m.Author)
	require.Nil(t, m.Description)
}

func TestYouTubeReadsOEmbedAndTranscript(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/oembed", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"title": "T", "author_name": "A", "thumbnail_url": "https://i/t.jpg"})
	})
	mux.HandleFunc("/watch", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`x"captionTracks": [ { "baseUrl": "` + srv.URL + `/caption?a=1&b=2"`))
	})
	mux.HandleFunc("/caption", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "a=1&b=2", r.URL.RawQuery)
		_, _ = w.Write([]byte(`<t>it&#39;s</t><t>&quot;a&quot; &amp; b</t>`))
	})
	m, err := YouTubeEnricher{Client: http.DefaultClient, OEmbedURL: srv.URL + "/oembed"}.Enrich(context.Background(), mustURL(t, srv.URL+"/watch?v=x"))
	require.NoError(t, err)
	require.Equal(t, "T", *m.Title)
	require.Equal(t, "A", *m.Author)
	require.Equal(t, "https://i/t.jpg", *m.ImageURL)
	require.Equal(t, `it's  "a" & b`, *m.Transcript)
}

func TestYouTubeWithoutCaptionsHasNoTranscript(t *testing.T) {
	raw := page(t, http.StatusOK, `{"title":"T"}`)
	m, err := YouTubeEnricher{Client: http.DefaultClient, OEmbedURL: raw}.Enrich(context.Background(), mustURL(t, raw))
	require.NoError(t, err)
	require.Equal(t, "T", *m.Title)
	require.Nil(t, m.Transcript)
}

func TestRenderMarkdown(t *testing.T) {
	title := `A "quoted" title`
	got := RenderMarkdown(EnrichedMetadata{
		Title: &title, Author: ptr(`Au"thor`), ImageURL: ptr("https://i/a.png"), Description: ptr("D"), Transcript: ptr("T"),
		Body: ptr("B"), ReadingTimeMinutes: ptr(3),
	}, `https://e.com/a"b`)
	require.Equal(t, "---\ntitle: \"A \\\"quoted\\\" title\"\nsource: \"https://e.com/a\"b\"\nauthor: \"Au\\\"thor\"\nimage: \"https://i/a.png\"\nreading_time: 3\n---\n\n"+
		"# A \"quoted\" title\n\n## Description\nD\n\n## Transcript\nT\n\n## Content\nB\n\n", got)
	require.Equal(t, "---\nsource: \"u\"\n---\n\n", RenderMarkdown(EnrichedMetadata{Title: ptr(""), Description: ptr("")}, "u"))
}

func TestCountWordsSplitsOnUnicodeWhitespace(t *testing.T) {
	require.Equal(t, 0, countWords(""))
	require.Equal(t, 3, countWords(" a\tb c\n"))
	require.Equal(t, 2, countWords("a\r\nb"))
}

func TestPlaceholderMarkdown(t *testing.T) {
	require.Equal(t, "---\nsource: \"u\"\n---\n\n# [Pending Enrichment]\nURL: u\n", placeholderMarkdown("u", nil))
	require.Equal(t, "---\nsource: \"u\"\n---\n\n# [Pending Enrichment]\nURL: u\n", placeholderMarkdown("u", ptr("")))
	require.Equal(t, "---\nsource: \"u\"\n---\n\n# [Pending Enrichment]\nURL: u\n\n## Note\n n \n", placeholderMarkdown("u", ptr(" n ")))
}
