package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/modules/ingestion/db"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v5"
)

// create is MultimodalIngestionService.create. There's no transaction, as in Swift:
// each item is validated only after the batch and the items before it are saved,
// so a refused request can leave those rows behind.
func (m *Module) create(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[createRequest](c)
	if err != nil {
		return err
	}
	if len(body.Items) == 0 || len(body.Items) > maxItems {
		return wire.NewError(http.StatusBadRequest, "ingestion batches require 1..."+strconv.Itoa(maxItems)+" items")
	}
	batchBytes := declaredBytes(body.Items)
	if batchBytes > maxBatchBytes {
		return wire.NewError(http.StatusRequestEntityTooLarge, "batch exceeds 5 GiB")
	}
	tier, err := m.tenantTier(ctx, acc.VaultID)
	if err != nil {
		return err
	}
	if used, limit, denied := m.quotaDenied(ctx, acc.VaultID, tier, batchBytes); denied {
		return wire.NewError(http.StatusRequestEntityTooLarge, quotaMessage(used, limit))
	}
	if err := m.disk.Check(batchBytes); err != nil {
		return err
	}
	var spaceID *uuid.UUID
	if body.SpaceID != nil {
		_, err := m.q.SpaceSlug(ctx, db.SpaceSlugParams{TenantID: acc.VaultID, ID: body.SpaceID.UUID})
		if err := notFound(err, wire.NewError(http.StatusBadRequest, "unknown space")); err != nil {
			return err
		}
		spaceID = &body.SpaceID.UUID
	}

	batchID := uuid.New()
	if err := m.q.InsertBatch(ctx, db.InsertBatchParams{ID: batchID, TenantID: acc.VaultID, SpaceID: nullUUID(spaceID), CreatedAt: m.now()}); err != nil {
		return err
	}
	for _, input := range body.Items {
		if err := validateItem(input); err != nil {
			return err
		}
		state := stateQueued
		if input.Kind == kindFile {
			state = stateAwaitingUpload
		}
		var sha *string
		if input.SHA256 != nil {
			lowered := vaultfiles.Lowercased(*input.SHA256)
			sha = &lowered
		}
		if err := m.q.InsertItem(ctx, db.InsertItemParams{
			ID: uuid.New(), TenantID: acc.VaultID, BatchID: batchID, Kind: string(input.Kind), State: state,
			FileName: input.FileName, ContentType: input.ContentType, SizeBytes: input.SizeBytes,
			ExpectedSha256: sha, Url: input.URL, CreatedAt: m.now(),
		}); err != nil {
			return err
		}
	}
	return m.respondDetail(c, acc.VaultID, batchID)
}

// declaredBytes sums the sizes the items declare, URL items too. An overflow traps
// Swift's reduce and kills the process; here it saturates, and so is refused as too
// large, or passes as the large negative number it was.
func declaredBytes(items []createItemRequest) int64 {
	var total int64
	for _, item := range items {
		if item.SizeBytes == nil {
			continue
		}
		size := *item.SizeBytes
		switch {
		case size > 0 && total > math.MaxInt64-size:
			total = math.MaxInt64
		case size < 0 && total < math.MinInt64-size:
			total = math.MinInt64
		default:
			total += size
		}
	}
	return total
}

// tenantTier is User.find(tenantID)?.tierEnum ?? .trial. A team vault's id is no
// user, so it reads as trial.
func (m *Module) tenantTier(ctx context.Context, tenant uuid.UUID) (string, error) {
	tier, err := m.q.UserTier(ctx, tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "trial", nil
	}
	return tier, err
}

// validateItem is MultimodalIngestionService.validate.
func validateItem(item createItemRequest) error {
	if item.Kind == kindFile {
		if item.FileName == nil || *item.FileName == "" || item.ContentType == nil ||
			item.SizeBytes == nil || *item.SizeBytes <= 0 || *item.SizeBytes > maxFileBytes {
			return wire.NewError(http.StatusBadRequest, "file metadata is incomplete or too large")
		}
		return nil
	}
	if item.URL == nil || !isHTTPURL(*item.URL) {
		return wire.NewError(http.StatusBadRequest, "invalid URL ingestion item")
	}
	return nil
}

// isHTTPURL is URL(string:) with an http or https scheme, in any case.
func isHTTPURL(raw string) bool {
	u, err := url.Parse(swiftstr.EncodeInvalidURLCharacters(raw))
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}

func (m *Module) requireBatch(ctx context.Context, tenant, batchID uuid.UUID) (db.IngestionBatch, error) {
	b, err := m.q.GetBatch(ctx, db.GetBatchParams{TenantID: tenant, ID: batchID})
	return b, notFound(err, errBatchNotFound)
}

func (m *Module) requireItem(ctx context.Context, tenant, batchID, itemID uuid.UUID) (db.IngestionItem, error) {
	item, err := m.q.GetItem(ctx, db.GetItemParams{TenantID: tenant, BatchID: batchID, ID: itemID})
	return item, notFound(err, errItemNotFound)
}

// detail is MultimodalIngestionService.detail.
func (m *Module) detail(ctx context.Context, tenant, batchID uuid.UUID) (batchDTO, error) {
	b, err := m.requireBatch(ctx, tenant, batchID)
	if err != nil {
		return batchDTO{}, err
	}
	return m.batchDetail(ctx, b)
}

func (m *Module) batchDetail(ctx context.Context, b db.IngestionBatch) (batchDTO, error) {
	items, err := m.q.ListItems(ctx, db.ListItemsParams{TenantID: b.TenantID, BatchID: b.ID})
	if err != nil {
		return batchDTO{}, err
	}
	out := batchDTO{
		ID: wire.UUID{UUID: b.ID}, State: b.State, Total: len(items), ChunkSizeBytes: chunkSize,
		Items: make([]itemDTO, 0, len(items)), CreatedAt: &wire.Time{Time: b.CreatedAt}, UpdatedAt: &wire.Time{Time: b.UpdatedAt},
	}
	for _, item := range items {
		d, err := m.itemDTO(ctx, item)
		if err != nil {
			return batchDTO{}, err
		}
		switch d.State {
		case stateCompleted:
			out.Completed++
		case stateFailed:
			out.Failed++
		}
		out.Items = append(out.Items, d)
	}
	return out, nil
}

func (m *Module) itemDTO(ctx context.Context, item db.IngestionItem) (itemDTO, error) {
	// An unknown state reads as failed, an unknown kind as file (the column is
	// CHECKed, so that one can't happen).
	state, ok := itemState(item.State)
	if !ok {
		state = stateFailed
	}
	entities, relationships, err := m.graphProjection(ctx, item)
	if err != nil {
		return itemDTO{}, err
	}
	credibility, err := decodeCredibility(item.Credibility)
	if err != nil {
		return itemDTO{}, err
	}
	pipeline := item.PipelineVersion
	return itemDTO{
		ID: wire.UUID{UUID: item.ID}, BatchID: wire.UUID{UUID: item.BatchID}, Kind: item.Kind, State: state,
		FileName: item.FileName, ContentType: item.ContentType, SizeBytes: item.SizeBytes,
		UploadedBytes: item.UploadedBytes, URL: item.Url,
		VaultFileID: wireUUID(item.VaultFileID), MemoryID: wireUUID(item.MemoryID),
		Summary: item.Summary, Error: item.ErrorMessage, Credibility: credibility,
		Entities: entities, Relationships: relationships,
		ContentSHA256: item.ContentSha256, PipelineVersion: &pipeline, ReusedFromItemID: wireUUID(item.ReusedFromItemID),
		GraphReadyAt: wireTime(item.GraphReadyAt),
		CreatedAt:    &wire.Time{Time: item.CreatedAt}, UpdatedAt: &wire.Time{Time: item.UpdatedAt},
	}, nil
}

// decodeCredibility is the JSONB column. A row Swift's Fluent can't decode fails
// the request there too.
func decodeCredibility(raw []byte) (*credibilityDTO, error) {
	if raw == nil {
		return nil, nil
	}
	var d credibilityDTO
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if d.Signals == nil {
		d.Signals = []string{}
	}
	return &d, nil
}

// graphProjection reads the knowledge graph the worker's extraction built on the
// item's memory. Edges to anything but a listed entity are dropped.
func (m *Module) graphProjection(ctx context.Context, item db.IngestionItem) ([]entityDTO, []relationshipDTO, error) {
	entities, relationships := []entityDTO{}, []relationshipDTO{}
	if !item.MemoryID.Valid {
		return entities, relationships, nil
	}
	memoryID := uuid.UUID(item.MemoryID.Bytes)
	nodes, err := m.q.GraphEntities(ctx, db.GraphEntitiesParams{TenantID: item.TenantID, MemoryID: memoryID})
	if err != nil {
		return nil, nil, err
	}
	known := make(map[uuid.UUID]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID] = true
		entities = append(entities, entityDTO{ID: wire.UUID{UUID: n.ID}, Name: n.Label, Type: "entity", Confidence: n.Confidence})
	}
	edges, err := m.q.GraphEdges(ctx, db.GraphEdgesParams{TenantID: item.TenantID, MemoryID: memoryID})
	if err != nil {
		return nil, nil, err
	}
	for _, e := range edges {
		if !known[e.FromNodeID] || !known[e.ToNodeID] {
			continue
		}
		quote := e.Quote
		relationships = append(relationships, relationshipDTO{
			ID: wire.UUID{UUID: e.ID}, SubjectEntityID: wire.UUID{UUID: e.FromNodeID}, Predicate: e.Predicate,
			ObjectEntityID: wire.UUID{UUID: e.ToNodeID}, Confidence: e.Confidence, Evidence: &quote,
		})
	}
	return entities, relationships, nil
}

func nullUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}

func wireUUID(id pgtype.UUID) *wire.UUID {
	if !id.Valid {
		return nil
	}
	return &wire.UUID{UUID: id.Bytes}
}
