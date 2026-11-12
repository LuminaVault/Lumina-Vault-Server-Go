package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/cronexpr"
	"lumina/internal/ids"
	"lumina/internal/jobauthor"
	"lumina/internal/llm"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// JobsController: POST /v1/jobs/detect classifies a chat message, POST /v1/jobs
// authors a scheduled vault cron skill.

const jobExcerptLength = 160

// POST /v1/jobs/detect

func (m *Module) detect(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[detectRequest](c)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, m.classify(c.Request().Context(), body.Text, tenant))
}

const classifierSystem = "You classify whether a user's message is a request to set up a RECURRING / " +
	"scheduled job — daily/weekly/etc. monitoring, alerts, digests, summaries — " +
	"as opposed to a one-off question or a note to save.\n" +
	"\n" +
	"Respond with ONLY a JSON object (no prose, no code fence):\n" +
	"{\"isJob\": true|false, \"title\": \"<short title>\", \"cron\": \"<crontab, e.g. 0 8 * * *>\",\n" +
	" \"scheduleHuman\": \"<e.g. Every day at 8:00 AM>\",\n" +
	" \"domain\": \"stocks|sports|ai|tech|health|news|finance|life|other\",\n" +
	" \"spec\": \"<imperative description of what to produce each run>\"}\n" +
	"\n" +
	"If it is NOT a recurring job, return exactly {\"isJob\": false}.\n" +
	"Infer a sensible cron from natural language (\"every morning\" → 0 8 * * *,\n" +
	"\"weekly\" → 0 9 * * 1). Keep the spec self-contained — it becomes the job's\n" +
	"instructions and will run without the original conversation."

type classifierMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type classifierRequest struct {
	Model       string              `json:"model"`
	Messages    []classifierMessage `json:"messages"`
	Temperature float64             `json:"temperature"`
	Stream      bool                `json:"stream"`
}

// classify is JobIntentClassifier.classify on the module's transport and model.
func (m *Module) classify(ctx context.Context, text string, tenant uuid.UUID) JobProposalDTO {
	return Classify(ctx, m.opt.Transport, m.opt.DefaultModel, text, tenant)
}

// Classify is JobIntentClassifier.classify: one completion through the routed
// transport, failing closed to {"isJob": false}. The cron bridge's preview
// (skills-cron) uses it as its LLM fallback.
func Classify(ctx context.Context, transport llm.ChatTransport, model, text string, tenant uuid.UUID) JobProposalDTO {
	notJob := JobProposalDTO{IsJob: false}
	if transport == nil {
		return notJob
	}
	payload, err := json.Marshal(classifierRequest{
		Model: model,
		Messages: []classifierMessage{
			{Role: "system", Content: classifierSystem},
			{Role: "user", Content: text},
		},
		Temperature: 0.1,
	})
	if err != nil {
		return notJob
	}
	meta, err := transport.ChatCompletions(ctx, payload, ids.Upper(tenant), "")
	if err != nil {
		return notJob
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Role    string  `json:"role"`
				Content *string `json:"content,omitempty"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := wire.DecodeInto(meta.Data, &resp); err != nil || len(resp.Choices) == 0 || resp.Choices[0].Message.Content == nil {
		return notJob
	}
	return parseProposal(*resp.Choices[0].Message.Content)
}

// parseProposal is JobIntentClassifier.parse: tolerate a ```json fence, and
// anything unparseable is not a job.
func parseProposal(content string) JobProposalDTO {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(content)
	unfenced := trimmed
	if strings.HasPrefix(trimmed, "```") {
		afterTag := strings.TrimLeft(trimmed, "`")
		if i := strings.IndexByte(afterTag, '\n'); i >= 0 {
			afterTag = afterTag[i+1:]
		} else {
			afterTag = ""
		}
		if end := strings.LastIndex(afterTag, "```"); end >= 0 {
			unfenced = afterTag[:end]
		} else {
			unfenced = afterTag
		}
	}
	var env struct {
		IsJob         *bool   `json:"isJob,omitempty"`
		Title         *string `json:"title,omitempty"`
		Cron          *string `json:"cron,omitempty"`
		ScheduleHuman *string `json:"scheduleHuman,omitempty"`
		Domain        *string `json:"domain,omitempty"`
		Spec          *string `json:"spec,omitempty"`
	}
	if err := wire.DecodeInto([]byte(unfenced), &env); err != nil || env.IsJob == nil || !*env.IsJob {
		return JobProposalDTO{IsJob: false}
	}
	return JobProposalDTO{
		IsJob: true, Title: env.Title, Cron: env.Cron, ScheduleHuman: env.ScheduleHuman,
		Domain: env.Domain, Spec: env.Spec,
	}
}

// POST /v1/jobs

func (m *Module) createJob(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[jobCreateRequest](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var origin *JobOriginDTO
	if body.ConversationID != nil {
		var exists bool
		if err := m.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversations WHERE tenant_id = $1 AND id = $2)`,
			tenant, body.ConversationID.UUID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return wire.NewError(http.StatusBadRequest, "conversationId does not belong to the caller")
		}
		origin = &JobOriginDTO{Kind: "conversation", ID: *body.ConversationID}
	}
	if _, ok := cronexpr.Parse(body.Cron); !ok {
		return wire.NewError(http.StatusBadRequest, "invalid cron schedule")
	}
	slug := jobauthor.Slug(body.Title)
	cron := body.Cron
	if err := jobauthor.Write(m.opt.Paths, tenant, slug, jobauthor.Markdown(slug, body.Title, &cron, body.Domain, body.Spec)); err != nil {
		return err
	}
	var spaceID *uuid.UUID
	if body.SpaceID != nil {
		spaceID = &body.SpaceID.UUID
	}
	var originKind *string
	var originID *uuid.UUID
	if origin != nil {
		originKind, originID = &origin.Kind, &origin.ID.UUID
	}
	if _, err := m.pool.Exec(ctx, `
		INSERT INTO skills_state (tenant_id, source, name, enabled, domain, space_id, run_at, origin_kind, origin_id)
		VALUES ($1, 'vault', $2, TRUE, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, source, name) DO UPDATE
		  SET enabled = TRUE, domain = EXCLUDED.domain, space_id = EXCLUDED.space_id, run_at = EXCLUDED.run_at,
		      origin_kind = COALESCE(EXCLUDED.origin_kind, skills_state.origin_kind),
		      origin_id = COALESCE(EXCLUDED.origin_id, skills_state.origin_id)`,
		tenant, slug, body.Domain, spaceID, (*time.Time)(nil), originKind, originID); err != nil {
		return err
	}
	domain := "-"
	if body.Domain != nil {
		domain = *body.Domain
	}
	m.opt.Log.InfoContext(ctx, fmt.Sprintf("job authored tenant=%s slug=%s when=%s domain=%s", ids.Upper(tenant), slug, cron, domain))

	return wire.JSON(c, http.StatusOK, SkillDTO{
		ID: slug, Source: "vault", Name: slug, Title: body.Title, DescriptionText: body.Spec,
		Capability: "medium", Schedule: &cron, Enabled: true,
		BodyExcerpt: swiftstr.Prefix(body.Spec, jobExcerptLength), Origin: origin,
	})
}
