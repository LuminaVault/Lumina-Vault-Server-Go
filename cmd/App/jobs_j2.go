package main

import (
	"fmt"
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/config"
	"lumina/internal/embedding"
	"lumina/internal/jobs"
	"lumina/internal/llm"
	"lumina/internal/modules/calendar"
	"lumina/internal/modules/knowledge"
	"lumina/internal/modules/me"
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
	return out
}
