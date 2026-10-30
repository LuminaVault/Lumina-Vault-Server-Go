package kanban_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"lumina/internal/modules/kanban"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Fixture ids from parity/seed/base.sql and parity/seed/kanban.sql.
var (
	alice     = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob       = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	ann       = uuid.MustParse("CA4B0000-0000-4000-8000-0000000000A1") // no boards
	ben       = uuid.MustParse("CA4B0000-0000-4000-8000-0000000000B2") // team editor
	teamVault = "CA4BF000-0000-4000-8000-000000000001"
	teamBoard = "CA4BB000-0000-4000-8000-0000000000F1"
	teamCol   = "CA4BC000-0000-4000-8000-0000000000F1"
	aliceBrd  = "CA4BB000-0000-4000-8000-0000000000A1"
	keys      = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t     *testing.T
	e     *echo.Echo
	pool  *pgxpool.Pool
	vault string
}

type options struct{ maxConns int32 }

func setup(t *testing.T, opts ...options) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	for _, f := range []string{"base.sql", "kanban.sql"} {
		seed, err := os.ReadFile("../../../parity/seed/" + f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	if len(opts) > 0 && opts[0].maxConns > 0 {
		cfg := pool.Config()
		cfg.MaxConns = opts[0].maxConns
		small, err := pgxpool.NewWithConfig(ctx, cfg)
		require.NoError(t, err)
		t.Cleanup(small.Close)
		pool = small
	}
	vault := t.TempDir()
	users := auth.PGUsers(pool)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, users),
		Modules: []httpserver.Module{
			kanban.New(pool, auth.Identify(keys, users), ratelimit.NewStore(), vaultpaths.Paths{Root: vault}),
		},
	})
	return env{t: t, e: e, pool: pool, vault: vault}
}

type resp struct {
	code int
	ct   string
	hdr  http.Header
	body []byte
}

func (r resp) errMessage() string {
	var b struct {
		Error struct{ Message string } `json:"error"`
	}
	_ = json.Unmarshal(r.body, &b)
	return b.Error.Message
}

func (v env) do(user uuid.UUID, method, path, body string, headers ...string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Header(), rec.Body.Bytes()}
}

// ok asserts 200 and decodes the body into T.
func ok[T any](t *testing.T, r resp) T {
	t.Helper()
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var out T
	require.NoError(t, json.Unmarshal(r.body, &out))
	return out
}

func (v env) exec(sql string, args ...any) {
	v.t.Helper()
	_, err := v.pool.Exec(context.Background(), sql, args...)
	require.NoError(v.t, err)
}

type card struct {
	ID              string     `json:"id"`
	ColumnID        string     `json:"columnID"`
	Title           string     `json:"title"`
	Body            *string    `json:"body"`
	Priority        *string    `json:"priority"`
	DueAt           *string    `json:"dueAt"`
	Rank            string     `json:"rank"`
	UpdatedAt       *string    `json:"updatedAt"`
	JobConfig       *jobConfig `json:"jobConfig"`
	CreatedByUserID *string    `json:"createdByUserId"`
}

type jobConfig struct {
	Source     string  `json:"source"`
	Cron       *string `json:"cron"`
	RunAt      *string `json:"runAt"`
	Domain     *string `json:"domain"`
	JobSlug    *string `json:"jobSlug"`
	PromotedAt *string `json:"promotedAt"`
}

type column struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Rank  string `json:"rank"`
	Cards []card `json:"cards"`
}

type board struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Version         int64    `json:"version"`
	Columns         []column `json:"columns"`
	CreatedByUserID *string  `json:"createdByUserId"`
}

type summary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Version     int64  `json:"version"`
	ColumnCount int    `json:"columnCount"`
	CardCount   int    `json:"cardCount"`
}

type skill struct {
	ID              string  `json:"id"`
	Source          string  `json:"source"`
	Name            string  `json:"name"`
	Title           string  `json:"title"`
	DescriptionText string  `json:"descriptionText"`
	Capability      string  `json:"capability"`
	Schedule        *string `json:"schedule"`
	Enabled         bool    `json:"enabled"`
	BodyExcerpt     string  `json:"bodyExcerpt"`
	Origin          struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"origin"`
}

// newBoard creates a board with columns for user and returns board and column ids.
func (v env) newBoard(user uuid.UUID, title string, columns ...string) (string, []string) {
	v.t.Helper()
	b := ok[board](v.t, v.do(user, http.MethodPost, "/v1/boards", fmt.Sprintf(`{"title":%q}`, title)))
	var cols []string
	for i, c := range columns {
		b = ok[board](v.t, v.do(user, http.MethodPost, "/v1/boards/"+b.ID+"/columns", fmt.Sprintf(`{"title":%q}`, c)))
		cols = append(cols, b.Columns[i].ID)
	}
	return b.ID, cols
}

func (v env) newCard(user uuid.UUID, boardID, columnID, title string, body *string) card {
	v.t.Helper()
	payload := map[string]any{"columnID": columnID, "title": title}
	if body != nil {
		payload["body"] = *body
	}
	raw, _ := json.Marshal(payload)
	return ok[card](v.t, v.do(user, http.MethodPost, "/v1/boards/"+boardID+"/cards", string(raw)))
}

func (v env) setExtra(cardID, extra string) {
	v.exec(`UPDATE kanban_cards SET extra = $2::jsonb WHERE id = $1`, uuid.MustParse(cardID), extra)
}

func strp(s string) *string { return &s }

// MARK: - KanbanControllerTests

// GET /v1/boards auto-creates the default board (3 columns) on first call.
func TestGetBoardsAutoCreatesDefaultBoard(t *testing.T) {
	v := setup(t)
	boards := ok[[]summary](t, v.do(ann, http.MethodGet, "/v1/boards", ""))
	require.Len(t, boards, 1)
	require.Equal(t, "My Board", boards[0].Title)
	require.Equal(t, 3, boards[0].ColumnCount)
	require.Equal(t, int64(3), boards[0].Version, "each default column claims a version")

	b := ok[board](t, v.do(ann, http.MethodGet, "/v1/boards/"+boards[0].ID, ""))
	require.Equal(t, []string{"Todo", "Doing", "Done"}, []string{b.Columns[0].Title, b.Columns[1].Title, b.Columns[2].Title})
	require.Equal(t, []string{"i", "r", "v"}, []string{b.Columns[0].Rank, b.Columns[1].Rank, b.Columns[2].Rank})
	require.Nil(t, b.CreatedByUserID, "the service-created default board has no author")

	again := ok[[]summary](t, v.do(ann, http.MethodGet, "/v1/boards", ""))
	require.Len(t, again, 1, "a second list reuses the board")

	// Archived boards are hidden, and do not count as the default.
	listed := ok[[]summary](t, v.do(alice, http.MethodGet, "/v1/boards", ""))
	require.Len(t, listed, 1)
	require.Equal(t, aliceBrd, listed[0].ID)
	require.Equal(t, 2, listed[0].CardCount)
}

// MARK: - KanbanServiceTests

// createBoard → createColumn ×2 → createCard ×2 → snapshot: columns and cards
// sorted by rank; version stable between reads and bumped by mutations.
func TestSnapshotSortedByRank(t *testing.T) {
	v := setup(t)
	boardID, cols := v.newBoard(ann, "Sprint 1", "Backlog", "In Progress")
	created := ok[board](t, v.do(ann, http.MethodGet, "/v1/boards/"+boardID, ""))
	require.Less(t, created.Columns[0].Rank, created.Columns[1].Rank, "first column rank must precede second")

	a := v.newCard(ann, boardID, cols[0], "Task A", nil)
	b := v.newCard(ann, boardID, cols[0], "Task B", nil)
	require.Less(t, a.Rank, b.Rank, "first card rank must precede second")
	require.Equal(t, ids.Upper(ann), *a.CreatedByUserID)

	dto := ok[board](t, v.do(ann, http.MethodGet, "/v1/boards/"+boardID, ""))
	require.Equal(t, boardID, dto.ID)
	require.Len(t, dto.Columns, 2)
	require.Equal(t, cols[0], dto.Columns[0].ID)
	require.Equal(t, cols[1], dto.Columns[1].ID)
	require.Len(t, dto.Columns[0].Cards, 2)
	require.Equal(t, "Task A", dto.Columns[0].Cards[0].Title)
	require.Equal(t, "Task B", dto.Columns[0].Cards[1].Title)
	require.Empty(t, dto.Columns[1].Cards)

	type version struct{ Version int64 }
	v1 := ok[version](t, v.do(ann, http.MethodGet, "/v1/boards/"+boardID+"/version", "")).Version
	v2 := ok[version](t, v.do(ann, http.MethodGet, "/v1/boards/"+boardID+"/version", "")).Version
	require.Equal(t, v1, v2, "version must not change between reads")
	require.Equal(t, int64(4), v1, "2 columns + 2 cards")

	moved := ok[card](t, v.do(ann, http.MethodPost, "/v1/cards/"+b.ID+"/move", fmt.Sprintf(`{"toColumnID":%q}`, cols[1])))
	require.Equal(t, cols[1], moved.ColumnID)
	dto2 := ok[board](t, v.do(ann, http.MethodGet, "/v1/boards/"+boardID, ""))
	require.Len(t, dto2.Columns[0].Cards, 1)
	require.Len(t, dto2.Columns[1].Cards, 1)
	require.Greater(t, dto2.Version, v1, "version must increment after moveCard")
}

// A stale shared-board version cannot mutate content.
func TestStaleSharedBoardVersionCannotMutate(t *testing.T) {
	v := setup(t)
	h := func(ver string) []string {
		return []string{"X-Vault-ID", teamVault, "X-Board-Version", ver}
	}
	r := v.do(alice, http.MethodPatch, "/v1/boards/"+teamBoard, `{"title":"First writer"}`, h("7")...)
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	r = v.do(alice, http.MethodPatch, "/v1/boards/"+teamBoard, `{"title":"Stale writer"}`, h("7")...)
	require.Equal(t, http.StatusConflict, r.code)
	require.Equal(t, "board_version_conflict", r.errMessage())

	snap := ok[board](t, v.do(alice, http.MethodGet, "/v1/boards/"+teamBoard, "", "X-Vault-ID", teamVault))
	require.Equal(t, "First writer", snap.Title)
	require.Equal(t, int64(8), snap.Version)

	r = v.do(alice, http.MethodPatch, "/v1/boards/"+teamBoard, `{"title":"x"}`, "X-Vault-ID", teamVault)
	require.Equal(t, http.StatusConflict, r.code)
	require.Equal(t, "shared board mutation requires X-Board-Version", r.errMessage())

	// The successful write was recorded as vault activity with the actor's username.
	var n int
	var actor string
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT count(*), max(actor_name) FROM vault_activity_events WHERE action = 'board.updated' AND target_title = 'First writer'`).Scan(&n, &actor))
	require.Equal(t, 1, n)
	require.Equal(t, "alice", actor)
}

type promoteEnv struct {
	env
	boardID, columnID string
}

// seedBoardColumn mirrors the Swift helper: a "Jobs Board" with a "Jobs" column.
func seedBoardColumn(t *testing.T, opts ...options) promoteEnv {
	v := setup(t, opts...)
	b, cols := v.newBoard(ann, "Jobs Board", "Jobs")
	return promoteEnv{v, b, cols[0]}
}

func (p promoteEnv) promote(cardID, body string) resp {
	return p.do(ann, http.MethodPost, "/v1/cards/"+cardID+"/promote", body)
}

// Happy path: slug returned, SKILL.md written, skills_state enabled with the
// card as origin, and the card's extra.job back-filled.
func TestPromoteAuthorsJobAndBackFillsCard(t *testing.T) {
	p := seedBoardColumn(t)
	c := p.newCard(ann, p.boardID, p.columnID, "Weekly Digest", nil)
	p.setExtra(c.ID, `{"job":{"source":"vault","cron":"0 9 * * 1","domain":"life","prompt":"Summarize the week"}}`)

	s := ok[skill](t, p.promote(c.ID, ""))
	require.Equal(t, "job-weekly-digest", s.ID)
	require.Equal(t, "job-weekly-digest", s.Name)
	require.Equal(t, "vault", s.Source)
	require.Equal(t, "medium", s.Capability)
	require.Equal(t, "0 9 * * 1", *s.Schedule)
	require.Equal(t, "Summarize the week", s.DescriptionText)
	require.True(t, s.Enabled)
	require.Equal(t, "card", s.Origin.Kind)
	require.Equal(t, c.ID, s.Origin.ID)

	md, err := os.ReadFile(filepath.Join(p.vault, "tenants", ids.Upper(ann), "skills", s.ID, "SKILL.md"))
	require.NoError(t, err)
	require.Contains(t, string(md), "name: job-weekly-digest\n")
	require.Contains(t, string(md), "  capability: medium\n  schedule: \"0 9 * * 1\"\n  on_event: []\n")
	require.Contains(t, string(md), "Domain: life.\n")

	var enabled bool
	var originKind string
	var originID uuid.UUID
	require.NoError(t, p.pool.QueryRow(context.Background(), `
		SELECT enabled, origin_kind, origin_id FROM skills_state
		WHERE tenant_id = $1 AND source = 'vault' AND name = $2`, ann, s.ID).Scan(&enabled, &originKind, &originID))
	require.True(t, enabled)
	require.Equal(t, "card", originKind)
	require.Equal(t, c.ID, ids.Upper(originID))

	b := ok[board](t, p.do(ann, http.MethodGet, "/v1/boards/"+p.boardID, ""))
	jc := b.Columns[0].Cards[0].JobConfig
	require.NotNil(t, jc)
	require.Equal(t, s.ID, *jc.JobSlug)
	require.NotNil(t, jc.PromotedAt)

	// promotedAt is stored as Foundation writes a Date: seconds since 2001.
	var promotedAt float64
	require.NoError(t, p.pool.QueryRow(context.Background(),
		`SELECT (extra->'job'->>'promotedAt')::float8 FROM kanban_cards WHERE id = $1`, uuid.MustParse(c.ID)).Scan(&promotedAt))
	require.InDelta(t, 815e6, promotedAt, 400e6, "seconds since 2001-01-01, not since 1970")
}

// Promotion runs in one transaction; authoring must not take a second
// pooled connection. With one connection that would deadlock.
func TestPromoteCompletesWithSinglePooledConnection(t *testing.T) {
	p := seedBoardColumn(t, options{maxConns: 1})
	c := p.newCard(ann, p.boardID, p.columnID, "Single Connection", nil)
	p.setExtra(c.ID, `{"job":{"source":"vault","cron":"0 9 * * 1","prompt":"Summarize"}}`)
	ctx, cancel := context.WithTimeout(context.Background(), 20e9)
	defer cancel()
	done := make(chan resp, 1)
	go func() { done <- p.promote(c.ID, "") }()
	select {
	case r := <-done:
		require.Equal(t, http.StatusOK, r.code, string(r.body))
	case <-ctx.Done():
		t.Fatal("promote deadlocked on a single connection")
	}
}

// Re-promoting returns the existing job without re-authoring.
func TestPromoteIsIdempotent(t *testing.T) {
	p := seedBoardColumn(t)
	c := p.newCard(ann, p.boardID, p.columnID, "Daily Brief", nil)
	p.setExtra(c.ID, `{"job":{"source":"vault","cron":"0 8 * * *","prompt":"Brief me"}}`)
	first := ok[skill](t, p.promote(c.ID, ""))
	type version struct{ Version int64 }
	before := ok[version](t, p.do(ann, http.MethodGet, "/v1/boards/"+p.boardID+"/version", "")).Version
	p.exec(`DELETE FROM skills_state`)

	second := ok[skill](t, p.promote(c.ID, `{"cron":"*/5 * * * *","prompt":"other"}`))
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, "*/5 * * * *", *second.Schedule, "Swift answers with the merged request config")
	require.Equal(t, "other", second.DescriptionText)
	after := ok[version](t, p.do(ann, http.MethodGet, "/v1/boards/"+p.boardID+"/version", "")).Version
	require.Equal(t, before, after, "no claim on re-promote")
	var n int
	require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT count(*) FROM skills_state`).Scan(&n))
	require.Zero(t, n, "no re-author")
	b := ok[board](t, p.do(ann, http.MethodGet, "/v1/boards/"+p.boardID, ""))
	require.Equal(t, "0 8 * * *", *b.Columns[0].Cards[0].JobConfig.Cron, "request fields are not persisted on re-promote")
}

// prompt is optional; the card body is the fallback spec.
func TestPromoteFallsBackToCardBody(t *testing.T) {
	p := seedBoardColumn(t)
	c := p.newCard(ann, p.boardID, p.columnID, "Body Job", strp("Watch the markets"))
	p.setExtra(c.ID, `{"job":{"source":"vault","cron":"*/30 * * * *"}}`)
	s := ok[skill](t, p.promote(c.ID, ""))
	require.Equal(t, "Watch the markets", s.DescriptionText)
}

// Single-call promote: inline request config is persisted and surfaced.
func TestPromoteAppliesInlineRequestConfig(t *testing.T) {
	p := seedBoardColumn(t)
	c := p.newCard(ann, p.boardID, p.columnID, "Inline Job", nil)
	s := ok[skill](t, p.promote(c.ID, `{"cron":"0 7 * * *","domain":"tech","prompt":"Scan tech news"}`))
	require.Equal(t, "Scan tech news", s.DescriptionText)

	b := ok[board](t, p.do(ann, http.MethodGet, "/v1/boards/"+p.boardID, ""))
	jc := b.Columns[0].Cards[0].JobConfig
	require.Equal(t, "0 7 * * *", *jc.Cron)
	require.Equal(t, "tech", *jc.Domain)
	require.Equal(t, s.ID, *jc.JobSlug)
	require.Equal(t, "vault", jc.Source, "CardJobConfig.source defaults to vault")
}

// A card with no job config cannot be promoted.
func TestPromoteRejectsCardWithoutJobConfig(t *testing.T) {
	p := seedBoardColumn(t)
	c := p.newCard(ann, p.boardID, p.columnID, "Plain Card", nil)
	r := p.promote(c.ID, "")
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "card_job_requires_cron_or_run_at", r.errMessage())

	r = p.promote(c.ID, `{"cron":"0 8 * * *"}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "card_job_requires_prompt_or_body", r.errMessage())
}

// An invalid cron is rejected and nothing is authored.
func TestPromoteRejectsInvalidCron(t *testing.T) {
	p := seedBoardColumn(t)
	c := p.newCard(ann, p.boardID, p.columnID, "Bad Cron", nil)
	p.setExtra(c.ID, `{"job":{"source":"vault","cron":"not a cron","prompt":"x"}}`)
	r := p.promote(c.ID, "")
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "invalid cron schedule", r.errMessage())
	var n int
	require.NoError(t, p.pool.QueryRow(context.Background(), `SELECT count(*) FROM skills_state`).Scan(&n))
	require.Zero(t, n)
	b := ok[board](t, p.do(ann, http.MethodGet, "/v1/boards/"+p.boardID, ""))
	require.Nil(t, b.Columns[0].Cards[0].JobConfig.JobSlug, "rolled back")
	require.Equal(t, int64(2), b.Version, "the claim rolled back too")
}

// One-shot promote: run_at on skills_state, no schedule line in SKILL.md.
func TestPromoteAuthorsOneShotJob(t *testing.T) {
	p := seedBoardColumn(t)
	c := p.newCard(ann, p.boardID, p.columnID, "One Shot", nil)
	// 2_000_000_000 since 1970 = 1_021_692_800 since 2001.
	p.setExtra(c.ID, `{"job":{"source":"vault","runAt":1021692800,"prompt":"Run me once"}}`)
	s := ok[skill](t, p.promote(c.ID, ""))
	require.Nil(t, s.Schedule)

	md, err := os.ReadFile(filepath.Join(p.vault, "tenants", ids.Upper(ann), "skills", s.ID, "SKILL.md"))
	require.NoError(t, err)
	require.NotContains(t, string(md), "schedule:")

	var enabled bool
	var runAt int64
	require.NoError(t, p.pool.QueryRow(context.Background(), `
		SELECT enabled, extract(epoch FROM run_at)::bigint FROM skills_state
		WHERE tenant_id = $1 AND source = 'vault' AND name = $2`, ann, s.ID).Scan(&enabled, &runAt))
	require.True(t, enabled)
	require.Equal(t, int64(2_000_000_000), runAt)

	b := ok[board](t, p.do(ann, http.MethodGet, "/v1/boards/"+p.boardID, ""))
	jc := b.Columns[0].Cards[0].JobConfig
	require.Equal(t, "2033-05-18T03:33:20Z", *jc.RunAt)
	require.Equal(t, s.ID, *jc.JobSlug)
}

// Neither cron nor runAt, or both, cannot be promoted.
func TestPromoteRejectsAmbiguousSchedule(t *testing.T) {
	p := seedBoardColumn(t)
	c1 := p.newCard(ann, p.boardID, p.columnID, "Neither", nil)
	p.setExtra(c1.ID, `{"job":{"source":"vault","prompt":"x"}}`)
	require.Equal(t, "card_job_requires_cron_or_run_at", p.promote(c1.ID, "").errMessage())

	c2 := p.newCard(ann, p.boardID, p.columnID, "Both", nil)
	p.setExtra(c2.ID, `{"job":{"source":"vault","cron":"0 9 * * *","runAt":815216400,"prompt":"x"}}`)
	r := p.promote(c2.ID, "")
	require.Equal(t, http.StatusBadRequest, r.code)
	require.Equal(t, "card_job_requires_cron_or_run_at", r.errMessage())
}

// MARK: - Go-side checks of Swift behaviour the parity scenarios also cover

func TestErrorOrderAndMessages(t *testing.T) {
	v := setup(t)
	cases := []struct {
		user    uuid.UUID
		method  string
		path    string
		body    string
		headers []string
		code    int
		msg     string
	}{
		{uuid.Nil, http.MethodGet, "/v1/boards", "", []string{"X-Vault-ID", "nope"}, 400, "invalid X-Vault-ID"},
		{uuid.Nil, http.MethodGet, "/v1/boards", "", nil, 401, "Authenticated identity is unavailable"},
		{bob, http.MethodPatch, "/v1/boards/nope/columns/nope", `{"title":"x"}`, nil, 400, "invalid_columnID"},
		{bob, http.MethodPatch, "/v1/boards/nope", `[]`, nil, 400, "Type mismatch for `` key, expected `Dictionary<String, Any>` type."},
		{
			bob, http.MethodPost, "/v1/boards/CA4BB000-0000-4000-8000-0000000000B1/cards", `{"body":5,"columnID":"nope"}`, nil, 400,
			"The given data was not valid input: Attempted to decode UUID from invalid UUID string.",
		},
		{
			bob, http.MethodPost, "/v1/boards/CA4BB000-0000-4000-8000-0000000000B1/cards", `{"columnID":"CA4BC000-0000-4000-8000-0000000000B1","title":"x","priority":"asap"}`, nil, 400,
			"The given data was not valid input: Cannot initialize CardPriority from invalid String value asap",
		},
		{
			bob, http.MethodPost, "/v1/boards/CA4BB000-0000-4000-8000-0000000000B1/cards", `{"columnID":"CA4BC000-0000-4000-8000-0000000000B1","title":"x","priority":3}`, nil, 400,
			"Type mismatch for `priority` key, expected `String` type.",
		},
		{bob, http.MethodPost, "/v1/boards/CA4BB000-0000-4000-8000-0000000000B1/cards", `{"columnID":"CA4BC000-0000-4000-8000-0000000000A1","title":"x"}`, nil, 404, "column_not_found"},
		{bob, http.MethodPost, "/v1/boards/CA4BB000-0000-4000-8000-0000000000B1/columns/reorder", `{"columnID":"CA4BC000-0000-4000-8000-0000000000B1","beforeID":"00000000-0000-4000-8000-000000000000"}`, nil, 422, "column_neighbor_not_found"},
		{bob, http.MethodPost, "/v1/cards/CA4BD000-0000-4000-8000-0000000000B1/move", `{"toColumnID":"CA4BC000-0000-4000-8000-0000000000B1","afterID":"00000000-0000-4000-8000-000000000000"}`, nil, 422, "card_neighbor_not_found"},
		{bob, http.MethodGet, "/v1/boards/" + aliceBrd, "", nil, 404, "board_not_found"},
		{bob, http.MethodPatch, "/v1/cards/CA4BD000-0000-4000-8000-0000000000A1", `{"title":"x"}`, nil, 404, "card_not_found"},
		{bob, http.MethodDelete, "/v1/cards/not-a-uuid", "", nil, 400, "invalid_cardID"},
		{bob, http.MethodGet, "/v1/boards", "", []string{"X-Vault-ID", "A11CE000-0000-4000-8000-000000000001"}, 403, "vault access denied"},
		{bob, http.MethodPatch, "/v1/boards/" + teamBoard, `{"title":"x"}`, []string{"X-Vault-ID", teamVault, "X-Board-Version", "7"}, 403, "vault write permission required"},
		{bob, http.MethodPost, "/v1/cards/CA4BD000-0000-4000-8000-0000000000F2/promote", "", []string{"X-Vault-ID", teamVault}, 403, "vault AI access required"},
		{
			alice, http.MethodPost, "/v1/cards/CA4BD000-0000-4000-8000-0000000000F2/promote", "",
			[]string{"X-Vault-ID", "CA4BF000-0000-4000-8000-000000000002"},
			402,
			"team owner subscription does not include AI access",
		},
		{alice, http.MethodGet, "/v1/boards", "", []string{"X-Vault-ID", "CA4BF000-0000-4000-8000-000000000003"}, 404, "vault not found"},
	}
	for _, c := range cases {
		r := v.do(c.user, c.method, c.path, c.body, c.headers...)
		require.Equal(t, c.code, r.code, "%s %s: %s", c.method, c.path, r.body)
		require.Equal(t, c.msg, r.errMessage(), "%s %s", c.method, c.path)
		require.Equal(t, "application/json; charset=utf-8", r.ct)
	}
	type version struct{ Version int64 }
	require.Equal(t, int64(1), ok[version](t, v.do(bob, http.MethodGet, "/v1/boards/CA4BB000-0000-4000-8000-0000000000B1/version", "")).Version,
		"failed mutations rolled their version claim back")
}

// A promoted team card ignores X-Board-Version (no claim); a new team
// promotion hits skills_state's users FK and fails as Swift's does.
func TestTeamPromote(t *testing.T) {
	v := setup(t)
	h := []string{"X-Vault-ID", teamVault, "X-Board-Version", "1"}
	s := ok[skill](t, v.do(alice, http.MethodPost, "/v1/cards/CA4BD000-0000-4000-8000-0000000000F2/promote", "", h...))
	require.Equal(t, "job-seeded", s.ID)
	require.Equal(t, "Seeded job", s.DescriptionText)

	r := v.do(alice, http.MethodPost, "/v1/cards/CA4BD000-0000-4000-8000-0000000000F1/promote", `{"cron":"0 9 * * *"}`,
		"X-Vault-ID", teamVault, "X-Board-Version", "7")
	require.Equal(t, http.StatusInternalServerError, r.code)
	require.Empty(t, r.body)
	snap := ok[board](t, v.do(alice, http.MethodGet, "/v1/boards/"+teamBoard, "", "X-Vault-ID", teamVault))
	require.Equal(t, int64(7), snap.Version)

	r = v.do(ben, http.MethodPost, "/v1/boards/"+teamBoard+"/cards", fmt.Sprintf(`{"columnID":%q,"title":"Ben card"}`, teamCol),
		"X-Vault-ID", teamVault, "X-Board-Version", "7")
	require.Equal(t, http.StatusOK, r.code, string(r.body))
	var actions []string
	rows, err := v.pool.Query(context.Background(), `SELECT action || ':' || actor_name FROM vault_activity_events ORDER BY created_at`)
	require.NoError(t, err)
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		actions = append(actions, a)
	}
	require.Equal(t, []string{"card.promoted:alice", "card.created:kanban_ben"}, actions)
}

func TestRateLimitSettingsByUser(t *testing.T) {
	v := setup(t)
	for i := range 30 {
		require.Equal(t, http.StatusOK, v.do(ann, http.MethodGet, "/v1/boards", "").code, "request %d", i+1)
	}
	r := v.do(ann, http.MethodGet, "/v1/boards", "")
	require.Equal(t, http.StatusTooManyRequests, r.code)
	require.Equal(t, "rate limit exceeded", r.errMessage())
	require.NotEmpty(t, r.hdr.Get("Retry-After"))
	require.Equal(t, http.StatusOK, v.do(ben, http.MethodGet, "/v1/boards", "").code, "per-user buckets")
	// Cards share the bucket with boards.
	r = v.do(ann, http.MethodDelete, "/v1/cards/not-a-uuid", "")
	require.Equal(t, http.StatusTooManyRequests, r.code)
}

func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t)
	boardID, cols := v.newBoard(ann, "Spec", "A", "B")
	c := v.newCard(ann, boardID, cols[0], "Card", strp("body"))
	c2 := v.newCard(ann, boardID, cols[0], "Other", nil)
	for _, k := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/boards", ""},
		{http.MethodPost, "/v1/boards", `{"title":"Spec2"}`},
		{http.MethodGet, "/v1/boards/" + boardID, ""},
		{http.MethodGet, "/v1/boards/" + boardID + "/version", ""},
		{http.MethodPatch, "/v1/boards/" + boardID, `{"title":"Spec3"}`},
		{http.MethodPost, "/v1/boards/" + boardID + "/columns", `{"title":"C"}`},
		{http.MethodPatch, "/v1/boards/" + boardID + "/columns/" + cols[0], `{"title":"A2"}`},
		{http.MethodPost, "/v1/boards/" + boardID + "/columns/reorder", fmt.Sprintf(`{"columnID":%q,"afterID":%q}`, cols[1], cols[0])},
		{http.MethodPost, "/v1/boards/" + boardID + "/cards", fmt.Sprintf(`{"columnID":%q,"title":"x","priority":"high","dueAt":"2026-11-01T09:00:00Z"}`, cols[0])},
		{http.MethodPatch, "/v1/cards/" + c.ID, `{"title":"y"}`},
		{http.MethodPost, "/v1/cards/" + c.ID + "/move", fmt.Sprintf(`{"toColumnID":%q}`, cols[1])},
		{http.MethodPost, "/v1/cards/" + c.ID + "/promote", `{"cron":"0 9 * * *"}`},
		{http.MethodDelete, "/v1/cards/" + c2.ID, ""},
		{http.MethodDelete, "/v1/boards/" + boardID + "/columns/" + cols[1], ""},
		{http.MethodDelete, "/v1/boards/" + boardID, ""},
	} {
		r := v.do(ann, k.method, k.path, k.body)
		require.Less(t, r.code, 300, "%s %s: %s", k.method, k.path, r.body)
		req := httptest.NewRequest(k.method, k.path, strings.NewReader(k.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, k.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", k.method, k.path, r.body)
	}
}
