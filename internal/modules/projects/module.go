// Package projects serves /v1/projects: the Go port of
// LuminaVaultServer Sources/App/Projects/ProjectsController.swift (wave 1 pilot).
package projects

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/projects/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/projects/db"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	defaultLimit = 100
	maxLimit     = 200
)

var (
	errNotFound  = wire.NewError(http.StatusNotFound, "project not found")
	errBadID     = wire.NewError(http.StatusBadRequest, "invalid project id")
	errNameReq   = wire.NewError(http.StatusBadRequest, "project name required")
	errNameEmpty = wire.NewError(http.StatusBadRequest, "project name cannot be empty")
)

type Module struct {
	q   *db.Queries
	now func() time.Time
}

func New(pool *pgxpool.Pool) *Module {
	return &Module{q: db.New(pool), now: time.Now}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/projects", m.list, authMW)
	r.POST("/v1/projects", m.create, authMW)
	r.PATCH("/v1/projects/:id", m.update, authMW)
	r.DELETE("/v1/projects/:id", m.delete, authMW)
}

func (m *Module) list(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	rows, err := m.q.ListProjects(ctx, db.ListProjectsParams{TenantID: tenant, Limit: parseLimit(c.QueryParam("limit"))})
	if err != nil {
		return err
	}
	counts, err := m.todoCounts(ctx, tenant)
	if err != nil {
		return err
	}
	out := ProjectListResponse{Projects: make([]ProjectDTO, 0, len(rows))}
	for _, p := range rows {
		out.Projects = append(out.Projects, toDTO(p, counts[p.ID]))
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) create(c *echo.Context) error {
	body, err := wire.Bind[ProjectCreateRequest](c)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return errNameReq
	}
	p, err := m.q.CreateProject(c.Request().Context(), db.CreateProjectParams{
		ID:          uuid.New(),
		TenantID:    auth.UserID(c),
		Name:        name,
		Description: body.Description,
		CreatedAt:   m.now(),
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(p, 0))
}

func (m *Module) update(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	id, err := parseID(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[ProjectPatchRequest](c)
	if err != nil {
		return err
	}
	p, err := m.q.GetProject(ctx, db.GetProjectParams{ID: id, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if body.Name != nil {
		trimmed := strings.TrimSpace(*body.Name)
		if trimmed == "" {
			return errNameEmpty
		}
		p.Name = trimmed
	}
	if body.Description != nil {
		if *body.Description == "" {
			p.Description = nil
		} else {
			p.Description = body.Description
		}
	}
	if body.Archived != nil {
		p.Archived = *body.Archived
	}
	p, err = m.q.UpdateProject(ctx, db.UpdateProjectParams{ID: id, TenantID: tenant, Name: p.Name, Description: p.Description, Archived: p.Archived})
	if err != nil {
		return err
	}
	counts, err := m.todoCounts(ctx, tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(p, counts[id]))
}

func (m *Module) delete(c *echo.Context) error {
	id, err := parseID(c)
	if err != nil {
		return err
	}
	n, err := m.q.DeleteProject(c.Request().Context(), db.DeleteProjectParams{ID: id, TenantID: auth.UserID(c)})
	if err != nil {
		return err
	}
	if n == 0 {
		return errNotFound
	}
	return c.NoContent(http.StatusNoContent)
}

// todoMeta is the subset of Swift's VaultFileMetadata that counting needs.
type todoMeta struct {
	IsTodo    *bool   `json:"isTodo"`
	ProjectID *string `json:"projectID"`
}

// todoCounts mirrors Swift: every tenant vault file whose metadata.isTodo is
// true counts toward its metadata.projectID (done or not).
func (m *Module) todoCounts(ctx context.Context, tenant uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := m.q.TenantFileMetadata(ctx, tenant)
	if err != nil {
		return nil, err
	}
	counts := map[uuid.UUID]int{}
	for _, raw := range rows {
		var md todoMeta
		if json.Unmarshal(raw, &md) != nil || md.IsTodo == nil || !*md.IsTodo || md.ProjectID == nil {
			continue
		}
		if pid, err := ids.Parse(*md.ProjectID); err == nil {
			counts[pid]++
		}
	}
	return counts, nil
}

func toDTO(p db.Project, todoCount int) ProjectDTO {
	return ProjectDTO{
		Id:          wire.UUID{UUID: p.ID},
		Name:        p.Name,
		Description: p.Description,
		Archived:    p.Archived,
		CreatedAt:   wire.Time{Time: p.CreatedAt},
		TodoCount:   &todoCount,
	}
}

func parseID(c *echo.Context) (uuid.UUID, error) {
	raw := c.Param("id")
	id, err := ids.Parse(raw)
	if err != nil || len(raw) != 36 {
		return uuid.Nil, errBadID
	}
	return id, nil
}

// parseLimit mirrors Swift's parseLimit: missing or non-integer → 100,
// otherwise clamped to 1..200.
func parseLimit(raw string) int32 {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultLimit
	}
	return int32(max(1, min(n, maxLimit))) //nolint:gosec // clamped to 1..200
}
