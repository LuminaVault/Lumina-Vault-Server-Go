package imports

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/capture"
	"lumina/internal/ids"
	"lumina/internal/memorycompile"
	"lumina/internal/modules/imports/db"
	"lumina/internal/modules/kbcompile"
	"lumina/internal/modules/spaces"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits and names from ImportService.
const (
	MaxBatch         = 500
	ImportedSlug     = "imported"
	enrichConcurrent = 4
	maxInlineEnrich  = 50
)

// ImportStatus and ImportItemStatus values.
const (
	StatusStaging   = "staging"
	StatusEnriching = "enriching"
	StatusReview    = "review"
	StatusFiling    = "filing"
	StatusCompiling = "compiling"
	StatusDone      = "done"

	ItemStaged  = "staged"
	ItemSkipped = "skipped"
)

var errSessionNotFound = wire.NewError(http.StatusNotFound, "import session not found")

// Links is the part of the link-capture pipeline import stages through.
type Links interface {
	CaptureLink(ctx context.Context, tenant uuid.UUID, url string, note *string, spaceID *uuid.UUID) (capture.CapturedLink, error)
}

// Enricher is URLEnrichmentService.enrichAndRewrite, which never fails.
type Enricher interface {
	EnrichAndRewrite(ctx context.Context, fileID uuid.UUID, url string, tenant uuid.UUID)
}

// Compiler is the scoped compile approve runs.
type Compiler interface {
	CompileExistingVaultFiles(ctx context.Context, tenant uuid.UUID, sessionKey string, rows []*vaultfiles.VaultFile, hint *string, runID uuid.UUID) (memorycompile.Result, error)
}

// Service is ImportService.
type Service struct {
	pool    *pgxpool.Pool
	q       *db.Queries
	links   Links
	spaces  *spaces.Module
	paths   vaultpaths.Paths
	store   *vaultfiles.Store
	compile Compiler
	enrich  Enricher
}

// NewService builds the service.
func NewService(pool *pgxpool.Pool, links Links, sp *spaces.Module, paths vaultpaths.Paths, compile Compiler, enrich Enricher) *Service {
	return &Service{
		pool: pool, q: db.New(pool), links: links, spaces: sp, paths: paths,
		store: vaultfiles.NewStore(pool, paths), compile: compile, enrich: enrich,
	}
}

// bookmarkHref is `(?i)href\s*=\s*["']([^"']+)["']`, with ICU's \s spelled out: RE2's
// is ASCII only.
var bookmarkHref = regexp.MustCompile(`(?i)href[\t\n\f\r\p{Z}]*=[\t\n\f\r\p{Z}]*["']([^"']+)["']`)

// ParseBookmarksHTML is parseBookmarksHTML: http(s) hrefs, first-seen order, no repeats.
func ParseBookmarksHTML(html string) []string {
	urls := []string{}
	seen := map[string]bool{}
	for _, match := range bookmarkHref.FindAllStringSubmatch(html, -1) {
		u := match[1]
		if !swiftstr.HasASCIIPrefix(u, "http://") && !swiftstr.HasASCIIPrefix(u, "https://") {
			continue
		}
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return urls
}

// StageResult is StageResult.
type StageResult struct {
	SessionID uuid.UUID
	Total     int
	Staged    int
	Skipped   int
}

// ImportLinks is importLinks. A capture failure other than a bad URL leaves the session
// in staging and answers with that failure, as in Swift.
func (s *Service) ImportLinks(ctx context.Context, tenant uuid.UUID, sourceType string, urls []string) (StageResult, error) {
	imported, err := s.ensureImportedSpace(ctx, tenant)
	if err != nil {
		return StageResult{}, err
	}
	session := uuid.New()
	err = s.q.InsertSession(ctx, db.InsertSessionParams{
		ID: session, TenantID: tenant, SourceType: sourceType, Status: StatusStaging,
		TotalItems: int64(len(urls)), CreatedAt: ptrTime(time.Now()),
	})
	if err != nil {
		return StageResult{}, err
	}
	prior, err := s.q.PriorURLs(ctx, tenant)
	if err != nil {
		return StageResult{}, err
	}
	seen := make(map[string]bool, len(prior))
	for _, u := range prior {
		if u != nil {
			seen[*u] = true
		}
	}
	staged, skipped := 0, 0
	for _, raw := range urls {
		url := swiftstr.TrimWhitespacesAndNewlines(raw)
		if url == "" {
			continue
		}
		if seen[url] {
			skipped++
			continue
		}
		seen[url] = true
		item := db.InsertItemParams{ID: uuid.New(), TenantID: tenant, SessionID: session, Url: &url}
		captured, err := s.links.CaptureLink(ctx, tenant, url, nil, &imported)
		switch {
		case err == nil:
			item.VaultFileID, item.Status = &captured.FileID, ItemStaged
			staged++
		case errors.Is(err, capture.ErrInvalidURL), errors.Is(err, capture.ErrNonPublicHost):
			item.Status = ItemSkipped
			skipped++
		default:
			return StageResult{}, err
		}
		item.CreatedAt = ptrTime(time.Now())
		if err := s.q.InsertItem(ctx, item); err != nil {
			return StageResult{}, err
		}
	}
	err = s.q.FinishStaging(ctx, db.FinishStagingParams{ID: session, Status: StatusEnriching, StagedItems: int64(staged), UpdatedAt: ptrTime(time.Now())})
	if err != nil {
		return StageResult{}, err
	}
	slog.InfoContext(ctx, "import staged", "tenant", tenant, "session", session, "staged", staged, "skipped", skipped)
	return StageResult{SessionID: session, Total: len(urls), Staged: staged, Skipped: skipped}, nil
}

// ImportFiles is importFiles. An id the tenant doesn't own is a skip.
func (s *Service) ImportFiles(ctx context.Context, tenant uuid.UUID, sourceType string, files []uuid.UUID) (StageResult, error) {
	session := uuid.New()
	err := s.q.InsertSession(ctx, db.InsertSessionParams{
		ID: session, TenantID: tenant, SourceType: sourceType, Status: StatusStaging,
		TotalItems: int64(len(files)), CreatedAt: ptrTime(time.Now()),
	})
	if err != nil {
		return StageResult{}, err
	}
	staged := 0
	for _, id := range files {
		file, err := s.store.FindByID(ctx, tenant, id)
		if err != nil {
			return StageResult{}, err
		}
		if file == nil {
			continue
		}
		title := vaultfiles.LastPathComponent(file.Path)
		err = s.q.InsertItem(ctx, db.InsertItemParams{
			ID: uuid.New(), TenantID: tenant, SessionID: session, VaultFileID: &id,
			Title: &title, Status: ItemStaged, CreatedAt: ptrTime(time.Now()),
		})
		if err != nil {
			return StageResult{}, err
		}
		staged++
	}
	err = s.q.FinishStaging(ctx, db.FinishStagingParams{ID: session, Status: StatusEnriching, StagedItems: int64(staged), UpdatedAt: ptrTime(time.Now())})
	if err != nil {
		return StageResult{}, err
	}
	return StageResult{SessionID: session, Total: len(files), Staged: staged, Skipped: len(files) - staged}, nil
}

// ApproveResult is ApproveResult.
type ApproveResult struct {
	Filed    int
	Memories int
}

// Approve is approve: files each item into its Space, moves the bytes under
// raw/<slug>/, enriches pending links and compiles what's still unprocessed. The move
// is best effort, as Swift's `try?`s are, and the row takes the new path either way.
//
// The paths are rawRoot + vf.path as read from the row, with no SanitizePath: stored
// paths passed it on the way in, and Swift doesn't check here either.
func (s *Service) Approve(ctx context.Context, tenant, session uuid.UUID, overrides map[string]string) (ApproveResult, error) {
	if _, err := s.findSession(ctx, tenant, session); err != nil {
		return ApproveResult{}, err
	}
	if err := s.setStatus(ctx, session, StatusFiling); err != nil {
		return ApproveResult{}, err
	}
	items, err := s.q.ActiveItems(ctx, db.ActiveItemsParams{TenantID: tenant, SessionID: session})
	if err != nil {
		return ApproveResult{}, err
	}
	rawRoot := s.paths.Raw(tenant)
	slugCache := map[string]uuid.UUID{}
	compiled := []uuid.UUID{}
	filed := 0
	for _, item := range items {
		if item.VaultFileID == nil {
			continue
		}
		file, err := s.store.FindByID(ctx, tenant, *item.VaultFileID)
		if err != nil {
			return ApproveResult{}, err
		}
		if file == nil {
			continue
		}
		target, ok := overrides[ids.Upper(item.ID)]
		if !ok {
			target = ImportedSlug
			if item.ProposedSpace != nil {
				target = *item.ProposedSpace
			}
		}
		dest, err := s.resolveDestSlug(ctx, tenant, target)
		if err != nil {
			return ApproveResult{}, err
		}
		spaceID, err := s.spaceID(ctx, tenant, dest, slugCache)
		if err != nil {
			return ApproveResult{}, err
		}
		newRel := dest + "/" + vaultfiles.LastPathComponent(file.Path)
		if file.Path != newRel {
			src, dst := rawRoot+"/"+file.Path, rawRoot+"/"+newRel
			_ = vaultfiles.CreateParent(dst)
			if vaultfiles.Exists(src) {
				if vaultfiles.Exists(dst) {
					_ = os.RemoveAll(dst)
				}
				_ = os.Rename(src, dst)
			}
		}
		if err := s.store.UpdateFiled(ctx, file.ID, newRel, spaceID); err != nil {
			return ApproveResult{}, err
		}
		compiled = append(compiled, file.ID)
		if err := s.q.SetItemFiled(ctx, item.ID); err != nil {
			return ApproveResult{}, err
		}
		filed++
	}

	s.enrichLinks(ctx, tenant, items)

	if err := s.setStatus(ctx, session, StatusCompiling); err != nil {
		return ApproveResult{}, err
	}
	memories := 0
	if len(compiled) > 0 {
		rows, err := s.unprocessed(ctx, tenant, compiled)
		if err != nil {
			return ApproveResult{}, err
		}
		if len(rows) > 0 {
			result, err := s.compile.CompileExistingVaultFiles(ctx, tenant, ids.Upper(tenant), rows, nil, uuid.New())
			if err != nil {
				return ApproveResult{}, err
			}
			memories = len(result.Memories)
		}
	}
	if err := s.setStatus(ctx, session, StatusDone); err != nil {
		return ApproveResult{}, err
	}
	return ApproveResult{Filed: filed, Memories: memories}, nil
}

func (s *Service) unprocessed(ctx context.Context, tenant uuid.UUID, fileIDs []uuid.UUID) ([]*vaultfiles.VaultFile, error) {
	all, err := kbcompile.ResolveRows(ctx, s.pool, tenant, wrapIDs(fileIDs), false)
	if err != nil {
		return nil, err
	}
	rows := all[:0]
	for _, f := range all {
		if f.ProcessedAt == nil {
			rows = append(rows, f)
		}
	}
	return rows, nil
}

func wrapIDs(in []uuid.UUID) []wire.UUID {
	out := make([]wire.UUID, len(in))
	for i, id := range in {
		out[i] = wire.UUID{UUID: id}
	}
	return out
}

// enrichLinks is enrichLinks: the session's links whose file is still pending, the
// first 50 of them, four at a time. A row that can't be read is skipped.
func (s *Service) enrichLinks(ctx context.Context, tenant uuid.UUID, items []db.ActiveItemsRow) {
	type job struct {
		file uuid.UUID
		url  string
	}
	jobs := []job{}
	for _, item := range items {
		if item.Url == nil || item.VaultFileID == nil {
			continue
		}
		file, err := s.store.FindByID(ctx, tenant, *item.VaultFileID)
		if err != nil || file == nil || file.Metadata == nil || file.Metadata.EnrichmentStatus == nil || *file.Metadata.EnrichmentStatus != "pending" {
			continue
		}
		jobs = append(jobs, job{file.ID, *item.Url})
	}
	if len(jobs) > maxInlineEnrich {
		jobs = jobs[:maxInlineEnrich]
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, enrichConcurrent)
	for _, j := range jobs {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.enrich.EnrichAndRewrite(ctx, j.file, j.url, tenant)
		}()
	}
	wg.Wait()
}

// Status is status: the session and its items, oldest first.
func (s *Service) Status(ctx context.Context, tenant, session uuid.UUID) (db.FindSessionRow, []db.SessionItemsRow, error) {
	row, err := s.findSession(ctx, tenant, session)
	if err != nil {
		return db.FindSessionRow{}, nil, err
	}
	items, err := s.q.SessionItems(ctx, db.SessionItemsParams{TenantID: tenant, SessionID: session})
	return row, items, err
}

func (s *Service) findSession(ctx context.Context, tenant, session uuid.UUID) (db.FindSessionRow, error) {
	row, err := s.q.FindSession(ctx, db.FindSessionParams{TenantID: tenant, ID: session})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.FindSessionRow{}, errSessionNotFound
	}
	return row, err
}

func (s *Service) setStatus(ctx context.Context, session uuid.UUID, status string) error {
	return s.q.SetSessionStatus(ctx, db.SetSessionStatusParams{ID: session, Status: status, UpdatedAt: ptrTime(time.Now())})
}

// resolveDestSlug is resolveDestSlug: slug, new:Name or imported to a real slug. A new
// Space that can't be created is swallowed, and the slug is still the destination.
func (s *Service) resolveDestSlug(ctx context.Context, tenant uuid.UUID, target string) (string, error) {
	if target == "" || target == ImportedSlug {
		return ImportedSlug, nil
	}
	if swiftstr.HasASCIIPrefix(target, "new:") {
		name := swiftstr.TrimWhitespaces(strings.TrimPrefix(target, "new:"))
		slug := Slugify(name)
		found, err := s.spaceExists(ctx, tenant, slug)
		if err != nil {
			return "", err
		}
		if !found {
			display := name
			if display == "" {
				display = slug
			}
			if _, _, err := s.spaces.CreateSpace(ctx, tenant, display, slug, nil, nil); err != nil {
				slog.WarnContext(ctx, "import could not create proposed space", "slug", slug, "error", err)
			}
		}
		return slug, nil
	}
	found, err := s.spaceExists(ctx, tenant, target)
	if err != nil {
		return "", err
	}
	if found {
		return target, nil
	}
	return ImportedSlug, nil
}

func (s *Service) spaceExists(ctx context.Context, tenant uuid.UUID, slug string) (bool, error) {
	_, err := s.q.SpaceIDBySlug(ctx, db.SpaceIDBySlugParams{TenantID: tenant, Slug: slug})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) spaceID(ctx context.Context, tenant uuid.UUID, slug string, cache map[string]uuid.UUID) (uuid.UUID, error) {
	if id, ok := cache[slug]; ok {
		return id, nil
	}
	id, err := s.q.SpaceIDBySlug(ctx, db.SpaceIDBySlugParams{TenantID: tenant, Slug: slug})
	if errors.Is(err, pgx.ErrNoRows) {
		id, err = s.ensureImportedSpace(ctx, tenant)
	}
	if err != nil {
		return uuid.Nil, err
	}
	cache[slug] = id
	return id, nil
}

func (s *Service) ensureImportedSpace(ctx context.Context, tenant uuid.UUID) (uuid.UUID, error) {
	id, err := s.q.SpaceIDBySlug(ctx, db.SpaceIDBySlugParams{TenantID: tenant, Slug: ImportedSlug})
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	description, icon := "Inbox for imported items awaiting Smart Import.", "tray"
	id, _, err = s.spaces.CreateSpace(ctx, tenant, "Imported", ImportedSlug, &description, &icon)
	return id, err
}

// Slugify is slugify: lowercase letters and numbers, runs of anything else one hyphen,
// at most 31 characters, "imported" when under two.
func Slugify(name string) string {
	var out []string
	lastDash := false
	for _, ch := range swiftstr.Characters(vaultfiles.Lowercased(name)) {
		if swiftstr.IsLetterOrNumber(ch) {
			out = append(out, ch)
			lastDash = false
		} else if !lastDash {
			out = append(out, "-")
			lastDash = true
		}
	}
	trimmed := strings.Trim(strings.Join(out, ""), "-")
	result := swiftstr.Prefix(trimmed, 31)
	if swiftstr.Count(result) < 2 {
		return ImportedSlug
	}
	return result
}

func ptrTime(t time.Time) *time.Time { return &t }
