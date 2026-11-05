// Package vision serves /v1/vision: the Go port of LuminaVaultServer
// Sources/App/Vision (VisionEmbedController, VisionEmbedService, the provider
// registry and its Cohere and stub adapters). Images are embedded on a platform
// key, so the group has no bring-your-own exemption and a tier floor of trial.
package vision

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/vision/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/ratelimit"
	"lumina/internal/usagemeter"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

const (
	// maxBodyBytes is the cap on the image, before any provider is called.
	maxBodyBytes = 8 * 1024 * 1024
	// targetDim is the width of memories.embedding.
	targetDim = 1536
	// defaultSearchLimit and maxSearchLimit bound ?limit=, which is rejected
	// rather than clamped: every hit carries a memory's full content.
	defaultSearchLimit = 10
	maxSearchLimit     = 50
)

var acceptedMimes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/heic": true}

var (
	errBadType = wire.NewError(http.StatusUnsupportedMediaType,
		"Content-Type must be one of: image/heic, image/jpeg, image/png, image/webp")
	errTooLarge = wire.NewError(http.StatusRequestEntityTooLarge,
		"image body exceeds "+strconv.Itoa(maxBodyBytes)+" byte cap")
	errBadLimit = wire.NewError(http.StatusBadRequest,
		"limit must be an integer between 1 and "+strconv.Itoa(maxSearchLimit))
	errBadIndexAs    = wire.NewError(http.StatusBadRequest, "indexAs must be 'memory' when supplied")
	errBadMemoryID   = wire.NewError(http.StatusBadRequest, "memoryId required and must be a UUID when indexAs=memory")
	errNoProvider    = wire.NewError(http.StatusServiceUnavailable, "vision embed provider not configured")
	errRejected      = wire.NewError(http.StatusBadGateway, "vision embed upstream rejected request")
	errUnavailable   = wire.NewError(http.StatusBadGateway, "vision embed upstream unavailable")
	errMemoryMissing = wire.NewError(http.StatusNotFound, "memory not found for tenant")
)

// Module is the route group: jwtAuthenticator, the platform-funded memoryQuery
// entitlement, then the per-minute and daily limits (App+build.swift
// "visionEmbedGroup"). The handlers ask for identity themselves, so with
// enforcement off an anonymous request still counts against the "ip:" buckets.
type Module struct {
	memories *memorystore.Repository
	usage    *usagemeter.Meter
	identify echo.MiddlewareFunc
	gate     echo.MiddlewareFunc
	limits   *ratelimit.Store
	reg      registry
	log      *slog.Logger
}

// New builds the module. gate is entitlement.PlatformFundedMiddleware for
// memoryQuery, and limits the process's shared rate-limit store.
func New(memories *memorystore.Repository, usage *usagemeter.Meter, identify, gate echo.MiddlewareFunc, limits *ratelimit.Store, cfg Config) *Module {
	log := slog.Default().With("module", "vision")
	return &Module{memories: memories, usage: usage, identify: identify, gate: gate, limits: limits, reg: newRegistry(cfg, log), log: log}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{
		m.identify,
		m.gate,
		ratelimit.Middleware(m.limits, ratelimit.VisionEmbedByUserPerMinute, ratelimit.UserOrIPKey),
		ratelimit.Middleware(m.limits, ratelimit.VisionEmbedByUserDaily, ratelimit.UserOrIPKey),
	}
	r.POST("/v1/vision/embed", m.embed, mw...)
	r.POST("/v1/vision/search", m.search, mw...)
}

// MARK: - POST /v1/vision/embed

func (m *Module) embed(c *echo.Context) error {
	tenant, mime, err := m.preflight(c)
	if err != nil {
		return err
	}
	image, err := m.readImage(c)
	if err != nil {
		return err
	}
	memoryID, err := parseIndexAsMemory(c)
	if err != nil {
		return err
	}
	out, err := m.embedImage(c.Request().Context(), image, mime, tenant, memoryID)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, responseOf(out))
}

// MARK: - POST /v1/vision/search

// search embeds, then runs the vector against memories.embedding: the column
// /embed?indexAs=memory writes, so image and memory vectors share a space. Never
// writes, and a lookup that isn't recall counts no hits.
func (m *Module) search(c *echo.Context) error {
	tenant, mime, err := m.preflight(c)
	if err != nil {
		return err
	}
	limit, err := parseSearchLimit(c)
	if err != nil {
		return err
	}
	image, err := m.readImage(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	out, err := m.embedImage(ctx, image, mime, tenant, nil)
	if err != nil {
		return err
	}
	results, err := m.memories.SemanticSearch(ctx, tenant, out.embedding, limit, memorystore.SearchOptions{SkipHitCount: true})
	if err != nil {
		return err
	}
	hits := make([]VisionSearchHit, len(results))
	for i, r := range results {
		hits[i] = VisionSearchHit{Id: wire.UUID{UUID: r.ID}, Content: r.Content, Distance: wire.Float32(r.Distance)}
		if r.CreatedAt != nil {
			hits[i].CreatedAt = &wire.Time{Time: *r.CreatedAt}
		}
	}
	return wire.JSON(c, http.StatusOK, VisionSearchResponse{Hits: hits, Model: out.model})
}

// MARK: - Request parsing

// preflight is what both routes check before touching the query: identity, the
// media type, and a declared length over the cap.
func (m *Module) preflight(c *echo.Context) (uuid.UUID, string, error) {
	tenant, ok := auth.Identity(c)
	if !ok {
		return uuid.Nil, "", wire.ErrUnauthenticated
	}
	mime := contentType(c.Request().Header)
	if !acceptedMimes[mime] {
		return uuid.Nil, "", errBadType
	}
	if c.Request().ContentLength > maxBodyBytes {
		return uuid.Nil, "", errTooLarge
	}
	return tenant, mime, nil
}

// contentType is the bare media type: no parameters, lowercased. Empty is not
// accepted, so a missing header is a 415. Swift's split drops empty pieces, so
// the type after a leading ";" still counts.
func contentType(h http.Header) string {
	for part := range strings.SplitSeq(strings.Join(h.Values("Content-Type"), ", "), ";") {
		if part != "" {
			return strings.ToLower(strings.Trim(part, " \t"))
		}
	}
	return ""
}

// parseSearchLimit rejects nonsense rather than clamping it.
func parseSearchLimit(c *echo.Context) (int, error) {
	raw, ok := wire.QueryParam(c, "limit")
	if !ok {
		return defaultSearchLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 || n > maxSearchLimit {
		return 0, errBadLimit
	}
	return n, nil
}

// parseIndexAsMemory is `?indexAs=memory&memoryId=<uuid>`: nil with neither, and
// a 400 with anything other than a well-formed pair.
func parseIndexAsMemory(c *echo.Context) (*uuid.UUID, error) {
	indexAs, hasIndexAs := wire.QueryParam(c, "indexAs")
	raw, hasMemoryID := wire.QueryParam(c, "memoryId")
	if !hasIndexAs && !hasMemoryID {
		return nil, nil //nolint:nilnil // nil is "don't index"
	}
	if indexAs != "memory" {
		return nil, errBadIndexAs
	}
	id, err := ids.Parse(raw)
	if !hasMemoryID || err != nil {
		return nil, errBadMemoryID
	}
	return &id, nil
}

// readImage collects the body up to the cap, for a length nothing declared.
func (m *Module) readImage(c *echo.Context) ([]byte, error) {
	image, err := io.ReadAll(io.LimitReader(c.Request().Body, maxBodyBytes+1))
	if err != nil {
		m.log.Warn("vision body collect failed", "error", err)
		return nil, errTooLarge
	}
	if len(image) > maxBodyBytes {
		return nil, errTooLarge
	}
	return image, nil
}

// MARK: - VisionEmbedService

// embedImage is VisionEmbedService.embed. A caller who hangs up doesn't cancel
// it: Swift's handler runs on, and the paid call has its own timeout.
func (m *Module) embedImage(ctx context.Context, image []byte, mime string, tenant uuid.UUID, indexAs *uuid.UUID) (upstreamResult, error) {
	if m.reg.active == nil {
		m.log.Error("no active vision embed provider: check VISION_EMBED_PROVIDER")
		return upstreamResult{}, errNoProvider
	}
	ctx = context.WithoutCancel(ctx)
	result, err := m.reg.active.embed(ctx, image, mime)
	if err != nil {
		m.log.Error("vision embed provider error", "error", err)
		var pe *providerError
		if errors.As(err, &pe) && pe.permanent {
			return upstreamResult{}, errRejected
		}
		return upstreamResult{}, errUnavailable
	}
	result.embedding = padOrTruncate(result.embedding, targetDim)
	if indexAs != nil {
		found, err := m.memories.SetEmbedding(ctx, tenant, *indexAs, result.embedding)
		if err != nil {
			return upstreamResult{}, err
		}
		if !found {
			return upstreamResult{}, errMemoryMissing
		}
	}
	m.usage.RecordVisionEmbed(tenant, m.reg.kind, nil)
	return result, nil
}

// padOrTruncate fits the provider's vector to the column: zero-padded on the
// right, or cut.
func padOrTruncate(v []float32, dim int) []float32 {
	if len(v) >= dim {
		return v[:dim]
	}
	out := make([]float32, dim)
	copy(out, v)
	return out
}

func responseOf(e upstreamResult) VisionEmbedResponse {
	out := make([]wire.Float32, len(e.embedding))
	for i, x := range e.embedding {
		out[i] = wire.Float32(x)
	}
	return VisionEmbedResponse{
		Embedding: out, Dim: targetDim, Model: e.model,
		SourceWidth: e.sourceWidth, SourceHeight: e.sourceHeight,
	}
}
