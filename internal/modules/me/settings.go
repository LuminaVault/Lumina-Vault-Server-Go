package me

import (
	"errors"
	"lumina/internal/auth"
	"lumina/internal/modules/me/db"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// ─── /v1/me/location (LastKnownLocationController.swift) ──────────────────

func (m *Module) getLocation(c *echo.Context) error {
	tenant := auth.UserID(c)
	row, err := m.q.LocationLoad(ctx(c), tenant)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.LastLocationLat == nil || row.LastLocationLng == nil)) {
		return wire.JSON(c, http.StatusOK, LastKnownLocationResponse{Cached: false})
	}
	if err != nil {
		return err
	}
	captured := time.Unix(0, 0).UTC()
	if row.LastLocationAt != nil {
		captured = *row.LastLocationAt
	}
	return wire.JSON(c, http.StatusOK, LastKnownLocationResponse{
		Cached: true, Place: row.LastLocationPlace, Latitude: row.LastLocationLat, Longitude: row.LastLocationLng,
		CapturedAt: &wire.Time{Time: captured},
	})
}

func (m *Module) deleteLocation(c *echo.Context) error {
	if err := m.q.LocationClear(ctx(c), auth.UserID(c)); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// ─── /v1/me/apns-categories (ApnsCategoryPrefsController.swift) ───────────

func apnsResponse(r db.ApnsPrefsGetRow) APNSCategoryPrefsResponse {
	meeting := r.MeetingBriefEnabled
	return APNSCategoryPrefsResponse{
		ChatEnabled: r.ChatEnabled, NudgeEnabled: r.NudgeEnabled, DigestEnabled: r.DigestEnabled,
		ApprovalEnabled: r.ApprovalEnabled, RunCompletedEnabled: r.RunCompletedEnabled, MeetingBriefEnabled: &meeting,
	}
}

// apnsDefaults mirrors the column defaults: everything on but the opt-in
// meeting brief.
var apnsDefaults = db.ApnsPrefsGetRow{ChatEnabled: true, NudgeEnabled: true, DigestEnabled: true, ApprovalEnabled: true, RunCompletedEnabled: true}

func (m *Module) getApns(c *echo.Context) error {
	tenant := auth.UserID(c)
	row, err := m.q.ApnsPrefsGet(ctx(c), tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return wire.JSON(c, http.StatusOK, apnsResponse(apnsDefaults))
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, apnsResponse(row))
}

// putApns is a sparse update: a field the body leaves out keeps its value.
// Fluent's save inserts a missing row (every field plus updated_at) and skips
// the UPDATE entirely when nothing was set.
func (m *Module) putApns(c *echo.Context) error {
	tenant := auth.UserID(c)
	body, err := wire.Bind[APNSCategoryPrefsPutRequest](c)
	if err != nil {
		return err
	}
	row, err := m.q.ApnsPrefsGet(ctx(c), tenant)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if !exists {
		row = apnsDefaults
	}
	changed := false
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
			changed = true
		}
	}
	set(&row.ChatEnabled, body.ChatEnabled)
	set(&row.NudgeEnabled, body.NudgeEnabled)
	set(&row.DigestEnabled, body.DigestEnabled)
	set(&row.ApprovalEnabled, body.ApprovalEnabled)
	set(&row.RunCompletedEnabled, body.RunCompletedEnabled)
	set(&row.MeetingBriefEnabled, body.MeetingBriefEnabled)
	now := m.now()
	switch {
	case !exists:
		err = m.q.ApnsPrefsInsert(ctx(c), db.ApnsPrefsInsertParams{
			TenantID: tenant, Chat: row.ChatEnabled, Nudge: row.NudgeEnabled, Digest: row.DigestEnabled,
			Approval: row.ApprovalEnabled, RunCompleted: row.RunCompletedEnabled, MeetingBrief: row.MeetingBriefEnabled, Now: now,
		})
	case changed:
		err = m.q.ApnsPrefsUpdate(ctx(c), db.ApnsPrefsUpdateParams{
			Chat: row.ChatEnabled, Nudge: row.NudgeEnabled, Digest: row.DigestEnabled, Approval: row.ApprovalEnabled,
			RunCompleted: row.RunCompletedEnabled, MeetingBrief: row.MeetingBriefEnabled, Now: now, TenantID: tenant,
		})
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, apnsResponse(row))
}
