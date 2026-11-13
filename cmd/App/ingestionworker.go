package main

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/apns"
	linkcapture "lumina/internal/capture"
	"lumina/internal/chunkindex"
	"lumina/internal/hermescaps"
	"lumina/internal/hermesingest"
	"lumina/internal/jobs"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/ingestion"
	"lumina/internal/ssrfguard"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

// ingestionWorkerJob is MultimodalIngestionWorker as App+build.swift builds it: the
// routed transport plus the Hermes ingestion endpoint (BYO-resolved, no redirects)
// and HERMES_DEFAULT_MODEL, link capture, APNs, the chunk indexer, and the Hermes
// capabilities service, which is managedDefault without the secret box. Like Swift,
// a malformed INGESTION_PUBLIC_BASE_URL, or a non-HTTPS one outside dev, stops boot.
func (a *agent) ingestionWorkerJob(mod *ingestion.Module, links *linkcapture.Service, memories *memorystore.Repository) (jobs.Job, error) {
	cfg := a.cfg
	var public *url.URL
	if raw := cfg.IngestionPublicBaseURL; raw != "" {
		u, err := url.Parse(raw)
		if err != nil || !u.IsAbs() {
			return jobs.Job{}, errors.New("INGESTION_PUBLIC_BASE_URL must be an absolute URL")
		}
		if cfg.LVEnvironment != "dev" && u.Scheme != "https" {
			return jobs.Job{}, errors.New("INGESTION_PUBLIC_BASE_URL must use HTTPS outside development")
		}
		public = u
	}
	ingest := &hermesingest.Transport{
		HTTP: llm.NoRedirectClient(),
		Log:  slog.Default().With("label", "lv.ingestion.hermes"),
	}
	if gateway, err := url.Parse(cfg.HermesGatewayURL); err == nil {
		ingest.DefaultBaseURL = gateway
	}
	if cfg.HermesAPIKey != "" {
		auth := "Bearer " + cfg.HermesAPIKey
		ingest.DefaultAuthHeader = &auth
	}
	capabilities := func(context.Context, uuid.UUID) hermescaps.Capabilities { return hermescaps.ManagedDefault() }
	if resolver := hermesResolver(a.pool, cfg, a.box); resolver != nil {
		ingest.Resolver = resolver
		guard := ssrfguard.Guard{
			AllowPrivateRanges: strings.EqualFold(cfg.BYOHermesAllowPrivate, "true"),
			RequireHTTPS:       cfg.BYOHermesRequireHTTPS(),
			AllowTailnetHTTP:   strings.EqualFold(cfg.BYOHermesAllowTailnet, "true"),
		}
		service := hermescaps.New(a.pool, resolver, a.box, guard)
		capabilities = func(ctx context.Context, tenant uuid.UUID) hermescaps.Capabilities {
			return service.Capabilities(ctx, tenant, false).Capabilities
		}
	}
	chunks := chunkindex.NewChunkRepository(a.pool)
	worker := mod.NewWorker(ingestion.WorkerDeps{
		LinkCapture: func(ctx context.Context, tenant uuid.UUID, rawURL string, space *uuid.UUID) (uuid.UUID, error) {
			captured, err := links.CaptureLink(ctx, tenant, rawURL, nil, space)
			return captured.FileID, err
		},
		Processor: &ingestion.HermesProcessor{Chat: a.stack.Transport, Ingest: ingest, Model: cfg.HermesDefaultModel},
		Memories:  memories, Embeddings: a.embeddings,
		Push: apns.New(a.pool, apns.Config{
			Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
			PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
		}),
		Capabilities:  capabilities,
		PublicBaseURL: public,
		Indexer:       chunkindex.NewIndexer(chunks, a.embeddings, chunkindex.NewLinkRepository(a.pool)),
		Log:           slog.Default().With("label", "lv.ingestion.worker"),
	})
	return worker.Job(), nil
}
