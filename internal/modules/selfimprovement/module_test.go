package selfimprovement_test

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"lumina/internal/modules/selfimprovement"
	"lumina/internal/selfimprove"
	"lumina/internal/skills"
	"lumina/internal/soul"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const base = "/v1/me/improvement"

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type env struct {
	t         *testing.T
	pool      *pgxpool.Pool
	paths     vaultpaths.Paths
	hermes    string
	soul      *soul.Service
	overrides map[uuid.UUID]bool
	e         *echo.Echo
}

type envOption func(*selfimprovement.Options)

func setup(t *testing.T, options ...envOption) *env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	root := t.TempDir()
	v := &env{
		t: t, pool: pool, paths: vaultpaths.Paths{Root: filepath.Join(root, "vault")},
		hermes: filepath.Join(root, "hermes"), overrides: map[uuid.UUID]bool{},
	}
	v.soul = soul.New(v.paths, v.hermes)
	opts := selfimprovement.Options{
		Identify:        auth.Identify(keys, auth.PGUsers(pool)),
		Paths:           v.paths,
		Soul:            v.soul,
		Catalog:         skills.NewCatalog(v.paths),
		EconomyModel:    "economy-stub",
		GloballyEnabled: true,
		IsUserOverride:  func(_ context.Context, tenant uuid.UUID) bool { return v.overrides[tenant] },
	}
	for _, option := range options {
		option(&opts)
	}
	v.e = httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{selfimprovement.New(pool, opts)},
	})
	return v
}

type resp struct {
	code int
	ct   string
	body string
}

func (v *env) do(user uuid.UUID, method, path, body string) resp {
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
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()}
}

func (v *env) user() uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	username := "si_" + strings.ReplaceAll(id.String(), "-", "")[:12]
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, username, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, now(), now())`, id, username+"@t", username)
	require.NoError(v.t, err)
	return id
}

func (v *env) username(tenant uuid.UUID) string {
	v.t.Helper()
	var username string
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT username FROM users WHERE id = $1`, tenant).Scan(&username))
	return username
}

func (v *env) exec(sql string, args ...any) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(), sql, args...)
	require.NoError(v.t, err)
}

func (v *env) scalar(sql string, dst any, args ...any) {
	v.t.Helper()
	require.NoError(v.t, v.pool.QueryRow(context.Background(), sql, args...).Scan(dst))
}

// change seeds a pending SOUL proposal.
func (v *env) change(tenant uuid.UUID, proposed, baseSHA *string) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	v.exec(`INSERT INTO self_improvement_changes (id, tenant_id, kind, state, trigger, title, summary, proposed_markdown, base_sha256)
		VALUES ($1, $2, 'soul', 'pending', 'manual', 'SOUL.md review', 'tiny', $3, $4)`, id, tenant, proposed, baseSHA)
	return id
}

func (v *env) changeState(id uuid.UUID) string {
	v.t.Helper()
	var state string
	v.scalar(`SELECT state FROM self_improvement_changes WHERE id = $1`, &state, id)
	return state
}

func (v *env) readSoul(tenant uuid.UUID) string {
	v.t.Helper()
	return v.soul.Read(tenant, v.username(tenant), time.Now())
}

func (v *env) runWithSnapshot(tenant uuid.UUID, snapshot any) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	var text *string
	switch s := snapshot.(type) {
	case nil:
	case string:
		text = &s
	default:
		raw, err := json.Marshal(s)
		require.NoError(v.t, err)
		value := string(raw)
		text = &value
	}
	v.exec(`INSERT INTO self_improvement_runs (id, tenant_id, kind, status, trigger, dry_run, snapshot_json)
		VALUES ($1, $2, 'curator', 'succeeded', 'manual', FALSE, $3)`, id, tenant, text)
	return id
}

func (v *env) skillPath(tenant uuid.UUID, name string) string {
	return filepath.Join(v.paths.TenantRoot(tenant), "skills", name, "SKILL.md")
}

func (v *env) writeSkill(tenant uuid.UUID, name, description string) {
	v.t.Helper()
	path := v.skillPath(tenant, name)
	require.NoError(v.t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(v.t, os.WriteFile(path, []byte(skillBody(name, description)), 0o600))
}

func skillBody(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\nmetadata:\n  capability: low\n---\n\nDo the thing.\n"
}

func errorMessage(t *testing.T, r resp) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &envelope), r.body)
	return envelope.Error.Message
}

func requireError(t *testing.T, r resp, status int, message string) {
	t.Helper()
	require.Equal(t, status, r.code, r.body)
	require.Equal(t, message, errorMessage(t, r))
}

type runDTO struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"`
	Status         string  `json:"status"`
	Trigger        string  `json:"trigger"`
	DryRun         bool    `json:"dryRun"`
	ActionsApplied int     `json:"actionsApplied"`
	ReportMarkdown *string `json:"reportMarkdown"`
}

func acceptedRun(t *testing.T, r resp) runDTO {
	t.Helper()
	require.Equal(t, http.StatusAccepted, r.code, r.body)
	var out struct {
		Run runDTO `json:"run"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &out))
	return out.Run
}

func changeResponse(t *testing.T, r resp) string {
	t.Helper()
	require.Equal(t, http.StatusOK, r.code, r.body)
	var out struct {
		Change struct {
			State string `json:"state"`
		} `json:"change"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &out))
	return out.Change.State
}

const validSettings = `{"settings":{"enabled":true,"curatorEnabled":true,"intervalHours":48,"minimumIdleHours":2,"consolidate":true,
"pruneBuiltins":false,"backupKeep":5,"soulReviewEnabled":true,"reviewComplexSessions":true,"soulReviewWindowDays":14,
"soulReviewCooldownHours":24,"modelMode":"main"}}`

// SelfImprovementServiceTests: "enqueue curator dry-run queues even when curator disabled".
func TestEnqueueCuratorDryRunQueuesEvenWhenCuratorDisabled(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	v.do(tenant, http.MethodGet, base, "")
	v.exec(`UPDATE self_improvement_settings SET enabled = TRUE, curator_enabled = FALSE, consolidate = FALSE WHERE tenant_id = $1`, tenant)

	run := acceptedRun(t, v.do(tenant, http.MethodPost, base+"/curator/runs", `{"dryRun":true}`))
	assert.True(t, run.DryRun)
	assert.Equal(t, "queued", run.Status)
	assert.Equal(t, "curator", run.Kind)

	requireError(t, v.do(tenant, http.MethodPost, base+"/curator/runs", `{"dryRun":false}`), http.StatusConflict, "curator_disabled")
}

// "enqueue curator rejects dry-run while live run is active".
func TestEnqueueCuratorRejectsDryRunWhileLiveRunActive(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	live := acceptedRun(t, v.do(tenant, http.MethodPost, base+"/curator/runs", `{"dryRun":false}`))
	assert.False(t, live.DryRun)
	assert.Equal(t, "queued", live.Status)

	requireError(t, v.do(tenant, http.MethodPost, base+"/curator/runs", `{"dryRun":true}`), http.StatusConflict, "self_improvement_run_already_active")

	var dryRun bool
	var status string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT dry_run, status FROM self_improvement_runs WHERE tenant_id = $1`, tenant).Scan(&dryRun, &status))
	assert.False(t, dryRun)
	assert.Equal(t, "queued", status)
}

// "setPinned marks vault skill curatorPinned".
func TestSetPinnedMarksVaultSkillCuratorPinned(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	v.writeSkill(tenant, "pin-me", "test vault skill")

	r := v.do(tenant, http.MethodPatch, base+"/resources/skill/pin-me", `{"pinned":true}`)
	require.Equal(t, http.StatusOK, r.code, r.body)
	var dto struct {
		Name           string `json:"name"`
		Title          string `json:"title"`
		Pinned         bool   `json:"pinned"`
		CuratorManaged bool   `json:"curatorManaged"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.body), &dto))
	assert.True(t, dto.Pinned)
	assert.True(t, dto.CuratorManaged)
	assert.Equal(t, "pin-me", dto.Name)
	assert.Equal(t, "test vault skill", dto.Title)

	var pinned bool
	v.scalar(`SELECT curator_pinned FROM skills_state WHERE tenant_id = $1 AND name = 'pin-me'`, &pinned, tenant)
	assert.True(t, pinned)
}

// "decide approve applies SOUL when baseSHA matches".
func TestDecideApproveAppliesSoulWhenBaseMatches(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	username := v.username(tenant)
	current, err := v.soul.Write(tenant, username, "# SOUL.md\n\n## Identity\n\nStable voice.\n")
	require.NoError(t, err)
	proposed := soul.Inject("# SOUL.md\n\n## Identity\n\nStable voice.\n\n## Note\n\nTiny tweak.\n")
	sha := selfimprove.SHA256(current)

	id := v.change(tenant, &proposed, &sha)
	assert.Equal(t, "applied", changeResponse(t, v.do(tenant, http.MethodPost, base+"/changes/"+id.String()+"/approve", "")))
	assert.Equal(t, proposed, v.readSoul(tenant))

	// Hermes reads the profile copy, and parity can't see it.
	inHermes, err := os.ReadFile(filepath.Join(v.hermes, "profiles", username, "SOUL.md"))
	require.NoError(t, err)
	assert.Equal(t, proposed, string(inHermes))

	var decided, applied *time.Time
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT decided_at, applied_at FROM self_improvement_changes WHERE id = $1`, id).Scan(&decided, &applied))
	assert.NotNil(t, decided)
	assert.NotNil(t, applied)
}

// "decide approve marks stale when SOUL changed since review".
func TestDecideApproveMarksStaleWhenSoulChanged(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	_, err := v.soul.Write(tenant, v.username(tenant), "# SOUL.md\n\nOriginal.\n")
	require.NoError(t, err)
	proposed := soul.Inject("# SOUL.md\n\nProposed.\n")
	sha := selfimprove.SHA256("not-current")
	id := v.change(tenant, &proposed, &sha)

	requireError(t, v.do(tenant, http.MethodPost, base+"/changes/"+id.String()+"/approve", ""), http.StatusConflict, "soul_changed_since_review")
	assert.Equal(t, "stale", v.changeState(id))
	assert.Contains(t, v.readSoul(tenant), "Original")

	noBase := v.change(tenant, &proposed, nil)
	requireError(t, v.do(tenant, http.MethodPost, base+"/changes/"+noBase.String()+"/approve", ""), http.StatusConflict, "soul_changed_since_review")
	assert.Equal(t, "stale", v.changeState(noBase))
}

// "decide reject leaves SOUL untouched".
func TestDecideRejectLeavesSoulUntouched(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	current, err := v.soul.Write(tenant, v.username(tenant), "# SOUL.md\n\nKeep me.\n")
	require.NoError(t, err)
	proposed := soul.Inject("# SOUL.md\n\nChanged.\n")
	sha := selfimprove.SHA256(current)
	id := v.change(tenant, &proposed, &sha)

	assert.Equal(t, "rejected", changeResponse(t, v.do(tenant, http.MethodPost, base+"/changes/"+id.String()+"/reject", "")))
	assert.Equal(t, current, v.readSoul(tenant))

	requireError(t, v.do(tenant, http.MethodPost, base+"/changes/"+id.String()+"/reject", ""), http.StatusConflict, "self_improvement_change_already_decided")
}

func TestDecideEdges(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	username := v.username(tenant)
	current, err := v.soul.Write(tenant, username, "# SOUL.md\n\nBase.\n")
	require.NoError(t, err)
	sha := selfimprove.SHA256(current)
	approve := func(id uuid.UUID) resp {
		return v.do(tenant, http.MethodPost, base+"/changes/"+id.String()+"/approve", "")
	}

	t.Run("a missing proposal fails the change", func(t *testing.T) {
		id := v.change(tenant, nil, &sha)
		requireError(t, approve(id), http.StatusConflict, "soul_proposal_missing")
		assert.Equal(t, "failed", v.changeState(id))
		var reason string
		v.scalar(`SELECT failure_reason FROM self_improvement_changes WHERE id = $1`, &reason, id)
		assert.Equal(t, "proposal payload missing", reason)
	})
	t.Run("the size limit counts the injected core", func(t *testing.T) {
		fits := strings.Repeat("x", soul.MaxSizeBytes-len(soul.Core)-len("\n\n"))
		require.Equal(t, soul.MaxSizeBytes, len(soul.Inject(fits)))

		over := fits + "x"
		tooBig := v.change(tenant, &over, &sha)
		r := approve(tooBig)
		assert.Equal(t, http.StatusInternalServerError, r.code, r.body)
		assert.Equal(t, "pending", v.changeState(tooBig), "a failed write stores nothing")
		assert.Equal(t, current, v.readSoul(tenant))

		id := v.change(tenant, &fits, &sha)
		assert.Equal(t, "applied", changeResponse(t, approve(id)))
		_, err := v.soul.Write(tenant, username, current)
		require.NoError(t, err)
	})
	t.Run("a failed Hermes copy leaves the vault copy written and the change pending", func(t *testing.T) {
		broken := setup(t, func(o *selfimprovement.Options) {
			root := filepath.Join(t.TempDir(), "a-file")
			require.NoError(t, os.WriteFile(root, nil, 0o600))
			o.Soul = soul.New(o.Paths, root)
		})
		owner := broken.user()
		require.NoError(t, os.MkdirAll(broken.paths.Raw(owner), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(broken.paths.Raw(owner), "SOUL.md"), []byte("# SOUL.md\n\nBase.\n"), 0o600))
		baseSHA := selfimprove.SHA256("# SOUL.md\n\nBase.\n")
		proposed := "# SOUL.md\n\nNew.\n"
		id := broken.change(owner, &proposed, &baseSHA)

		r := broken.do(owner, http.MethodPost, base+"/changes/"+id.String()+"/approve", "")
		assert.Equal(t, http.StatusInternalServerError, r.code, r.body)
		assert.Equal(t, "pending", broken.changeState(id))
		onDisk, err := os.ReadFile(filepath.Join(broken.paths.Raw(owner), "SOUL.md"))
		require.NoError(t, err)
		assert.Equal(t, soul.Inject(proposed), string(onDisk))
	})
	t.Run("another tenant's change is not found", func(t *testing.T) {
		id := v.change(tenant, nil, &sha)
		requireError(t, v.do(v.user(), http.MethodPost, base+"/changes/"+id.String()+"/approve", ""), http.StatusNotFound, "self_improvement_change_not_found")
		assert.Equal(t, "pending", v.changeState(id))
	})
	t.Run("a BOM drops before the hash", func(t *testing.T) {
		owner := v.user()
		require.NoError(t, os.MkdirAll(v.paths.Raw(owner), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(v.paths.Raw(owner), "SOUL.md"), []byte("\xef\xbb\xbfbody"), 0o600))
		baseSHA := selfimprove.SHA256("body")
		proposed := "# SOUL.md\n\nNew.\n"
		assert.Equal(t, "applied", changeResponse(t, v.do(owner, http.MethodPost, base+"/changes/"+v.change(owner, &proposed, &baseSHA).String()+"/approve", "")))
	})
}

func TestStatusAndSettings(t *testing.T) {
	v := setup(t)
	type status struct {
		Availability          string  `json:"availability"`
		EconomyModelAvailable bool    `json:"economyModelAvailable"`
		PendingChanges        int     `json:"pendingChanges"`
		NextReviewAt          *string `json:"nextReviewAt"`
		Message               *string `json:"message"`
		Settings              struct {
			Enabled, CuratorEnabled, Consolidate, PruneBuiltins, SoulReviewEnabled, ReviewComplexSessions bool
			IntervalHours, MinimumIdleHours, BackupKeep, SoulReviewWindowDays, SoulReviewCooldownHours    int
			ModelMode                                                                                     string
		} `json:"settings"`
	}
	read := func(t *testing.T, r resp) status {
		t.Helper()
		require.Equal(t, http.StatusOK, r.code, r.body)
		var s status
		require.NoError(t, json.Unmarshal([]byte(r.body), &s))
		return s
	}

	// SelfImprovementPolicyTests: "safe defaults match Lumina product contract".
	t.Run("safe defaults", func(t *testing.T) {
		tenant := v.user()
		s := read(t, v.do(tenant, http.MethodGet, base, ""))
		assert.True(t, s.Settings.Enabled && s.Settings.CuratorEnabled && s.Settings.Consolidate && s.Settings.SoulReviewEnabled && s.Settings.ReviewComplexSessions)
		assert.False(t, s.Settings.PruneBuiltins)
		assert.Equal(t, []int{168, 2, 5, 14, 24}, []int{s.Settings.IntervalHours, s.Settings.MinimumIdleHours, s.Settings.BackupKeep, s.Settings.SoulReviewWindowDays, s.Settings.SoulReviewCooldownHours})
		assert.Equal(t, "economy", s.Settings.ModelMode)
		assert.Equal(t, "managed", s.Availability)
		assert.True(t, s.EconomyModelAvailable)
		assert.Nil(t, s.Message)
		require.NotNil(t, s.NextReviewAt)
		next, err := time.Parse(time.RFC3339, *s.NextReviewAt)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(168*time.Hour), next, time.Minute)
	})
	t.Run("put replaces the settings and reschedules", func(t *testing.T) {
		tenant := v.user()
		s := read(t, v.do(tenant, http.MethodPut, base, validSettings))
		assert.Equal(t, 48, s.Settings.IntervalHours)
		assert.Equal(t, "main", s.Settings.ModelMode)
		next, err := time.Parse(time.RFC3339, *s.NextReviewAt)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(48*time.Hour), next, time.Minute)
	})
	t.Run("the first failing key is Swift's, in declaration order", func(t *testing.T) {
		tenant := v.user()
		requireError(t, v.do(tenant, http.MethodPut, base, `{"settings":{}}`), http.StatusBadRequest, "Coding key `enabled` not found.")
		requireError(t, v.do(tenant, http.MethodPut, base, `{"settings":{"enabled":true}}`), http.StatusBadRequest, "Coding key `curatorEnabled` not found.")
		requireError(t, v.do(tenant, http.MethodPut, base, strings.Replace(validSettings, `"main"`, `"premium"`, 1)), http.StatusBadRequest,
			"The given data was not valid input: Cannot initialize ImprovementModelMode from invalid String value premium")
		requireError(t, v.do(tenant, http.MethodPut, base, strings.Replace(validSettings, `"main"`, `1`, 1)), http.StatusBadRequest,
			"Type mismatch for `settings.modelMode` key, expected `String` type.")
	})
	t.Run("identity comes after decoding and before the range check", func(t *testing.T) {
		requireError(t, v.do(uuid.Nil, http.MethodPut, base, `{`), http.StatusBadRequest, "The given data was not valid input: The given data was not valid JSON.")
		assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodPut, base, strings.Replace(validSettings, `:48`, `:1`, 1)).code)
		requireError(t, v.do(v.user(), http.MethodPut, base, strings.Replace(validSettings, `:48`, `:1`, 1)), http.StatusBadRequest, "invalid_self_improvement_settings")
		assert.Equal(t, http.StatusUnauthorized, v.do(uuid.Nil, http.MethodGet, base, "").code)
	})
	t.Run("BYO Hermes is read only", func(t *testing.T) {
		tenant := v.user()
		v.overrides[tenant] = true
		s := read(t, v.do(tenant, http.MethodGet, base, ""))
		assert.Equal(t, "read_only", s.Availability)
		require.NotNil(t, s.Message)
		assert.Contains(t, *s.Message, "read-only")
		requireError(t, v.do(tenant, http.MethodPut, base, validSettings), http.StatusConflict, "self_improvement_read_only_on_byo_hermes")
		requireError(t, v.do(tenant, http.MethodPost, base+"/curator/runs", ""), http.StatusConflict, "self_improvement_read_only_on_byo_hermes")
		requireError(t, v.do(tenant, http.MethodPost, base+"/soul/reviews", ""), http.StatusConflict, "soul_review_unsupported_on_byo_hermes")
		requireError(t, v.do(tenant, http.MethodPost, base+"/changes/"+uuid.NewString()+"/reject", ""), http.StatusConflict, "self_improvement_read_only_on_byo_hermes")
		requireError(t, v.do(tenant, http.MethodPost, base+"/runs/"+uuid.NewString()+"/rollback", ""), http.StatusConflict, "self_improvement_read_only_on_byo_hermes")
		requireError(t, v.do(tenant, http.MethodPatch, base+"/resources/skill/x", `{"pinned":true}`), http.StatusConflict, "self_improvement_read_only_on_byo_hermes")
	})
	t.Run("the operator's switch", func(t *testing.T) {
		off := setup(t, func(o *selfimprovement.Options) { o.GloballyEnabled = false; o.EconomyModel = "" })
		tenant := off.user()
		s := read(t, off.do(tenant, http.MethodGet, base, ""))
		assert.Equal(t, "unavailable", s.Availability)
		assert.False(t, s.EconomyModelAvailable)
		requireError(t, off.do(tenant, http.MethodPut, base, validSettings), http.StatusServiceUnavailable, "self_improvement_disabled")
		requireError(t, off.do(tenant, http.MethodPost, base+"/curator/runs", ""), http.StatusServiceUnavailable, "self_improvement_disabled")
		requireError(t, off.do(tenant, http.MethodPost, base+"/soul/reviews", ""), http.StatusServiceUnavailable, "self_improvement_disabled")
	})
	t.Run("a nil override means never", func(t *testing.T) {
		none := setup(t, func(o *selfimprovement.Options) { o.IsUserOverride = nil })
		assert.Equal(t, "managed", read(t, none.do(none.user(), http.MethodGet, base, "")).Availability)
	})
}

func TestQueue(t *testing.T) {
	v := setup(t)
	settingsRows := func(tenant uuid.UUID) int {
		var n int
		v.scalar(`SELECT count(*) FROM self_improvement_settings WHERE tenant_id = $1`, &n, tenant)
		return n
	}
	t.Run("a dry curator run writes no settings row, a live one does", func(t *testing.T) {
		tenant := v.user()
		acceptedRun(t, v.do(tenant, http.MethodPost, base+"/curator/runs", ""))
		assert.Zero(t, settingsRows(tenant))
		v.exec(`UPDATE self_improvement_runs SET status = 'succeeded' WHERE tenant_id = $1`, tenant)
		acceptedRun(t, v.do(tenant, http.MethodPost, base+"/curator/runs", `{"dryRun":false}`))
		assert.Equal(t, 1, settingsRows(tenant))
	})
	t.Run("a bad body means the default", func(t *testing.T) {
		tenant := v.user()
		for _, body := range []string{"", "{", "[]", "null", "{}", `{"dryRun":"no"}`, `{"dryRun":null}`} {
			assert.True(t, acceptedRun(t, v.do(tenant, http.MethodPost, base+"/curator/runs", body)).DryRun, body)
		}
	})
	t.Run("soul review: only a known non-manual trigger is refused", func(t *testing.T) {
		tenant := v.user()
		requireError(t, v.do(uuid.Nil, http.MethodPost, base+"/soul/reviews", `{"trigger":"weekly"}`), http.StatusBadRequest, "manual_trigger_required")
		requireError(t, v.do(tenant, http.MethodPost, base+"/soul/reviews", `{"trigger":"complex_session"}`), http.StatusBadRequest, "manual_trigger_required")
		for _, body := range []string{"", "{", `{"trigger":"nope"}`, `{"trigger":1}`, `{"trigger":"manual"}`} {
			run := acceptedRun(t, v.do(tenant, http.MethodPost, base+"/soul/reviews", body))
			assert.Equal(t, "soul", run.Kind, body)
			assert.Equal(t, "manual", run.Trigger, body)
			assert.False(t, run.DryRun, body)
		}
		var runs int
		v.scalar(`SELECT count(*) FROM self_improvement_runs WHERE tenant_id = $1`, &runs, tenant)
		assert.Equal(t, 1, runs)
	})
	t.Run("the switches", func(t *testing.T) {
		tenant := v.user()
		v.do(tenant, http.MethodGet, base, "")
		v.exec(`UPDATE self_improvement_settings SET soul_review_enabled = FALSE WHERE tenant_id = $1`, tenant)
		requireError(t, v.do(tenant, http.MethodPost, base+"/soul/reviews", ""), http.StatusConflict, "soul_review_disabled")
		v.exec(`UPDATE self_improvement_settings SET soul_review_enabled = TRUE, enabled = FALSE WHERE tenant_id = $1`, tenant)
		requireError(t, v.do(tenant, http.MethodPost, base+"/soul/reviews", ""), http.StatusConflict, "soul_review_disabled")
		requireError(t, v.do(tenant, http.MethodPost, base+"/curator/runs", `{"dryRun":false}`), http.StatusConflict, "curator_disabled")
	})
	t.Run("the status writes nothing to the queue, and the rows suit the scheduler's claim", func(t *testing.T) {
		tenant := v.user()
		run := acceptedRun(t, v.do(tenant, http.MethodPost, base+"/soul/reviews", ""))
		var claimed uuid.UUID
		v.scalar(`UPDATE self_improvement_runs SET status = 'running', started_at = now()
			WHERE id = (SELECT id FROM self_improvement_runs WHERE status = 'queued' AND tenant_id = $1
				ORDER BY created_at ASC FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING id`, &claimed, tenant)
		assert.Equal(t, strings.ToLower(run.ID), claimed.String())
		assert.Equal(t, ids.Upper(claimed), run.ID)
	})
}

func TestRunsAndChanges(t *testing.T) {
	v := setup(t)
	tenant := v.user()
	start := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for i := range 105 {
		v.exec(`INSERT INTO self_improvement_runs (id, tenant_id, kind, status, trigger, dry_run, created_at)
			VALUES ($1, $2, 'curator', 'succeeded', 'manual', TRUE, $3)`, uuid.New(), tenant, start.Add(time.Duration(i)*time.Hour))
	}
	count := func(query string) int {
		r := v.do(tenant, http.MethodGet, base+"/runs"+query, "")
		require.Equal(t, http.StatusOK, r.code, r.body)
		var out struct {
			Runs []runDTO `json:"runs"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.body), &out))
		return len(out.Runs)
	}
	for query, want := range map[string]int{
		"": 50, "?limit=1": 1, "?limit=0": 1, "?limit=-5": 1, "?limit=100": 100, "?limit=101": 100,
		"?limit=abc": 50, "?limit=1.5": 50, "?limit=%2B2": 2, "?limit=": 50,
	} {
		assert.Equal(t, want, count(query), query)
	}
	requireError(t, v.do(uuid.Nil, http.MethodGet, base+"/runs/nope", ""), http.StatusBadRequest, "invalid_id")
	requireError(t, v.do(tenant, http.MethodGet, base+"/runs/"+uuid.NewString(), ""), http.StatusNotFound, "self_improvement_run_not_found")

	t.Run("a run of another tenant", func(t *testing.T) {
		other := v.runWithSnapshot(v.user(), nil)
		requireError(t, v.do(tenant, http.MethodGet, base+"/runs/"+other.String(), ""), http.StatusNotFound, "self_improvement_run_not_found")
	})
	t.Run("unknown values fall back", func(t *testing.T) {
		id := uuid.New()
		v.exec(`INSERT INTO self_improvement_runs (id, tenant_id, kind, status, trigger, dry_run) VALUES ($1, $2, 'x', 'x', 'x', TRUE)`, id, tenant)
		r := v.do(tenant, http.MethodGet, base+"/runs/"+id.String(), "")
		require.Equal(t, http.StatusOK, r.code, r.body)
		var run runDTO
		require.NoError(t, json.Unmarshal([]byte(r.body), &run))
		assert.Equal(t, []string{"curator", "failed", "manual"}, []string{run.Kind, run.Status, run.Trigger})

		change := uuid.New()
		v.exec(`INSERT INTO self_improvement_changes (id, tenant_id, kind, state, trigger, title, summary) VALUES ($1, $2, 'x', 'x', 'x', 't', 's')`, change, tenant)
		r = v.do(tenant, http.MethodGet, base+"/changes", "")
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.JSONEq(t, `"soul"`, jsonField(t, r.body, "kind"))
		assert.JSONEq(t, `"failed"`, jsonField(t, r.body, "state"))
	})
}

func jsonField(t *testing.T, body, key string) string {
	t.Helper()
	var out struct {
		Changes []map[string]json.RawMessage `json:"changes"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	require.NotEmpty(t, out.Changes)
	return string(out.Changes[0][key])
}

func TestRollback(t *testing.T) {
	v := setup(t)
	rollback := func(tenant, run uuid.UUID) resp {
		return v.do(tenant, http.MethodPost, base+"/runs/"+run.String()+"/rollback", "")
	}
	type snap = map[string]any
	unavailable := func(t *testing.T, r resp) {
		t.Helper()
		requireError(t, r, http.StatusConflict, "rollback_snapshot_unavailable")
	}

	t.Run("every way a snapshot can be unusable is the one 409", func(t *testing.T) {
		tenant := v.user()
		for _, snapshot := range []any{
			nil, "not json", "[]", "null", "{}", `[null]`, `[{}]`, `[1]`,
			`[{"name":"x"}]`, `[{"name":"x","markdown":null,"state":"a"}]`, `[{"name":1,"markdown":"m","state":"a"}]`,
			`[{"name":"x","markdown":"m","state":"a","afterSHA":1}]`, `[{"name":"x","markdown":"m","state":"a","archivedPath":[]}]`,
			`[{"NAME":"x","markdown":"m","state":"a"}]`,
		} {
			unavailable(t, rollback(tenant, v.runWithSnapshot(tenant, snapshot)))
		}
		unavailable(t, rollback(tenant, uuid.New()))
		unavailable(t, rollback(v.user(), v.runWithSnapshot(tenant, []snap{{"name": "x", "markdown": "m", "state": "a"}})))
		assert.NoFileExists(t, v.skillPath(tenant, "x"))
	})
	t.Run("restores the files and state", func(t *testing.T) {
		tenant := v.user()
		run := v.runWithSnapshot(tenant, []snap{
			{"name": "alpha", "markdown": skillBody("alpha", "A"), "state": "stale", "afterSHA": nil, "archivedPath": nil, "extra": 1},
			{"name": "beta", "markdown": skillBody("beta", "B"), "state": "active"},
		})
		r := rollback(tenant, run)
		require.Equal(t, http.StatusOK, r.code, r.body)
		var out struct {
			Run runDTO `json:"run"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.body), &out))
		assert.Equal(t, "rolled_back", out.Run.Status)

		onDisk, err := os.ReadFile(v.skillPath(tenant, "alpha"))
		require.NoError(t, err)
		assert.Equal(t, skillBody("alpha", "A"), string(onDisk))
		var state string
		var enabled, pinned bool
		require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT curator_state, enabled, curator_pinned FROM skills_state WHERE tenant_id = $1 AND source = 'vault' AND name = 'alpha'`, tenant).Scan(&state, &enabled, &pinned))
		assert.Equal(t, "stale", state)
		assert.True(t, enabled)
		assert.False(t, pinned)
	})
	t.Run("a changed file stops the run before any write", func(t *testing.T) {
		tenant := v.user()
		v.writeSkill(tenant, "alpha", "edited by hand")
		run := v.runWithSnapshot(tenant, []snap{
			{"name": "beta", "markdown": "b", "state": "active"},
			{"name": "alpha", "markdown": "a", "state": "active", "afterSHA": selfimprove.SHA256("what the curator wrote")},
		})
		requireError(t, rollback(tenant, run), http.StatusConflict, "resource_changed_since_curator_run")
		assert.NoFileExists(t, v.skillPath(tenant, "beta"))
		onDisk, err := os.ReadFile(v.skillPath(tenant, "alpha"))
		require.NoError(t, err)
		assert.Equal(t, skillBody("alpha", "edited by hand"), string(onDisk))
		var status string
		v.scalar(`SELECT status FROM self_improvement_runs WHERE id = $1`, &status, run)
		assert.Equal(t, "succeeded", status)
	})
	t.Run("a missing file, a file that isn't UTF-8, and no afterSHA all pass the check", func(t *testing.T) {
		tenant := v.user()
		path := v.skillPath(tenant, "bin")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte{0xff, 0xfe}, 0o600))
		v.writeSkill(tenant, "plain", "x")
		run := v.runWithSnapshot(tenant, []snap{
			{"name": "bin", "markdown": "new", "state": "active", "afterSHA": "whatever"},
			{"name": "missing", "markdown": "new", "state": "active", "afterSHA": "whatever"},
			{"name": "plain", "markdown": "new", "state": "active"},
		})
		require.Equal(t, http.StatusOK, rollback(tenant, run).code)
		onDisk, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "new", string(onDisk))
	})
	t.Run("archivedPath is removed, recursively, and a missing one is ignored", func(t *testing.T) {
		tenant := v.user()
		archive := filepath.Join(v.paths.TenantRoot(tenant), "skills", ".archive", "alpha-run")
		require.NoError(t, os.MkdirAll(filepath.Join(archive, "deep"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(archive, "deep", "SKILL.md"), []byte("x"), 0o600))
		run := v.runWithSnapshot(tenant, []snap{
			{"name": "alpha", "markdown": "a", "state": "active", "archivedPath": archive},
			{"name": "beta", "markdown": "b", "state": "active", "archivedPath": filepath.Join(archive, "never")},
		})
		require.Equal(t, http.StatusOK, rollback(tenant, run).code)
		assert.NoDirExists(t, archive)
	})
	t.Run("state: Swift's save() on a loaded row updates every row of the tenant", func(t *testing.T) {
		tenant := v.user()
		other := v.user()
		v.exec(`INSERT INTO skills_state (tenant_id, source, name, curator_state, curator_archived_at) VALUES
			($1, 'vault', 'alpha', 'archived', now()),
			($1, 'vault', 'unrelated', 'stale', now()),
			($1, 'builtin', 'daily-brief', 'stale', now()),
			($2, 'vault', 'alpha', 'stale', now())`, tenant, other)
		run := v.runWithSnapshot(tenant, []snap{{"name": "alpha", "markdown": "a", "state": "active"}})
		require.Equal(t, http.StatusOK, rollback(tenant, run).code)

		var updated int
		v.scalar(`SELECT count(*) FROM skills_state WHERE tenant_id = $1 AND curator_state = 'active' AND curator_archived_at IS NULL`, &updated, tenant)
		assert.Equal(t, 3, updated)
		var state string
		v.scalar(`SELECT curator_state FROM skills_state WHERE tenant_id = $1`, &state, other)
		assert.Equal(t, "stale", state)
	})
	t.Run("a failure partway keeps what came first and leaves the status", func(t *testing.T) {
		tenant := v.user()
		v.writeSkill(tenant, "alpha", "x")
		run := v.runWithSnapshot(tenant, []snap{
			{"name": "first", "markdown": "1", "state": "active"},
			{"name": "alpha/SKILL.md/under-a-file", "markdown": "2", "state": "active"},
		})
		assert.Equal(t, http.StatusInternalServerError, rollback(tenant, run).code)
		assert.FileExists(t, v.skillPath(tenant, "first"))
		var status string
		v.scalar(`SELECT status FROM self_improvement_runs WHERE id = $1`, &status, run)
		assert.Equal(t, "succeeded", status)
	})
	t.Run("a name is not cleaned on the way to the filesystem, as in Swift", func(t *testing.T) {
		tenant := v.user()
		run := v.runWithSnapshot(tenant, []snap{{"name": "no-such-dir/../escaped", "markdown": "x", "state": "active"}})
		assert.Equal(t, http.StatusOK, rollback(tenant, run).code)
		assert.DirExists(t, filepath.Join(v.paths.TenantRoot(tenant), "skills", "no-such-dir"))
		assert.FileExists(t, v.skillPath(tenant, "escaped"))
	})
}

func TestResources(t *testing.T) {
	v := setup(t)
	type item struct {
		Name           string  `json:"name"`
		Title          string  `json:"title"`
		Kind           string  `json:"kind"`
		State          string  `json:"state"`
		Pinned         bool    `json:"pinned"`
		CuratorManaged bool    `json:"curatorManaged"`
		LastActivityAt *string `json:"lastActivityAt"`
	}
	list := func(t *testing.T, tenant uuid.UUID) []item {
		t.Helper()
		r := v.do(tenant, http.MethodGet, base+"/resources", "")
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Contains(t, r.body, `"skills":[`, "an empty list is [], never null")
		var out struct {
			Skills []item `json:"skills"`
		}
		require.NoError(t, json.Unmarshal([]byte(r.body), &out))
		return out.Skills
	}
	pin := func(tenant uuid.UUID, kind, name, body string) resp {
		return v.do(tenant, http.MethodPatch, base+"/resources/"+kind+"/"+name, body)
	}
	assert.Empty(t, list(t, v.user()))

	tenant := v.user()
	v.writeSkill(tenant, "zeta", "Zeta")
	v.writeSkill(tenant, "alpha", "Alpha")
	v.writeSkill(tenant, "job-nightly", "Nightly")
	schedule := v.skillPath(tenant, "cron-skill")
	require.NoError(t, os.MkdirAll(filepath.Dir(schedule), 0o755))
	require.NoError(t, os.WriteFile(schedule, []byte("---\nname: cron-skill\ndescription: Cron\nmetadata:\n  capability: low\n  schedule: \"0 7 * * *\"\n---\n\nBody.\n"), 0o600))
	v.exec(`INSERT INTO skills_state (tenant_id, source, name, curator_state, curator_pinned, curator_last_activity_at) VALUES
		($1, 'vault', 'zeta', 'stale', TRUE, '2026-03-01T00:00:00Z'),
		($1, 'vault', 'gone', 'archived', TRUE, NULL),
		($1, 'vault', 'job-gone', 'archived', FALSE, NULL),
		($1, 'vault', 'orphan', 'active', FALSE, NULL),
		($1, 'vault', 'belief-evolution', 'archived', FALSE, NULL),
		($1, 'builtin', 'creative', 'archived', FALSE, NULL)`, tenant)

	items := list(t, tenant)
	names := make([]string, len(items))
	byName := map[string]item{}
	for i, it := range items {
		names[i] = it.Name
		byName[it.Name] = it
		assert.True(t, it.CuratorManaged)
	}
	assert.Equal(t, []string{"alpha", "belief-evolution", "cron-skill", "gone", "job-gone", "job-nightly", "zeta"}, names)
	assert.Equal(t, item{Name: "alpha", Title: "Alpha", Kind: "skill", State: "active", CuratorManaged: true}, byName["alpha"])
	assert.Equal(t, "job", byName["job-nightly"].Kind)
	assert.Equal(t, "job", byName["cron-skill"].Kind)
	assert.Equal(t, "job", byName["job-gone"].Kind)
	assert.Equal(t, "stale", byName["zeta"].State)
	assert.True(t, byName["zeta"].Pinned)
	require.NotNil(t, byName["zeta"].LastActivityAt)
	assert.Equal(t, "gone", byName["gone"].Title)

	t.Run("refusals", func(t *testing.T) {
		requireError(t, pin(uuid.Nil, "nope", "alpha", `{"pinned":true}`), http.StatusBadRequest, "invalid_resource")
		requireError(t, pin(uuid.Nil, "skill", "alpha", `{`), http.StatusBadRequest, "The given data was not valid input: The given data was not valid JSON.")
		assert.Equal(t, http.StatusUnauthorized, pin(uuid.Nil, "skill", "a.b", `{"pinned":true}`).code)
		requireError(t, pin(tenant, "skill", "alpha", `{}`), http.StatusBadRequest, "Coding key `pinned` not found.")
		requireError(t, pin(tenant, "skill", "alpha", `{"pinned":null}`), http.StatusBadRequest, "Type mismatch for `pinned` key, expected `Bool` type.")
		requireError(t, pin(tenant, "skill", "a.b", `{"pinned":true}`), http.StatusBadRequest, "invalid_resource_name")
		requireError(t, pin(tenant, "skill", strings.Repeat("a", 81), `{"pinned":true}`), http.StatusBadRequest, "invalid_resource_name")
		requireError(t, pin(tenant, "job", "alpha", `{"pinned":true}`), http.StatusConflict, "resource_kind_mismatch")
		requireError(t, pin(tenant, "skill", "job-nightly", `{"pinned":true}`), http.StatusConflict, "resource_kind_mismatch")
		requireError(t, pin(tenant, "skill", "cron-skill", `{"pinned":true}`), http.StatusConflict, "resource_kind_mismatch")
		requireError(t, pin(tenant, "skill", "orphan", `{"pinned":true}`), http.StatusNotFound, "curator_resource_not_found")
		requireError(t, pin(tenant, "skill", "daily-brief", `{"pinned":true}`), http.StatusNotFound, "curator_resource_not_found")
		requireError(t, pin(v.user(), "skill", "alpha", `{"pinned":true}`), http.StatusNotFound, "curator_resource_not_found")
	})
	t.Run("path parameters are not percent-decoded, as in Hummingbird", func(t *testing.T) {
		requireError(t, pin(tenant, "skill", "al%70ha", `{"pinned":true}`), http.StatusBadRequest, "invalid_resource_name")
		requireError(t, pin(tenant, "skill", "a%2Fb", `{"pinned":true}`), http.StatusBadRequest, "invalid_resource_name")
		requireError(t, pin(tenant, "skill", "caf%C3%A9", `{"pinned":true}`), http.StatusBadRequest, "invalid_resource_name")
		requireError(t, pin(tenant, "ski%6C", "alpha", `{"pinned":true}`), http.StatusBadRequest, "invalid_resource")
		requireError(t, v.do(tenant, http.MethodGet, base+"/runs/%35"+uuid.NewString()[1:], ""), http.StatusBadRequest, "invalid_id")
	})
	t.Run("archived rows: any kind unless a manifest decides, and the built-in title", func(t *testing.T) {
		r := pin(tenant, "skill", "job-gone", `{"pinned":true}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Contains(t, r.body, `"kind":"skill"`)
		assert.Contains(t, r.body, `"state":"archived"`)
		assert.Contains(t, r.body, `"title":"job-gone"`)

		requireError(t, pin(tenant, "job", "belief-evolution", `{"pinned":true}`), http.StatusConflict, "resource_kind_mismatch")
		r = pin(tenant, "skill", "belief-evolution", `{"pinned":true}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.NotContains(t, r.body, `"title":"belief-evolution"`)
	})
	t.Run("a new row is inserted and touches no other", func(t *testing.T) {
		owner := v.user()
		v.writeSkill(owner, "fresh", "F")
		v.exec(`INSERT INTO skills_state (tenant_id, source, name) VALUES ($1, 'vault', 'other')`, owner)
		require.Equal(t, http.StatusOK, pin(owner, "skill", "fresh", `{"pinned":true}`).code)
		var pinned bool
		v.scalar(`SELECT curator_pinned FROM skills_state WHERE tenant_id = $1 AND name = 'other'`, &pinned, owner)
		assert.False(t, pinned)
	})
	t.Run("a loaded row: Swift's save() updates every row of the tenant", func(t *testing.T) {
		owner := v.user()
		other := v.user()
		v.writeSkill(owner, "fresh", "F")
		v.exec(`INSERT INTO skills_state (tenant_id, source, name, curator_state) VALUES
			($1, 'vault', 'fresh', 'stale'),
			($1, 'vault', 'other', 'active'),
			($1, 'builtin', 'daily-brief', 'active'),
			($2, 'vault', 'fresh', 'active')`, owner, other)
		r := pin(owner, "skill", "fresh", `{"pinned":true}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Contains(t, r.body, `"state":"stale"`)

		var pinned int
		v.scalar(`SELECT count(*) FROM skills_state WHERE tenant_id = $1 AND curator_pinned`, &pinned, owner)
		assert.Equal(t, 3, pinned)
		v.scalar(`SELECT count(*) FROM skills_state WHERE tenant_id = $1 AND curator_pinned`, &pinned, other)
		assert.Zero(t, pinned)
	})
	t.Run("with the built-ins off, a built-in name has no manifest", func(t *testing.T) {
		vaultOnly := setup(t, func(o *selfimprovement.Options) { o.Catalog = skills.NewVaultCatalog(o.Paths) })
		owner := vaultOnly.user()
		vaultOnly.exec(`INSERT INTO skills_state (tenant_id, source, name, curator_state) VALUES ($1, 'vault', 'belief-evolution', 'archived')`, owner)
		r := vaultOnly.do(owner, http.MethodPatch, base+"/resources/job/belief-evolution", `{"pinned":true}`)
		require.Equal(t, http.StatusOK, r.code, r.body)
		assert.Contains(t, r.body, `"title":"belief-evolution"`)
	})
}

// Every success body must validate against api/openapi.yaml (decisions.md D2).
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	tenant := v.user()
	username := v.username(tenant)
	current, err := v.soul.Write(tenant, username, "# SOUL.md\n\nBase.\n")
	require.NoError(t, err)
	sha := selfimprove.SHA256(current)
	proposed := "# SOUL.md\n\nNew.\n"
	approve := v.change(tenant, &proposed, &sha)
	reject := v.change(tenant, &proposed, &sha)
	v.writeSkill(tenant, "alpha", "Alpha")
	run := v.runWithSnapshot(tenant, []map[string]any{{"name": "beta", "markdown": skillBody("beta", "B"), "state": "active"}})

	for _, c := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, base, "", http.StatusOK},
		{http.MethodPut, base, validSettings, http.StatusOK},
		{http.MethodPost, base + "/curator/runs", `{"dryRun":true}`, http.StatusAccepted},
		{http.MethodPost, base + "/soul/reviews", `{"trigger":"manual"}`, http.StatusAccepted},
		{http.MethodGet, base + "/runs", "", http.StatusOK},
		{http.MethodGet, base + "/runs/" + run.String(), "", http.StatusOK},
		{http.MethodPost, base + "/runs/" + run.String() + "/rollback", "", http.StatusOK},
		{http.MethodGet, base + "/resources", "", http.StatusOK},
		{http.MethodPatch, base + "/resources/skill/alpha", `{"pinned":true}`, http.StatusOK},
		{http.MethodGet, base + "/changes", "", http.StatusOK},
		{http.MethodPost, base + "/changes/" + approve.String() + "/approve", "", http.StatusOK},
		{http.MethodPost, base + "/changes/" + reject.String() + "/reject", "", http.StatusOK},
	} {
		r := v.do(tenant, c.method, c.path, c.body)
		require.Equal(t, c.status, r.code, "%s %s: %s", c.method, c.path, r.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes([]byte(r.body))
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}
