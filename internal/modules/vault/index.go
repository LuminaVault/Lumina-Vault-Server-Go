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
	"lumina/internal/vaultindex"
	"lumina/internal/wire"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// VaultIndexController and VaultLintService; the other Indexing services are in
// internal/vaultindex. Every route here is a pure read: none of them index, embed or
// re-resolve anything.
const (
	maxFilesScanned     = 2000
	maxFindingsPerCheck = 50
	oversizedBytes      = 5 * 1024 * 1024
)

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
	status, err := m.index.Status(c.Request().Context(), a.VaultID)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, status)
}

// context is VaultIndexController.context.
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
	siblingLimit := vaultindex.DefaultSiblingLimit
	if v, ok := wire.QueryInt(c, "siblingLimit"); ok {
		siblingLimit = v
	}
	packet, err := m.index.Context(c.Request().Context(), tenant, id, siblingLimit)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, packet)
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
		return vaultindex.ErrNoFileWithID
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
	status, err := m.index.Status(ctx, tenant)
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
		text, ok := vaultfiles.DecodeUTF8(data)
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
