// Package spaces serves /v1/spaces: the Go port of LuminaVaultServer
// Sources/App/Spaces (SpacesController, SpacesService). A space is a row in
// `spaces` plus a folder <vault>/tenants/<vault id>/raw/<slug> on the shared PVC.
package spaces

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/spaces/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/spaces/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultactivity"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

var (
	errNotFound     = wire.NewError(http.StatusNotFound, "space not found")
	errSlugTaken    = wire.NewError(http.StatusConflict, "slug already used in this vault")
	errNameRequired = wire.NewError(http.StatusBadRequest, "name required")
	errBadID        = wire.NewError(http.StatusBadRequest, "invalid space id")
)

type Module struct {
	q        *db.Queries
	access   *vaultaccess.Service
	activity *vaultactivity.Recorder
	paths    vaultpaths.Paths
	identify echo.MiddlewareFunc
	now      func() time.Time
}

// New builds the module. identify is auth.Identify: vault access reads
// X-Vault-ID before it requires identity (decisions.md D-identify).
func New(pool *pgxpool.Pool, paths vaultpaths.Paths, identify echo.MiddlewareFunc) *Module {
	return &Module{
		q:        db.New(pool),
		access:   vaultaccess.New(pool),
		activity: vaultactivity.New(pool),
		paths:    paths,
		identify: identify,
		now:      time.Now,
	}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.GET("/v1/spaces", m.list, m.identify)
	r.POST("/v1/spaces", m.create, m.identify)
	r.GET("/v1/spaces/:id", m.get, m.identify)
	r.PUT("/v1/spaces/:id", m.update, m.identify)
	r.DELETE("/v1/spaces/:id", m.delete, m.identify)
}

func (m *Module) list(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	spaces, err := m.List(c.Request().Context(), a.VaultID)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, SpaceListResponse{Spaces: spaces})
}

func (m *Module) get(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	s, n, err := m.load(c.Request().Context(), a.VaultID, id)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(s, n))
}

func (m *Module) create(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[SpaceCreateRequest](c)
	if err != nil {
		return err
	}
	actor, _ := auth.Identity(c)
	dto, err := m.Create(c.Request().Context(), a.VaultID, actor, CreateInput{
		Name: body.Name, Slug: body.Slug, Description: body.Description,
		Color: body.Color, Icon: body.Icon, Category: body.Category,
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, dto)
}

// createSpace is SpacesService.create followed by the controller's second
// save of created_by/updated_by, done in one insert.
func (m *Module) createSpace(ctx context.Context, vault, actor uuid.UUID, name, slugRaw string,
	description, color, icon, category *string,
) (db.Space, error) {
	name = trim(name)
	if name == "" {
		return db.Space{}, errNameRequired
	}
	slug, err := validateSlug(slugRaw)
	if err != nil {
		return db.Space{}, err
	}
	taken, err := m.q.SlugTaken(ctx, db.SlugTakenParams{TenantID: vault, Slug: slug})
	if err != nil {
		return db.Space{}, err
	}
	if taken {
		return db.Space{}, errSlugTaken
	}
	if err := m.paths.EnsureTenantDirectories(vault); err != nil {
		return db.Space{}, err
	}
	folder := filepath.Join(m.paths.Raw(vault), slug)
	if err := os.MkdirAll(folder, 0o755); err != nil { //nolint:gosec // Foundation createDirectory gives 0755; both servers share the PVC
		return db.Space{}, err
	}
	if category != nil && *category == "" {
		category = nil
	}
	now := m.now()
	var by *uuid.UUID
	if actor != uuid.Nil {
		by = &actor
	}
	s, err := m.q.CreateSpace(ctx, db.CreateSpaceParams{
		ID: uuid.New(), TenantID: vault, Name: name, Slug: slug,
		Description: description, Color: color, Icon: icon, Category: category,
		CreatedByUserID: by, CreatedAt: &now,
	})
	if err != nil {
		// DB write lost: remove the empty folder, as Swift does.
		_ = os.Remove(folder)
		return db.Space{}, err
	}
	slog.InfoContext(ctx, fmt.Sprintf("space created tenant=%s slug=%s", ids.Upper(vault), slug))
	return s, nil
}

func (m *Module) update(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[SpaceUpdateRequest](c)
	if err != nil {
		return err
	}
	actor, _ := auth.Identity(c)
	dto, err := m.Update(c.Request().Context(), a.VaultID, actor, id, UpdateInput{
		Name: body.Name, Description: body.Description, Color: body.Color, Icon: body.Icon, Category: body.Category,
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, dto)
}

func (m *Module) delete(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	id, err := parseID(c)
	if err != nil {
		return err
	}
	actor, _ := auth.Identity(c)
	if _, err := m.Delete(c.Request().Context(), a.VaultID, actor, id); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// load is SpacesService.get: the row plus its live note count.
func (m *Module) load(ctx context.Context, vault, id uuid.UUID) (db.Space, int, error) {
	s, err := m.q.GetSpace(ctx, db.GetSpaceParams{TenantID: vault, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Space{}, 0, errNotFound
	}
	if err != nil {
		return db.Space{}, 0, err
	}
	counts, err := m.noteCounts(ctx, vault, []db.Space{s})
	return s, counts[s.ID], err
}

// noteCounts recomputes note counts from vault_files (HER-105); the stored
// note_count is only refreshed by compiles.
func (m *Module) noteCounts(ctx context.Context, vault uuid.UUID, spaces []db.Space) (map[uuid.UUID]int, error) {
	counts := map[uuid.UUID]int{}
	if len(spaces) == 0 {
		return counts, nil
	}
	idList := make([]uuid.UUID, 0, len(spaces))
	for _, s := range spaces {
		idList = append(idList, s.ID)
	}
	rows, err := m.q.NoteCounts(ctx, db.NoteCountsParams{TenantID: vault, SpaceIds: idList})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		counts[r.SpaceID] = int(r.N)
	}
	return counts, nil
}

func (m *Module) record(ctx context.Context, vault, actor uuid.UUID, action string, target uuid.UUID, title string) error {
	return m.activity.Record(ctx, vaultactivity.Event{
		VaultID: vault, Actor: actor, Action: action, TargetType: "space", TargetID: &target, TargetTitle: &title,
	})
}

// setOrClear: nil keeps the current value, "" clears it, anything else sets it.
func setOrClear(cur, in *string) *string {
	switch {
	case in == nil:
		return cur
	case *in == "":
		return nil
	default:
		return in
	}
}

func toDTO(s db.Space, noteCount int) SpaceDTO {
	dto := SpaceDTO{
		Id: wire.UUID{UUID: s.ID}, Name: s.Name, Slug: s.Slug,
		Description: s.Description, Color: s.Color, Icon: s.Icon, Category: s.Category,
		NoteCount: noteCount,
	}
	if s.LastCompiledAt != nil {
		dto.LastCompiledAt = &wire.Time{Time: *s.LastCompiledAt}
	}
	if s.CreatedAt != nil {
		dto.CreatedAt = &wire.Time{Time: *s.CreatedAt}
	}
	return dto
}

func parseID(c *echo.Context) (uuid.UUID, error) {
	raw := c.Param("id")
	id, err := ids.Parse(raw)
	if err != nil {
		return uuid.Nil, errBadID
	}
	return id, nil
}

// CreateSpace is SpacesService.create with no actor, for the import routes: the same
// validation and folder, the same errors.
func (m *Module) CreateSpace(ctx context.Context, vault uuid.UUID, name, slug string, description, icon *string) (id uuid.UUID, createdSlug string, err error) {
	s, err := m.createSpace(ctx, vault, uuid.Nil, name, slug, description, nil, icon, nil)
	if err != nil {
		return uuid.Nil, "", err
	}
	return s.ID, s.Slug, nil
}
