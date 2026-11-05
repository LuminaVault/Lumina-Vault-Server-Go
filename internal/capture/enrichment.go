package capture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// EnrichmentOptions are URLEnrichmentService's dependencies. A nil Jina, Hooks,
// Embeddings or Memories disables that step, as the chat pre-enricher does.
type EnrichmentOptions struct {
	Paths      vaultpaths.Paths
	Store      *vaultfiles.Store
	Client     *http.Client
	Jina       *JinaEnricher
	Hooks      *HookDispatcher
	Embeddings embedding.Service
	Memories   *memorystore.Repository
	// Enrichers replaces YouTube, X and generic OG, in that order. Tests set it.
	Enrichers []Enricher
}

// EnrichmentService is URLEnrichmentService.
type EnrichmentService struct {
	paths      vaultpaths.Paths
	store      *vaultfiles.Store
	jina       *JinaEnricher
	hooks      *HookDispatcher
	embeddings embedding.Service
	memories   *memorystore.Repository
	enrichers  []Enricher
}

// NewEnrichmentService builds the service. A nil Client gets one that follows
// redirects like URLSession and keeps no cookies.
func NewEnrichmentService(o EnrichmentOptions) *EnrichmentService {
	client := o.Client
	if client == nil {
		client = &http.Client{}
	}
	enrichers := o.Enrichers
	if enrichers == nil {
		enrichers = []Enricher{YouTubeEnricher{Client: client}, XEnricher{Client: client}, GenericOGEnricher{Client: client}}
	}
	return &EnrichmentService{
		paths: o.Paths, store: o.Store, jina: o.Jina, hooks: o.Hooks,
		embeddings: o.Embeddings, memories: o.Memories, enrichers: enrichers,
	}
}

// EnrichURL is enrichURL: the enricher chain with no database or disk effects,
// for chat's URL pre-enrichment (wave 5). Nil when the URL doesn't parse, isn't
// public or the enricher fails.
func (s *EnrichmentService) EnrichURL(ctx context.Context, urlString string) *EnrichedMetadata {
	u, ok := ParseURL(urlString)
	if !ok {
		return nil
	}
	if !IsPublic(u) {
		slog.WarnContext(ctx, "chat-enrichment rejected non-public url", "url", urlString)
		return nil
	}
	enricher := s.enricherFor(u)
	if enricher == nil {
		return nil
	}
	metadata, err := enricher.Enrich(ctx, u)
	if err != nil {
		slog.WarnContext(ctx, "chat-enrichment failed", "url", urlString, "error", err)
		return nil
	}
	return &metadata
}

func (s *EnrichmentService) enricherFor(u *URL) Enricher {
	for _, e := range s.enrichers {
		if e.CanHandle(u) {
			return e
		}
	}
	return nil
}

// EnrichAndRewrite is enrichAndRewrite: fetch, render, replace the file and update
// the row's size, hash and status. Any failure marks the row "failed", best
// effort, and never reaches the caller, because the capture already succeeded.
//
// QUIRK, as in Swift: the render doesn't carry the "## Note" section of the
// placeholder, so the note a user typed in the share sheet is on disk only until
// enrichment finishes. The whole metadata value read here is written back, so an
// edit to the row while this runs can be lost.
func (s *EnrichmentService) EnrichAndRewrite(ctx context.Context, fileID uuid.UUID, urlString string, tenant uuid.UUID) {
	if err := s.enrichAndRewrite(ctx, fileID, urlString, tenant); err != nil {
		slog.ErrorContext(ctx, "enrichment failed", "tenant", ids.Upper(tenant), "file", ids.Upper(fileID), "error", err)
		row, findErr := s.store.FindByID(ctx, tenant, fileID)
		if findErr != nil || row == nil {
			return
		}
		if err := s.store.UpdateMetadata(ctx, row.ID, withEnrichmentStatus(row.Metadata, "failed")); err != nil {
			slog.WarnContext(ctx, "enrichment: marking failed", "file", ids.Upper(fileID), "error", err)
		}
	}
}

func withEnrichmentStatus(m *vaultfiles.Metadata, status string) *vaultfiles.Metadata {
	if m == nil {
		m = &vaultfiles.Metadata{}
	}
	m.EnrichmentStatus = &status
	return m
}

func (s *EnrichmentService) enrichAndRewrite(ctx context.Context, fileID uuid.UUID, urlString string, tenant uuid.UUID) error {
	u, ok := ParseURL(urlString)
	if !ok {
		return errors.New("invalid URL string")
	}
	if !IsPublic(u) {
		slog.WarnContext(ctx, "enrichment rejected non-public url", "tenant", ids.Upper(tenant), "url", urlString)
		return errors.New("URL host is not enrichable")
	}
	enricher := s.enricherFor(u)
	if enricher == nil {
		return errors.New("no enricher can handle this URL")
	}
	primary, err := enricher.Enrich(ctx, u)
	if err != nil {
		return fmt.Errorf("enrich: %w", err)
	}
	metadata := s.applyJinaIfShallow(ctx, primary, u, tenant)
	if s.hooks != nil {
		metadata = s.hooks.Run(ctx, PostEnrich, HookContext{Tenant: tenant, URL: urlString, Config: map[string]string{}, Metadata: metadata}).Metadata
	}
	data := []byte(RenderMarkdown(metadata, urlString))

	// Tenant-scoped lookup: a stray id can't reach another tenant's row.
	row, err := s.store.FindByID(ctx, tenant, fileID)
	if err != nil {
		return err
	}
	if row == nil {
		return errors.New("vault file not found")
	}
	target, err := vaultfiles.ResolveInside(s.paths.Raw(tenant), row.Path)
	if err != nil {
		return err
	}
	if err := vaultfiles.WriteAtomic(target, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if err := s.store.UpdateContent(ctx, row.ID, int64(len(data)), hex.EncodeToString(sum[:]), withEnrichmentStatus(row.Metadata, "done")); err != nil {
		return err
	}
	s.indexMemory(ctx, metadata, fileID, row.SpaceID, tenant)
	return nil
}

// RenderMarkdown is the vault file an enriched link becomes. Only double quotes
// are escaped in the frontmatter.
func RenderMarkdown(m EnrichedMetadata, urlString string) string {
	var b strings.Builder
	frontmatterQuote := func(s string) string { return strings.ReplaceAll(s, `"`, `\"`) }
	nonEmpty := func(s *string) bool { return s != nil && *s != "" }
	b.WriteString("---\n")
	if nonEmpty(m.Title) {
		b.WriteString(`title: "` + frontmatterQuote(*m.Title) + "\"\n")
	}
	b.WriteString(`source: "` + urlString + "\"\n")
	if nonEmpty(m.Author) {
		b.WriteString(`author: "` + frontmatterQuote(*m.Author) + "\"\n")
	}
	if nonEmpty(m.ImageURL) {
		b.WriteString(`image: "` + *m.ImageURL + "\"\n")
	}
	if m.ReadingTimeMinutes != nil {
		b.WriteString("reading_time: " + strconv.Itoa(*m.ReadingTimeMinutes) + "\n")
	}
	b.WriteString("---\n\n")
	if nonEmpty(m.Title) {
		b.WriteString("# " + *m.Title + "\n\n")
	}
	if nonEmpty(m.Description) {
		b.WriteString("## Description\n" + *m.Description + "\n\n")
	}
	if nonEmpty(m.Transcript) {
		b.WriteString("## Transcript\n" + *m.Transcript + "\n\n")
	}
	if nonEmpty(m.Body) {
		b.WriteString("## Content\n" + *m.Body + "\n\n")
	}
	return b.String()
}

// indexMemory embeds the enriched link into the recall index: it updates the
// memory already bound to this file, else creates one. It never fails the
// enrichment. A failed embed still saves the memory, with a NULL vector.
func (s *EnrichmentService) indexMemory(ctx context.Context, m EnrichedMetadata, fileID uuid.UUID, spaceID *uuid.UUID, tenant uuid.UUID) {
	if s.embeddings == nil || s.memories == nil {
		return
	}
	var parts []string
	for _, part := range []*string{m.Title, m.Description, m.Transcript, m.Body} {
		if part == nil {
			continue
		}
		if t := swiftstr.TrimWhitespacesAndNewlines(*part); t != "" {
			parts = append(parts, t)
		}
	}
	text := strings.Join(parts, "\n\n")
	if text == "" {
		return
	}
	logFailure := func(err error) {
		slog.WarnContext(ctx, "link memory indexing failed", "tenant", ids.Upper(tenant), "file", ids.Upper(fileID), "error", err)
	}
	vec, err := embedding.EmbedForWrite(ctx, s.embeddings, text, tenant, "capture.link")
	if err != nil {
		logFailure(err)
		return
	}
	existing, err := s.memories.IDBySourceVaultFileID(ctx, tenant, fileID)
	if err != nil {
		logFailure(err)
		return
	}
	if existing != nil {
		if _, err := s.memories.UpdateContent(ctx, tenant, *existing, text, vec, nil); err != nil {
			logFailure(err)
		}
		return
	}
	if _, err := s.memories.Create(ctx, memorystore.CreateParams{
		Tenant: tenant, Content: text, Embedding: vec, Tags: []string{"link"},
		SourceVaultFileID: &fileID, SpaceID: spaceID, ReviewState: memorystore.ReviewAuto,
	}); err != nil {
		logFailure(err)
	}
}

// applyJinaIfShallow is the tier-2 step: it runs when the primary enricher found
// no body and both description and transcript are under 500 characters. A failure
// returns the primary metadata, so the capture still lands. QUIRK, as in Swift: a
// successful call sets Body even when it is empty.
func (s *EnrichmentService) applyJinaIfShallow(ctx context.Context, m EnrichedMetadata, u *URL, tenant uuid.UUID) EnrichedMetadata {
	if s.jina == nil {
		return m
	}
	if m.Body != nil && *m.Body != "" {
		return m
	}
	length := func(p *string) int {
		if p == nil {
			return 0
		}
		return swiftstr.Count(*p)
	}
	if length(m.Description) >= 500 || length(m.Transcript) >= 500 {
		return m
	}
	jina, err := s.jina.Enrich(ctx, u)
	if err != nil {
		slog.WarnContext(ctx, "jina tier-2 enrichment failed; using primary", "tenant", ids.Upper(tenant), "error", err)
		return m
	}
	m.Body = jina.Body
	return m
}
