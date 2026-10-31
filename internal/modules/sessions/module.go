// Package sessions serves GET /v1/sessions: the Go port of LuminaVaultServer
// Sources/App/Sessions/SessionsController.swift (HER-245 / HER-259).
//
// A "session" here is a chat conversation (conversations +
// conversation_messages), not an auth session or refresh token. The route
// only reads; conversations are written by the chat modules, still in Swift.
package sessions

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/sessions/oapi-codegen.yaml api/openapi.yaml"

import (
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/sessions/db"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	defaultLimit = 50
	maxLimit     = 100
	previewChars = 140
)

type Module struct {
	q *db.Queries
}

func New(pool *pgxpool.Pool) *Module {
	return &Module{q: db.New(pool)}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/sessions", m.list, authMW)
}

func (m *Module) list(c *echo.Context) error {
	rows, err := m.q.ListSessions(c.Request().Context(), db.ListSessionsParams{
		TenantID:    auth.UserID(c),
		WorkspaceID: parseWorkspace(c.QueryParam("workspace")),
		RowLimit:    parseLimit(c.QueryParam("limit")),
	})
	if err != nil {
		return err
	}
	out := SessionListResponse{Sessions: make([]SessionDTO, 0, len(rows))}
	for _, r := range rows {
		var ws *wire.UUID
		if r.SpaceID != nil {
			ws = &wire.UUID{UUID: *r.SpaceID}
		}
		out.Sessions = append(out.Sessions, SessionDTO{
			Id:            wire.UUID{UUID: r.ID},
			Title:         r.Title,
			Preview:       swiftstr.Prefix(r.LastPreview, previewChars),
			MessageCount:  int(r.MessageCount),
			LastMessageAt: wire.Time{Time: r.UpdatedAt},
			WorkspaceID:   ws,
			// pinned / archived are not columns yet; Swift hard-codes false.
		})
	}
	return wire.JSON(c, http.StatusOK, out)
}

// parseLimit mirrors Swift: a missing or non-Int value gives 50, otherwise it
// is clamped to 1...100. strconv.Atoi and Swift's Int(String) agree on what
// parses (optional sign, ASCII digits, no overflow).
func parseLimit(raw string) int32 {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return defaultLimit
	}
	return int32(max(1, min(n, maxLimit))) //nolint:gosec // clamped to 1..100
}

// parseWorkspace mirrors `UUID(uuidString:)`: only the 36-character form, any
// letter case. Anything else means "every workspace", not an error.
func parseWorkspace(raw string) *uuid.UUID {
	if len(raw) != 36 {
		return nil
	}
	id, err := ids.Parse(raw)
	if err != nil {
		return nil
	}
	return &id
}
