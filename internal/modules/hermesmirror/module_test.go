package hermesmirror_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/hermesdash"
	"lumina/internal/hermesmirror"
	"lumina/internal/httpserver"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	module "lumina/internal/modules/hermesmirror"
	"lumina/internal/modules/imports"
	skillsmod "lumina/internal/modules/skills"
	"lumina/internal/modules/spaces"
	"lumina/internal/modules/workflows"
	"lumina/internal/ratelimit"
	"lumina/internal/secretbox"
	"lumina/internal/skills"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// managedProvider is the managed transport on a temp Hermes home; sessions go
// to a gateway nothing listens on, as in the Swift test reader.
type managedProvider struct{ root string }

func (p managedProvider) Kind(context.Context, uuid.UUID) hermesdash.TransportKind {
	return hermesdash.KindManaged
}

func (p managedProvider) Transport(context.Context, uuid.UUID) (hermesdash.Transport, error) {
	dead, _ := url.Parse("http://127.0.0.1:9")
	return hermesdash.NewFilesystem(p.root, hermesdash.NewGateway(dead, nil, nil)), nil
}

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  http.Handler
	root string
	now  time.Time
}

func setup(t *testing.T) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	box, err := secretbox.New(base64.StdEncoding.EncodeToString(master))
	require.NoError(t, err)

	vault := t.TempDir()
	hermesRoot := t.TempDir()
	paths := vaultpaths.Paths{Root: vault}
	identify := auth.Identify(keys, auth.PGUsers(pool))
	importService := imports.NewService(pool, nil, spaces.New(pool, paths, identify), paths, nil, nil)
	indexer := chunkindex.NewIndexer(chunkindex.NewChunkRepository(pool), embedding.Deterministic{}, chunkindex.NewLinkRepository(pool))
	ingest := imports.NewIngest(pool, importService, memorystore.New(pool, nil), embedding.Deterministic{}, indexer, diskguard.New(vault, 0, 0, diskguard.Options{}))
	limits := hermesmirror.DefaultLimits()
	limits.ReadPause = 0
	service := &hermesmirror.Service{
		Pool: pool, Transports: managedProvider{root: hermesRoot}, Ingest: ingest, Limits: limits,
	}
	now := time.Now()
	m := module.New(pool, module.Options{
		Identify: identify, Limits: ratelimit.NewStore(), Box: box, Service: service,
		Now: func() time.Time { return now },
	})
	// GET /v1/skills merges mirrored skills and /run refuses them (the Swift
	// controller test drives both through one app).
	catalog := skillsmod.New(pool, skillsmod.Options{
		Identify: identify, Limits: ratelimit.NewStore(), Catalog: skills.NewCatalog(paths), Paths: paths, Mirror: true,
	})
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(keys, auth.PGUsers(pool)), Modules: []httpserver.Module{m, catalog}})
	return env{t: t, pool: pool, srv: e, root: hermesRoot, now: now}
}

func (v env) user() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	ctx := context.Background()
	_, err := v.pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, now(), now())`, id, id.String()+"@t", "u"+strings.ReplaceAll(id.String(), "-", "")[:12])
	require.NoError(v.t, err)
	_, err = v.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'Personal', now(), now())`, id)
	require.NoError(v.t, err)
	return id
}

type resp struct {
	code int
	body []byte
}

func (r resp) decode(t *testing.T, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(r.body, v), string(r.body))
}

func (v env) send(req *http.Request) resp {
	rec := httptest.NewRecorder()
	v.srv.ServeHTTP(rec, req)
	return resp{code: rec.Code, body: rec.Body.Bytes()}
}

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		token, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return v.send(req)
}

func (v env) seedSkill(name string) {
	dir := filepath.Join(v.root, "skills", name)
	require.NoError(v.t, os.MkdirAll(dir, 0o750))
	require.NoError(v.t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Mirror test skill\n---\n# "+name+"\n"), 0o600))
}

func (v env) seedJobOutput(job, stamp, markdown string) {
	dir := filepath.Join(v.root, "cron", "output", job)
	require.NoError(v.t, os.MkdirAll(dir, 0o750))
	require.NoError(v.t, os.WriteFile(filepath.Join(dir, stamp+".md"), []byte(markdown), 0o600))
}

// TestMirrorRoutes ports HermesMirrorControllerTests (its status-code mapping
// test is TestTransportErrorsMapToStableStatusCodes).
func TestMirrorRoutes(t *testing.T) {
	v := setup(t)

	t.Run("unauthenticated requests are rejected", func(t *testing.T) {
		assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, "/v1/hermes/mirror/status", "").code)
	})

	t.Run("status starts empty on the managed transport", func(t *testing.T) {
		r := v.do(v.user(), http.MethodGet, "/v1/hermes/mirror/status", "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var status map[string]any
		r.decode(t, &status)
		assert.Equal(t, "managed", status["transport"])
		assert.Equal(t, "never", status["lastStatus"])
		assert.EqualValues(t, 0, status["skillsCount"])
		assert.Equal(t, "absent", status["vaultState"])
		assert.NotContains(t, status, "dashboard")
	})

	t.Run("sync mirrors managed skills and toggle writes through", func(t *testing.T) {
		user := v.user()
		name := "mirror-" + uuid.NewString()[:6]
		v.seedSkill(name)
		r := v.do(user, http.MethodPost, "/v1/hermes/mirror/sync", `{"scope":["skills","jobs"]}`)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var status hermesmirror.StatusDTO
		r.decode(t, &status)
		assert.Equal(t, "ok", status.LastStatus)
		assert.GreaterOrEqual(t, status.SkillsCount, int64(1))
		assert.NotNil(t, status.LastSyncAt)

		r = v.do(user, http.MethodGet, "/v1/hermes/mirror/skills", "")
		require.Equal(t, http.StatusOK, r.code)
		var skills hermesmirror.SkillsResponse
		r.decode(t, &skills)
		var mine *hermesmirror.SkillDTO
		for i := range skills.Skills {
			if skills.Skills[i].Name == name {
				mine = &skills.Skills[i]
			}
		}
		require.NotNil(t, mine)
		assert.True(t, mine.Enabled)
		assert.Equal(t, "Mirror test skill", mine.Description)

		r = v.do(user, http.MethodPut, "/v1/hermes/mirror/skills/"+name, `{"enabled":false}`)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var toggled hermesmirror.SkillDTO
		r.decode(t, &toggled)
		assert.False(t, toggled.Enabled)
		config, err := os.ReadFile(filepath.Join(v.root, "config.yaml"))
		require.NoError(t, err)
		assert.Contains(t, string(config), name)
		assert.Equal(t, http.StatusOK, v.do(user, http.MethodPut, "/v1/hermes/mirror/skills/"+name, `{"enabled":true}`).code)

		// GET /v1/skills appends the mirrored skill as source `hermes`, and /run refuses it.
		r = v.do(user, http.MethodGet, "/v1/skills", "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var list struct {
			Skills []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Source      string `json:"source"`
				DailyRunCap int    `json:"dailyRunCap"`
			} `json:"skills"`
		}
		r.decode(t, &list)
		merged := false
		for _, s := range list.Skills {
			if s.ID == "hermes-"+name {
				merged = true
				assert.Equal(t, "hermes", s.Source)
				assert.Equal(t, name, s.Name)
				assert.Equal(t, 0, s.DailyRunCap)
			}
		}
		assert.True(t, merged, string(r.body))
		r = v.do(user, http.MethodPost, "/v1/skills/hermes-"+name+"/run", `{}`)
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Contains(t, string(r.body), "hermes_skill_runs_on_hermes")
		assert.Equal(t, http.StatusNotFound, v.do(user, http.MethodPut, "/v1/hermes/mirror/skills/not-a-skill", `{"enabled":true}`).code)
		assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodPut, "/v1/hermes/mirror/skills/"+name, `{}`).code)
	})

	t.Run("jobs, vault create, compile job install and import-sessions on the managed transport", func(t *testing.T) {
		user := v.user()
		r := v.do(user, http.MethodGet, "/v1/hermes/mirror/jobs", "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var jobs hermesmirror.JobsResponse
		r.decode(t, &jobs)
		assert.Equal(t, "live", jobs.Source)

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/vault/create", "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var created hermesmirror.VaultCreateResultDTO
		r.decode(t, &created)
		assert.Equal(t, filepath.Join(v.root, "kb"), created.VaultPath)
		info, err := os.Stat(filepath.Join(v.root, "kb", "raw"))
		require.NoError(t, err)
		assert.True(t, info.IsDir())
		_, err = os.Stat(filepath.Join(v.root, "kb", ".kb", "manifest.json"))
		require.NoError(t, err)

		r = v.do(user, http.MethodGet, "/v1/hermes/mirror/status", "")
		var status hermesmirror.StatusDTO
		r.decode(t, &status)
		assert.Contains(t, []string{"created", "detected"}, status.VaultState)
		require.NotNil(t, status.VaultPath)
		assert.True(t, strings.HasSuffix(*status.VaultPath, "/kb"))

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/install-compile", "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var install hermesmirror.CompileJobInstallResultDTO
		r.decode(t, &install)
		assert.Equal(t, hermesmirror.CompileJobSchedule, install.Schedule)
		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/install-compile", "")
		var again hermesmirror.CompileJobInstallResultDTO
		r.decode(t, &again)
		assert.False(t, again.Created)
		assert.Equal(t, install.JobID, again.JobID)

		r = v.do(user, http.MethodGet, "/v1/hermes/mirror/jobs", "")
		r.decode(t, &jobs)
		found := false
		for _, j := range jobs.Jobs {
			found = found || (j.HermesJobID == install.JobID && j.Name != nil && *j.Name == hermesmirror.CompileJobName)
		}
		assert.True(t, found, string(r.body))

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/vault/import", `{"vaultPath":"/tmp/../etc"}`)
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Contains(t, string(r.body), "hermes_mirror_invalid_path")

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/vault/import-sessions", "")
		assert.Equal(t, http.StatusBadGateway, r.code, string(r.body))
	})

	t.Run("collect pulls a job's output into the vault and is idempotent", func(t *testing.T) {
		user := v.user()
		r := v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs", `{"name":"collect-me","schedule":"0 9 * * *","prompt":"p"}`)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var job hermesmirror.JobDTO
		r.decode(t, &job)
		v.seedJobOutput(job.HermesJobID, "2026-09-02_03-00-00", "# Nightly compile\n\nWrote 3 articles.\n")

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/"+job.HermesJobID+"/collect", "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var result hermesmirror.CollectResultDTO
		r.decode(t, &result)
		assert.Equal(t, job.HermesJobID, result.HermesJobID)
		assert.Equal(t, 1, result.Fetched)
		assert.Equal(t, 1, result.Inserted)
		assert.Equal(t, 1, result.FilesWritten)
		assert.False(t, result.Truncated)
		assert.NotNil(t, result.HighWaterMark)

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/"+job.HermesJobID+"/collect", "")
		r.decode(t, &result)
		assert.Equal(t, 0, result.Inserted)
		assert.Equal(t, 1, result.Skipped)
		assert.Equal(t, 0, result.FilesWritten)

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/does-not-exist/collect", "")
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.Contains(t, string(r.body), "hermes_job_not_found")
		assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/..%2Fetc/collect", "").code)
	})

	t.Run("job control creates, updates, pauses, resumes, triggers and deletes a job", func(t *testing.T) {
		user := v.user()
		name := "control-" + uuid.NewString()[:6]
		r := v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs",
			`{"name":"`+name+`","schedule":"0 9 * * *","prompt":"Write the brief","deliver":"origin","skills":["kb-compile"]}`)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var job hermesmirror.JobDTO
		r.decode(t, &job)
		assert.Equal(t, name, *job.Name)
		assert.Equal(t, "0 9 * * *", *job.Schedule)
		assert.False(t, job.Paused)
		id := job.HermesJobID

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs", `{"name":"  ","schedule":"0 9 * * *"}`)
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Contains(t, string(r.body), "hermes_job_name_required")

		r = v.do(user, http.MethodPut, "/v1/hermes/mirror/jobs/"+id, `{"schedule":"0 6 * * *","prompt":"Write it earlier"}`)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		r.decode(t, &job)
		assert.Equal(t, "0 6 * * *", *job.Schedule)
		assert.Equal(t, "Write it earlier", *job.Prompt)

		r = v.do(user, http.MethodPut, "/v1/hermes/mirror/jobs/"+id, `{}`)
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Contains(t, string(r.body), "hermes_job_update_empty")

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/"+id+"/pause", "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		r.decode(t, &job)
		assert.True(t, job.Paused)
		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/"+id+"/resume", "")
		r.decode(t, &job)
		assert.False(t, job.Paused)
		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/"+id+"/trigger", "")
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		r.decode(t, &job)
		assert.NotNil(t, job.NextRunAt)

		r = v.do(user, http.MethodGet, "/v1/hermes/mirror/jobs/"+id+"/runs?limit=5", "")
		require.Equal(t, http.StatusOK, r.code)
		var runs hermesmirror.JobRunsResponse
		r.decode(t, &runs)
		assert.Equal(t, id, runs.HermesJobID)
		assert.Empty(t, runs.Runs)
		assert.Contains(t, string(r.body), `"runs":[]`)

		assert.Equal(t, http.StatusNoContent, v.do(user, http.MethodDelete, "/v1/hermes/mirror/jobs/"+id, "").code)
		r = v.do(user, http.MethodDelete, "/v1/hermes/mirror/jobs/"+id, "")
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.Contains(t, string(r.body), "hermes_job_not_found")
	})

	t.Run("job control refuses unknown ids and path traversal", func(t *testing.T) {
		user := v.user()
		patch := `{"schedule":"0 6 * * *"}`
		r := v.do(user, http.MethodPut, "/v1/hermes/mirror/jobs/does-not-exist", patch)
		assert.Equal(t, http.StatusNotFound, r.code)
		for _, action := range []string{"pause", "resume", "trigger"} {
			r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/does-not-exist/"+action, "")
			assert.Equal(t, http.StatusNotFound, r.code, action)
			assert.Contains(t, string(r.body), "hermes_job_not_found")
		}
		assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodPut, "/v1/hermes/mirror/jobs/..%2Fetc", patch).code)
		assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodDelete, "/v1/hermes/mirror/jobs/..%2Fetc", "").code)
		assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/..%2Fetc/pause", "").code)
		assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodGet, "/v1/hermes/mirror/jobs/..%2Fetc/runs", "").code)
	})

	t.Run("workspace is unsupported on the managed transport and needs its parameters", func(t *testing.T) {
		user := v.user()
		r := v.do(user, http.MethodGet, "/v1/hermes/workspace/status", "")
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Contains(t, string(r.body), "path required")
		r = v.do(user, http.MethodGet, "/v1/hermes/workspace/diff?path=/repo", "")
		assert.Equal(t, http.StatusBadRequest, r.code)
		assert.Contains(t, string(r.body), "file required")
		r = v.do(user, http.MethodGet, "/v1/hermes/workspace/files?path=%20", "")
		assert.Equal(t, http.StatusBadRequest, r.code)
		r = v.do(user, http.MethodGet, "/v1/hermes/workspace/file?path=/repo/a.md", "")
		assert.Equal(t, http.StatusNotImplemented, r.code)
		assert.Contains(t, string(r.body), "hermes_mirror_unsupported")
	})
}

func TestTransportErrorsMapToStableStatusCodes(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, module.StatusFor(hermesdash.InvalidPath("x")))
	assert.Equal(t, http.StatusBadRequest, module.StatusFor(hermesdash.BodyTooLarge("x", 1)))
	assert.Equal(t, http.StatusNotFound, module.StatusFor(hermesdash.NotFound("x")))
	assert.Equal(t, http.StatusNotImplemented, module.StatusFor(hermesdash.Unsupported("sessions")))
	assert.Equal(t, http.StatusBadGateway, module.StatusFor(hermesdash.ErrAuthModeUnsupported))
	assert.Equal(t, http.StatusBadGateway, module.StatusFor(hermesdash.DashboardUnreachable("x")))
	assert.Equal(t, http.StatusConflict, module.StatusFor(hermesdash.ErrNotConfiguredErr))
	assert.Equal(t, "hermes_dashboard_auth_mode_unsupported", hermesdash.ErrAuthModeUnsupported.Code())
}

// MARK: - Webhook (HermesMirrorWebhookControllerTests)

func (v env) rotate(user uuid.UUID) module.WebhookDTO {
	v.t.Helper()
	r := v.do(user, http.MethodPost, "/v1/hermes/mirror/webhook", "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var hook module.WebhookDTO
	r.decode(v.t, &hook)
	return hook
}

func (v env) push(hook module.WebhookDTO, body string, ts time.Time, secret string, path string) resp {
	v.t.Helper()
	if path == "" {
		path = hook.Path
	}
	stamp := strconv.FormatInt(ts.Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(module.TimestampHeader, stamp)
	req.Header.Set(module.SignatureHeader, workflows.SignWebhook(secret, stamp, []byte(body)))
	return v.send(req)
}

func TestWebhook(t *testing.T) {
	v := setup(t)

	t.Run("rotate mints a one-time secret and the read-back never returns it", func(t *testing.T) {
		user := v.user()
		r := v.do(user, http.MethodGet, "/v1/hermes/mirror/webhook", "")
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.Contains(t, string(r.body), "hermes_webhook_not_configured")
		hook := v.rotate(user)
		require.NotNil(t, hook.Secret)
		assert.NotEmpty(t, *hook.Secret)
		assert.Equal(t, "/v1/hermes/mirror/webhook/"+hook.Token, hook.Path)
		assert.Equal(t, "X-Webhook-Signature-V2", hook.SignatureHeader)
		assert.Equal(t, 300, hook.ReplayWindowSeconds)
		assert.NotNil(t, hook.RotatedAt)

		r = v.do(user, http.MethodGet, "/v1/hermes/mirror/webhook", "")
		require.Equal(t, http.StatusOK, r.code)
		var read module.WebhookDTO
		r.decode(t, &read)
		assert.Equal(t, hook.Token, read.Token)
		assert.Nil(t, read.Secret)
		assert.NotContains(t, string(r.body), "secret\"")

		rotated := v.rotate(user)
		assert.NotEqual(t, hook.Token, rotated.Token)
		assert.NotEqual(t, *hook.Secret, *rotated.Secret)
		assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPost, "/v1/hermes/mirror/webhook", "").code)
	})

	t.Run("a signed push collects the job and agrees with the polling collect", func(t *testing.T) {
		user := v.user()
		hook := v.rotate(user)
		r := v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs", `{"name":"pushed","schedule":"0 9 * * *","prompt":"p"}`)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		var job hermesmirror.JobDTO
		r.decode(t, &job)
		v.seedJobOutput(job.HermesJobID, "2026-09-02_03-00-00", "# Pushed\n")

		body := fmt.Sprintf(`{"job_id":%q,"status":"ok"}`, job.HermesJobID)
		pushed := v.push(hook, body, v.now, *hook.Secret, "")
		require.Equal(t, http.StatusOK, pushed.code, string(pushed.body))
		var result hermesmirror.CollectResultDTO
		pushed.decode(t, &result)
		assert.Equal(t, job.HermesJobID, result.HermesJobID)
		assert.Equal(t, 1, result.Inserted)
		assert.Equal(t, 1, result.FilesWritten)

		r = v.do(user, http.MethodPost, "/v1/hermes/mirror/jobs/"+job.HermesJobID+"/collect", "")
		require.Equal(t, http.StatusOK, r.code)
		r.decode(t, &result)
		assert.Equal(t, 0, result.Inserted)
		assert.Equal(t, 1, result.Skipped)

		again := v.push(hook, body, v.now, *hook.Secret, "")
		require.Equal(t, http.StatusOK, again.code)
		again.decode(t, &result)
		assert.Equal(t, 0, result.Inserted)

		r = v.do(user, http.MethodGet, "/v1/hermes/mirror/jobs/"+job.HermesJobID+"/runs", "")
		var runs hermesmirror.JobRunsResponse
		r.decode(t, &runs)
		require.Len(t, runs.Runs, 1)
		require.NotNil(t, runs.Runs[0].VaultFilePath)
		assert.Contains(t, *runs.Runs[0].VaultFilePath, "raw/jobs/")
	})

	t.Run("unsigned, missigned, replayed and unknown pushes are all the same 401", func(t *testing.T) {
		user := v.user()
		hook := v.rotate(user)
		body := `{"job_id":"j1"}`
		forged := v.push(hook, body, v.now, "not-the-secret", "")
		assert.Equal(t, http.StatusUnauthorized, forged.code)
		assert.Contains(t, string(forged.body), "hermes_webhook_unauthorized")
		replayed := v.push(hook, body, v.now.Add(-10*time.Minute), *hook.Secret, "")
		assert.Equal(t, http.StatusUnauthorized, replayed.code)
		req := httptest.NewRequest(http.MethodPost, hook.Path, strings.NewReader(body))
		assert.Equal(t, http.StatusUnauthorized, v.send(req).code)
		unknown := v.push(hook, body, v.now, *hook.Secret, module.WebhookRoutePrefix+"/"+strings.Repeat("0", 64))
		assert.Equal(t, http.StatusUnauthorized, unknown.code)
		assert.Contains(t, string(unknown.body), "hermes_webhook_unauthorized")
	})

	t.Run("a signed push with a traversing job id never reaches Hermes", func(t *testing.T) {
		user := v.user()
		hook := v.rotate(user)
		traversal := v.push(hook, `{"job_id":"../etc"}`, v.now, *hook.Secret, "")
		assert.Equal(t, http.StatusBadRequest, traversal.code)
		assert.Contains(t, string(traversal.body), "hermes_mirror_invalid_path")
		unknownJob := v.push(hook, `{"job_id":"never-mirrored"}`, v.now, *hook.Secret, "")
		require.Equal(t, http.StatusOK, unknownJob.code)
		var result hermesmirror.CollectResultDTO
		unknownJob.decode(t, &result)
		assert.Equal(t, 0, result.Inserted)
		assert.Equal(t, 0, result.Fetched)
		bad := v.push(hook, `{"nope":1}`, v.now, *hook.Secret, "")
		assert.Equal(t, http.StatusBadRequest, bad.code)
		assert.Contains(t, string(bad.body), "hermes_webhook_invalid_payload")
	})
}

func TestTheAdminConfigRWSignalIsWhatGatesTheCredential(t *testing.T) {
	yes, no := true, false
	assert.Equal(t, &yes, hermesdash.AdminConfigRW(map[string]any{"admin_config_rw": true}))
	assert.Equal(t, &yes, hermesdash.AdminConfigRW(map[string]any{"capabilities": []any{"cron", "admin_config_rw"}}))
	assert.Equal(t, &no, hermesdash.AdminConfigRW(map[string]any{"capabilities": []any{"cron"}}))
	assert.Equal(t, &no, hermesdash.AdminConfigRW(map[string]any{"capabilities": map[string]any{"admin_config_rw": false}}))
	assert.Nil(t, hermesdash.AdminConfigRW(map[string]any{"version": "0.20.0"}))
}

// MARK: - Artifacts (HermesArtifactsFilterTests)

func (v env) artifact(tenant uuid.UUID, kind, label, session string, occurred time.Time) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO hermes_artifacts (id, tenant_id, kind, value, href, label, session_id, session_title, occurred_at, content_hash)
		VALUES ($1, $2, $3, $4, $4, $5, $6, 'Session', $7, $8)`,
		uuid.New(), tenant, kind, "https://x.test/"+label, label, session, occurred, uuid.NewString())
	require.NoError(v.t, err)
}

func (v env) list(user uuid.UUID, query string) hermesmirror.ArtifactListResponse {
	v.t.Helper()
	r := v.do(user, http.MethodGet, "/v1/hermes/artifacts"+query, "")
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
	var out hermesmirror.ArtifactListResponse
	r.decode(v.t, &out)
	return out
}

func labels(out hermesmirror.ArtifactListResponse) []string {
	var l []string
	for _, a := range out.Artifacts {
		l = append(l, a.Label)
	}
	return l
}

func TestArtifacts(t *testing.T) {
	v := setup(t)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	t.Run("filtering by session returns only that session's artifacts", func(t *testing.T) {
		user := v.user()
		v.artifact(user, "link", "one", "s1", base)
		v.artifact(user, "link", "two", "s1", base.Add(time.Minute))
		v.artifact(user, "link", "other", "s2", base)
		assert.ElementsMatch(t, []string{"one", "two"}, labels(v.list(user, "?sessionID=s1")))
	})

	t.Run("omitting the session filter returns every artifact", func(t *testing.T) {
		user := v.user()
		v.artifact(user, "link", "a", "s1", base)
		v.artifact(user, "link", "b", "s2", base)
		assert.Len(t, v.list(user, "").Artifacts, 2)
	})

	t.Run("an empty session id is treated as no filter", func(t *testing.T) {
		user := v.user()
		v.artifact(user, "link", "a", "s1", base)
		v.artifact(user, "link", "b", "s2", base)
		assert.Len(t, v.list(user, "?sessionID=").Artifacts, 2)
	})

	t.Run("an unknown session returns empty rather than everything", func(t *testing.T) {
		user := v.user()
		v.artifact(user, "link", "a", "s1", base)
		out := v.list(user, "?sessionID=nope")
		assert.Empty(t, out.Artifacts)
		assert.NotNil(t, out.Artifacts)
	})

	t.Run("session and kind filters compose", func(t *testing.T) {
		user := v.user()
		v.artifact(user, "link", "link-one", "s1", base)
		v.artifact(user, "image", "image-one", "s1", base)
		v.artifact(user, "link", "link-two", "s2", base)
		assert.Equal(t, []string{"link-one"}, labels(v.list(user, "?sessionID=s1&kind=link")))
	})

	t.Run("search escapes wildcards, pages with a cursor and get reads one", func(t *testing.T) {
		user := v.user()
		v.artifact(user, "file", "100%_done", "s1", base)
		v.artifact(user, "file", "1000 done", "s1", base.Add(time.Minute))
		v.artifact(user, "file", "third", "s1", base.Add(2*time.Minute))
		assert.Equal(t, []string{"100%_done"}, labels(v.list(user, "?q=%25_")))
		page := v.list(user, "?limit=2")
		assert.Equal(t, []string{"third", "1000 done"}, labels(page))
		require.NotNil(t, page.NextCursor)
		assert.Equal(t, "2026-09-01T12:01:00.000Z", *page.NextCursor)
		rest := v.list(user, "?limit=2&before="+url.QueryEscape(*page.NextCursor))
		assert.Equal(t, []string{"100%_done"}, labels(rest))
		assert.Nil(t, rest.NextCursor)

		r := v.do(user, http.MethodGet, "/v1/hermes/artifacts/"+page.Artifacts[0].ID.String(), "")
		require.Equal(t, http.StatusOK, r.code)
		assert.Equal(t, http.StatusBadRequest, v.do(user, http.MethodGet, "/v1/hermes/artifacts/not-a-uuid", "").code)
		r = v.do(user, http.MethodGet, "/v1/hermes/artifacts/"+uuid.NewString(), "")
		assert.Equal(t, http.StatusNotFound, r.code)
		assert.Contains(t, string(r.body), "hermes_artifact_not_found")
	})
}
