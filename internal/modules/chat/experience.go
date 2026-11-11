package chat

import (
	"context"
	"errors"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// ChatExperienceController: the inbox and the two preference documents.

const (
	inboxDefaultLimit = 50
	inboxMaxLimit     = 100
)

func (m *Module) inbox(c *echo.Context) error {
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	limit := inboxDefaultLimit
	if n, ok := wire.QueryInt(c, "limit"); ok {
		limit = max(1, min(n, inboxMaxLimit))
	}
	var workspace *uuid.UUID
	if raw, ok := wire.QueryParam(c, "workspace"); ok && len(raw) == 36 {
		if id, err := uuid.Parse(raw); err == nil {
			workspace = &id
		}
	}
	rows, err := m.pool.Query(c.Request().Context(), `
		SELECT c.id, c.title, c.updated_at, c.space_id,
		       COALESCE(m.cnt, 0)::int AS message_count,
		       (SELECT content FROM conversation_messages WHERE conversation_id = c.id ORDER BY created_at DESC LIMIT 1) AS last_preview
		FROM conversations c
		LEFT JOIN (SELECT conversation_id, COUNT(*) AS cnt FROM conversation_messages GROUP BY conversation_id) m
		  ON m.conversation_id = c.id
		WHERE c.tenant_id = $1 AND ($2::uuid IS NULL OR c.space_id = $2)
		ORDER BY c.updated_at DESC
		LIMIT $3`, tenant, workspace, limit)
	if err != nil {
		return errSQLUnavailableOr(err)
	}
	defer rows.Close()
	label := "Lumina"
	out := inboxResponse{Items: []inboxItem{}}
	for rows.Next() {
		var (
			id       uuid.UUID
			title    string
			updated  time.Time
			space    *uuid.UUID
			count    int
			preview  *string
			previewS string
		)
		if err := rows.Scan(&id, &title, &updated, &space, &count, &preview); err != nil {
			return err
		}
		if preview != nil {
			previewS = swiftstr.CharacterPrefix(*preview, 180)
		}
		item := inboxItem{
			ID: wire.UUID{UUID: id}, Title: title, Preview: previewS, MessageCount: count,
			LastMessageAt: wire.Time{Time: updated}, SourceLabel: &label,
		}
		if space != nil {
			item.WorkspaceID = &wire.UUID{UUID: *space}
		}
		out.Items = append(out.Items, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, out)
}

// errSQLUnavailableOr keeps the query's own error: Swift's "sql_unavailable" is only
// for a database that isn't SQL, which never happens here.
func errSQLUnavailableOr(err error) error {
	if err == nil {
		return errSQLUnavailable
	}
	return err
}

type preferenceRow struct {
	exists                 bool
	autoExpandThinking     bool
	sendOnReturn           bool
	hybridProfile          string
	localFallbackEnabled   bool
	cloudFallbackEnabled   bool
	syncLocalConversations bool
}

// defaultPreferences is UserChatPreference.init().
func defaultPreferences() preferenceRow {
	return preferenceRow{
		autoExpandThinking: true, hybridProfile: "balanced",
		localFallbackEnabled: true, cloudFallbackEnabled: true, syncLocalConversations: true,
	}
}

func (m *Module) loadPreferences(ctx context.Context, tenant uuid.UUID) (preferenceRow, error) {
	p := preferenceRow{exists: true}
	err := m.pool.QueryRow(ctx, `SELECT auto_expand_thinking, send_on_return, hybrid_profile, local_fallback_enabled,
		cloud_fallback_enabled, sync_local_conversations FROM user_chat_preferences WHERE tenant_id = $1 LIMIT 1`, tenant).
		Scan(&p.autoExpandThinking, &p.sendOnReturn, &p.hybridProfile, &p.localFallbackEnabled, &p.cloudFallbackEnabled, &p.syncLocalConversations)
	if errors.Is(err, pgx.ErrNoRows) {
		return defaultPreferences(), nil
	}
	return p, err
}

// savePreferences is row.save: an insert for a new row, an update otherwise. Fluent
// stamps updated_at on both, created_at on insert.
func (m *Module) savePreferences(ctx context.Context, tenant uuid.UUID, p preferenceRow) error {
	now := m.now()
	if !p.exists {
		_, err := m.pool.Exec(ctx, `INSERT INTO user_chat_preferences
			(id, tenant_id, auto_expand_thinking, send_on_return, hybrid_profile, local_fallback_enabled,
			 cloud_fallback_enabled, sync_local_conversations, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
			uuid.New(), tenant, p.autoExpandThinking, p.sendOnReturn, p.hybridProfile, p.localFallbackEnabled,
			p.cloudFallbackEnabled, p.syncLocalConversations, now)
		return err
	}
	_, err := m.pool.Exec(ctx, `UPDATE user_chat_preferences SET auto_expand_thinking = $2, send_on_return = $3,
		hybrid_profile = $4, local_fallback_enabled = $5, cloud_fallback_enabled = $6, sync_local_conversations = $7,
		updated_at = $8 WHERE tenant_id = $1`,
		tenant, p.autoExpandThinking, p.sendOnReturn, p.hybridProfile, p.localFallbackEnabled,
		p.cloudFallbackEnabled, p.syncLocalConversations, now)
	return err
}

func (p preferenceRow) chatDTO() chatPreferencesResponse {
	return chatPreferencesResponse{Preferences: chatPreferences{AutoExpandThinking: p.autoExpandThinking, SendOnReturn: p.sendOnReturn}}
}

func (p preferenceRow) hybridDTO() hybridPreferences {
	profile := hybridProfile(p.hybridProfile)
	if profile != "private" && profile != "balanced" && profile != "quality" {
		profile = "balanced"
	}
	return hybridPreferences{
		Profile: profile, LocalFallbackEnabled: p.localFallbackEnabled,
		CloudFallbackEnabled: p.cloudFallbackEnabled, SyncLocalConversations: p.syncLocalConversations,
	}
}

func (m *Module) getPreferences(c *echo.Context) error {
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	p, err := m.loadPreferences(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, p.chatDTO())
}

func (m *Module) putPreferences(c *echo.Context) error {
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[chatPreferencesPut](c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	p, err := m.loadPreferences(ctx, tenant)
	if err != nil {
		return err
	}
	p.autoExpandThinking = body.Preferences.AutoExpandThinking
	p.sendOnReturn = body.Preferences.SendOnReturn
	if err := m.savePreferences(ctx, tenant, p); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, p.chatDTO())
}

func (m *Module) getHybridPreferences(c *echo.Context) error {
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	p, err := m.loadPreferences(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, p.hybridDTO())
}

func (m *Module) putHybridPreferences(c *echo.Context) error {
	tenant, err := requireUser(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[hybridPreferences](c)
	if err != nil {
		return err
	}
	if body.Profile == "private" && body.CloudFallbackEnabled {
		return errPrivateFallback
	}
	ctx := c.Request().Context()
	p, err := m.loadPreferences(ctx, tenant)
	if err != nil {
		return err
	}
	p.hybridProfile = string(body.Profile)
	p.localFallbackEnabled = body.LocalFallbackEnabled
	p.cloudFallbackEnabled = body.CloudFallbackEnabled
	p.syncLocalConversations = body.Profile != "private" && body.SyncLocalConversations
	if err := m.savePreferences(ctx, tenant, p); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, p.hybridDTO())
}
