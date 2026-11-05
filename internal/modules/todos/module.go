// Package todos serves /v1/todos: the Go port of LuminaVaultServer
// Sources/App/Todos/TodosController.swift.
//
// A todo is a vault file whose metadata.isTodo is true, so this API and the note
// browser are two views of the same rows. A standalone todo is a small markdown
// note whose body is its title (decisions.md D-todos).
package todos

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/todos/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/diskguard"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/modules/todos/db"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

var (
	errBadID          = wire.NewError(http.StatusBadRequest, "invalid todo id")
	errNotFound       = wire.NewError(http.StatusNotFound, "todo not found")
	errTitleRequired  = wire.NewError(http.StatusBadRequest, "todo title required")
	errUnknownProject = wire.NewError(http.StatusBadRequest, "unknown project")
)

// distantFuture is Date.distantFuture, where a todo with no due date sorts.
var distantFuture = time.Date(4001, 1, 1, 0, 0, 0, 0, time.UTC)

// Options are the module's optional parts. Without Memories and Embeddings a new
// todo isn't indexed for recall. Without Disk nothing refuses a write.
type Options struct {
	Disk       *diskguard.Guard
	Memories   *memorystore.Repository
	Embeddings embedding.Service
}

type Module struct {
	pool  *pgxpool.Pool
	q     *db.Queries
	store *vaultfiles.Store
	paths vaultpaths.Paths
	opt   Options
	now   func() time.Time
}

func New(pool *pgxpool.Pool, paths vaultpaths.Paths, o Options) *Module {
	return &Module{pool: pool, q: db.New(pool), store: vaultfiles.NewStore(pool, paths), paths: paths, opt: o, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/todos", m.list, authMW)
	r.POST("/v1/todos", m.create, authMW)
	r.PATCH("/v1/todos/:id", m.update, authMW)
	r.DELETE("/v1/todos/:id", m.delete, authMW)
}

func (m *Module) list(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	var doneFilter *bool
	switch wire.Query(c, "done") {
	case "true":
		v := true
		doneFilter = &v
	case "false":
		v := false
		doneFilter = &v
	}
	var projectFilter *uuid.UUID
	if id, err := ids.Parse(wire.Query(c, "projectID")); err == nil {
		projectFilter = &id
	}

	rows, err := m.pool.Query(ctx, `SELECT `+vaultfiles.Columns+` FROM vault_files WHERE tenant_id = $1`, tenant)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := TodoListResponse{Todos: []TodoDTO{}}
	for rows.Next() {
		f, err := vaultfiles.Scan(rows)
		if err != nil {
			return err
		}
		md := f.Metadata
		if md == nil || md.IsTodo == nil || !*md.IsTodo {
			continue
		}
		if doneFilter != nil && isDone(md) != *doneFilter {
			continue
		}
		if projectFilter != nil && (md.ProjectID == nil || *md.ProjectID != *projectFilter) {
			continue
		}
		out.Todos = append(out.Todos, m.toDTO(f))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sort.SliceStable(out.Todos, func(i, j int) bool {
		a, b := out.Todos[i], out.Todos[j]
		if a.Done != b.Done {
			return !a.Done // open before done
		}
		return dueOf(a).Before(dueOf(b))
	})
	return wire.JSON(c, http.StatusOK, out)
}

func isDone(md *vaultfiles.Metadata) bool { return md.Done != nil && *md.Done }

func dueOf(t TodoDTO) time.Time {
	if t.DueAt == nil {
		return distantFuture
	}
	return t.DueAt.Time
}

func (m *Module) create(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	body, err := wire.Bind[TodoCreateRequest](c)
	if err != nil {
		return err
	}
	title := swiftstr.TrimWhitespacesAndNewlines(body.Title)
	if title == "" {
		return errTitleRequired
	}
	if err := m.validateProject(ctx, body.ProjectID, tenant); err != nil {
		return err
	}

	// QUIRK, as in Swift: the file's name uses its own UUID, not the row's id.
	relative, err := vaultfiles.SanitizePath("inbox/" + ids.Upper(uuid.New()) + ".md")
	if err != nil {
		return err
	}
	data := []byte(title)
	if m.opt.Disk != nil {
		if err := m.opt.Disk.Check(int64(len(data))); err != nil {
			return err
		}
	}
	if err := m.paths.EnsureTenantDirectories(tenant); err != nil {
		return err
	}
	target, err := vaultfiles.ResolveInside(m.paths.Raw(tenant), relative)
	if err != nil {
		return err
	}
	if err := vaultfiles.CreateParent(target); err != nil {
		return err
	}
	if err := vaultfiles.WriteAtomic(target, data); err != nil {
		return err
	}

	sum := sha256.Sum256(data)
	isTodo, done := true, false
	md := &vaultfiles.Metadata{Title: &title, IsTodo: &isTodo, Done: &done}
	if body.DueAt != nil {
		md.DueAt = &body.DueAt.Time
	}
	if body.ProjectID != nil {
		md.ProjectID = &body.ProjectID.UUID
	}
	id, err := m.store.Upsert(ctx, vaultfiles.UpsertParams{
		TenantID: tenant, Path: relative, ContentType: "text/markdown",
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Processed: true, Metadata: md,
	})
	if err != nil {
		return err
	}
	row, err := m.store.FindByID(ctx, tenant, id)
	if err != nil {
		return err
	}
	if row == nil {
		return errNotFound
	}
	m.indexForRecall(ctx, title, id, tenant)
	return wire.JSON(c, http.StatusOK, m.toDTO(row))
}

// indexForRecall embeds the title so the todo surfaces in chat, as a note-todo
// does. Best effort: the row already exists.
func (m *Module) indexForRecall(ctx context.Context, title string, id, tenant uuid.UUID) {
	if m.opt.Memories == nil || m.opt.Embeddings == nil {
		return
	}
	vec, err := embedding.EmbedForWrite(ctx, m.opt.Embeddings, title, tenant, "todo.create")
	if err == nil {
		_, err = m.opt.Memories.Create(ctx, memorystore.CreateParams{
			Tenant: tenant, Content: title, Embedding: vec, SourceVaultFileID: &id, ReviewState: memorystore.ReviewAuto,
		})
	}
	if err != nil {
		slog.ErrorContext(ctx, "todo memory create failed", "tenant", ids.Upper(tenant), "error", err)
	}
}

// update also takes any other vault file of the tenant and makes it a todo. A null
// or missing field stays as it is, so a due date or project can't be cleared.
func (m *Module) update(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return errBadID
	}
	patch, err := wire.Bind[TodoPatchRequest](c)
	if err != nil {
		return err
	}
	row, err := m.store.FindByID(ctx, tenant, id)
	if err != nil {
		return err
	}
	if row == nil {
		return errNotFound
	}
	if err := m.validateProject(ctx, patch.ProjectID, tenant); err != nil {
		return err
	}
	md := row.Metadata
	if md == nil {
		md = &vaultfiles.Metadata{}
	}
	isTodo := true
	md.IsTodo = &isTodo
	if patch.Title != nil {
		md.Title = patch.Title
	}
	if patch.Done != nil {
		md.Done = patch.Done
	}
	if patch.DueAt != nil {
		md.DueAt = &patch.DueAt.Time
	}
	if patch.ProjectID != nil {
		md.ProjectID = &patch.ProjectID.UUID
	}
	row.Metadata = md
	if err := m.store.UpdateMetadata(ctx, row.ID, md); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, m.toDTO(row))
}

// delete also takes any other vault file of the tenant. The file's rename and the
// memory's cascade are best effort: the row goes either way.
func (m *Module) delete(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return errBadID
	}
	row, err := m.store.FindByID(ctx, tenant, id)
	if err != nil {
		return err
	}
	if row == nil {
		return errNotFound
	}
	rawRoot := m.paths.Raw(tenant)
	// row.Path comes from the database and isn't sanitized again, as in Swift.
	if target, err := vaultfiles.ResolveInside(rawRoot, row.Path); err == nil {
		if err := vaultfiles.SoftDeleteFile(rawRoot, target, row.Path, m.now()); err != nil {
			slog.WarnContext(ctx, "todo soft delete failed", "tenant", ids.Upper(tenant), "error", err)
		}
	}
	if m.opt.Memories != nil {
		if _, err := m.opt.Memories.DeleteBySourceVaultFileID(ctx, tenant, row.ID); err != nil {
			slog.WarnContext(ctx, "todo memory delete failed", "tenant", ids.Upper(tenant), "error", err)
		}
	}
	if err := m.store.DeleteRow(ctx, row.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) validateProject(ctx context.Context, projectID *wire.UUID, tenant uuid.UUID) error {
	if projectID == nil {
		return nil
	}
	exists, err := m.q.ProjectExists(ctx, db.ProjectExistsParams{ID: projectID.UUID, TenantID: tenant})
	if err != nil {
		return err
	}
	if !exists {
		return errUnknownProject
	}
	return nil
}

func (m *Module) toDTO(f *vaultfiles.VaultFile) TodoDTO {
	dto := TodoDTO{Id: wire.UUID{UUID: f.ID}, Title: vaultfiles.LastPathComponent(f.Path)}
	if md := f.Metadata; md != nil {
		if md.Title != nil && *md.Title != "" {
			dto.Title = *md.Title
		}
		dto.Done = isDone(md)
		if md.DueAt != nil {
			dto.DueAt = &wire.Time{Time: *md.DueAt}
		}
		if md.ProjectID != nil {
			dto.ProjectID = &wire.UUID{UUID: *md.ProjectID}
		}
	}
	created := m.now()
	if f.CreatedAt != nil {
		created = *f.CreatedAt
	}
	dto.CreatedAt = wire.Time{Time: created}
	return dto
}
