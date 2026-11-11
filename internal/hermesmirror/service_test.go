package hermesmirror

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/chunkindex"
	"lumina/internal/dbtest"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/hermesdash"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/imports"
	"lumina/internal/modules/spaces"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testClock = time.Unix(1_756_800_000, 0).UTC()

type recordingCompiler struct {
	mu    sync.Mutex
	calls [][]uuid.UUID
}

func (r *recordingCompiler) Compile(_ context.Context, _ uuid.UUID, ids []uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, ids)
	return nil
}

func (r *recordingCompiler) recorded() [][]uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]uuid.UUID(nil), r.calls...)
}

type harness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	tenant    uuid.UUID
	transport *fakeTransport
	service   *Service
	compile   *recordingCompiler
}

var (
	sharedPoolOnce sync.Once
	sharedPool     *pgxpool.Pool
)

// pool is one Postgres for every service test in the package.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test (needs Docker); skipped with -short")
	}
	sharedPoolOnce.Do(func() {
		sharedPool = dbtest.Postgres(t)
		require.NoError(t, migrations.Up(context.Background(), sharedPool, migrations.SQL()))
	})
	if sharedPool == nil {
		t.Fatal("no shared pool")
	}
	return sharedPool
}

func noLimitsPause() Limits {
	l := DefaultLimits()
	l.ReadPause = 0
	return l
}

func newHarness(t *testing.T, pool *pgxpool.Pool, kind hermesdash.TransportKind, limits Limits) harness {
	t.Helper()
	ctx := context.Background()
	tenant := uuid.New()
	slug := "u" + tenant.String()[:8]
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'trial', now(), now())`, tenant, slug+"@test.luminavault", slug)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'Personal', now(), now())`, tenant)
	require.NoError(t, err)

	root := t.TempDir()
	paths := vaultpaths.Paths{Root: root}
	repo := memorystore.New(pool, nil)
	service := imports.NewService(pool, nil, spaces.New(pool, paths, auth.Identify(auth.Keyset{}, auth.PGUsers(pool))), paths, nil, nil)
	indexer := chunkindex.NewIndexer(chunkindex.NewChunkRepository(pool), embedding.Deterministic{}, chunkindex.NewLinkRepository(pool))
	ingest := imports.NewIngest(pool, service, repo, embedding.Deterministic{}, indexer, diskguard.New(root, 0, 0, diskguard.Options{}))

	skills := fstest.MapFS{}
	for _, name := range []string{"kb-compile", "kb-ingest"} {
		skills[name+"/SKILL.md"] = &fstest.MapFile{Data: []byte("---\nname: " + name + "\ndescription: " + name + " skill\n---\n")}
	}
	transport := newFake(kind)
	compile := &recordingCompiler{}
	return harness{
		t: t, pool: pool, tenant: tenant, transport: transport, compile: compile,
		service: &Service{
			Pool: pool, Transports: fixedProvider{transport}, Ingest: ingest, Compile: compile,
			BundledSkills: skills, Limits: limits, Now: func() time.Time { return testClock },
		},
	}
}

func skill(name string, enabled bool, description string) hermesdash.Skill {
	return hermesdash.Skill{Name: name, Description: description, Enabled: enabled, Source: "custom"}
}

func job(id, name string, paused bool) hermesdash.Job {
	schedule, prompt := "0 9 * * *", "p"
	raw, _ := json.Marshal(map[string]string{"id": id})
	return hermesdash.Job{ID: id, Name: &name, Schedule: &schedule, Prompt: &prompt, Paused: paused, Raw: raw}
}

func at(seconds float64) time.Time {
	return time.Unix(0, int64(seconds*1e9)).UTC()
}

func run(key string, started float64, status string) hermesdash.JobRun {
	r := hermesdash.JobRun{Key: key, Status: status, StartedAt: at(started)}
	if status != hermesdash.RunRunning {
		f := at(started + 60)
		r.FinishedAt = &f
	}
	return r
}

func wireStatus(t *testing.T, err error) (int, string) {
	t.Helper()
	var we *wire.Error
	require.True(t, errors.As(err, &we), "want a wire.Error, got %v", err)
	return we.Status, we.Message
}

func scopes(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func vaultFiles(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) (paths []string, provenances []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT path, coalesce(metadata->>'provenance', '') FROM vault_files WHERE tenant_id = $1 ORDER BY path`, tenant)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var p, prov string
		require.NoError(t, rows.Scan(&p, &prov))
		paths = append(paths, p)
		provenances = append(provenances, prov)
	}
	return paths, provenances
}

func TestService(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	t.Run("sync mirrors skills and jobs, updates changed rows and tombstones removed ones", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setSkills(skill("a", true, "d"), skill("b", false, "d"))
		h.transport.setJobs(job("j1", "One", false), job("j2", "Two", false))
		status, err := h.service.Sync(ctx, h.tenant, scopes(ScopeSkills, ScopeJobs))
		require.NoError(t, err)
		assert.Equal(t, "ok", status.LastStatus)
		assert.EqualValues(t, 2, status.SkillsCount)
		assert.EqualValues(t, 2, status.JobsCount)
		assert.Equal(t, "remote", status.Transport)
		skills, err := h.service.MirroredSkills(ctx, h.tenant)
		require.NoError(t, err)
		require.Len(t, skills, 2)
		assert.Equal(t, "a", skills[0].Name)
		assert.False(t, skills[1].Enabled)

		h.transport.setSkills(skill("b", true, "changed"), skill("c", true, "d"))
		h.transport.setJobs(job("j2", "Two renamed", true))
		status, err = h.service.Sync(ctx, h.tenant, scopes(ScopeSkills, ScopeJobs))
		require.NoError(t, err)
		skills, err = h.service.MirroredSkills(ctx, h.tenant)
		require.NoError(t, err)
		require.Len(t, skills, 2)
		assert.Equal(t, []string{"b", "c"}, []string{skills[0].Name, skills[1].Name})
		assert.True(t, skills[0].Enabled)
		assert.Equal(t, "changed", skills[0].Description)
		rows, err := h.service.jobRows(ctx, h.tenant, ``)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, "j2", rows[0].HermesJobID)
		assert.Equal(t, "Two renamed", *rows[0].Name)
		assert.True(t, rows[0].Paused)
		assert.EqualValues(t, 1, status.JobsCount)
	})

	t.Run("sync records a partial failure when one scope fails and keeps the other", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setSkills(skill("a", true, "d"))
		h.transport.fail("listJobs", hermesdash.ErrAuthModeUnsupported)
		status, err := h.service.Sync(ctx, h.tenant, scopes(ScopeSkills, ScopeJobs))
		require.NoError(t, err)
		assert.Equal(t, "partial", status.LastStatus)
		assert.EqualValues(t, 1, status.SkillsCount)
		require.NotNil(t, status.LastError)
		assert.Contains(t, *status.LastError, "hermes_dashboard_auth_mode_unsupported")

		h.transport.fail("listSkills", hermesdash.DashboardUnreachable("x"))
		failed, err := h.service.Sync(ctx, h.tenant, scopes(ScopeSkills, ScopeJobs))
		require.NoError(t, err)
		assert.Equal(t, "failed", failed.LastStatus)
	})

	t.Run("toggleSkill reaches Hermes and persists the flag", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setSkills(skill("a", true, "d"))
		_, err := h.service.Sync(ctx, h.tenant, scopes(ScopeSkills))
		require.NoError(t, err)
		toggled, err := h.service.ToggleSkill(ctx, h.tenant, "a", false)
		require.NoError(t, err)
		assert.False(t, toggled.Enabled)
		assert.Contains(t, h.transport.recorded(), "toggleSkill:a:false")
		skills, err := h.service.MirroredSkills(ctx, h.tenant)
		require.NoError(t, err)
		assert.False(t, skills[0].Enabled)
		_, err = h.service.ToggleSkill(ctx, h.tenant, "missing", true)
		status, msg := wireStatus(t, err)
		assert.Equal(t, 404, status)
		assert.Equal(t, "hermes_skill_not_found", msg)
	})

	t.Run("jobs are live when Hermes answers and a snapshot otherwise", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setJobs(job("j1", "One", false))
		live, err := h.service.Jobs(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, "live", live.Source)
		require.Len(t, live.Jobs, 1)
		assert.Equal(t, "j1", live.Jobs[0].HermesJobID)
		assert.Nil(t, live.Jobs[0].UpdatedAt)

		h.transport.fail("listJobs", hermesdash.DashboardUnreachable("down"))
		snapshot, err := h.service.Jobs(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, "snapshot", snapshot.Source)
		require.Len(t, snapshot.Jobs, 1)
		assert.Equal(t, "j1", snapshot.Jobs[0].HermesJobID)
		assert.NotNil(t, snapshot.Jobs[0].UpdatedAt)
	})

	t.Run("detectVault finds the kb root by manifest, obsidian-vault child, or kb-config pointer", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.addFile("/home/hermes/kb/.kb/manifest.json", "{}")
		got, err := h.service.detectVault(ctx, h.transport, nil)
		require.NoError(t, err)
		assert.Equal(t, "/home/hermes/kb", *got)

		h2 := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h2.transport.addFile("/home/hermes/obsidian-vault/Fernando/raw/a.md", "# a")
		h2.transport.addDirectory("/home/hermes/obsidian-vault/Fernando/wiki")
		got, err = h2.service.detectVault(ctx, h2.transport, nil)
		require.NoError(t, err)
		assert.Equal(t, "/home/hermes/obsidian-vault/Fernando", *got)

		h3 := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h3.transport.addFile("/home/hermes/kb-config.json", `{"kb_path":"/srv/notes"}`)
		h3.transport.addFile("/srv/notes/.kb/manifest.json", "{}")
		got, err = h3.service.detectVault(ctx, h3.transport, nil)
		require.NoError(t, err)
		assert.Equal(t, "/srv/notes", *got)

		h4 := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		gone := "/gone"
		got, err = h4.service.detectVault(ctx, h4.transport, &gone)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("detectVault accepts a raw-only vault that has never been compiled", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.addFile("/home/hermes/vault/raw/note.md", "# note")
		got, err := h.service.detectVault(ctx, h.transport, nil)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "/home/hermes/vault", *got)
	})

	t.Run("detectVault still rejects a directory with no raw and no manifest", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.addFile("/home/hermes/vault/notes/note.md", "# note")
		got, err := h.service.detectVault(ctx, h.transport, nil)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("importVault ingests markdown, skips hidden dirs, resumes from the cursor and reports counts", func(t *testing.T) {
		limits := noLimitsPause()
		limits.VaultFilesPerRun = 2
		h := newHarness(t, pool, hermesdash.KindRemote, limits)
		h.transport.addFile("/home/hermes/kb/.kb/manifest.json", "{}")
		h.transport.addFile("/home/hermes/kb/raw/a.md", "# A\nalpha")
		h.transport.addFile("/home/hermes/kb/raw/b.md", "# B\nbeta")
		h.transport.addFile("/home/hermes/kb/wiki/c.md", "# C\ngamma")
		h.transport.addFile("/home/hermes/kb/raw/image.png", "binary")
		h.transport.addFile("/home/hermes/kb/.obsidian/workspace.md", "ignored")
		h.transport.addFile("/home/hermes/kb/.trash/old.md", "ignored")

		first, err := h.service.ImportVault(ctx, h.tenant, nil)
		require.NoError(t, err)
		assert.Equal(t, "/home/hermes/kb", first.VaultPath)
		assert.Equal(t, 2, first.Imported)
		assert.True(t, first.Truncated)
		assert.NotNil(t, first.Cursor)

		second, err := h.service.ImportVault(ctx, h.tenant, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, second.Imported)
		assert.False(t, second.Truncated)
		assert.Nil(t, second.Cursor)
		assert.Equal(t, 3, second.Scanned)

		status, err := h.service.Status(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, "imported", status.VaultState)
		assert.EqualValues(t, 3, status.VaultFilesCount)
		paths, provs := vaultFiles(t, pool, h.tenant)
		assert.Len(t, paths, 3)
		for i, p := range paths {
			assert.Equal(t, VaultProvenance, provs[i])
			assert.NotContains(t, p, "obsidian")
			assert.NotContains(t, p, "trash")
		}

		third, err := h.service.ImportVault(ctx, h.tenant, nil)
		require.NoError(t, err)
		assert.Equal(t, 0, third.Imported)
		assert.Equal(t, 2, third.Skipped)
		assert.True(t, third.Truncated)
	})

	t.Run("importVault rejects traversal and unknown paths", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		traversal := "/home/../etc"
		_, err := h.service.ImportVault(ctx, h.tenant, &traversal)
		var te *hermesdash.Error
		require.True(t, errors.As(err, &te))
		assert.Equal(t, hermesdash.ErrInvalidPath, te.Kind)
		nowhere := "/nowhere"
		_, err = h.service.ImportVault(ctx, h.tenant, &nowhere)
		status, msg := wireStatus(t, err)
		assert.Equal(t, 400, status)
		assert.Equal(t, "hermes_vault_path_not_found", msg)
		_, err = h.service.ImportVault(ctx, h.tenant, nil)
		status, msg = wireStatus(t, err)
		assert.Equal(t, 404, status)
		assert.Equal(t, "hermes_vault_not_found", msg)
	})

	t.Run("importVault skips files over the size cap", func(t *testing.T) {
		limits := noLimitsPause()
		limits.MaxFileBytes = 10
		h := newHarness(t, pool, hermesdash.KindRemote, limits)
		h.transport.addFile("/home/hermes/kb/.kb/manifest.json", "{}")
		h.transport.addFile("/home/hermes/kb/raw/small.md", "# s")
		h.transport.addFile("/home/hermes/kb/raw/big.md", strings.Repeat("x", 64))
		requested := "/home/hermes/kb"
		result, err := h.service.ImportVault(ctx, h.tenant, &requested)
		require.NoError(t, err)
		assert.Equal(t, 1, result.Imported)
		assert.Equal(t, 1, result.Skipped)
	})

	t.Run("createVault builds the skeleton, installs missing kb skills, and is idempotent", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setSkills(skill("kb-ingest", true, "d"))
		created, err := h.service.CreateVault(ctx, h.tenant)
		require.NoError(t, err)
		assert.False(t, created.AlreadyExisted)
		assert.Equal(t, "/home/hermes/kb", created.VaultPath)
		assert.Equal(t, []string{"/home/hermes/kb", "/home/hermes/kb/raw", "/home/hermes/kb/wiki", "/home/hermes/kb/.kb"}, created.CreatedDirectories)
		assert.Equal(t, []string{"kb-compile"}, created.InstalledSkills)
		files := h.transport.fileContents()
		assert.Contains(t, files["/home/hermes/kb/.kb/manifest.json"], `"created_by" : "luminavault"`)
		assert.Contains(t, files["/home/hermes/kb/README.md"], "LuminaVault knowledge base")
		status, err := h.service.Status(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, "created", status.VaultState)
		assert.Equal(t, "/home/hermes/kb", *status.VaultPath)

		again, err := h.service.CreateVault(ctx, h.tenant)
		require.NoError(t, err)
		assert.True(t, again.AlreadyExisted)
		assert.Empty(t, again.CreatedDirectories)
		assert.Empty(t, again.InstalledSkills)
	})

	t.Run("importSessions writes transcripts, skips empty sessions, triggers compile and honours the high-water mark", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		base := 1_756_800_000.0
		ts := func(d float64) *time.Time { v := at(base + d); return &v }
		str := func(s string) *string { return &s }
		h.transport.setSessions(
			hermesdash.Session{ID: "s-new", Title: str("Planning"), Source: str("cli"), StartedAt: ts(7200), LastActiveAt: ts(7300), MessageCount: 2},
			hermesdash.Session{ID: "s-empty", Source: str("cli"), StartedAt: ts(0), LastActiveAt: ts(0), MessageCount: 0},
			hermesdash.Session{ID: "s-old", Source: str("telegram"), StartedAt: ts(-86400), LastActiveAt: ts(-86000), MessageCount: 1},
		)
		h.transport.setMessages("s-new",
			hermesdash.Message{Role: "user", Content: "What is the plan?", Timestamp: ts(0)},
			hermesdash.Message{Role: "assistant", Content: "Ship the mirror.", Timestamp: ts(1)})
		h.transport.setMessages("s-old", hermesdash.Message{Role: "user", Content: "old note"})

		first, err := h.service.ImportSessions(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, 2, first.SessionsImported)
		assert.Equal(t, 1, first.SessionsSkipped)
		assert.Equal(t, 2, first.FilesWritten)
		assert.False(t, first.Truncated)
		assert.True(t, first.CompileTriggered)
		require.Len(t, h.compile.recorded(), 1)
		assert.Len(t, h.compile.recorded()[0], 2)

		paths, provs := vaultFiles(t, pool, h.tenant)
		require.Len(t, paths, 2)
		found := false
		for i, p := range paths {
			assert.Equal(t, SessionProvenance, provs[i])
			found = found || strings.Contains(p, "sessions/2025-09-02-s-new")
		}
		assert.True(t, found, paths)
		status, err := h.service.Status(ctx, h.tenant)
		require.NoError(t, err)
		assert.EqualValues(t, 2, status.SessionsImported)

		second, err := h.service.ImportSessions(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, 0, second.SessionsImported)
		assert.Equal(t, 0, second.FilesWritten)
		assert.False(t, second.CompileTriggered)
		assert.Len(t, h.compile.recorded(), 1)

		h.transport.setSessions(
			hermesdash.Session{ID: "s-newer", Title: str("Later"), Source: str("cli"), StartedAt: ts(9000), LastActiveAt: ts(9100), MessageCount: 1},
			hermesdash.Session{ID: "s-new", Title: str("Planning"), Source: str("cli"), StartedAt: ts(7200), LastActiveAt: ts(7300), MessageCount: 2},
		)
		h.transport.setMessages("s-newer", hermesdash.Message{Role: "user", Content: "again"})
		third, err := h.service.ImportSessions(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, 1, third.SessionsImported)
		paths, _ = vaultFiles(t, pool, h.tenant)
		assert.Len(t, paths, 3)
	})

	t.Run("importSessions caps a run and resumes from the offset cursor", func(t *testing.T) {
		limits := noLimitsPause()
		limits.SessionsPerRun = 2
		limits.SessionPageSize = 2
		h := newHarness(t, pool, hermesdash.KindRemote, limits)
		var sessions []hermesdash.Session
		for i := 0; i < 5; i++ {
			id := "s" + string(rune('0'+i))
			title := id
			started := at(1_756_800_000)
			last := at(1_756_800_000 + float64(100-i))
			source := "cli"
			sessions = append(sessions, hermesdash.Session{ID: id, Title: &title, Source: &source, StartedAt: &started, LastActiveAt: &last, MessageCount: 1})
			h.transport.setMessages(id, hermesdash.Message{Role: "user", Content: "m" + id})
		}
		h.transport.setSessions(sessions...)
		first, err := h.service.ImportSessions(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, 2, first.SessionsImported)
		assert.True(t, first.Truncated)
		require.NotNil(t, first.Cursor)
		assert.Contains(t, *first.Cursor, `"offset":2`)
		second, err := h.service.ImportSessions(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, 2, second.SessionsImported)
		assert.True(t, second.Truncated)
		third, err := h.service.ImportSessions(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, 1, third.SessionsImported)
		assert.False(t, third.Truncated)
		paths, _ := vaultFiles(t, pool, h.tenant)
		assert.Len(t, paths, 5)
	})

	t.Run("installCompileJob creates the nightly job once and remembers its id", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.addFile("/home/hermes/kb/.kb/manifest.json", "{}")
		created, err := h.service.InstallCompileJob(ctx, h.tenant)
		require.NoError(t, err)
		assert.True(t, created.Created)
		assert.Equal(t, CompileJobSchedule, created.Schedule)
		jobs := h.transport.jobList()
		require.Len(t, jobs, 1)
		assert.Equal(t, CompileJobName, *jobs[0].Name)
		assert.Contains(t, *jobs[0].Prompt, "/home/hermes/kb")
		status, err := h.service.Status(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, created.JobID, *status.CompileJobID)

		again, err := h.service.InstallCompileJob(ctx, h.tenant)
		require.NoError(t, err)
		assert.False(t, again.Created)
		assert.Equal(t, created.JobID, again.JobID)
		assert.Len(t, h.transport.jobList(), 1)
	})

	t.Run("status for a tenant that never synced is empty", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindManaged, noLimitsPause())
		status, err := h.service.Status(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, "never", status.LastStatus)
		assert.Equal(t, "managed", status.Transport)
		assert.Zero(t, status.SkillsCount)
		assert.Equal(t, "absent", status.VaultState)
		assert.Zero(t, status.SessionsImported)
	})

	t.Run("collect stores finished runs, files the output and logs a hermes skill run", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setJobs(job("j1", "Daily Digest", false))
		_, err := h.service.Sync(ctx, h.tenant, scopes(ScopeJobs))
		require.NoError(t, err)
		var rawAfterSync string
		require.NoError(t, pool.QueryRow(ctx, `SELECT raw::text FROM hermes_mirrored_jobs WHERE tenant_id = $1`, h.tenant).Scan(&rawAfterSync))
		in, out := 12, 34
		r := run("cron_j1_2", 1_756_800_000, hermesdash.RunOK)
		r.TokensIn, r.TokensOut = &in, &out
		h.transport.setRuns("j1", r)
		h.transport.setRunOutput("j1", "cron_j1_2", "# Tuesday brief\n\nAll quiet.\n")

		result, err := h.service.CollectJobRuns(ctx, h.tenant, "j1")
		require.NoError(t, err)
		assert.Equal(t, 1, result.Fetched)
		assert.Equal(t, 1, result.Inserted)
		assert.Equal(t, 0, result.Skipped)
		assert.Equal(t, 1, result.FilesWritten)
		assert.False(t, result.Truncated)
		require.NotNil(t, result.HighWaterMark)
		assert.True(t, result.HighWaterMark.Equal(at(1_756_800_000)))

		var key, state string
		var output *string
		var fileID, logID *uuid.UUID
		var tokens string
		require.NoError(t, pool.QueryRow(ctx, `SELECT hermes_run_key, status, output, vault_file_id, skill_run_log_id, tokens::text
			FROM hermes_job_runs WHERE tenant_id = $1`, h.tenant).Scan(&key, &state, &output, &fileID, &logID, &tokens))
		assert.Equal(t, "cron_j1_2", key)
		assert.Equal(t, "ok", state)
		assert.Equal(t, "# Tuesday brief\n\nAll quiet.\n", *output)
		require.NotNil(t, fileID)
		assert.NotNil(t, logID)
		assert.JSONEq(t, `{"input":12,"output":34}`, tokens)

		var path, prov string
		require.NoError(t, pool.QueryRow(ctx, `SELECT path, metadata->>'provenance' FROM vault_files WHERE id = $1`, *fileID).Scan(&path, &prov))
		assert.True(t, strings.HasSuffix(path, "raw/jobs/daily-digest/2025-09-02-0800.md"), path)
		assert.Equal(t, JobProvenance, prov)

		var source, name, logStatus string
		var markdown *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT source, name, status, markdown FROM skill_run_log WHERE tenant_id = $1`, h.tenant).Scan(&source, &name, &logStatus, &markdown))
		assert.Equal(t, "hermes", source)
		assert.Equal(t, "Daily Digest", name)
		assert.Equal(t, "success", logStatus)
		assert.Equal(t, "# Tuesday brief\n\nAll quiet.\n", *markdown)

		listed, err := h.service.JobRuns(ctx, h.tenant, "j1", 10)
		require.NoError(t, err)
		require.Len(t, listed.Runs, 1)
		assert.Equal(t, "cron_j1_2", listed.Runs[0].RunKey)
		assert.NotNil(t, listed.CollectedAt)
		require.NotNil(t, listed.Runs[0].Tokens)
		assert.Equal(t, 12, *listed.Runs[0].Tokens.Input)
		require.NotNil(t, listed.Runs[0].VaultFilePath)
		assert.True(t, strings.HasSuffix(*listed.Runs[0].VaultFilePath, "raw/jobs/daily-digest/2025-09-02-0800.md"))

		var rawAfter string
		var highWater time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT raw::text, runs_high_water_at FROM hermes_mirrored_jobs WHERE tenant_id = $1`, h.tenant).Scan(&rawAfter, &highWater))
		assert.Equal(t, rawAfterSync, rawAfter)
		assert.True(t, highWater.Equal(at(1_756_800_000)))
	})

	t.Run("job control mutates on Hermes, mirrors the answer and leaves raw intact", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		prompt := "compile"
		created, err := h.service.CreateJob(ctx, h.tenant, JobCreateRequest{Name: "Nightly", Schedule: "0 3 * * *", Prompt: &prompt})
		require.NoError(t, err)
		assert.Equal(t, "Nightly", *created.Name)
		jobs := h.transport.jobList()
		require.Len(t, jobs, 1)
		assert.Equal(t, created.HermesJobID, jobs[0].ID)
		var rawAfterCreate string
		require.NoError(t, pool.QueryRow(ctx, `SELECT raw::text FROM hermes_mirrored_jobs WHERE tenant_id = $1`, h.tenant).Scan(&rawAfterCreate))

		paused, err := h.service.PauseJob(ctx, h.tenant, created.HermesJobID)
		require.NoError(t, err)
		assert.True(t, paused.Paused)
		resumed, err := h.service.ResumeJob(ctx, h.tenant, created.HermesJobID)
		require.NoError(t, err)
		assert.False(t, resumed.Paused)
		_, err = h.service.TriggerJob(ctx, h.tenant, created.HermesJobID)
		require.NoError(t, err)
		schedule := "0 6 * * *"
		updated, err := h.service.UpdateJob(ctx, h.tenant, created.HermesJobID, JobUpdateRequest{Schedule: &schedule})
		require.NoError(t, err)
		assert.Equal(t, "0 6 * * *", *updated.Schedule)

		var rawAfter string
		require.NoError(t, pool.QueryRow(ctx, `SELECT raw::text FROM hermes_mirrored_jobs WHERE tenant_id = $1`, h.tenant).Scan(&rawAfter))
		assert.Equal(t, rawAfterCreate, rawAfter)
		var decoded map[string]any
		assert.NoError(t, json.Unmarshal([]byte(rawAfter), &decoded))

		_, err = h.service.UpdateJob(ctx, h.tenant, created.HermesJobID, JobUpdateRequest{})
		status, _ := wireStatus(t, err)
		assert.Equal(t, 400, status)
		n := 0
		for _, c := range h.transport.recorded() {
			if strings.HasPrefix(c, "updateJob") {
				n++
			}
		}
		assert.Equal(t, 1, n)

		require.NoError(t, h.service.DeleteJob(ctx, h.tenant, created.HermesJobID))
		rows, err := h.service.jobRows(ctx, h.tenant, ``)
		require.NoError(t, err)
		assert.Empty(t, rows)
		_, err = h.service.PauseJob(ctx, h.tenant, created.HermesJobID)
		status, _ = wireStatus(t, err)
		assert.Equal(t, 404, status)
		_, err = h.service.PauseJob(ctx, h.tenant, "../etc")
		var te *hermesdash.Error
		assert.True(t, errors.As(err, &te))
	})

	t.Run("collecting the same run twice inserts nothing and reads no output", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setJobs(job("j1", "Digest", false))
		_, err := h.service.Sync(ctx, h.tenant, scopes(ScopeJobs))
		require.NoError(t, err)
		h.transport.setRuns("j1", run("cron_j1_1", 1_756_800_000, hermesdash.RunOK))
		h.transport.setRunOutput("j1", "cron_j1_1", "# One\n")
		_, err = h.service.CollectJobRuns(ctx, h.tenant, "j1")
		require.NoError(t, err)

		second, err := h.service.CollectJobRuns(ctx, h.tenant, "j1")
		require.NoError(t, err)
		assert.Equal(t, 0, second.Inserted)
		assert.Equal(t, 1, second.Skipped)
		assert.Equal(t, 0, second.FilesWritten)
		outputs, listings := 0, 0
		for _, c := range h.transport.recorded() {
			if strings.HasPrefix(c, "jobRunOutput") {
				outputs++
			}
			if strings.HasPrefix(c, "jobRuns") {
				listings++
			}
		}
		assert.Equal(t, 1, outputs)
		assert.Equal(t, 2, listings)

		h.transport.setRuns("j1", run("cron_j1_1", 1_756_800_000, hermesdash.RunOK), run("cron_j1_2", 1_756_803_600, hermesdash.RunOK))
		h.transport.setRunOutput("j1", "cron_j1_2", "# Two\n")
		third, err := h.service.CollectJobRuns(ctx, h.tenant, "j1")
		require.NoError(t, err)
		assert.Equal(t, 1, third.Inserted)
		assert.Equal(t, 1, third.Skipped)
	})

	t.Run("running runs are left for later and failed runs are stored without a file", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setJobs(job("j1", "Digest", false))
		_, err := h.service.Sync(ctx, h.tenant, scopes(ScopeJobs))
		require.NoError(t, err)
		failed := run("failed", 1_756_800_000, hermesdash.RunError)
		msg := "provider timeout"
		failed.Error = &msg
		h.transport.setRuns("j1", run("in-flight", 1_756_807_200, hermesdash.RunRunning), failed, run("empty", 1_756_803_600, hermesdash.RunOK))
		h.transport.setRunOutput("j1", "empty", "   \n")

		result, err := h.service.CollectJobRuns(ctx, h.tenant, "j1")
		require.NoError(t, err)
		assert.Equal(t, 3, result.Fetched)
		assert.Equal(t, 2, result.Inserted)
		assert.Equal(t, 1, result.Skipped)
		assert.Equal(t, 0, result.FilesWritten)

		rows, err := pool.Query(ctx, `SELECT hermes_run_key, status, error, output, vault_file_id FROM hermes_job_runs WHERE tenant_id = $1 ORDER BY started_at`, h.tenant)
		require.NoError(t, err)
		var keys []string
		for rows.Next() {
			var k, s string
			var e, o *string
			var f *uuid.UUID
			require.NoError(t, rows.Scan(&k, &s, &e, &o, &f))
			keys = append(keys, k)
			assert.Nil(t, o)
			assert.Nil(t, f)
			if k == "failed" {
				assert.Equal(t, "error", s)
				assert.Equal(t, "provider timeout", *e)
			}
		}
		rows.Close()
		assert.Equal(t, []string{"failed", "empty"}, keys)
		var highWater time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT runs_high_water_at FROM hermes_mirrored_jobs WHERE tenant_id = $1`, h.tenant).Scan(&highWater))
		assert.True(t, highWater.Equal(at(1_756_803_600)))

		h.transport.setRuns("j1", run("in-flight", 1_756_807_200, hermesdash.RunOK))
		h.transport.setRunOutput("j1", "in-flight", "# Landed\n")
		again, err := h.service.CollectJobRuns(ctx, h.tenant, "j1")
		require.NoError(t, err)
		assert.Equal(t, 1, again.Inserted)
	})

	t.Run("collect caps runs per tick and reports truncation", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setJobs(job("j1", "Chatty", false))
		_, err := h.service.Sync(ctx, h.tenant, scopes(ScopeJobs))
		require.NoError(t, err)
		var many []hermesdash.JobRun
		for i := 0; i < runsPerJobPerTick+10; i++ {
			many = append(many, run("run-"+uuid.NewString()[:8], 1_756_800_000+float64(i)*60, hermesdash.RunOK))
		}
		h.transport.setRuns("j1", many...)
		result, err := h.service.CollectJobRuns(ctx, h.tenant, "j1")
		require.NoError(t, err)
		assert.Equal(t, runsPerJobPerTick, result.Fetched)
		assert.Equal(t, runsPerJobPerTick, result.Inserted)
		assert.True(t, result.Truncated)
	})

	t.Run("collectAllJobRuns isolates a failing job and an unknown job is a 404", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setJobs(job("j1", "Good", false), job("j2", "Broken", false))
		_, err := h.service.Sync(ctx, h.tenant, scopes(ScopeJobs))
		require.NoError(t, err)
		h.transport.setRuns("j1", run("cron_j1_1", 1_756_800_000, hermesdash.RunOK))
		h.transport.setRunOutput("j1", "cron_j1_1", "# Fine\n")
		h.transport.fail("jobRuns:j2", hermesdash.DashboardUnreachable("boom"))

		summary, err := h.service.CollectAllJobRuns(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, 1, summary.Jobs)
		assert.Equal(t, 1, summary.Failed)
		assert.Equal(t, 1, summary.Inserted)
		assert.Equal(t, 1, summary.FilesWritten)
		var count int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM hermes_job_runs WHERE tenant_id = $1`, h.tenant).Scan(&count))
		assert.Equal(t, 1, count)

		_, err = h.service.CollectJobRuns(ctx, h.tenant, "nope")
		status, msg := wireStatus(t, err)
		assert.Equal(t, 404, status)
		assert.Equal(t, "hermes_job_not_found", msg)
	})

	t.Run("collectAllJobRuns stops at a tenant-wide error instead of failing every job", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		h.transport.setJobs(job("j1", "A", false), job("j2", "B", false), job("j3", "C", false))
		_, err := h.service.Sync(ctx, h.tenant, scopes(ScopeJobs))
		require.NoError(t, err)
		h.transport.fail("jobRuns:j1", hermesdash.Unsupported("jobRuns"))

		summary, err := h.service.CollectAllJobRuns(ctx, h.tenant)
		require.NoError(t, err)
		assert.Equal(t, 0, summary.Failed)
		assert.Equal(t, 0, summary.Jobs)
		require.NotNil(t, summary.Unavailable)
		assert.Equal(t, "hermes_mirror_unsupported", *summary.Unavailable)
		var runCalls []string
		for _, c := range h.transport.recorded() {
			if strings.HasPrefix(c, "jobRuns:") {
				runCalls = append(runCalls, c)
			}
		}
		assert.Equal(t, []string{"jobRuns:j1"}, runCalls)
	})

	t.Run("an unknown job is a 404 on collect", func(t *testing.T) {
		h := newHarness(t, pool, hermesdash.KindRemote, noLimitsPause())
		_, err := h.service.CollectJobRuns(ctx, h.tenant, "nope")
		status, msg := wireStatus(t, err)
		assert.Equal(t, 404, status)
		assert.Equal(t, "hermes_job_not_found", msg)
	})
}

// Pure helpers (HermesMirrorServiceHelperTests).

func TestSessionMarkdownRendersATranscriptAndTruncatesAtTheCap(t *testing.T) {
	title, source := "Plan", "cli"
	started := time.Unix(0, 0).UTC()
	session := hermesdash.Session{ID: "abc", Title: &title, Source: &source, StartedAt: &started, MessageCount: 2}
	messages := []hermesdash.Message{{Role: "user", Content: "hello"}, {Role: "tool", Content: "ignored"}, {Role: "assistant", Content: "world"}}
	markdown := SessionMarkdown(session, messages, 4096)
	require.NotNil(t, markdown)
	assert.True(t, strings.HasPrefix(*markdown, "# Plan\n"))
	assert.Contains(t, *markdown, "## User\n\nhello")
	assert.Contains(t, *markdown, "## Assistant\n\nworld")
	assert.NotContains(t, *markdown, "ignored")
	truncated := SessionMarkdown(session, messages, 120)
	require.NotNil(t, truncated)
	assert.Contains(t, *truncated, "Transcript truncated")
	assert.Nil(t, SessionMarkdown(session, []hermesdash.Message{{Role: "tool", Content: "x"}}, 100))
}

func TestSessionFileNamesAreDatedAndSanitised(t *testing.T) {
	started := at(1_756_800_000)
	assert.Equal(t, "sessions/2025-09-02-abcd.md", SessionFileName(hermesdash.Session{ID: "ab/../c d", StartedAt: &started, MessageCount: 1}))
}

func TestCompilePromptNamesTheVaultPathAndTheSkill(t *testing.T) {
	kb := "/kb"
	assert.Contains(t, CompilePrompt(&kb), "/kb-compile")
	assert.Contains(t, CompilePrompt(&kb), "`/kb`")
	assert.Contains(t, CompilePrompt(nil), "Locate the knowledge base")
}

func TestTheVaultPathSlugifiesTheJobNameAndStampsTheRunStartInUTC(t *testing.T) {
	started := at(1_756_800_000)
	name := "Daily Digest — Morning"
	assert.Equal(t, "raw/jobs/daily-digest-morning/2025-09-02-0800.md", JobOutputVaultPath(&name, "8628eaf81dda", started))
	blank := "   "
	assert.Equal(t, "raw/jobs/8628eaf81dda/2025-09-02-0800.md", JobOutputVaultPath(&blank, "8628eaf81dda", started))
	assert.Equal(t, "1970-01-01-0000", Stamp(time.Unix(0, 0)))
}

func TestStatusDTOMapsTheStateRow(t *testing.T) {
	job := "j"
	st := &mirrorState{LastStatus: "partial", VaultState: "imported", SkillsCount: 3, CompileJobID: &job}
	dto := statusDTO(st, hermesdash.KindRemote, nil)
	assert.Equal(t, "partial", dto.LastStatus)
	assert.Equal(t, "imported", dto.VaultState)
	assert.EqualValues(t, 3, dto.SkillsCount)
	assert.Equal(t, "j", *dto.CompileJobID)
	assert.Equal(t, "never", statusDTO(nil, hermesdash.KindManaged, nil).LastStatus)
}

// HermesJobRunModelTests: truncation on a grapheme boundary with the marker.
func TestTruncateKeepsShortOutputAndCutsLongOutputOnAGraphemeBoundary(t *testing.T) {
	assert.Equal(t, "short", Truncate("short"))
	exact := strings.Repeat("a", MaxOutputBytes)
	assert.Equal(t, exact, Truncate(exact))
	// A 4-byte emoji straddling the cap is dropped whole, never split.
	long := strings.Repeat("a", MaxOutputBytes-2) + "👍🏽" + "tail"
	out := Truncate(long)
	assert.True(t, strings.HasSuffix(out, TruncationMarker))
	body := strings.TrimSuffix(out, TruncationMarker)
	assert.Equal(t, strings.Repeat("a", MaxOutputBytes-2), body)
	assert.LessOrEqual(t, len(body), MaxOutputBytes)
}

func TestKBSkillsReadsEverySkillSortedAndIgnoresOthers(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"kb-zeta", "kb-alpha", "other"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, name), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(root, name, "SKILL.md"), []byte(name), 0o600))
	}
	skills, err := KBSkills(os.DirFS(root))
	require.NoError(t, err)
	require.Len(t, skills, 2)
	assert.Equal(t, "kb-alpha", skills[0].Name)
	assert.Equal(t, "kb-zeta", skills[1].Name)
	missing, err := KBSkills(os.DirFS(filepath.Join(root, "nope")))
	require.NoError(t, err)
	assert.Empty(t, missing)
}
