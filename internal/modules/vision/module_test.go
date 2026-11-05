package vision_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/vision"
	"lumina/internal/ratelimit"
	"lumina/internal/usagemeter"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func fixture(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("6B0E0000-0000-4000-8000-%012X", n))
}

var (
	srch    = fixture(0xA6)
	oth     = fixture(0xA7)
	coh     = fixture(0xA8)
	rateCap = fixture(0xA4)
	keys    = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	stubCfg = vision.Config{Provider: "stub", StubDim: 1024, StubFill: 0.1, StubModel: "stub-clip", StubSourceWidth: 768, StubSourceHeight: 768}
)

type env struct {
	t      *testing.T
	e      *echo.Echo
	pool   *pgxpool.Pool
	limits *ratelimit.Store
}

func setup(t *testing.T, cfg vision.Config, enforcement bool) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "vision.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err, f)
	}
	limits := ratelimit.NewStore()
	m := vision.New(memorystore.New(pool, nil), usagemeter.New(pool), auth.Identify(keys, auth.PGUsers(pool)),
		entitlement.PlatformFundedMiddleware(pool, entitlement.MemoryQuery, enforcement), limits, cfg)
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{m}})
	return env{t: t, e: e, pool: pool, limits: limits}
}

// image posts body with the given media type; "" sends no Content-Type.
func (v env) image(user uuid.UUID, path, mime string, body []byte) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	if mime != "" {
		req.Header.Set("Content-Type", mime)
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func (v env) png(user uuid.UUID, path string) *httptest.ResponseRecorder {
	v.t.Helper()
	return v.image(user, path, "image/png", []byte("\x89PNG"))
}

func message(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out.Error.Message
}

type embedOut struct {
	Embedding    []float64 `json:"embedding"`
	Dim          int       `json:"dim"`
	Model        string    `json:"model"`
	SourceWidth  int       `json:"source_width"`
	SourceHeight int       `json:"source_height"`
}

type searchOut struct {
	Hits []struct {
		ID        string  `json:"id"`
		Content   string  `json:"content"`
		Distance  float64 `json:"distance"`
		CreatedAt *string `json:"createdAt"`
	} `json:"hits"`
	Model string `json:"model"`
}

func searchHits(t *testing.T, rec *httptest.ResponseRecorder) searchOut {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out searchOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// VisionSearchLimitTests in Vision/VisionSearchLimitTests.swift, over HTTP.
func TestSearchLimit(t *testing.T) {
	v := setup(t, stubCfg, false)
	t.Run("absent limit falls back to the default", func(t *testing.T) {
		require.Equal(t, http.StatusOK, v.png(srch, "/v1/vision/search").Code)
	})
	t.Run("an explicit limit inside the range is honoured", func(t *testing.T) {
		require.Len(t, searchHits(t, v.png(srch, "/v1/vision/search?limit=2")).Hits, 2)
	})
	t.Run("the boundaries are inclusive", func(t *testing.T) {
		require.Len(t, searchHits(t, v.png(oth, "/v1/vision/search?limit=1")).Hits, 1)
		require.Len(t, searchHits(t, v.png(oth, "/v1/vision/search?limit=50")).Hits, 1)
	})
	t.Run("out of range is rejected, not clamped", func(t *testing.T) {
		for _, raw := range []string{"0", "-1", "51", "500"} {
			r := v.png(coh, "/v1/vision/search?limit="+raw)
			require.Equal(t, http.StatusBadRequest, r.Code, raw)
			require.Equal(t, "limit must be an integer between 1 and 50", message(t, r))
		}
	})
	t.Run("a non-numeric limit is rejected rather than silently defaulted", func(t *testing.T) {
		require.Equal(t, http.StatusBadRequest, v.png(coh, "/v1/vision/search?limit=ten").Code)
		require.Equal(t, http.StatusBadRequest, v.png(coh, "/v1/vision/search?limit=").Code)
	})
}

func TestEmbedWithStub(t *testing.T) {
	v := setup(t, stubCfg, false)
	r := v.png(oth, "/v1/vision/embed")
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	var out embedOut
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &out))
	require.Equal(t, 1536, out.Dim)
	require.Len(t, out.Embedding, 1536)
	require.InDelta(t, 0.1, out.Embedding[1023], 1e-9)
	require.Zero(t, out.Embedding[1024], "padded with zeros")
	require.Equal(t, "stub-clip", out.Model)
	require.Equal(t, 768, out.SourceWidth)
	require.Equal(t, 768, out.SourceHeight)
	require.Contains(t, r.Body.String(), `[0.1,0.1,`, "a Float prints as itself, not as a widened double")
	require.Equal(t, http.StatusOK, v.image(oth, "/v1/vision/embed", "image/png", nil).Code, "an empty body is passed on")
}

func (v env) embeddingOf(id uuid.UUID) *string {
	v.t.Helper()
	var text *string
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT embedding::text FROM memories WHERE id = $1`, id).Scan(&text))
	return text
}

func TestIndexAsMemory(t *testing.T) {
	v := setup(t, stubCfg, false)
	target := fixture(0xE7)
	foreign := fixture(0xE8)

	t.Run("bad pairs are 400s and write nothing", func(t *testing.T) {
		id := ids.Upper(target)
		for query, want := range map[string]string{
			"?indexAs=memory":               "memoryId required and must be a UUID when indexAs=memory",
			"?indexAs=memory&memoryId=nope": "memoryId required and must be a UUID when indexAs=memory",
			"?memoryId=" + id:               "indexAs must be 'memory' when supplied",
			"?indexAs=note&memoryId=" + id:  "indexAs must be 'memory' when supplied",
			"?indexAs=&memoryId=" + id:      "indexAs must be 'memory' when supplied",
		} {
			r := v.png(srch, "/v1/vision/embed"+query)
			require.Equal(t, http.StatusBadRequest, r.Code, query)
			require.Equal(t, want, message(t, r), query)
		}
		require.Nil(t, v.embeddingOf(target))
	})
	t.Run("another tenant's memory is a 404 and stays as it was", func(t *testing.T) {
		before := v.embeddingOf(foreign)
		r := v.png(srch, "/v1/vision/embed?indexAs=memory&memoryId="+ids.Upper(foreign))
		require.Equal(t, http.StatusNotFound, r.Code)
		require.Equal(t, "memory not found for tenant", message(t, r))
		require.Equal(t, *before, *v.embeddingOf(foreign))
	})
	t.Run("a valid pair writes the vector and leaves the rest of the row", func(t *testing.T) {
		var content string
		var updated *time.Time
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT content, updated_at FROM memories WHERE id = $1`, target).Scan(&content, &updated))
		r := v.png(srch, "/v1/vision/embed?indexAs=memory&memoryId="+strings.ToLower(target.String()))
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		require.NotNil(t, v.embeddingOf(target))
		var after string
		var updatedAfter *time.Time
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT content, updated_at FROM memories WHERE id = $1`, target).Scan(&after, &updatedAfter))
		require.Equal(t, content, after)
		require.Equal(t, updated, updatedAfter)
	})
}

func TestSearchFindsNearestAndWritesNothing(t *testing.T) {
	v := setup(t, stubCfg, false)
	var hitsBefore int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT coalesce(sum(query_hit_count),0)::bigint FROM memories`).Scan(&hitsBefore))
	out := searchHits(t, v.png(srch, "/v1/vision/search"))
	require.Equal(t, "stub-clip", out.Model)
	contents := make([]string, len(out.Hits))
	for i, h := range out.Hits {
		contents[i] = h.Content
	}
	require.Equal(t, "Same as the stub vector", contents[0], "closest first")
	require.Contains(t, contents, "A model answer")
	require.NotContains(t, contents, "No embedding yet")
	require.NotContains(t, contents, "Rejected in review")
	require.NotContains(t, contents, "Another tenant")
	for i := 1; i < len(out.Hits); i++ {
		require.GreaterOrEqual(t, out.Hits[i].Distance, out.Hits[i-1].Distance)
	}
	var nullDate bool
	for _, h := range out.Hits {
		if h.Content == "Far from the stub vector" {
			nullDate = h.CreatedAt == nil
		}
	}
	require.True(t, nullDate, "a memory with no created_at has no key")
	var hitsAfter int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT coalesce(sum(query_hit_count),0)::bigint FROM memories`).Scan(&hitsAfter))
	require.Equal(t, hitsBefore, hitsAfter, "a lookup that isn't recall counts no hits")
	empty := searchHits(t, v.png(coh, "/v1/vision/search"))
	require.NotNil(t, empty.Hits)
	require.Empty(t, empty.Hits)
}

func (v env) usage(tenant uuid.UUID, model string) int64 {
	v.t.Helper()
	var in int64
	require.NoError(v.t, v.pool.QueryRow(context.Background(),
		`SELECT coalesce(sum(mtok_in),0)::bigint FROM usage_meter WHERE tenant_id = $1 AND model = $2`, tenant, model).Scan(&in))
	return in
}

func TestUsageIsOneCallPerEmbedByProviderKind(t *testing.T) {
	v := setup(t, stubCfg, false)
	require.Equal(t, http.StatusOK, v.png(oth, "/v1/vision/embed").Code)
	require.Equal(t, http.StatusOK, v.png(oth, "/v1/vision/search").Code)
	require.Eventually(t, func() bool { return v.usage(oth, "vision.embed:stub") == 2 }, 5*time.Second, 20*time.Millisecond)
}

func TestRequestChecksInOrder(t *testing.T) {
	v := setup(t, stubCfg, false)
	big := make([]byte, 8*1024*1024+1)
	exact := big[:8*1024*1024]

	t.Run("no token first, whatever else is wrong", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, v.png(uuid.Nil, "/v1/vision/embed").Code)
		require.Equal(t, http.StatusUnauthorized, v.image(uuid.Nil, "/v1/vision/search?limit=0", "text/plain", big).Code)
	})
	t.Run("a bad type next", func(t *testing.T) {
		r := v.image(srch, "/v1/vision/search?limit=0", "text/plain", big)
		require.Equal(t, http.StatusUnsupportedMediaType, r.Code)
		require.Equal(t, "Content-Type must be one of: image/heic, image/jpeg, image/png, image/webp", message(t, r))
		require.Equal(t, http.StatusUnsupportedMediaType, v.image(srch, "/v1/vision/embed", "", []byte("x")).Code)
	})
	t.Run("then a declared length over the cap", func(t *testing.T) {
		r := v.image(srch, "/v1/vision/search?limit=0", "image/png", big)
		require.Equal(t, http.StatusRequestEntityTooLarge, r.Code)
		require.Equal(t, "image body exceeds 8388608 byte cap", message(t, r))
	})
	t.Run("a body with no declared length is cut at the cap", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/vision/embed", io.NopCloser(strings.NewReader(string(big))))
		req.ContentLength = -1
		req.Header.Set("Content-Type", "image/png")
		tok, err := scenario.Mint(keys, oth)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		v.e.ServeHTTP(rec, req)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	})
	t.Run("exactly the cap is fine", func(t *testing.T) {
		require.Equal(t, http.StatusOK, v.image(oth, "/v1/vision/embed", "image/png", exact).Code)
	})
}

func TestRateLimits(t *testing.T) {
	v := setup(t, stubCfg, false)
	now := time.Now()
	v.limits.SetClock(func() time.Time { return now })
	t.Run("ten a minute, shared by both routes", func(t *testing.T) {
		for i := range 10 {
			require.Equal(t, http.StatusUnsupportedMediaType, v.image(rateCap, "/v1/vision/embed", "text/plain", nil).Code, i)
		}
		r := v.image(rateCap, "/v1/vision/search", "text/plain", nil)
		require.Equal(t, http.StatusTooManyRequests, r.Code)
		require.NotEmpty(t, r.Header().Get("Retry-After"))
	})
	t.Run("two hundred a day", func(t *testing.T) {
		user := fixture(0xA5)
		for i := range 200 {
			if i%10 == 0 {
				now = now.Add(61 * time.Second)
			}
			require.Equal(t, http.StatusUnsupportedMediaType, v.image(user, "/v1/vision/embed", "text/plain", nil).Code, i)
		}
		now = now.Add(61 * time.Second)
		require.Equal(t, http.StatusTooManyRequests, v.image(user, "/v1/vision/embed", "text/plain", nil).Code)
	})
}

func TestNoProviderIsA503(t *testing.T) {
	for name, cfg := range map[string]vision.Config{
		"cohere without a key":   {Provider: "cohere"},
		"a kind with no adapter": {Provider: "openai", CohereAPIKey: "k"},
	} {
		t.Run(name, func(t *testing.T) {
			v := setup(t, cfg, false)
			for _, path := range []string{"/v1/vision/embed", "/v1/vision/search"} {
				r := v.png(srch, path)
				require.Equal(t, http.StatusServiceUnavailable, r.Code)
				require.Equal(t, "vision embed provider not configured", message(t, r))
			}
			require.Equal(t, http.StatusBadRequest, v.png(srch, "/v1/vision/embed?indexAs=bogus").Code, "parsing comes first")
			var n int
			require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM usage_meter WHERE model LIKE 'vision.%'`).Scan(&n))
			require.Zero(t, n)
		})
	}
}

func TestPlatformFundedGate(t *testing.T) {
	v := setup(t, stubCfg, true)
	require.Equal(t, http.StatusUnauthorized, v.image(uuid.Nil, "/v1/vision/embed", "text/plain", nil).Code)
	for _, tenant := range []uuid.UUID{fixture(0xB1), fixture(0xB2), fixture(0xB3)} {
		r := v.png(tenant, "/v1/vision/embed")
		require.Equal(t, http.StatusPaymentRequired, r.Code)
		require.JSONEq(t, `{"paywall":true,"paywallId":"default","requiredTier":"pro"}`, r.Body.String())
	}
	for _, tenant := range []uuid.UUID{fixture(0xB4), fixture(0xB5), fixture(0xB6)} {
		require.Equal(t, http.StatusOK, v.png(tenant, "/v1/vision/search").Code)
	}
}

// A stand-in for Cohere's POST /v2/embed. No real key is ever sent anywhere.
func TestCohereAdapter(t *testing.T) {
	var seen struct {
		path, auth, contentType string
		body                    map[string]any
	}
	answer := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":{"float":[[0.25,1.5e-05]]}}`))
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path, seen.auth, seen.contentType = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seen.body)
		answer(w, r)
	}))
	defer upstream.Close()
	v := setup(t, vision.Config{Provider: "cohere", CohereAPIKey: "test-key", CohereBaseURL: upstream.URL + "/", CohereModel: "embed-image-v3.0"}, false)

	t.Run("the request Swift sends", func(t *testing.T) {
		r := v.image(oth, "/v1/vision/embed", "IMAGE/PNG; x=y", []byte("\x89PNG"))
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		require.Equal(t, "/v2/embed", seen.path)
		require.Equal(t, "Bearer test-key", seen.auth)
		require.Equal(t, "application/json", seen.contentType)
		require.Equal(t, map[string]any{
			"model": "embed-image-v3.0", "input_type": "image", "embedding_types": []any{"float"},
			"images": []any{"data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG"))},
		}, seen.body)
		var out embedOut
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &out))
		require.Equal(t, "embed-image-v3.0", out.Model)
		require.Len(t, out.Embedding, 1536)
		require.Zero(t, out.SourceWidth, "Cohere reports no size")
		require.Contains(t, r.Body.String(), `"embedding":[0.25,1.5e-05,0,`)
		require.Eventually(t, func() bool { return v.usage(oth, "vision.embed:cohere") == 1 }, 5*time.Second, 20*time.Millisecond)
	})
	t.Run("failures map to 502s and meter nothing", func(t *testing.T) {
		for _, c := range []struct {
			name    string
			respond http.HandlerFunc
			want    string
		}{
			{"400", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }, "vision embed upstream rejected request"},
			{"401", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, "vision embed upstream rejected request"},
			{"429", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }, "vision embed upstream unavailable"},
			{"503", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, "vision embed upstream unavailable"},
			{"not json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) }, "vision embed upstream unavailable"},
			{"empty", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"embeddings":{"float":[[]]}}`)) }, "vision embed upstream unavailable"},
		} {
			answer = c.respond
			r := v.png(srch, "/v1/vision/embed")
			require.Equal(t, http.StatusBadGateway, r.Code, c.name)
			require.Equal(t, c.want, message(t, r), c.name)
		}
		require.Zero(t, v.usage(srch, "vision.embed:cohere"))
	})
}

func TestNetworkFailureIsUnavailable(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	v := setup(t, vision.Config{Provider: "cohere", CohereAPIKey: "k", CohereBaseURL: url, CohereModel: "m"}, false)
	r := v.png(srch, "/v1/vision/embed")
	require.Equal(t, http.StatusBadGateway, r.Code)
	require.Equal(t, "vision embed upstream unavailable", message(t, r))
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)
	v := setup(t, stubCfg, false)
	for _, path := range []string{"/v1/vision/embed", "/v1/vision/search"} {
		r := v.png(srch, path)
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("x"))
		req.Header.Set("Content-Type", "image/png")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, path)
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			},
			Status: r.Code,
			Header: http.Header{"Content-Type": {r.Header().Get("Content-Type")}},
		}
		out.SetBodyBytes(r.Body.Bytes())
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s: %s", path, r.Body.String())
	}
}
