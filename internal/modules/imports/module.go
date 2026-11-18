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
	"lumina/internal/notionimport"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const (
	maxBookmarksBytes = 8 * 1024 * 1024
	maxBulkBytes      = 96 * 1024 * 1024
	maxBulkFiles      = 2000
	notionPath        = "/v1/vault/import/notion"
	// notionSpoolAge is how old a leftover upload must be before the next
	// import removes it: longer than any import runs.
	notionSpoolAge = 6 * time.Hour
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
	// E7: the Notion ZIP is up to 200 MB, which no phone sends in the server's
	// 30 s read timeout.
	r.POST(notionPath, m.notion, httpserver.LongUpload(), m.identify)
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
		Space      string      `json:"space"`
		Files      []FileInput `json:"files"`
		OnConflict *string     `json:"onConflict"`
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
	opts := IngestOptions{OnConflict: OnConflictOverwrite}
	if body.OnConflict != nil {
		switch VaultBulkImportRequestOnConflict(*body.OnConflict) {
		case VaultBulkImportRequestOnConflictOverwrite:
		case VaultBulkImportRequestOnConflictSkip:
			opts.OnConflict = OnConflictSkip
		default:
			return wire.NewError(http.StatusBadRequest, "invalid_on_conflict")
		}
	}
	result, err := m.ingest.IngestBatchWith(c.Request().Context(), tenant, body.Space, body.Files, opts)
	if err != nil {
		return err
	}
	files := make([]VaultBulkImportFileResult, len(result.Files))
	for n, f := range result.Files {
		files[n] = VaultBulkImportFileResult{
			Path: f.Path, SavedPath: f.SavedPath, Status: VaultBulkImportFileResultStatus(f.Status),
		}
	}
	return wire.JSON(c, http.StatusOK, VaultBulkImportResponse{
		SpaceID: wire.UUID{UUID: result.SpaceID}, SpaceSlug: result.SpaceSlug,
		Imported: result.Imported, Skipped: result.Skipped, Failed: result.Failed, Files: files,
	})
}

// notion is POST /v1/vault/import/notion (backlog E7): the raw ZIP of a Notion
// export in, the report out. Like vault-bulk it asks for identity alone and
// writes to the caller's own vault.
//
// The upload goes to a temp file on the vault's volume, not into memory: the
// pod has less memory than the 200 MB this route accepts.
func (m *Module) notion(c *echo.Context) error {
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	opts := NotionOptions{Space: wire.Query(c, "space"), OnConflict: OnConflictSkip}
	switch wire.Query(c, "dryRun") {
	case "", "false", "0":
	case "true", "1":
		opts.DryRun = true
	default:
		return wire.NewError(http.StatusBadRequest, "invalid_dry_run")
	}
	switch VaultImportNotionParamsOnConflict(wire.Query(c, "onConflict")) {
	case "", VaultImportNotionParamsOnConflictSkip:
	case VaultImportNotionParamsOnConflictOverwrite:
		opts.OnConflict = OnConflictOverwrite
	default:
		return wire.NewError(http.StatusBadRequest, "invalid_on_conflict")
	}
	r := c.Request()
	if r.ContentLength > notionimport.MaxCompressedBytes {
		return wire.ErrBodyTooLarge
	}
	if m.ingest.guard != nil {
		if err := m.ingest.guard.Check(max(r.ContentLength, 0)); err != nil {
			return err
		}
	}
	dir, err := m.ingest.notionSpool()
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	upload, err := os.Create(filepath.Join(dir, "upload.zip")) //nolint:gosec // dir is ours, from MkdirTemp
	if err != nil {
		return err
	}
	defer func() { _ = upload.Close() }()
	size, err := io.Copy(upload, io.LimitReader(r.Body, notionimport.MaxCompressedBytes+1))
	if err != nil {
		return err
	}
	if size > notionimport.MaxCompressedBytes {
		return wire.ErrBodyTooLarge
	}
	report, err := m.ingest.ImportNotion(r.Context(), tenant, upload, size, dir, opts)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, report)
}

// notionSpool makes a private temp directory for one upload, beside the
// tenants' data so the disk guard's volume is the one it fills. Directories a
// crashed import left behind are removed once they are old.
func (i *Ingest) notionSpool() (string, error) {
	root := filepath.Join(i.paths.Root, "tmp", "notion-import")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", err
	}
	if left, err := os.ReadDir(root); err == nil {
		for _, entry := range left {
			if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > notionSpoolAge {
				_ = os.RemoveAll(filepath.Join(root, entry.Name()))
			}
		}
	}
	return os.MkdirTemp(root, "upload-*")
}
