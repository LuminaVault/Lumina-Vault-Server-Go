package kanban

import (
	"context"
	"encoding/json"
	"lumina/internal/modules/kanban/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// promotedJob is KanbanService.PromotedJob.
type promotedJob struct {
	slug           string
	title          string
	cron           *string
	runAt          *time.Time
	spec           string
	alreadyPromote bool
}

// promoteCard is KanbanController.promoteCard: card → scheduled vault job,
// answered as a SkillDTO (the POST /v1/jobs shape).
func (m *Module) promoteCard(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.AI)
	if err != nil {
		return err
	}
	if !acc.CanWrite() {
		return errWriteRequired
	}
	// `try? req.decode`: any undecodable body counts as no body.
	body, err := wire.Bind[cardPromoteRequest](c)
	if err != nil {
		body = nil
	}
	id, err := uuidParam(c, "cardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	var job promotedJob
	if err := m.tx(ctx, func(q *db.Queries) error {
		job, err = m.promote(ctx, q, acc.VaultID, id, body, expected)
		return err
	}); err != nil {
		return err
	}
	m.record(c, acc, "card.promoted", "card", &id, &job.title)
	excerpt := []rune(job.spec)
	return wire.JSON(c, http.StatusOK, SkillDTO{
		Id:              job.slug,
		Source:          Vault,
		Name:            job.slug,
		Title:           job.title,
		DescriptionText: job.spec,
		Capability:      SkillCapabilityMedium,
		Schedule:        job.cron,
		Enabled:         true,
		DailyRunCount:   0,
		DailyRunCap:     0,
		BodyExcerpt:     string(excerpt[:min(160, len(excerpt))]),
		Origin:          &JobOriginDTO{Kind: Card, Id: wire.UUID{UUID: id}},
	})
}

// promote is KanbanService.promoteCard, inside the caller's transaction.
func (m *Module) promote(ctx context.Context, q *db.Queries, tenant, cardID uuid.UUID,
	req *cardPromoteRequest, expected *int64,
) (promotedJob, error) {
	card, err := requireCard(ctx, q, tenant, cardID)
	if err != nil {
		return promotedJob{}, err
	}
	extra, err := decodeExtra(card.Extra)
	if err != nil {
		return promotedJob{}, err
	}
	if extra == nil {
		extra = &cardExtra{}
	}
	job := cardJobConfig{Source: "vault"}
	if extra.Job != nil {
		job = *extra.Job
	}
	// Request fields win over the card's stored config.
	if req != nil {
		if req.Cron != nil {
			job.Cron = req.Cron
		}
		if req.RunAt != nil {
			job.RunAt = &refDate{req.RunAt.Time}
		}
		if req.Domain != nil {
			job.Domain = req.Domain
		}
		if req.Prompt != nil {
			job.Prompt = req.Prompt
		}
		if req.SpaceID != nil {
			job.SpaceID = req.SpaceID
		}
	}
	// Exactly one of cron (recurring) or runAt (one-shot).
	var cron *string
	if job.Cron != nil {
		t := trimSwiftWhitespace(*job.Cron)
		cron = &t
	}
	hasCron := cron != nil && *cron != ""
	hasRunAt := job.RunAt != nil
	if hasCron == hasRunAt {
		return promotedJob{}, errNeedsSchedule
	}
	var effCron *string
	var effRunAt *time.Time
	if hasCron {
		effCron = cron
	} else {
		effRunAt = &job.RunAt.Time
	}
	specSrc := job.Prompt
	if specSrc == nil {
		specSrc = card.Body
	}
	if specSrc == nil {
		return promotedJob{}, errNeedsPrompt
	}
	spec := trimSwiftWhitespaceAndNewlines(*specSrc)
	if spec == "" {
		return promotedJob{}, errNeedsPrompt
	}

	// Already promoted: no re-author, no version claim, nothing persisted.
	if job.JobSlug != nil {
		return promotedJob{slug: *job.JobSlug, title: card.Title, cron: effCron, runAt: effRunAt, spec: spec, alreadyPromote: true}, nil
	}

	if _, err := m.claim(ctx, q, tenant, card.BoardID, expected); err != nil {
		return promotedJob{}, err
	}
	var spaceID *uuid.UUID
	if job.SpaceID != nil {
		spaceID = &job.SpaceID.UUID
	}
	slug, err := m.jobs.author(ctx, q, tenant, card.Title, effCron, effRunAt, job.Domain, spec, spaceID,
		&jobOrigin{kind: "card", id: card.ID})
	if err != nil {
		return promotedJob{}, err
	}
	now := m.now()
	job.JobSlug = &slug
	job.PromotedAt = &refDate{now}
	extra.Job = &job
	raw, err := json.Marshal(extra)
	if err != nil {
		return promotedJob{}, err
	}
	if err := q.UpdateCardExtra(ctx, db.UpdateCardExtraParams{ID: card.ID, Extra: raw, UpdatedAt: &now}); err != nil {
		return promotedJob{}, err
	}
	return promotedJob{slug: slug, title: card.Title, cron: effCron, runAt: effRunAt, spec: spec}, nil
}
