package main

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/migrations"
	"lumina/internal/modules/quickassistant"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultpaths"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// qaAgent is just enough of the agent for chatModules: a migrated database,
// the LLM stack on an empty environment and the /v1/mcp service.
func qaAgent(t *testing.T, enabled string) (*agent, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	keys := auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
	a := &agent{
		pool: pool, cfg: config.Config{QuickAssistantEnabled: enabled, HermesGatewayURL: "http://127.0.0.1:9"},
		paths:    vaultpaths.Paths{Root: t.TempDir()},
		identify: auth.Identify(keys, auth.PGUsers(pool)), limits: ratelimit.NewStore(),
		embeddings: embedding.Deterministic{}, memories: memorystore.New(pool, nil),
		stack: llm.NewStack(llm.StackConfig{Getenv: func(string) string { return "" }, Pool: pool}),
	}
	a.mcpModule(nil)
	return a, pool
}

func quickModules(t *testing.T, a *agent) int {
	t.Helper()
	n := 0
	for _, m := range a.chatModules(nil, nil, nil) {
		if _, ok := m.(*quickassistant.Module); ok {
			n++
		}
	}
	return n
}

func TestQuickAssistantIsWiredWhenEnabled(t *testing.T) {
	a, _ := qaAgent(t, "TRUE")
	qa, module := a.quickAssistant()
	require.NotNil(t, qa)
	require.NotNil(t, module)
	require.Same(t, a.mcp, qa.Deps.MCP, "the /v1/mcp service, not a second one")
	require.Nil(t, qa.Deps.Devices, "no device broker before the ws cutover")
	for _, tool := range qa.Service.Tools {
		require.NotEqual(t, "calendar_create", tool.Name)
	}
	require.Equal(t, 1, quickModules(t, a))
}

func TestQuickAssistantIsOffByDefault(t *testing.T) {
	a, _ := qaAgent(t, "")
	qa, module := a.quickAssistant()
	require.Nil(t, qa)
	require.Nil(t, module)
	require.Zero(t, quickModules(t, a))
}

// Review Focus 2: on, but the table is missing (production before owner
// question 1 is settled): the feature stays off and chat is unchanged.
func TestEnabledWithoutTableStaysOff(t *testing.T) {
	a, pool := qaAgent(t, "true")
	_, err := pool.Exec(context.Background(), `DROP TABLE assistant_pending_actions`)
	require.NoError(t, err)

	qa, module := a.quickAssistant()

	require.Nil(t, qa)
	require.Nil(t, module)
	require.Zero(t, quickModules(t, a))
}
