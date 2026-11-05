package vault

import (
	"context"
	"fmt"
	"log/slog"
	"lumina/internal/chunkindex"
	"lumina/internal/ids"
	"lumina/internal/modules/vault/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// VaultIndexController and the Indexing services behind it. Every route here is a
// pure read: none of them index, embed or re-resolve anything.
const (
	samplePathLimit     = 20
	defaultSiblingLimit = 25
	maxSiblingLimit     = 100
	maxFilesScanned     = 2000
	maxFindingsPerCheck = 50
	oversizedBytes      = 5 * 1024 * 1024
)

var errNoFileWithID = wire.NewError(http.StatusNotFound, "no vault file with that id")

// linksResponse is VaultLinksResponse. chunkindex's link rows already carry
// Swift's wire names.
type linksResponse struct {
	VaultFileID wire.UUID                 `json:"vaultFileID"`
	Path        string                    `json:"path"`
	Outgoing    []chunkindex.OutgoingLink `json:"outgoing"`
	Incoming    []chunkindex.IncomingLink `json:"incoming"`
}

func (m *Module) indexStatus(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	status, err := m.indexStatusOf(c.Request().Context(), a.VaultID)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, status)
}

// indexStatusOf is VaultIndexStatusService.status.
func (m *Module) indexStatusOf(ctx context.Context, tenant uuid.UUID) (VaultIndexStatus, error) {
	var out VaultIndexStatus
	counts, err := m.q.IndexCounts(ctx, tenant)
	if err != nil {
		return out, err
	}
	lastIndexedAt, err := m.q.LastIndexedAt(ctx, tenant)
	if err != nil {
		return out, err
	}
	unindexed, err := m.q.UnindexedPaths(ctx, tenant)
	if err != nil {
		return out, err
	}
	stale, err := m.q.StalePaths(ctx, tenant)
	if err != nil {
		return out, err
	}
	links, err := m.links.Counts(ctx, tenant)
	if err != nil {
		return out, err
	}
	var lastIndexed *time.Time
	if lastIndexedAt.Valid {
		lastIndexed = &lastIndexedAt.Time
	}
	reasons := []string{}
	if len(unindexed) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d document(s) have no chunks — run the chunk backfill", len(unindexed)))
	}
	if len(stale) > 0 {
		reasons = append(reasons, fmt.Sprintf("%d document(s) changed since they were indexed", len(stale)))
	}
	if links.Ambiguous > 0 {
		reasons = append(reasons, fmt.Sprintf("%d wiki link(s) match more than one document", links.Ambiguous))
	}
	return VaultIndexStatus{
		DocumentCount:          int(counts.Documents),
		MemoryCount:            int(counts.Memories),
		ChunkCount:             int(counts.Chunks),
		ResolvedLinkCount:      links.Resolved,
		UnresolvedLinkCount:    links.Unresolved,
		AmbiguousLinkCount:     links.Ambiguous,
		UnindexedDocumentCount: len(unindexed),
		StaleDocumentCount:     len(stale),
		UnindexedSample:        firstN(unindexed, samplePathLimit),
		StaleSample:            firstN(stale, samplePathLimit),
		LastIndexedAt:          wireTime(lastIndexed),
		Stale:                  len(reasons) > 0,
		StaleReasons:           reasons,
	}, nil
}

// context is VaultNavigationService.context.
func (m *Module) context(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	id, err := parseVaultFileID(c)
	if err != nil {
		return err
	}
	siblingLimit := defaultSiblingLimit
	if v, ok := wire.QueryInt(c, "siblingLimit"); ok {
		siblingLimit = v
	}
	siblingLimit = max(1, min(siblingLimit, maxSiblingLimit))

	ctx := c.Request().Context()
	file, err := m.store.FindByID(ctx, tenant, id)
	if err != nil {
		return err
	}
	if file == nil {
		return errNoFileWithID
	}
	headingRows, err := m.q.HeadingRows(ctx, db.HeadingRowsParams{TenantID: tenant, VaultFileID: &id})
	if err != nil {
		return err
	}
	headings := []VaultContextHeading{}
	for _, row := range headingRows {
		path := chunkindex.DecodeHeadingPath(&row.HeadingPathJson)
		if len(path) > 0 {
			headings = append(headings, VaultContextHeading{Path: path, StartLine: int(row.StartLine)})
		}
	}
	chunkCount, err := m.q.ChunkCount(ctx, db.ChunkCountParams{TenantID: tenant, VaultFileID: &id})
	if err != nil {
		return err
	}
	// An unfiled document has no Space and so no siblings, not "every unfiled note".
	siblings := []VaultContextNeighbour{}
	if file.SpaceID != nil {
		rows, err := m.q.Siblings(ctx, db.SiblingsParams{TenantID: tenant, SpaceID: file.SpaceID, ID: id, Limit: int32(siblingLimit)}) //nolint:gosec // clamped to 1...100
		if err != nil {
			return err
		}
		for _, row := range rows {
			siblings = append(siblings, VaultContextNeighbour{VaultFileID: wire.UUID{UUID: row.ID}, Path: row.Path})
		}
	}
	outgoing, err := m.links.Outgoing(ctx, tenant, id)
	if err != nil {
		return err
	}
	incoming, err := m.links.Incoming(ctx, tenant, id)
	if err != nil {
		return err
	}
	var targets, sources []string
	dangling := []string{}
	for _, l := range outgoing {
		if l.TargetPath != nil {
			targets = append(targets, *l.TargetPath)
		}
		if l.ResolutionState != string(chunkindex.Resolved) {
			dangling = append(dangling, l.RawTarget)
		}
	}
	for _, l := range incoming {
		if l.SourcePath != nil {
			sources = append(sources, *l.SourcePath)
		}
	}
	linksTo, err := m.neighbours(ctx, tenant, targets)
	if err != nil {
		return err
	}
	linkedFrom, err := m.neighbours(ctx, tenant, sources)
	if err != nil {
		return err
	}
	slices.Sort(dangling)
	return wire.JSON(c, http.StatusOK, VaultContextPacket{
		VaultFileID:   wire.UUID{UUID: id},
		Path:          file.Path,
		SpaceID:       wireUUID(file.SpaceID),
		ChunkCount:    int(chunkCount),
		Headings:      headings,
		Siblings:      siblings,
		LinksTo:       linksTo,
		LinkedFrom:    linkedFrom,
		DanglingLinks: slices.Compact(dangling),
	})
}

// neighbours is resolveNeighbours: deduplicated, and paths that no longer exist
// are dropped.
func (m *Module) neighbours(ctx context.Context, tenant uuid.UUID, paths []string) ([]VaultContextNeighbour, error) {
	out := []VaultContextNeighbour{}
	if len(paths) == 0 {
		return out, nil
	}
	rows, err := m.q.NeighboursByPath(ctx, db.NeighboursByPathParams{TenantID: tenant, Column2: paths})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out = append(out, VaultContextNeighbour{VaultFileID: wire.UUID{UUID: row.ID}, Path: row.Path})
	}
	return out, nil
}

// documentLinks is VaultIndexController.documentLinks.
func (m *Module) documentLinks(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	id, err := parseVaultFileID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	file, err := m.store.FindByID(ctx, tenant, id)
	if err != nil {
		return err
	}
	if file == nil {
		return errNoFileWithID
	}
	outgoing, err := m.links.Outgoing(ctx, tenant, id)
	if err != nil {
		return err
	}
	incoming, err := m.links.Incoming(ctx, tenant, id)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, linksResponse{
		VaultFileID: wire.UUID{UUID: id}, Path: file.Path, Outgoing: outgoing, Incoming: incoming,
	})
}

// lint is VaultLintService.lint: it reads files and rows and rewrites neither.
func (m *Module) lint(c *echo.Context) error {
	a, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	tenant := a.VaultID
	ctx := c.Request().Context()

	checks, err := m.linkChecks(ctx, tenant)
	if err != nil {
		return err
	}
	duplicates, err := m.duplicateSlugs(ctx, tenant)
	if err != nil {
		return err
	}
	orphans, err := m.orphans(ctx, tenant)
	if err != nil {
		return err
	}
	stale, err := m.staleIndex(ctx, tenant)
	if err != nil {
		return err
	}
	fileChecks, err := m.fileChecks(ctx, tenant)
	if err != nil {
		return err
	}
	checks = append(checks, duplicates, orphans, stale)
	checks = append(checks, fileChecks...)

	report := VaultLintReport{Checks: checks}
	for _, check := range checks {
		switch check.Severity {
		case Pass:
			report.Passed++
		case Warning:
			report.Warned++
		case Error:
			report.Failed++
		}
	}
	report.HasFindings = report.Warned+report.Failed > 0
	return wire.JSON(c, http.StatusOK, report)
}

func (m *Module) linkChecks(ctx context.Context, tenant uuid.UUID) ([]VaultLintCheck, error) {
	rows, err := m.q.DanglingLinks(ctx, tenant)
	if err != nil {
		return nil, err
	}
	broken, ambiguous := []VaultLintFinding{}, []VaultLintFinding{}
	for _, row := range rows {
		finding := VaultLintFinding{Path: row.SourcePath, Line: ptr(int(row.SourceLine))}
		switch chunkindex.ResolutionState(row.ResolutionState) {
		case chunkindex.Unresolved:
			finding.Message = "unresolved link [[" + row.RawTarget + "]]"
			broken = append(broken, finding)
		case chunkindex.Ambiguous:
			finding.Message = "ambiguous link [[" + row.RawTarget + "]] — use a full path"
			ambiguous = append(ambiguous, finding)
		}
	}
	// A link to a note not written yet is intent, not damage: warn, never error.
	return []VaultLintCheck{
		warnCheck(BrokenLinks, broken, "no broken wiki links", "%d link(s) point at a document that does not exist"),
		warnCheck(AmbiguousLinks, ambiguous, "no ambiguous wiki links", "%d link(s) match more than one document"),
	}, nil
}

func (m *Module) duplicateSlugs(ctx context.Context, tenant uuid.UUID) (VaultLintCheck, error) {
	paths, err := m.q.AllPaths(ctx, tenant)
	if err != nil {
		return VaultLintCheck{}, err
	}
	byStem := map[string][]string{}
	for _, path := range paths {
		stem := chunkindex.LinkStem(path)
		byStem[stem] = append(byStem[stem], path)
	}
	stems := []string{}
	for stem, group := range byStem {
		if len(group) > 1 {
			stems = append(stems, stem)
		}
	}
	slices.Sort(stems)
	findings := []VaultLintFinding{}
	for _, stem := range stems {
		group := byStem[stem]
		for _, path := range group {
			findings = append(findings, VaultLintFinding{
				Path:    ptr(path),
				Message: fmt.Sprintf("name '%s' also used by %d other document(s)", stem, len(group)-1),
			})
		}
	}
	summary := "no duplicate document names"
	severity := Pass
	if len(stems) > 0 {
		summary = fmt.Sprintf("%d name(s) are used by more than one document", len(stems))
		severity = Warning
	}
	return makeCheck(DuplicateSlugs, severity, summary, findings), nil
}

func (m *Module) orphans(ctx context.Context, tenant uuid.UUID) (VaultLintCheck, error) {
	paths, err := m.links.OrphanPaths(ctx, tenant, maxFindingsPerCheck*4)
	if err != nil {
		return VaultLintCheck{}, err
	}
	findings := []VaultLintFinding{}
	for _, path := range paths {
		findings = append(findings, VaultLintFinding{Path: ptr(path), Message: "no other document links here"})
	}
	return warnCheck(Orphans, findings, "every document is linked from somewhere", "%d document(s) have no incoming links"), nil
}

// staleIndex reuses /v1/vault/index's computation, so the two can't disagree.
// Unlike the link checks it's an error: those documents are unreachable.
func (m *Module) staleIndex(ctx context.Context, tenant uuid.UUID) (VaultLintCheck, error) {
	status, err := m.indexStatusOf(ctx, tenant)
	if err != nil {
		return VaultLintCheck{}, err
	}
	findings := []VaultLintFinding{}
	for _, path := range status.UnindexedSample {
		findings = append(findings, VaultLintFinding{Path: ptr(path), Message: "not indexed — invisible to search"})
	}
	for _, path := range status.StaleSample {
		findings = append(findings, VaultLintFinding{Path: ptr(path), Message: "changed since it was last indexed"})
	}
	if status.Stale {
		return makeCheck(StaleIndex, Error, strings.Join(status.StaleReasons, "; "), findings), nil
	}
	return makeCheck(StaleIndex, Pass, "index is fresh", findings), nil
}

// fileChecks is the one disk pass behind oversized-files, empty-documents and
// unreadable-files. It calls ResolveInside on a stored path without SanitizePath,
// as Swift does: a row isn't trusted, but it isn't re-validated either.
func (m *Module) fileChecks(ctx context.Context, tenant uuid.UUID) ([]VaultLintCheck, error) {
	files, err := m.q.LintFiles(ctx, db.LintFilesParams{TenantID: tenant, Limit: maxFilesScanned})
	if err != nil {
		return nil, err
	}
	rawRoot := m.paths.Raw(tenant)
	oversized, empty, unreadable := []VaultLintFinding{}, []VaultLintFinding{}, []VaultLintFinding{}
	for _, file := range files {
		if file.SizeBytes > oversizedBytes {
			oversized = append(oversized, VaultLintFinding{
				Path:    ptr(file.Path),
				Message: strconv.FormatInt(file.SizeBytes/(1024*1024), 10) + " MiB — chunking and embedding this is slow and expensive",
			})
			continue // not opened: that's the cost this check warns about
		}
		target, err := vaultfiles.ResolveInside(rawRoot, file.Path)
		if err != nil {
			unreadable = append(unreadable, VaultLintFinding{Path: ptr(file.Path), Message: "path escapes the vault boundary"})
			continue
		}
		data, err := os.ReadFile(target) //nolint:gosec // target comes from ResolveInside
		if err != nil {
			slog.DebugContext(ctx, "vault lint: unreadable", "tenant", ids.Upper(tenant), "error", err)
		}
		text, ok := decodeUTF8(data)
		if err != nil || !ok {
			unreadable = append(unreadable, VaultLintFinding{Path: ptr(file.Path), Message: "missing, unreadable, or not valid UTF-8"})
			continue
		}
		if len(chunkindex.Chunk(text)) == 0 {
			empty = append(empty, VaultLintFinding{Path: ptr(file.Path), Message: "no body content — nothing to retrieve"})
		}
	}
	suffix := ""
	if len(files) >= maxFilesScanned {
		suffix = fmt.Sprintf(" (first %d documents only)", maxFilesScanned)
	}
	unreadableCheck := makeCheck(UnreadableFiles, Pass, "all files readable"+suffix, unreadable)
	if len(unreadable) > 0 {
		// An error: the row claims a document the vault can't produce.
		unreadableCheck = makeCheck(UnreadableFiles, Error, fmt.Sprintf("%d file(s) could not be read", len(unreadable))+suffix, unreadable)
	}
	return []VaultLintCheck{
		warnCheck(OversizedFiles, oversized, "no oversized files"+suffix, "%d file(s) over 5 MiB"+suffix),
		warnCheck(EmptyDocuments, empty, "no empty documents"+suffix, "%d document(s) have no body content"+suffix),
		unreadableCheck,
	}, nil
}

// warnCheck is a check that passes when empty and warns otherwise. format takes
// the exact count, which stays right when the findings are cut to the limit.
func warnCheck(name VaultLintCheckName, findings []VaultLintFinding, pass, format string) VaultLintCheck {
	if len(findings) == 0 {
		return makeCheck(name, Pass, pass, findings)
	}
	return makeCheck(name, Warning, fmt.Sprintf(format, len(findings)), findings)
}

func makeCheck(name VaultLintCheckName, severity VaultLintCheckSeverity, message string, findings []VaultLintFinding) VaultLintCheck {
	return VaultLintCheck{Name: name, Severity: severity, Message: message, Findings: firstN(findings, maxFindingsPerCheck)}
}

// firstN is `Array(s.prefix(n))`, never nil, because these lists are required.
func firstN[T any](s []T, n int) []T {
	if s == nil {
		return []T{}
	}
	return s[:min(n, len(s))]
}
