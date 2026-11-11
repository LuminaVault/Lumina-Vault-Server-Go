package main

import (
	"lumina/internal/apns"
	"lumina/internal/chunkindex"
	"lumina/internal/devicerpc"
	"lumina/internal/devicetools"
	"lumina/internal/httpserver"
	"lumina/internal/modules/mcp"
	"lumina/internal/soul"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultindex"
)

// mcpModule is App+build's MCP block: /v1/mcp and /v1/me/agent-connections (5F).
// Personal-data tools reach the phone through this process's device broker, so
// mcp flips with /v1/ws (flip-blockers.md, the ws bundle).
func (a *agent) mcpModule(broker *devicerpc.Broker) httpserver.Module {
	cfg := a.cfg
	chunks := chunkindex.NewChunkRepository(a.pool)
	links := chunkindex.NewLinkRepository(a.pool)
	indexer := chunkindex.NewIndexer(chunks, a.embeddings, links)
	push := apns.New(a.pool, apns.Config{
		Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
		PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
	})
	base := cfg.MCPPublicBaseURL
	if base == "" {
		base = cfg.IngestionPublicBaseURL
	}
	return mcp.New(mcp.Options{
		Identify:    a.identify,
		Limits:      a.limits,
		VaultAccess: vaultaccess.New(a.pool),
		Connections: mcp.NewConnections(a.pool),
		Service: mcp.NewService(mcp.ServiceDeps{
			Pool: a.pool, Paths: a.paths,
			Status:     vaultindex.New(a.pool, vaultfiles.NewStore(a.pool, a.paths), links),
			Links:      links,
			Hybrid:     chunkindex.NewHybrid(a.memories, chunks),
			Embeddings: a.embeddings,
			Backfill:   chunkindex.NewBackfiller(a.pool, a.paths, indexer),
			Indexer:    indexer,
			Memories:   a.memories,
			Personal:   devicetools.New(a.pool, broker, devicerpc.NewQueue(a.pool, push)),
			Soul:       soul.New(a.paths, cfg.HermesDataRoot),
		}),
		PublicBaseURL: base,
	})
}
