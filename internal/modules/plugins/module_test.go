package plugins_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/imports"
	"lumina/internal/modules/plugins"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/skills"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type stubImporter struct {
	source string
	urls   []string
}

func (s *stubImporter) ImportLinks(_ context.Context, _ uuid.UUID, sourceType string, urls []string) (imports.StageResult, error) {
	s.source, s.urls = sourceType, urls
	return imports.StageResult{SessionID: uuid.New(), Total: len(urls), Staged: len(urls)}, nil
}

type fixedConnector struct{ urls []string }

func (fixedConnector) Binding() string { return "readwise" }
func (f fixedConnector) FetchURLs(context.Context, map[string]string, uuid.UUID) ([]string, error) {
	return f.urls, nil
}

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	srv      http.Handler
	importer *stubImporter
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	master := make([]byte, 32)
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(master))
	require.NoError(t, err)
	imp := &stubImporter{}
	module := plugins.New(pool, plugins.Options{
		Identify: auth.Identify(keys, auth.PGUsers(pool)), Limits: ratelimit.NewStore(), Box: box,
		Importer:   imp,
		Connectors: plugins.NewRegistry(fixedConnector{urls: []string{"https://a.example/1", "https://a.example/2"}}),
		Catalog:    skills.NewCatalog(vaultpaths.Paths{Root: t.TempDir()}),
		Containers: hermescontainers.New(pool, box),
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{module}})
	return env{t: t, pool: pool, srv: e, importer: imp}
}

func (v env) user(tier string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, username, tier, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, $4, now(), now())`, id, id.String()+"@t", "u"+strings.ReplaceAll(id.String(), "-", "")[:12], tier)
	require.NoError(v.t, err)
	return id
}

type resp struct {
	code int
	body string
}

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := scenario.Mint(keys, user)
	require.NoError(v.t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Body.String()}
}

func (v env) install(user uuid.UUID, body string) plugins.InstallDTO {
	v.t.Helper()
	r := v.do(user, http.MethodPost, "/v1/plugins/installs", body)
	require.Equal(v.t, http.StatusOK, r.code, r.body)
	var dto plugins.InstallDTO
	require.NoError(v.t, json.Unmarshal([]byte(r.body), &dto))
	return dto
}

func TestCatalogMergesSkills(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	r := v.do(u, http.MethodGet, "/v1/plugins/catalog", "")
	require.Equal(t, http.StatusOK, r.code)
	require.Contains(t, r.body, `"slug":"readwise"`)
	require.Contains(t, r.body, `"slug":"skill-daily-brief"`)
	r = v.do(u, http.MethodGet, "/v1/plugins/catalog?featured=true", "")
	require.NotContains(t, r.body, "skill-")
	r = v.do(u, http.MethodGet, "/v1/plugins/catalog?category=bogus", "")
	require.Contains(t, r.body, `"slug":"rss"`)
	r = v.do(u, http.MethodGet, "/v1/plugins/hermes-skills", "")
	require.Equal(t, `{"items":[]}`, strings.TrimSpace(r.body))
}

func TestConnectorLifecycle(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	r := v.do(u, http.MethodPost, "/v1/plugins/installs", `{"pluginSlug":"readwise","config":{}}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Contains(t, r.body, "missing_field:access_token")
	dto := v.install(u, `{"pluginSlug":"readwise","config":{"access_token":"t"}}`)
	require.True(t, dto.HasConfig)
	require.Equal(t, "enabled", dto.Status)
	again := v.install(u, `{"pluginSlug":"readwise","config":{"access_token":"u"}}`)
	require.Equal(t, dto.ID, again.ID, "a second install updates the row")

	id := dto.ID.String()
	r = v.do(u, http.MethodPatch, "/v1/plugins/installs/"+id, `{"status":"disabled"}`)
	require.Equal(t, http.StatusOK, r.code, r.body)
	require.Contains(t, r.body, `"status":"disabled"`)
	r = v.do(u, http.MethodPost, "/v1/plugins/installs/"+id+"/sync", "")
	require.Equal(t, http.StatusConflict, r.code)
	r = v.do(u, http.MethodPatch, "/v1/plugins/installs/"+id, `{"status":"bogus"}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	v.do(u, http.MethodPatch, "/v1/plugins/installs/"+id, `{"status":"enabled"}`)
	r = v.do(u, http.MethodPost, "/v1/plugins/installs/"+id+"/sync", "")
	require.Equal(t, http.StatusOK, r.code, r.body)
	require.Contains(t, r.body, `"status":"enriching"`)
	require.Equal(t, "connector:readwise", v.importer.source)
	require.Len(t, v.importer.urls, 2)
	r = v.do(u, http.MethodGet, "/v1/plugins/installs", "")
	require.Contains(t, r.body, "lastSyncAt")

	require.Equal(t, http.StatusBadRequest, v.do(u, http.MethodDelete, "/v1/plugins/installs/nope", "").code)
	require.Equal(t, http.StatusNotFound, v.do(v.user("trial"), http.MethodDelete, "/v1/plugins/installs/"+id, "").code)
	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodDelete, "/v1/plugins/installs/"+id, "").code)
}

func TestPremiumGate(t *testing.T) {
	v := setup(t)
	r := v.do(v.user("trial"), http.MethodPost, "/v1/plugins/installs", `{"pluginSlug":"byok-embeddings","config":{"access_token":"k"}}`)
	require.Equal(t, http.StatusPaymentRequired, r.code)
	require.Contains(t, r.body, "premium_required")
	v.install(v.user("pro"), `{"pluginSlug":"byok-embeddings","config":{"access_token":"k"}}`)
}

func TestNotAConnector(t *testing.T) {
	v := setup(t)
	u := v.user("pro")
	dto := v.install(u, `{"pluginSlug":"byok-embeddings","config":{"access_token":"k"}}`)
	r := v.do(u, http.MethodPost, "/v1/plugins/installs/"+dto.ID.String()+"/sync", "")
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Contains(t, r.body, "not_a_connector")
	rss := v.install(u, `{"pluginSlug":"rss","config":{"feed_url":"https://x.example/f"}}`)
	r = v.do(u, http.MethodPost, "/v1/plugins/installs/"+rss.ID.String()+"/sync", "")
	require.Equal(t, http.StatusServiceUnavailable, r.code, "rss has no connector in this registry")
}

func TestSkillInstallFlipsSkillsState(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	r := v.do(u, http.MethodPost, "/v1/plugins/installs", `{"pluginSlug":"skill-no-such-skill","config":{}}`)
	require.Equal(t, http.StatusNotFound, r.code)
	dto := v.install(u, `{"pluginSlug":"skill-daily-brief","config":{}}`)
	enabled := func() bool {
		var e bool
		require.NoError(t, v.pool.QueryRow(context.Background(),
			`SELECT enabled FROM skills_state WHERE tenant_id = $1 AND name = 'daily-brief'`, u).Scan(&e))
		return e
	}
	require.True(t, enabled())
	v.do(u, http.MethodPatch, "/v1/plugins/installs/"+dto.ID.String(), `{"status":"disabled"}`)
	require.False(t, enabled())
	require.Equal(t, http.StatusNoContent, v.do(u, http.MethodDelete, "/v1/plugins/installs/"+dto.ID.String(), "").code)
	require.False(t, enabled())
}

func TestHubInstallNeedsAContainer(t *testing.T) {
	v := setup(t)
	u := v.user("trial")
	r := v.do(u, http.MethodPost, "/v1/plugins/hermes-skills/install", "")
	require.Equal(t, http.StatusBadRequest, r.code)
	r = v.do(u, http.MethodPost, "/v1/plugins/hermes-skills/install?id=--x", "")
	require.Equal(t, http.StatusBadRequest, r.code)
	r = v.do(u, http.MethodPost, "/v1/plugins/hermes-skills/install?id=gif-search", "")
	require.Equal(t, http.StatusConflict, r.code)
	require.Contains(t, r.body, "hermes_container_not_found")
	r = v.do(u, http.MethodDelete, "/v1/plugins/hermes-skills/gif-search", "")
	require.Equal(t, http.StatusConflict, r.code)
}
