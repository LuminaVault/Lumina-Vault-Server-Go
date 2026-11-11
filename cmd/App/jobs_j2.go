package main

import (
	"log/slog"
	"lumina/internal/apns"
	"lumina/internal/config"
	"lumina/internal/embedding"
	"lumina/internal/jobs"
	"lumina/internal/modules/calendar"
	"lumina/internal/modules/me"
	"strings"

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

// j2Jobs builds group J2's jobs (wave-6-8-plan.md "J1, J2") under Swift's own start
// conditions. None runs unless LV_GO_JOBS names it.
func j2Jobs(pool *pgxpool.Pool, cfg config.Config, embeddings embedding.Service) []jobs.Job {
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
	return out
}
