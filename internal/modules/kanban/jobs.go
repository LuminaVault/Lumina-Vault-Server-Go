package kanban

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/jobauthor"
	"lumina/internal/modules/kanban/db"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Port of Skills/JobAuthoring.swift, the part card promotion uses: validate
// the schedule, write <vault>/tenants/<ID>/skills/<slug>/SKILL.md, and upsert
// the skills_state row the Swift CronScheduler reads. JobsController
// (POST /v1/jobs) shares it in Swift and will reuse this when it is ported.

var (
	errInvalidCron = wire.NewError(http.StatusBadRequest, "invalid cron schedule")
	errOneSchedule = wire.NewError(http.StatusBadRequest, "job requires exactly one of cron or run_at")
)

type jobOrigin struct {
	kind string
	id   uuid.UUID
}

type jobAuthoring struct {
	paths vaultpaths.Paths
	log   *slog.Logger
}

// author writes the skill and enables it on q (the caller's transaction, as
// in Swift, so the upsert commits or rolls back with the card update).
func (a jobAuthoring) author(ctx context.Context, q *db.Queries, tenant uuid.UUID, title string,
	cron *string, runAt *time.Time, domain *string, spec string, spaceID *uuid.UUID, origin *jobOrigin,
) (string, error) {
	switch {
	case cron != nil && runAt == nil:
		if !validCron(*cron) {
			return "", errInvalidCron
		}
	case cron == nil && runAt != nil:
	default:
		return "", errOneSchedule
	}
	slug := jobSlug(title)

	if err := jobauthor.Write(a.paths, tenant, slug, skillMarkdown(slug, title, cron, domain, spec)); err != nil {
		return "", err
	}

	params := db.UpsertSkillStateParams{TenantID: tenant, Name: slug, Domain: domain, SpaceID: spaceID, RunAt: runAt}
	if origin != nil {
		params.OriginKind, params.OriginID = &origin.kind, &origin.id
	}
	if err := q.UpsertSkillState(ctx, params); err != nil {
		return "", err
	}

	when := "-"
	switch {
	case cron != nil:
		when = *cron
	case runAt != nil:
		when = "once@" + runAt.UTC().Format("2006-01-02 15:04:05 +0000")
	}
	d := "-"
	if domain != nil {
		d = *domain
	}
	a.log.InfoContext(ctx, fmt.Sprintf("job authored tenant=%s slug=%s when=%s domain=%s", ids.Upper(tenant), slug, when, d))
	return slug, nil
}

func jobSlug(title string) string { return jobauthor.Slug(title) }

func skillMarkdown(slug, title string, cron, domain *string, spec string) string {
	return jobauthor.Markdown(slug, title, cron, domain, spec)
}
