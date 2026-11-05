// Package imports holds /v1/import: staging link batches and uploaded files, Smart
// Import's categorize and approve, and the bulk vault import. All of it sits behind JWT
// alone: no BYO Hermes, entitlement or limit, and each handler asks for identity first.
package imports

import (
	"bytes"
	"io"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const (
	maxBookmarksBytes = 8 * 1024 * 1024
	maxBulkBytes      = 96 * 1024 * 1024
	maxBulkFiles      = 2000
)

// Module is the routes.
type Module struct {
	service     *Service
	categorizer *Categorizer
	ingest      *Ingest
	identify    echo.MiddlewareFunc
}

// New builds the module.
func New(service *Service, categorizer *Categorizer, ingest *Ingest, identify echo.MiddlewareFunc) *Module {
	return &Module{service: service, categorizer: categorizer, ingest: ingest, identify: identify}
}

// Register mounts the routes. Hummingbird's trie answers a whole path that ends on a
// constant child from that node, so GET /v1/import/files is a 404, not :id's 400.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.POST("/v1/import", m.create, m.identify)
	r.POST("/v1/import/bookmarks", m.bookmarks, m.identify)
	r.POST("/v1/import/files", m.files, m.identify)
	r.POST("/v1/import/vault-bulk", m.bulk, m.identify)
	r.GET("/v1/import/:id", m.status, m.identify)
	r.POST("/v1/import/:id/categorize", m.categorize, m.identify)
	r.POST("/v1/import/:id/approve", m.approve, m.identify)
	notFound := func(*echo.Context) error { return echo.ErrNotFound }
	for _, literal := range []string{"bookmarks", "files", "vault-bulk"} {
		r.GET("/v1/import/"+literal, notFound)
	}
}

func sessionID(c *echo.Context) (uuid.UUID, error) {
	id, err := ids.Parse(c.Param("id"))
	if err != nil {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid import session id")
	}
	return id, nil
}

func stageResponse(r StageResult) ImportCreateResponse {
	return ImportCreateResponse{
		SessionId: wire.UUID{UUID: r.SessionID}, Status: StatusEnriching,
		Total: r.Total, Staged: r.Staged, Skipped: r.Skipped,
	}
}

// collect is collectBody(upTo:). Past the limit it's the same bare 413 as ReadBody's.
func collect(c *echo.Context, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(c.Request().Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, wire.ErrBodyTooLarge
	}
	return raw, nil
}

func (m *Module) create(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[ImportCreateRequest](c)
	if err != nil {
		return err
	}
	if len(body.Urls) == 0 {
		return wire.NewError(http.StatusBadRequest, "urls required")
	}
	if len(body.Urls) > MaxBatch {
		return wire.NewError(http.StatusBadRequest, "max "+strconv.Itoa(MaxBatch)+" urls per import")
	}
	source := body.SourceType
	if source == "" {
		source = "bookmarks"
	}
	result, err := m.service.ImportLinks(c.Request().Context(), tenant, source, body.Urls)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, stageResponse(result))
}

func (m *Module) bookmarks(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	raw, err := collect(c, maxBookmarksBytes)
	if err != nil {
		return err
	}
	// String(buffer:) repairs invalid UTF-8.
	urls := ParseBookmarksHTML(string(bytes.ToValidUTF8(raw, []byte("�"))))
	if len(urls) == 0 {
		return wire.NewError(http.StatusBadRequest, "no http(s) links found in bookmarks file")
	}
	result, err := m.service.ImportLinks(c.Request().Context(), tenant, "bookmarks", urls[:min(len(urls), MaxBatch)])
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, stageResponse(result))
}

func (m *Module) files(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[ImportFilesRequest](c)
	if err != nil {
		return err
	}
	if len(body.VaultFileIds) == 0 {
		return wire.NewError(http.StatusBadRequest, "vaultFileIds required")
	}
	if len(body.VaultFileIds) > MaxBatch {
		return wire.NewError(http.StatusBadRequest, "max "+strconv.Itoa(MaxBatch)+" files per import")
	}
	source := body.SourceType
	if source == "" {
		source = "documents"
	}
	fileIDs := make([]uuid.UUID, len(body.VaultFileIds))
	for i, id := range body.VaultFileIds {
		fileIDs[i] = id.UUID
	}
	result, err := m.service.ImportFiles(c.Request().Context(), tenant, source, fileIDs)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, stageResponse(result))
}

func (m *Module) categorize(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	session, err := sessionID(c)
	if err != nil {
		return err
	}
	if err := m.categorizer.Categorize(c.Request().Context(), tenant, session); err != nil {
		return err
	}
	return m.statusResponse(c, tenant, session)
}

func (m *Module) approve(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	session, err := sessionID(c)
	if err != nil {
		return err
	}
	// `try? req.decode`: any failure at all is no overrides.
	overrides := map[string]string{}
	if body, err := wire.Bind[ImportApproveRequest](c); err == nil && body.Overrides != nil {
		overrides = *body.Overrides
	}
	result, err := m.service.Approve(c.Request().Context(), tenant, session, overrides)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, ImportApproveResponse{
		SessionId: wire.UUID{UUID: session}, Status: StatusDone, Filed: result.Filed, MemoriesIngested: result.Memories,
	})
}

func (m *Module) status(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	session, err := sessionID(c)
	if err != nil {
		return err
	}
	return m.statusResponse(c, tenant, session)
}

func (m *Module) statusResponse(c *echo.Context, tenant, session uuid.UUID) error {
	row, items, err := m.service.Status(c.Request().Context(), tenant, session)
	if err != nil {
		return err
	}
	list := make([]ImportItemDTO, len(items))
	for i, item := range items {
		list[i] = ImportItemDTO{Id: wire.UUID{UUID: item.ID}, Url: item.Url, Title: item.Title, ProposedSpace: item.ProposedSpace, Status: item.Status}
	}
	return wire.JSON(c, http.StatusOK, ImportStatusResponse{
		Id: wire.UUID{UUID: row.ID}, SourceType: row.SourceType, Status: row.Status,
		Total: int(row.TotalItems), Staged: int(row.StagedItems), Items: list,
	})
}

func (m *Module) bulk(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	raw, err := collect(c, maxBulkBytes)
	if err != nil {
		return err
	}
	body, err := wire.Decode[struct {
		Space string      `json:"space"`
		Files []FileInput `json:"files"`
	}](raw)
	if err != nil {
		return wire.NewError(http.StatusBadRequest, "invalid_body")
	}
	if isBlank(body.Space) {
		return wire.NewError(http.StatusBadRequest, "space_required")
	}
	if len(body.Files) == 0 {
		return wire.NewError(http.StatusBadRequest, "no_files")
	}
	if len(body.Files) > maxBulkFiles {
		return wire.NewError(http.StatusBadRequest, "too_many_files_max_"+strconv.Itoa(maxBulkFiles))
	}
	result, err := m.ingest.IngestBatch(c.Request().Context(), tenant, body.Space, body.Files)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, map[string]any{
		"spaceID": ids.Upper(result.SpaceID), "spaceSlug": result.SpaceSlug,
		"imported": result.Imported, "skipped": result.Skipped, "failed": result.Failed,
	})
}
