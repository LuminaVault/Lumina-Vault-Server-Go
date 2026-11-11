package skills

import (
	"context"
	"io"
	"lumina/internal/entitlement"
	"lumina/internal/llm"
	"lumina/internal/memorycompile"
	"lumina/internal/skills"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// POST /v1/skills/:name/run and POST /v1/skills/slash.

// SkillRunResponse is LuminaVaultShared.SkillRunResponse; savedPath is never set.
type SkillRunResponse struct {
	ID        wire.UUID  `json:"id"`
	SkillName string     `json:"skillName"`
	Status    string     `json:"status"`
	Markdown  string     `json:"markdown"`
	ModelUsed *string    `json:"modelUsed,omitempty"`
	MtokIn    *int       `json:"mtokIn,omitempty"`
	MtokOut   *int       `json:"mtokOut,omitempty"`
	StartedAt wire.Time  `json:"startedAt"`
	EndedAt   *wire.Time `json:"endedAt,omitempty"`
}

type skillRunRequest struct {
	Input     *string            `json:"input,omitempty"`
	Arguments *map[string]string `json:"arguments,omitempty"`
	Save      *bool              `json:"save,omitempty"`
}

type slashRequest struct {
	Command string `json:"command"`
}

// Compiler is MemoryCompileController.compile for /kb-compile.
type Compiler func(ctx, llmCtx context.Context, tenant uuid.UUID, fileIDs []wire.UUID, forceFull bool) (memorycompile.Response, error)

func (m *Module) runSkill(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	name := nameParam(c, 1)
	// `try? req.decode(...) ?? SkillRunRequest()`: any undecodable body runs bare.
	var body skillRunRequest
	if raw, readErr := io.ReadAll(c.Request().Body); readErr != nil || wire.DecodeInto(raw, &body) != nil {
		body = skillRunRequest{}
	}
	arguments := map[string]string{}
	if body.Arguments != nil {
		arguments = *body.Arguments
	}
	return m.run(c, tenant, name, body.Input, arguments)
}

func (m *Module) slash(c *echo.Context) error {
	tenant, err := requireIdentity(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[slashRequest](c)
	if err != nil {
		return err
	}
	inv, ok := skills.ParseSlash(body.Command)
	if !ok {
		return wire.NewError(http.StatusBadRequest, "slash command must start with /")
	}
	switch inv.Kind {
	case skills.KindKBCompile:
		started := time.Now()
		if m.opt.Compile == nil {
			return wire.NewError(http.StatusInternalServerError, "kb-compile is not wired on this server")
		}
		resp, err := m.opt.Compile(c.Request().Context(), llm.RequestContext(c), tenant, nil, false)
		if err != nil {
			return err
		}
		ended := wire.Time{Time: time.Now()}
		return wire.JSON(c, http.StatusOK, SkillRunResponse{
			ID: wire.UUID{UUID: uuid.New()}, SkillName: "kb-compile", Status: "success",
			Markdown: kbCompileMarkdown(resp), StartedAt: wire.Time{Time: started}, EndedAt: &ended,
		})
	case skills.KindHelp:
		now := wire.Time{Time: time.Now()}
		return wire.JSON(c, http.StatusOK, SkillRunResponse{
			ID: wire.UUID{UUID: uuid.New()}, SkillName: "slash-help", Status: "success",
			Markdown: inv.HelpMarkdown, StartedAt: now, EndedAt: &now,
		})
	}
	return m.run(c, tenant, inv.SkillName, inv.Input, inv.Arguments)
}

// run is SkillsController.runSkill(name:user:input:arguments:).
func (m *Module) run(c *echo.Context, tenant uuid.UUID, name string, input *string, arguments map[string]string) error {
	if strings.HasPrefix(name, hermesSkillPrefix) {
		return wire.NewError(http.StatusBadRequest, "hermes_skill_runs_on_hermes")
	}
	mf, ok := m.opt.Catalog.Manifest(name, tenant)
	if !ok {
		return wire.NewError(http.StatusNotFound, "no skill named "+name)
	}
	ctx := c.Request().Context()
	user := runUser{tenant: tenant}
	if err := m.pool.QueryRow(ctx, `SELECT tier, tier_override, username FROM users WHERE id = $1`, tenant).
		Scan(&user.tier, &user.override, &user.username); err != nil {
		return err
	}
	if m.opt.EnforcementEnabled {
		capability := entitlement.SkillBuiltinRun
		if mf.Source == skills.SourceVault {
			capability = entitlement.SkillVaultRun
		}
		if !entitlement.Entitled(user.tier, user.override, capability) {
			return entitlement.WritePaywall(c, capability)
		}
	}
	if user.tier == "" {
		user.tier = "trial"
	}
	if m.opt.Runner == nil {
		return wire.NewError(http.StatusInternalServerError, "skill runner is not wired on this server")
	}
	res, err := m.opt.Runner.Run(ctx, mf, user, input, arguments)
	if err != nil {
		return err
	}
	ended := wire.Time{Time: res.endedAt}
	status := "error"
	if res.status == "ok" {
		status = "success"
	}
	return wire.JSON(c, http.StatusOK, SkillRunResponse{
		ID: wire.UUID{UUID: res.runID}, SkillName: mf.Name, Status: status, Markdown: res.markdown,
		ModelUsed: res.modelUsed, MtokIn: &res.mtokIn, MtokOut: &res.mtokOut,
		StartedAt: wire.Time{Time: res.startedAt}, EndedAt: &ended,
	})
}

// kbCompileMarkdown is SkillsController.kbCompileMarkdown.
func kbCompileMarkdown(r memorycompile.Response) string {
	if r.MemoriesIngested == 0 {
		return "KB compile finished. No pending vault files needed ingestion."
	}
	updated, duration := "", ""
	if r.MemoriesUpdated != nil {
		updated = "\n- Memories updated: " + strconv.Itoa(*r.MemoriesUpdated)
	}
	if r.DurationMs != nil {
		duration = "\n- Duration: " + strconv.Itoa(*r.DurationMs) + " ms"
	}
	return "KB compile finished.\n\n- Memories ingested: " + strconv.Itoa(r.MemoriesIngested) + updated + duration
}
