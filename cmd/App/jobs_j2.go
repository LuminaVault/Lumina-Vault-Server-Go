package main

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/config"
	"lumina/internal/embedding"
	"lumina/internal/jobs"
	"lumina/internal/llm"
	"lumina/internal/modules/calendar"
	"lumina/internal/modules/community"
	"lumina/internal/modules/dashboard"
	"lumina/internal/modules/knowledge"
	"lumina/internal/modules/me"
	"lumina/internal/modules/news"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// swiftBoolDefaultTrue is reader.bool(forKey:default: true): only an explicit
// false turns it off.
func swiftBoolDefaultTrue(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "no", "0", "false":
		return false
	}
	return true
}

// swiftBoolDefaultFalse is reader.bool(forKey:default: false): only an explicit
// true turns it on.
func swiftBoolDefaultFalse(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yes", "1", "true":
		return true
	}
	return false
}

// swiftInt is reader.int(forKey:default:): an unreadable value is the default.
func swiftInt(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return n
}

// j2Jobs builds group J2's jobs (wave-6-8-plan.md "J1, J2") under Swift's own start
// conditions. None runs unless LV_GO_JOBS names it.
func j2Jobs(pool *pgxpool.Pool, cfg config.Config, embeddings embedding.Service, embeddingKind embedding.Kind, transport llm.ChatTransport) []jobs.Job {
	push := apns.New(pool, apns.Config{
		Enabled: cfg.APNSEnabled, BundleID: cfg.APNSBundleID, TeamID: cfg.APNSTeamID, KeyID: cfg.APNSKeyID,
		PrivateKeyPath: cfg.APNSPrivateKeyPath, Environment: cfg.APNSEnvironment,
	})
	out := []jobs.Job{}
	if cfg.LVEnvironment != "test" && swiftBoolDefaultTrue(cfg.LearnedDigestEnabled) {
		out = append(out, (&me.LearnedDigestScheduler{Pool: pool, Push: push, Logger: slog.Default().With("label", "lv.learned.digest")}).Job())
	}
	if cfg.LVEnvironment != "test" && swiftBoolDefaultTrue(cfg.MeetingBriefEnabled) {
		out = append(out, calendar.NewMeetingBriefScheduler(pool, push, calendar.Options{
			Embeddings: embeddings, MaxDistance: float32(cfg.MeetingBriefMaxDistance),
		}, slog.Default().With("label", "lv.meeting.brief")).Job())
	}
	// Re-embed off by default (each row is a paid call); heal on by default but
	// only with a real provider, so it does not run on deterministic.
	reembedOn := swiftBoolDefaultFalse(cfg.EmbeddingReembedEnabled)
	healOn := swiftBoolDefaultTrue(cfg.EmbeddingHealEnabled) && embeddingKind != embedding.KindDeterministic
	if cfg.LVEnvironment != "test" && (reembedOn || healOn) {
		minutes := func(raw string, def int) time.Duration {
			return time.Duration(max(1, swiftInt(raw, def))) * time.Minute
		}
		out = append(out, embedding.NewReembedService(pool, embeddings, embeddingKind, reembedOn, healOn, embedding.ReembedConfig{
			BatchSize:                  swiftInt(cfg.EmbeddingReembedBatchSize, 50),
			Pause:                      time.Duration(swiftInt(cfg.EmbeddingReembedPauseMS, 2000)) * time.Millisecond,
			FailedBatchesBeforeBackoff: 3,
			BackoffBase:                30 * time.Second,
			MaxBackoff:                 900 * time.Second,
			HealInterval:               minutes(cfg.EmbeddingHealIntervalMinutes, 10),
			HealMaxBackoff:             minutes(cfg.EmbeddingHealMaxBackoffMinutes, 60),
		}, slog.Default().With("label", "lv.embedding.reembed")).Job())
	}
	if cfg.LVEnvironment != "test" && strings.ToLower(cfg.KnowledgeGraphWorkerEnabled) == "true" {
		worker := &knowledge.ExtractionWorker{Pool: pool, Push: push, Logger: slog.Default().With("label", "lv.knowledge.extraction.worker")}
		modelOn := strings.ToLower(cfg.KnowledgeGraphModelExtractionEnabled) == "true"
		if modelOn {
			worker.Transport, worker.Model = transport, cfg.HermesDefaultModel
		}
		slog.Info(fmt.Sprintf("knowledge graph model adjudication %s", map[bool]string{true: "enabled", false: "disabled"}[modelOn]))
		out = append(out, worker.Job())
	} else {
		slog.Info("knowledge graph worker disabled (set KNOWLEDGE_GRAPH_WORKER_ENABLED=true to enable)")
	}
	// Swift checks only this flag (no LV_ENVIRONMENT test guard).
	if strings.ToLower(cfg.SynthesisWorkerEnabled) == "true" {
		out = append(out, dashboard.NewSynthesisWorker(pool, transport, cfg.HermesDefaultModel, slog.Default().With("label", "lv.synthesis")).Job())
	} else {
		slog.Info("synthesis worker disabled (set SYNTHESIS_WORKER_ENABLED=true to enable)")
	}
	return out
}

// communityConfig is CommunityConfig(reader:).
func communityConfig(cfg config.Config) community.Config {
	return community.Config{
		Enabled: community.ParseFlag(cfg.CommunityEnabled), PostingEnabled: community.ParseFlag(cfg.CommunityPostingEnabled),
		AdminToken:       cfg.AdminToken,
		DailyPostEnabled: community.ParseFlag(cfg.CommunityDailyPostEnabled),
		DailyPostHourUTC: community.ParseHour(cfg.CommunityDailyPostHourUTC),
	}
}

// communityDailyJobs wires the LLM moderator into the board and builds
// DailyPostWorker (App+build.swift: the worker exists when the board is on, and
// runs outside LV_ENVIRONMENT=test). The admin trigger uses the same worker.
func communityDailyJobs(cfg config.Config, board *community.Module, transport llm.ChatTransport) []jobs.Job {
	var headlines community.HeadlineSource
	if cfg.FeedsBaseURL != "" {
		headlines = newsHeadlines{news.NewService(cfg.FeedsBaseURL, news.CuratedFeeds(cfg.FeedsDefaultFeeds), nil, nil).WithCacheTTL(15 * time.Minute)}
	}
	worker := board.UseModel(transport, cfg.HermesDefaultModel, headlines)
	if !community.ParseFlag(cfg.CommunityEnabled) || cfg.LVEnvironment == "test" {
		return nil
	}
	return []jobs.Job{worker.Job()}
}

// newsHeadlines is NewsTickerService's DailyPostHeadlineSource conformance.
type newsHeadlines struct{ svc *news.Service }

func (h newsHeadlines) Headlines(ctx context.Context, limit int) ([]community.Headline, error) {
	resp, err := h.svc.CuratedHeadlines(ctx, limit)
	if err != nil {
		return nil, err
	}
	var out []community.Headline
	for _, item := range resp.Items {
		if item.Url == nil || item.Title == "" {
			continue
		}
		source := item.Source
		out = append(out, community.Headline{Title: item.Title, Source: &source, URL: *item.Url})
	}
	return out, nil
}
