package main

import (
	"lumina/internal/apns"
	"lumina/internal/chunkindex"
	"lumina/internal/connectedapps"
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
	a.mcp = mcp.NewService(mcp.ServiceDeps{
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
	})
	return mcp.New(a.mcpOptions(mcp.Options{
		Identify:      a.identify,
		Limits:        a.limits,
		VaultAccess:   vaultaccess.New(a.pool),
		Connections:   mcp.NewConnections(a.pool),
		Service:       a.mcp,
		PublicBaseURL: base,
	}))
}

var _ mcp.ConnectedApps = (*connectedapps.Service)(nil)

// mcpOptions adds connected apps to /v1/mcp when they are on: the service the
// app tools go through and the registry ids a key can be granted. With them
// off both stay unset, and the interface is never given a typed nil.
func (a *agent) mcpOptions(opts mcp.Options) mcp.Options {
	if a.connectedApps == nil {
		return opts
	}
	opts.ConnectedApps = a.connectedApps
	for _, app := range connectedapps.NewRegistry(a.cfg).All() {
		opts.ConnectedAppIDs = append(opts.ConnectedAppIDs, app.ID)
	}
	return opts
}
