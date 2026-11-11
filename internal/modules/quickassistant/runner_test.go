package quickassistant

import (
	"context"
	"encoding/json"
	"lumina/internal/chunkindex"
	"lumina/internal/dbtest"
	"lumina/internal/devicetools"
	"lumina/internal/embedding"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/apple"
	"lumina/internal/modules/mcp"
	"lumina/internal/modules/reminders"
	"lumina/internal/soul"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// rig is one database with the services a runner needs. Ports the setup of
// QuickAssistantActionsTests and the E2E authorisation cases.
type rig struct {
	t     *testing.T
	pool  *pgxpool.Pool
	paths vaultpaths.Paths
	deps  Deps
}

func newRig(t *testing.T) rig {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	paths := vaultpaths.Paths{Root: t.TempDir()}
	return rig{t: t, pool: pool, paths: paths, deps: Deps{
		Pool:      pool,
		Access:    vaultaccess.New(pool),
		MCP:       mcpService(t, pool, paths, embedding.Deterministic{}),
		Store:     NewStore(pool),
		Reminders: reminders.NewStore(pool),
		Consent:   apple.NewConsentReader(pool),
	}}
}

// mcpService is the /v1/mcp tool runner the vault tools go through. Its own
// personal tools have no broker: the runner must never reach them.
func mcpService(t *testing.T, pool *pgxpool.Pool, paths vaultpaths.Paths, embeddings embedding.Service) *mcp.Service {
	memories := memorystore.New(pool, nil)
	chunks := chunkindex.NewChunkRepository(pool)
	return mcp.NewService(mcp.ServiceDeps{
		Pool: pool, Paths: paths,
		Links:      chunkindex.NewLinkRepository(pool),
		Hybrid:     chunkindex.NewHybrid(memories, chunks),
		Embeddings: embeddings,
		Indexer:    chunkindex.NewIndexer(chunks, embeddings, chunkindex.NewLinkRepository(pool)),
		Memories:   memories,
		Personal:   devicetools.New(pool, nil, nil),
		Soul:       soul.New(paths, t.TempDir()),
	})
}

func (g rig) exec(sql string, args ...any) {
	g.t.Helper()
	_, err := g.pool.Exec(context.Background(), sql, args...)
	require.NoError(g.t, err)
}

// user makes an account with its personal vault, whose id is the user id.
func (g rig) user() uuid.UUID {
	g.t.Helper()
	id := uuid.New()
	short := strings.ToLower(id.String()[:8])
	g.exec(`INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'pro', now(), now())`, id, "qa-"+short+"@test.luminavault", "qa-"+short)
	g.exec(`INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'mine', now(), now())`, id)
	return id
}

// sharedVault is a team vault owner owns, with member in role.
func (g rig) sharedVault(owner, member uuid.UUID, role string) uuid.UUID {
	g.t.Helper()
	team, vault := uuid.New(), uuid.New()
	g.exec(`INSERT INTO teams (id, name, owner_user_id, billing_sponsor_user_id, created_at, updated_at)
		VALUES ($1, 'qa-team', $2, $2, now(), now())`, team, owner)
	g.exec(`INSERT INTO vaults (id, team_id, name, created_at, updated_at) VALUES ($1, $2, 'shared', now(), now())`, vault, team)
	g.exec(`INSERT INTO vault_memberships (id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, true, $5, now(), now())`, uuid.New(), vault, member, role, owner)
	return vault
}

func (g rig) runner(actor, vault uuid.UUID) *Runner {
	return NewRunner(g.deps, actor, vault, "qa-user", nil, nil, nil)
}

func (g rig) count(sql string, args ...any) int {
	g.t.Helper()
	var n int
	require.NoError(g.t, g.pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func (g rig) memories(tenant uuid.UUID) int {
	return g.count(`SELECT count(*) FROM memories WHERE tenant_id = $1`, tenant)
}

// memory inserts a row directly: a team vault's provenance row cannot be
// written (memory_contributions.tenant_id references users).
func (g rig) memory(tenant uuid.UUID, content string, kind *string, createdAt time.Time) uuid.UUID {
	g.t.Helper()
	id := uuid.New()
	vec, err := embedding.Deterministic{}.Embed(context.Background(), content, tenant)
	require.NoError(g.t, err)
	g.exec(`INSERT INTO memories (id, tenant_id, content, kind, created_at, embedding) VALUES ($1, $2, $3, $4, $5, $6::vector)`,
		id, tenant, content, kind, createdAt, memorystore.VectorArg(vec))
	return id
}

func (g rig) file(tenant uuid.UUID, path string, updated time.Time) {
	g.t.Helper()
	g.exec(`INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		VALUES ($1, $2, $3, 'text/markdown', 1, $4, $5, $5)`, uuid.New(), tenant, path, uuid.NewString(), updated)
}

func decodeObject(t *testing.T, content string) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(content), &out), content)
	return out
}

func kindPtr(s string) *string { return &s }

// MARK: - Vault tools

func TestSearchReadsTheTurnVault(t *testing.T) {
	g := newRig(t)
	owner, viewer := g.user(), g.user()
	vault := g.sharedVault(owner, viewer, "viewer")
	g.memory(vault, "zebra offsite plan for the team", nil, time.Now())
	g.memory(viewer, "zebra private diary entry", nil, time.Now())

	result := g.runner(viewer, vault).Run(context.Background(), "search", map[string]any{"query": "zebra"})

	require.Nil(t, result.Action)
	require.Contains(t, result.ModelContent, "offsite plan")
	require.NotContains(t, result.ModelContent, "private diary")
	out := decodeObject(t, result.ModelContent)
	require.Equal(t, "zebra", out["query"])
	require.EqualValues(t, 1, out["total"])
}

func TestRecentReadsTheSharedVaultNotTheViewersOwn(t *testing.T) {
	g := newRig(t)
	owner, viewer := g.user(), g.user()
	vault := g.sharedVault(owner, viewer, "viewer")
	g.file(vault, "team/shared-plan.md", time.Now())
	g.file(viewer, "me/private-diary.md", time.Now())

	result := g.runner(viewer, vault).Run(context.Background(), "recent", map[string]any{})

	require.Contains(t, result.ModelContent, "shared-plan")
	require.NotContains(t, result.ModelContent, "private-diary")
}

func TestBrowseListsTheTurnVaultInPathOrder(t *testing.T) {
	g := newRig(t)
	owner, viewer := g.user(), g.user()
	vault := g.sharedVault(owner, viewer, "viewer")
	for _, p := range []string{"notes/b.md", "notes/a.md", "other/c.md"} {
		g.file(vault, p, time.Now())
	}
	g.file(viewer, "mine/d.md", time.Now())

	// pathPrefix is not exercised: mcp's range compares in the database's
	// collation, as Swift's does, and under en_US.utf8 it matches nothing
	// (ruling 75, report Task 8R).
	result := g.runner(viewer, vault).Run(context.Background(), "browse", map[string]any{"limit": 2.0})

	out := decodeObject(t, result.ModelContent)
	require.EqualValues(t, 2, out["count"])
	docs := out["documents"].([]any)
	require.Equal(t, "notes/a.md", docs[0].(map[string]any)["path"])
	require.Equal(t, "notes/b.md", docs[1].(map[string]any)["path"])
	require.NotContains(t, result.ModelContent, "mine/d.md")
}

func TestReadReturnsTheFileAndReportsTruncation(t *testing.T) {
	g := newRig(t)
	me := g.user()
	g.file(me, "notes/a.md", time.Now())
	require.NoError(t, os.MkdirAll(filepath.Join(g.paths.Raw(me), "notes"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(g.paths.Raw(me), "notes", "a.md"), []byte("héllo world"), 0o600))
	r := g.runner(me, me)

	full := decodeObject(t, r.Run(context.Background(), "read", map[string]any{"path": "notes/a.md"}).ModelContent)
	require.Equal(t, "héllo world", full["content"])
	require.Equal(t, false, full["truncated"])
	require.EqualValues(t, 11, full["totalChars"])

	cut := decodeObject(t, r.Run(context.Background(), "read", map[string]any{"path": "./notes/a.md", "maxChars": 2.9}).ModelContent)
	require.Equal(t, "hé", cut["content"])
	require.Equal(t, true, cut["truncated"])

	missing := decodeObject(t, r.Run(context.Background(), "read", map[string]any{"path": "nope.md"}).ModelContent)
	require.Equal(t, true, missing["isError"])

	tooMany := r.Run(context.Background(), "read", map[string]any{"path": "notes/a.md", "maxChars": 100001.0})
	require.Equal(t, "The tool did not run: maxChars must be between 1 and 100000; got 100001", tooMany.ModelContent)
}

func TestUnknownArgumentIsRefused(t *testing.T) {
	g := newRig(t)
	me := g.user()

	result := g.runner(me, me).Run(context.Background(), "search", map[string]any{"query": "x", "zzz": 1.0, "bbb": 2.0})

	require.Nil(t, result.Action)
	require.Equal(t, "The tool did not run: it has no argument named 'bbb'.", result.ModelContent)
}

func TestSearchWithEmptyQueryIsRefused(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	for _, args := range []map[string]any{{"query": "   "}, {}, {"query": 3.0}} {
		result := r.Run(context.Background(), "search", args)
		require.Equal(t, "The tool did not run: query is required and must be a non-empty string", result.ModelContent, "%v", args)
	}
	long := r.Run(context.Background(), "search", map[string]any{"query": strings.Repeat("q", 1001)})
	require.Equal(t, "The tool did not run: query must be at most 1000 characters", long.ModelContent)
	badLimit := r.Run(context.Background(), "search", map[string]any{"query": "q", "limit": 51.0})
	require.Equal(t, "The tool did not run: limit must be between 1 and 50; got 51", badLimit.ModelContent)
	badSpace := r.Run(context.Background(), "search", map[string]any{"query": "q", "space": "work"})
	require.Equal(t, "The tool did not run: unknown space 'work'. Known spaces: none, or inbox", badSpace.ModelContent)
}

func TestMemoryContextIncludesStrippedSoulReadOnly(t *testing.T) {
	g := newRig(t)
	me := g.user()
	written := soul.Inject("# SOUL.md\n\nI like tea.\n")
	require.NoError(t, os.MkdirAll(g.paths.Raw(me), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(g.paths.Raw(me), soul.FileName), []byte(written), 0o600))
	now := time.Now()
	g.memory(me, "older decision", kindPtr("decision"), now.Add(-2*time.Hour))
	g.memory(me, "newer preference", kindPtr("preference"), now.Add(-time.Hour))
	g.memory(me, "a fact", kindPtr("fact"), now)
	g.memory(me, "no kind", nil, now)

	result := g.runner(me, me).Run(context.Background(), "memory_context", map[string]any{})

	out := decodeObject(t, result.ModelContent)
	require.Equal(t, false, out["soulEditable"])
	require.Equal(t, swiftstr.TrimWhitespacesAndNewlines(soul.Strip(written)), out["soul"])
	require.NotContains(t, out["soul"], soul.Core)
	items := out["memories"].([]any)
	require.Len(t, items, 2)
	require.Equal(t, "newer preference", items[0].(map[string]any)["text"])
	require.Equal(t, "preference", items[0].(map[string]any)["kind"])
	require.Equal(t, "older decision", items[1].(map[string]any)["text"])
}

// MARK: - memory_save

func TestMemorySaveEmitsDoneChip(t *testing.T) {
	g := newRig(t)
	me := g.user()

	result := g.runner(me, me).Run(context.Background(), "memory_save", map[string]any{"content": "  My dentist is Dr Okafor\n"})

	require.NotNil(t, result.Action)
	require.Equal(t, KindDone, result.Action.Kind)
	require.Equal(t, "Saved to memory: My dentist is Dr Okafor", result.Action.Summary)
	out := decodeObject(t, result.ModelContent)
	require.Equal(t, false, out["duplicate"])
	require.Equal(t, 1, g.memories(me))
	require.Equal(t, 1, g.count(`SELECT count(*) FROM memories WHERE tenant_id = $1 AND created_by_user_id = $1
		AND content = 'My dentist is Dr Okafor'`, me))
}

func TestReadOnlyMemberRefusedMemorySave(t *testing.T) {
	g := newRig(t)
	owner, viewer := g.user(), g.user()
	vault := g.sharedVault(owner, viewer, "viewer")

	result := g.runner(viewer, vault).Run(context.Background(), "memory_save", map[string]any{"content": "viewer note"})

	require.Nil(t, result.Action)
	require.Equal(t, "The user doesn't have access to that in this vault.", result.ModelContent)
	require.Zero(t, g.memories(vault))
	require.Zero(t, g.memories(viewer))
}

func TestSharedVaultSaveAfterAPersonalToolIsRefused(t *testing.T) {
	g := newRig(t)
	owner, editor := g.user(), g.user()
	vault := g.sharedVault(owner, editor, "editor")
	r := g.runner(editor, vault)

	r.Run(context.Background(), "health_query", map[string]any{})
	result := r.Run(context.Background(), "memory_save", map[string]any{"content": "my resting heart rate is 48"})

	require.Nil(t, result.Action)
	require.Equal(t, "The tool did not run: this turn used the user's private data, so nothing can be saved to a shared vault. Tell the user.", result.ModelContent)
	require.Zero(t, g.memories(vault))
	require.Zero(t, g.memories(editor))
}

func TestSharedVaultSaveIsNotAvailableYet(t *testing.T) {
	g := newRig(t)
	owner, editor := g.user(), g.user()
	vault := g.sharedVault(owner, editor, "editor")

	result := g.runner(editor, vault).Run(context.Background(), "memory_save", map[string]any{"content": "team offsite is in May"})

	require.Nil(t, result.Action)
	require.Equal(t, "The tool did not run: saving to a shared vault is not available yet. Tell the user.", result.ModelContent)
	require.Zero(t, g.memories(vault))
	require.Zero(t, g.memories(editor))
}

func TestSaveInTheUsersOwnVaultAfterAPersonalToolIsAllowed(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	r.Run(context.Background(), "health_query", map[string]any{})
	result := r.Run(context.Background(), "memory_save", map[string]any{"content": "I run on Mondays"})

	require.NotNil(t, result.Action)
	require.Equal(t, 1, g.memories(me))
}

func TestDuplicateSaveMakesNoChipAndNoWrite(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	first := r.Run(context.Background(), "memory_save", map[string]any{"content": "fact one"})
	again := r.Run(context.Background(), "memory_save", map[string]any{"content": "fact one"})

	require.NotNil(t, first.Action)
	require.Nil(t, again.Action)
	out := decodeObject(t, again.ModelContent)
	require.Equal(t, true, out["duplicate"])
	require.Equal(t, "An identical memory already exists; nothing was added.", out["message"])
	require.Equal(t, decodeObject(t, first.ModelContent)["memoryID"], out["memoryID"])
	require.Equal(t, 1, g.memories(me))
	require.Equal(t, 1, r.turn.Writes(), "a duplicate is not a write")
}

func TestMemorySaveValidatesLikeMCP(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	cases := map[string]map[string]any{
		"The tool did not run: content is required and must be a non-empty string":   {"content": "  "},
		"The tool did not run: content must be at most 4000 characters":              {"content": strings.Repeat("c", 4001)},
		"The tool did not run: kind must be one of preference, decision, fact, note": {"content": "x", "kind": "wish"},
		"The tool did not run: tags must be an array of strings":                     {"content": "x", "tags": "a"},
		"The tool did not run: tags must be non-empty strings":                       {"content": "x", "tags": []any{" "}},
	}
	for want, args := range cases {
		require.Equal(t, want, r.Run(context.Background(), "memory_save", args).ModelContent)
	}
	require.Zero(t, g.memories(me))
}

// MARK: - open_screen

func TestOpenScreenEmitsFrame(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	brain := r.Run(context.Background(), "open_screen", map[string]any{"screen": "brain"})
	require.NotNil(t, brain.Action)
	require.Equal(t, KindOpenScreen, brain.Action.Kind)
	require.Equal(t, ScreenBrain, *brain.Action.Screen)
	require.Nil(t, brain.Action.MemoryID)
	require.Equal(t, "Opening Brain", brain.Action.Summary)
	require.Equal(t, "The app is opening Brain for the user.", brain.ModelContent)

	id := uuid.New()
	memory := r.Run(context.Background(), "open_screen", map[string]any{"screen": "memory", "memoryID": id.String()})
	require.Equal(t, ScreenMemory, *memory.Action.Screen)
	require.Equal(t, id, memory.Action.MemoryID.UUID)
	require.Equal(t, "Opening the memory", memory.Action.Summary)

	// A memoryID on another screen is checked but not sent.
	settings := r.Run(context.Background(), "open_screen", map[string]any{"screen": "dataAccess", "memoryID": id.String()})
	require.Nil(t, settings.Action.MemoryID)
	require.Equal(t, "Opening Data Access", settings.Action.Summary)
}

func TestOpenMemoryRequiresID(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	for want, args := range map[string]map[string]any{
		"The tool did not run: memoryID is required to open a memory.":                     {"screen": "memory"},
		"The tool did not run: memoryID must be a memory UUID from search or memory_save.": {"screen": "memory", "memoryID": "not-a-uuid"},
	} {
		result := r.Run(context.Background(), "open_screen", args)
		require.Nil(t, result.Action, "%v", args)
		require.Equal(t, want, result.ModelContent)
	}
	bad := r.Run(context.Background(), "open_screen", map[string]any{"screen": "brain", "memoryID": "not-a-uuid"})
	require.Nil(t, bad.Action)
	require.True(t, strings.HasPrefix(bad.ModelContent, "The tool did not run"))
}

func TestOpenScreenRejectsUnknown(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	for _, args := range []map[string]any{{"screen": "unknown"}, {"screen": "nowhere"}, {}, {"screen": 3.0}} {
		result := r.Run(context.Background(), "open_screen", args)
		require.Nil(t, result.Action, "%v", args)
		require.Equal(t, "The tool did not run: screen must be one of the listed screens.", result.ModelContent)
	}
}

// MARK: - Policy, caps and helpers

func TestRefusedNameIsNotAvailable(t *testing.T) {
	g := newRig(t)
	me := g.user()
	r := g.runner(me, me)

	for _, name := range []string{"memory_update", "index", "nonsense"} {
		result := r.Run(context.Background(), name, map[string]any{})
		require.Nil(t, result.Action)
		require.Equal(t, "This is not available yet. Tell the user you can't do it right now.", result.ModelContent)
	}
	// calendar_create is not offered without a device to write to.
	calendar := r.Run(context.Background(), "calendar_create", map[string]any{"title": "x", "start": "2030-01-01T09:00:00Z"})
	require.Equal(t, "This is not available yet. Tell the user you can't do it right now.", calendar.ModelContent)
	require.Zero(t, g.count(`SELECT count(*) FROM assistant_pending_actions`))
}

func TestProposalsStopAtThreeCardsPerTurn(t *testing.T) {
	g := newRig(t)
	me := g.user()
	conversation := uuid.New()
	g.exec(`INSERT INTO conversations (id, tenant_id, title, created_at, updated_at) VALUES ($1, $2, 't', now(), now())`, conversation, me)
	r := NewRunner(g.deps, me, me, "qa-user", nil, &conversation, nil)

	for i := range 3 {
		result, err := r.propose(context.Background(), "memory_forget", me, map[string]any{"memoryID": "X"}, "card")
		require.NoError(t, err)
		require.Equal(t, KindNeedsConfirm, result.Action.Kind, "card %d", i+1)
		require.NotNil(t, result.Action.ExpiresAt)
	}
	fourth, err := r.propose(context.Background(), "memory_forget", me, map[string]any{"memoryID": "X"}, "card")
	require.NoError(t, err)
	require.Nil(t, fourth.Action)
	require.Equal(t, "The tool did not run: too many confirmations are waiting in this turn. Tell the user what is waiting and ask them to send the rest separately.", fourth.ModelContent)
	require.Equal(t, 3, g.count(`SELECT count(*) FROM assistant_pending_actions WHERE tenant_id = $1`, me))

	nowhere, err := g.runner(me, me).propose(context.Background(), "memory_forget", me, map[string]any{"memoryID": "X"}, "card")
	require.NoError(t, err)
	require.Nil(t, nowhere.Action)
	require.Equal(t, "The tool did not run: there is nowhere to show a confirmation here. Tell the user you can't do it right now.", nowhere.ModelContent)
}

// hangingEmbeddings never answers until its context ends.
type hangingEmbeddings struct{}

func (hangingEmbeddings) Embed(ctx context.Context, _ string, _ uuid.UUID) ([]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAHungToolIsCutOffByTheRunnersOwnTimeout(t *testing.T) {
	g := newRig(t)
	me := g.user()
	deps := g.deps
	deps.MCP = mcpService(t, g.pool, g.paths, hangingEmbeddings{})
	r := NewRunner(deps, me, me, "qa-user", nil, nil, nil)
	r.timeout = 100 * time.Millisecond

	start := time.Now()
	// The loop hands Run a context that is never cancelled.
	result := r.Run(context.WithoutCancel(context.Background()), "search", map[string]any{"query": "anything"})

	require.Less(t, time.Since(start), 5*time.Second)
	require.Equal(t, "The tool failed. Do not try it again; tell the user you couldn't get that.", result.ModelContent)
}

func TestShortenedStripsControlAndFormatCharacters(t *testing.T) {
	require.Equal(t, "ab[31mc d", Shortened("a\u202eb\x1b[31mc\nd", 80))
	require.Equal(t, "a b", Shortened("a\r\n\r\nb", 80))
	long := strings.Repeat("x", 81)
	cut := Shortened(long, 80)
	require.Equal(t, strings.Repeat("x", 79)+"…", cut)
	require.Equal(t, 80, swiftstr.Count(cut))
	require.Equal(t, strings.Repeat("x", 80), Shortened(strings.Repeat("x", 80), 80))
	// Characters, not bytes or scalars.
	require.Equal(t, strings.Repeat("é", 80), Shortened(strings.Repeat("é", 80), 80))
}

func TestAZeroValueRunnerAnswersInsteadOfPanicking(t *testing.T) {
	var r Runner

	require.Equal(t, toolFailed, r.Run(context.Background(), "search", map[string]any{"query": "x"}).ModelContent)
	require.Equal(t, toolFailed, r.Run(context.Background(), "memory_save", map[string]any{"content": "x"}).ModelContent)
	require.Equal(t, toolFailed, r.Run(context.Background(), "app_reminders_list", map[string]any{}).ModelContent)
	require.Equal(t, toolFailed, r.Run(context.Background(), "health_query", map[string]any{}).ModelContent)
	require.Equal(t, toolFailed, r.Run(context.Background(), "app_reminder_create",
		map[string]any{"title": "x", "fireAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}).ModelContent)
	result, err := r.propose(context.Background(), "memory_forget", uuid.New(), map[string]any{}, "card")
	require.NoError(t, err)
	require.Nil(t, result.Action)
	// The zero timeout is the default, not an instant deadline, and the turn
	// is counted.
	require.NotNil(t, r.Run(context.Background(), "open_screen", map[string]any{"screen": "home"}).Action)
	require.True(t, r.state().PersonalRan())
}
