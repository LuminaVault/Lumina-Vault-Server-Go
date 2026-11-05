package capture

// Ports Tests/AppTests/Capture/CaptureFilingServiceTests.swift

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/dbtest"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/migrations"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type filingTransport struct {
	reply string
	calls int
}

func (s *filingTransport) ChatCompletions(context.Context, []byte, string, string) (llm.Metadata, error) {
	s.calls++
	data, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": s.reply}}}})
	return llm.Metadata{Data: data}, err
}

func (s *filingTransport) ChatStream(context.Context, []byte, string, string) llm.Stream {
	return func(yield func(llm.Chunk, error) bool) { yield(llm.Chunk{}, errors.New("no stream")) }
}

type filingFixture struct {
	pool          *pgxpool.Pool
	root          string
	tenant        uuid.UUID
	file, spaceID uuid.UUID
}

// setupFiling is one unfiled note on disk and in vault_files, plus a "work" Space.
func setupFiling(t *testing.T) filingFixture {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	f := filingFixture{pool: pool, root: t.TempDir(), tenant: uuid.New(), file: uuid.New(), spaceID: uuid.New()}
	slug := "file" + f.tenant.String()[:8]
	_, err := pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, is_verified, tier, created_at, updated_at)
		VALUES ($1, $2, $3, 'stub', true, 'trial', now(), now())`, f.tenant, slug+"@test.luminavault", slug)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'test', now(), now())`, f.tenant)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO spaces (id, tenant_id, name, slug, description, created_at, updated_at)
		VALUES ($1, $2, 'Work', 'work', 'Job, team, deploys', now(), now())`, f.spaceID, f.tenant)
	require.NoError(t, err)
	path := "inbox/standup.md"
	target := filepath.Join(f.root, "tenants", ids.Upper(f.tenant), "raw", path)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o750))
	require.NoError(t, os.WriteFile(target, []byte("Standup moved to 9:30 for the whole team."), 0o600))
	_, err = pool.Exec(ctx, `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		VALUES ($1, $2, $3, 'text/markdown', 40, $4, now(), now())`, f.file, f.tenant, path, strings.Repeat("a", 64))
	require.NoError(t, err)
	return f
}

func (f filingFixture) service(reply string) (*FilingService, *filingTransport) {
	transport := &filingTransport{reply: reply}
	paths := vaultpaths.Paths{Root: f.root}
	return &FilingService{Pool: f.pool, Transport: transport, Paths: paths, Store: vaultfiles.NewStore(f.pool, paths), DefaultModel: "test-model"}, transport
}

func (f filingFixture) row(t *testing.T) *vaultfiles.VaultFile {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT `+vaultfiles.Columns+` FROM vault_files WHERE id = $1`, f.file)
	require.NoError(t, err)
	defer rows.Close()
	require.True(t, rows.Next())
	file, err := vaultfiles.Scan(rows)
	require.NoError(t, err)
	return file
}

func TestAFittingSpaceIsSuggestedAndNothingMoves(t *testing.T) {
	f := setupFiling(t)
	service, _ := f.service(`{"space":"work"}`)
	slug, err := service.Suggest(context.Background(), f.tenant, f.file)
	require.NoError(t, err)
	assert.Equal(t, "work", slug)
	file := f.row(t)
	require.NotNil(t, file.Metadata)
	require.NotNil(t, file.Metadata.SuggestedSpaceID)
	assert.Equal(t, f.spaceID, *file.Metadata.SuggestedSpaceID)
	assert.Nil(t, file.SpaceID, "a suggestion must not file anything")
	assert.Equal(t, "inbox/standup.md", file.Path)
	data, err := json.Marshal(file.Metadata)
	require.NoError(t, err)
	assert.Contains(t, string(data), ids.Upper(f.spaceID), "stored where the file list reads it")
}

func TestNoneInventedSpaceOrAFiledNoteGiveNoSuggestion(t *testing.T) {
	f := setupFiling(t)
	ctx := context.Background()
	for _, reply := range []string{`{"space":"none"}`, `{"space":"finance"}`, "I think Work."} {
		service, _ := f.service(reply)
		slug, err := service.Suggest(ctx, f.tenant, f.file)
		require.NoError(t, err, reply)
		assert.Empty(t, slug, reply)
	}
	if md := f.row(t).Metadata; md != nil {
		assert.Nil(t, md.SuggestedSpaceID)
	}
	_, err := f.pool.Exec(ctx, `UPDATE vault_files SET space_id = $1 WHERE id = $2`, f.spaceID, f.file)
	require.NoError(t, err)
	service, transport := f.service(`{"space":"work"}`)
	slug, err := service.Suggest(ctx, f.tenant, f.file)
	require.NoError(t, err)
	assert.Empty(t, slug)
	assert.Zero(t, transport.calls, "a filed note costs no model call")
}

func TestReplyParserAcceptsJSONInProseAndFences(t *testing.T) {
	check := func(raw, want string, wantOK bool) {
		t.Helper()
		got, ok := ParseSuggestion(raw)
		assert.Equal(t, wantOK, ok, raw)
		assert.Equal(t, want, got, raw)
	}
	check(`{"space":"Work"}`, "work", true)
	check("```json\n{\"space\":\"ops\"}\n```", "ops", true)
	check(`Sure: {"space":"none"}`, "", false)
	check("", "", false)
}
