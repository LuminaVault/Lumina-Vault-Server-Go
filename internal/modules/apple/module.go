// Package apple serves the Apple-integration routes: the Go port of
// LuminaVaultServer Sources/App/Apple (AppleConsentController,
// AppleRemindersController, PhotoIndexController, DeviceCommandController).
// It flips together with devices and reminders (decisions.md D-shared-prefixes).
package apple

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/apple/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/modules/apple/db"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// maxReminderBatch is AppleRemindersController.maxBatchSize's default.
const maxReminderBatch = 1000

var (
	errRemindersForbidden = wire.NewError(http.StatusForbidden, "reminders access not allowed by the user")
	errPhotosForbidden    = wire.NewError(http.StatusForbidden, "photos access not allowed by the user")
	errRemindersRequired  = wire.NewError(http.StatusBadRequest, "reminders array required")
	errBatchTooLarge      = wire.NewError(http.StatusRequestEntityTooLarge, fmt.Sprintf("batch exceeds %d reminders", maxReminderBatch))
)

type Module struct {
	q        *db.Queries
	embedder Embedder
	now      func() time.Time
}

// Options carries the configuration Swift reads for these routes.
type Options struct {
	// EmbeddingProvider is `embedding.provider` (EMBEDDING_PROVIDER).
	EmbeddingProvider string
	// EmbeddingMonthlyCap is `embedding.monthlyTokenCapDefault`.
	EmbeddingMonthlyCap int64
}

func New(pool *pgxpool.Pool, opts Options) *Module {
	q := db.New(pool)
	m := &Module{q: q, now: time.Now}
	m.embedder = newEmbedder(q, opts, func() time.Time { return m.now() })
	return m
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/apple/consent", m.getConsent, authMW)
	r.PUT("/v1/apple/consent", m.putConsent, authMW)
	r.POST("/v1/reminders/sync", m.syncReminders, authMW)
	r.POST("/v1/photos/index", m.indexPhotos, authMW)
	r.POST("/v1/devices/command/:id/result", m.commandResult, authMW)
	r.GET("/v1/devices/commands/pending", m.pendingCommands, authMW)
}

// MARK: consent

func (m *Module) getConsent(c *echo.Context) error {
	out, err := m.snapshot(c.Request().Context(), auth.UserID(c))
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) putConsent(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	body, err := wire.Bind[consentUpdateRequest](c)
	if err != nil {
		return err
	}
	// Swift: `body.allowWrites ?? false`, so an omitted allowWrites resets it
	// (the DTO comment says "leave unchanged"; the code does not).
	writes := body.AllowWrites != nil && *body.AllowWrites
	if err := m.q.UpsertConsent(ctx, db.UpsertConsentParams{
		TenantID: tenant, Domain: string(body.Domain), Allowed: body.Allowed, AllowWrites: writes,
	}); err != nil {
		return err
	}
	// Privacy contract: disallow = delete the synced server copy for the domain.
	if !body.Allowed {
		if err := m.purgeDomain(ctx, tenant, body.Domain); err != nil {
			return err
		}
	}
	out, err := m.snapshot(ctx, tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

// snapshot lists every domain in AppleDataDomain.allCases order; a domain
// without a row is not allowed.
func (m *Module) snapshot(ctx context.Context, tenant uuid.UUID) (AppleConsentResponse, error) {
	rows, err := m.q.ConsentRows(ctx, tenant)
	if err != nil {
		return AppleConsentResponse{}, err
	}
	byDomain := map[string]db.ConsentRowsRow{}
	for _, r := range rows {
		if _, dup := byDomain[r.Domain]; !dup {
			byDomain[r.Domain] = r
		}
	}
	out := AppleConsentResponse{Consents: make([]AppleConsentDTO, 0, len(allDomains))}
	for _, d := range allDomains {
		dto := AppleConsentDTO{Domain: AppleDataDomain(d)}
		if r, ok := byDomain[string(d)]; ok {
			dto.Allowed, dto.AllowWrites = r.Allowed, r.AllowWrites
			if r.LastSyncAt != nil {
				dto.LastSyncAt = &wire.Time{Time: *r.LastSyncAt}
			}
		}
		out.Consents = append(out.Consents, dto)
	}
	return out, nil
}

// isAllowed is AppleConsentController.isAllowed: fails closed on any error.
func (m *Module) isAllowed(ctx context.Context, tenant uuid.UUID, d domain) (allowed, writes bool) {
	row, err := m.q.ConsentFor(ctx, db.ConsentForParams{TenantID: tenant, Domain: string(d)})
	if err != nil {
		return false, false
	}
	return row.Allowed, row.AllowWrites
}

func (m *Module) purgeDomain(ctx context.Context, tenant uuid.UUID, d domain) error {
	switch d {
	case domainPhotos:
		return m.q.PurgePhotoIndex(ctx, tenant)
	case domainLocation:
		return m.q.ClearLastKnownLocation(ctx, tenant)
	default:
		return nil
	}
}

// MARK: POST /v1/reminders/sync

func (m *Module) syncReminders(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	// The consent gate runs before the body is decoded, as in Swift.
	if ok, _ := m.isAllowed(ctx, tenant, domainReminders); !ok {
		return errRemindersForbidden
	}
	body, err := wire.Bind[remindersSyncRequest](c)
	if err != nil {
		return err
	}
	if len(body.Reminders) == 0 {
		return errRemindersRequired
	}
	if len(body.Reminders) > maxReminderBatch {
		return errBatchTooLarge
	}
	var out AppleSyncResponse
	for _, in := range body.Reminders {
		externalID, title := trimSpace(in.ExternalID), trimSpace(in.Title)
		if externalID == "" || title == "" {
			out.Skipped++
			continue
		}
		inserted, err := m.q.UpsertAppleReminder(ctx, db.UpsertAppleReminderParams{
			TenantID:        tenant,
			ExternalID:      externalID,
			Title:           title,
			Notes:           in.Notes,
			DueAt:           timePtr(in.DueAt),
			Completed:       in.Completed,
			CompletedAt:     timePtr(in.CompletedAt),
			ListName:        in.ListName,
			Priority:        in.Priority,
			RemoteUpdatedAt: timePtr(in.RemoteUpdatedAt),
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			out.Skipped++ // last-writer-wins rejected this stale delta
		case err != nil:
			return err
		case inserted:
			out.Inserted++
		default:
			out.Updated++
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: POST /v1/photos/index

func (m *Module) indexPhotos(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	if ok, _ := m.isAllowed(ctx, tenant, domainPhotos); !ok {
		return errPhotosForbidden
	}
	body, err := wire.Bind[photoIndexSyncRequest](c)
	if err != nil {
		return err
	}
	var out AppleSyncResponse
	for _, item := range body.Items {
		assetID := trimSpace(item.AssetLocalID)
		if assetID == "" {
			out.Skipped++
			continue
		}
		var tags []string
		if item.SceneTags != nil {
			tags = *item.SceneTags
		}
		// Each item is embedded before its upsert and nothing is wrapped in a
		// transaction, so an embedding failure mid-batch keeps earlier rows
		// (as in Swift).
		var vector *string
		if src, ok := EmbedSource(item.OcrText, tags); ok {
			vec, err := m.embedder.Embed(ctx, src, tenant)
			if err != nil {
				return err
			}
			lit := FormatVector(vec)
			vector = &lit
		}
		var tagParam []string
		if len(tags) > 0 {
			tagParam = tags
		}
		inserted, err := m.q.UpsertPhoto(ctx, db.UpsertPhotoParams{
			ID:           uuid.New(),
			TenantID:     tenant,
			AssetLocalID: assetID,
			TakenAt:      timePtr(item.TakenAt),
			IsScreenshot: item.IsScreenshot,
			OcrText:      item.OcrText,
			SceneTags:    tagParam,
			Embedding:    vector,
		})
		if err != nil {
			return err
		}
		if inserted {
			out.Inserted++
		} else {
			out.Updated++
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

// MARK: device commands

// commandResult is DeviceCommandController.result. The `:id` path segment is
// never read (Swift ignores it too); the body's id is what counts.
//
// Swift also resumes DeviceCommandBroker's in-memory continuation here. Go
// holds no live device RPCs (the broker's callers, MCP and Skills, stay in
// Swift), so only the DB-backed queue is updated. See coupling.md row 7.
func (m *Module) commandResult(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	body, err := wire.Bind[deviceCommandResult](c)
	if err != nil {
		return err
	}
	now := m.now()
	ok := body.Ok
	if err := m.q.MarkDelivered(ctx, db.MarkDeliveredParams{
		DeliveredAt: &now, ResultOk: &ok, ResultError: body.Error, TenantID: tenant, ID: body.ID.UUID,
	}); err != nil {
		return err
	}
	return c.NoContent(http.StatusOK)
}

func (m *Module) pendingCommands(c *echo.Context) error {
	rows, err := m.q.PendingCommands(c.Request().Context(), db.PendingCommandsParams{TenantID: auth.UserID(c), Now: m.now()})
	if err != nil {
		return err
	}
	out := PendingDeviceCommandsResponse{Commands: make([]DeviceCommand, 0, len(rows))}
	for _, r := range rows {
		if cmd, ok := asCommand(r); ok {
			out.Commands = append(out.Commands, cmd)
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

func timePtr(t *wire.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.Time
	return &v
}
