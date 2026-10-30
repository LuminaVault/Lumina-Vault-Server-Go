// Package kanban serves /v1/boards and /v1/cards: the Go port of
// LuminaVaultServer Sources/App/Kanban (KanbanController, KanbanService,
// RankString) plus the card→job half of Skills/JobAuthoring.swift.
package kanban

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/kanban/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/kanban/db"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultactivity"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

// boardVersionHeader is KanbanController.expectedVersionHeader.
const boardVersionHeader = "X-Board-Version"

var (
	errBoardNotFound   = wire.NewError(http.StatusNotFound, "board_not_found")
	errColumnNotFound  = wire.NewError(http.StatusNotFound, "column_not_found")
	errCardNotFound    = wire.NewError(http.StatusNotFound, "card_not_found")
	errVersionConflict = wire.NewError(http.StatusConflict, "board_version_conflict")
	errVersionRequired = wire.NewError(http.StatusConflict, "shared board mutation requires X-Board-Version")
	errColumnNeighbor  = wire.NewError(http.StatusUnprocessableEntity, "column_neighbor_not_found")
	errCardNeighbor    = wire.NewError(http.StatusUnprocessableEntity, "card_neighbor_not_found")
	errWriteRequired   = wire.NewError(http.StatusForbidden, "vault write permission required")
	errNeedsSchedule   = wire.NewError(http.StatusBadRequest, "card_job_requires_cron_or_run_at")
	errNeedsPrompt     = wire.NewError(http.StatusBadRequest, "card_job_requires_prompt_or_body")
)

// Module is the kanban route group. Swift wraps both groups in
// jwtAuthenticator + RateLimitMiddleware(.settingsByUser) (App+build.swift
// "Native Kanban"); identify + limits reproduce that, and each handler
// resolves vault access itself, so a bad X-Vault-ID is a 400 before the
// missing token is a 401, and an anonymous request still counts against the
// "ip:" bucket.
type Module struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	access   *vaultaccess.Service
	activity *vaultactivity.Recorder
	jobs     jobAuthoring
	identify echo.MiddlewareFunc
	limits   *ratelimit.Store
	now      func() time.Time
	log      *slog.Logger
}

// New builds the module. identify is auth.Identify; limits is the process's
// shared rate-limit store; vault is the tenant vault root (VAULT_ROOT_PATH).
func New(pool *pgxpool.Pool, identify echo.MiddlewareFunc, limits *ratelimit.Store, vault vaultpaths.Paths) *Module {
	log := slog.Default().With("module", "kanban")
	return &Module{
		pool:     pool,
		q:        db.New(pool),
		access:   vaultaccess.New(pool),
		activity: vaultactivity.New(pool),
		jobs:     jobAuthoring{paths: vault, log: log},
		identify: identify,
		limits:   limits,
		now:      time.Now,
		log:      log,
	}
}

// Register ignores the shared auth middleware on purpose; see Module.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{m.identify, ratelimit.Middleware(m.limits, ratelimit.SettingsByUser, ratelimit.UserOrIPKey)}
	r.GET("/v1/boards", m.listBoards, mw...)
	r.POST("/v1/boards", m.createBoard, mw...)
	r.GET("/v1/boards/:boardID", m.getBoard, mw...)
	r.GET("/v1/boards/:boardID/version", m.getVersion, mw...)
	r.PATCH("/v1/boards/:boardID", m.patchBoard, mw...)
	r.DELETE("/v1/boards/:boardID", m.deleteBoard, mw...)
	r.POST("/v1/boards/:boardID/columns", m.createColumn, mw...)
	r.PATCH("/v1/boards/:boardID/columns/:columnID", m.patchColumn, mw...)
	r.DELETE("/v1/boards/:boardID/columns/:columnID", m.deleteColumn, mw...)
	r.POST("/v1/boards/:boardID/columns/reorder", m.reorderColumn, mw...)
	r.POST("/v1/boards/:boardID/cards", m.createCard, mw...)
	r.PATCH("/v1/cards/:cardID", m.patchCard, mw...)
	r.DELETE("/v1/cards/:cardID", m.deleteCard, mw...)
	r.POST("/v1/cards/:cardID/move", m.moveCard, mw...)
	r.POST("/v1/cards/:cardID/promote", m.promoteCard, mw...)
}

// MARK: - Boards

func (m *Module) listBoards(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	if err := m.defaultBoard(ctx, acc.VaultID); err != nil {
		return err
	}
	rows, err := m.q.ListBoards(ctx, acc.VaultID)
	if err != nil {
		return err
	}
	out := make([]KanbanBoardSummary, 0, len(rows))
	for _, b := range rows {
		cols, err := m.q.CountColumns(ctx, b.ID)
		if err != nil {
			return err
		}
		cards, err := m.q.CountCards(ctx, b.ID)
		if err != nil {
			return err
		}
		out = append(out, KanbanBoardSummary{
			Id: wire.UUID{UUID: b.ID}, Title: b.Title, Version: b.Version,
			ColumnCount: int(cols), CardCount: int(cards), UpdatedAt: wireTime(b.UpdatedAt),
		})
	}
	return wire.JSON(c, http.StatusOK, out)
}

// defaultBoard is KanbanService.defaultBoard: the oldest live board, or a new
// "My Board" with Todo / Doing / Done (each column its own transaction).
func (m *Module) defaultBoard(ctx context.Context, tenant uuid.UUID) error {
	_, err := m.q.DefaultBoard(ctx, tenant)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	id, err := m.insertBoard(ctx, tenant, "My Board")
	if err != nil {
		return err
	}
	for _, t := range []string{"Todo", "Doing", "Done"} {
		if err := m.tx(ctx, func(q *db.Queries) error {
			return m.createColumnTx(ctx, q, tenant, id, t, nil)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m *Module) insertBoard(ctx context.Context, tenant uuid.UUID, title string) (uuid.UUID, error) {
	id := uuid.New()
	now := m.now()
	return id, m.q.InsertBoard(ctx, db.InsertBoardParams{ID: id, TenantID: tenant, Title: title, CreatedAt: &now})
}

func (m *Module) createBoard(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[boardCreateRequest](c)
	if err != nil {
		return err
	}
	id, err := m.insertBoard(ctx, acc.VaultID, body.Title)
	if err != nil {
		return err
	}
	actor := auth.UserID(c)
	now := m.now()
	if err := m.q.SetBoardAuthors(ctx, db.SetBoardAuthorsParams{ID: id, CreatedByUserID: &actor, UpdatedAt: &now}); err != nil {
		return err
	}
	m.record(c, acc, "board.created", "board", &id, &body.Title)
	return m.respondSnapshot(c, acc.VaultID, id)
}

func (m *Module) getBoard(c *echo.Context) error {
	acc, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	id, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	return m.respondSnapshot(c, acc.VaultID, id)
}

func (m *Module) getVersion(c *echo.Context) error {
	acc, err := m.access.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	id, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	b, err := m.requireBoard(c.Request().Context(), m.q, acc.VaultID, id)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, BoardVersionResponse{Version: b.Version})
}

func (m *Module) patchBoard(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[boardPatchRequest](c)
	if err != nil {
		return err
	}
	id, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	err = m.tx(ctx, func(q *db.Queries) error {
		if _, err := m.claim(ctx, q, acc.VaultID, id, expected); err != nil {
			return err
		}
		b, err := m.requireBoard(ctx, q, acc.VaultID, id)
		if err != nil {
			return err
		}
		changed := false
		if body.Title != nil {
			b.Title, changed = *body.Title, true
		}
		if body.Archived != nil && *body.Archived {
			now := m.now()
			b.ArchivedAt, changed = &now, true
		}
		if !changed { // Fluent skips an update with no assigned fields
			return nil
		}
		now := m.now()
		return q.UpdateBoard(ctx, db.UpdateBoardParams{ID: id, Title: b.Title, ArchivedAt: b.ArchivedAt, UpdatedAt: &now})
	})
	if err != nil {
		return err
	}
	snap, err := m.snapshot(ctx, acc.VaultID, id)
	if err != nil {
		return err
	}
	m.record(c, acc, "board.updated", "board", &id, &snap.Title)
	return wire.JSON(c, http.StatusOK, snap)
}

func (m *Module) deleteBoard(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	id, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	err = m.tx(ctx, func(q *db.Queries) error {
		if _, err := m.claim(ctx, q, acc.VaultID, id, expected); err != nil {
			return err
		}
		if _, err := m.requireBoard(ctx, q, acc.VaultID, id); err != nil {
			return err
		}
		return q.DeleteBoard(ctx, id)
	})
	if err != nil {
		return err
	}
	m.record(c, acc, "board.deleted", "board", &id, nil)
	return c.NoContent(http.StatusNoContent)
}

// MARK: - Columns

func (m *Module) createColumn(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[columnCreateRequest](c)
	if err != nil {
		return err
	}
	bid, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	if err := m.tx(ctx, func(q *db.Queries) error {
		return m.createColumnTx(ctx, q, acc.VaultID, bid, body.Title, expected)
	}); err != nil {
		return err
	}
	m.record(c, acc, "column.created", "column", nil, &body.Title)
	return m.respondSnapshot(c, acc.VaultID, bid)
}

func (m *Module) createColumnTx(ctx context.Context, q *db.Queries, tenant, board uuid.UUID, title string, expected *int64) error {
	if _, err := m.claim(ctx, q, tenant, board, expected); err != nil {
		return err
	}
	last, err := optional(q.LastColumnRank(ctx, board))
	if err != nil {
		return err
	}
	now := m.now()
	return q.InsertColumn(ctx, db.InsertColumnParams{
		ID: uuid.New(), TenantID: tenant, BoardID: board, Title: title, Rank: Between(last, nil), CreatedAt: &now,
	})
}

func (m *Module) patchColumn(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[columnPatchRequest](c)
	if err != nil {
		return err
	}
	// Swift reads columnID before boardID.
	cid, err := uuidParam(c, "columnID")
	if err != nil {
		return err
	}
	bid, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	err = m.tx(ctx, func(q *db.Queries) error {
		if _, err := m.boardColumn(ctx, q, acc.VaultID, bid, cid, expected); err != nil {
			return err
		}
		now := m.now()
		return q.UpdateColumnTitle(ctx, db.UpdateColumnTitleParams{ID: cid, Title: body.Title, UpdatedAt: &now})
	})
	if err != nil {
		return err
	}
	snap, err := m.snapshot(ctx, acc.VaultID, bid)
	if err != nil {
		return err
	}
	m.record(c, acc, "column.updated", "column", &cid, &body.Title)
	return wire.JSON(c, http.StatusOK, snap)
}

func (m *Module) deleteColumn(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	cid, err := uuidParam(c, "columnID")
	if err != nil {
		return err
	}
	bid, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	err = m.tx(ctx, func(q *db.Queries) error {
		if _, err := m.boardColumn(ctx, q, acc.VaultID, bid, cid, expected); err != nil {
			return err
		}
		return q.DeleteColumn(ctx, cid)
	})
	if err != nil {
		return err
	}
	snap, err := m.snapshot(ctx, acc.VaultID, bid)
	if err != nil {
		return err
	}
	m.record(c, acc, "column.deleted", "column", &cid, nil)
	return wire.JSON(c, http.StatusOK, snap)
}

func (m *Module) reorderColumn(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[columnReorderRequest](c)
	if err != nil {
		return err
	}
	bid, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	cid := body.ColumnID.UUID
	err = m.tx(ctx, func(q *db.Queries) error {
		if _, err := m.boardColumn(ctx, q, acc.VaultID, bid, cid, expected); err != nil {
			return err
		}
		before, err := siblingColumn(ctx, q, body.BeforeID, bid)
		if err != nil {
			return err
		}
		after, err := siblingColumn(ctx, q, body.AfterID, bid)
		if err != nil {
			return err
		}
		now := m.now()
		return q.UpdateColumnRank(ctx, db.UpdateColumnRankParams{ID: cid, Rank: Between(before, after), UpdatedAt: &now})
	})
	if err != nil {
		return err
	}
	snap, err := m.snapshot(ctx, acc.VaultID, bid)
	if err != nil {
		return err
	}
	m.record(c, acc, "column.reordered", "column", &cid, nil)
	return wire.JSON(c, http.StatusOK, snap)
}

// boardColumn is the shared prologue of patch/delete/reorder column: claim
// the board, then the column must be the tenant's and on that board.
func (m *Module) boardColumn(ctx context.Context, q *db.Queries, tenant, board, column uuid.UUID, expected *int64) (db.GetColumnRow, error) {
	if _, err := m.claim(ctx, q, tenant, board, expected); err != nil {
		return db.GetColumnRow{}, err
	}
	col, err := requireColumn(ctx, q, tenant, column)
	if err != nil {
		return col, err
	}
	if col.BoardID != board {
		return col, errColumnNotFound
	}
	return col, nil
}

func siblingColumn(ctx context.Context, q *db.Queries, id *wire.UUID, board uuid.UUID) (*string, error) {
	if id == nil {
		return nil, nil //nolint:nilnil // no neighbour = unbounded
	}
	rank, err := q.SiblingColumn(ctx, db.SiblingColumnParams{ID: id.UUID, BoardID: board})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errColumnNeighbor
	}
	return &rank, err
}

// MARK: - Cards

func (m *Module) createCard(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[cardCreateRequest](c)
	if err != nil {
		return err
	}
	bid, err := uuidParam(c, "boardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	id := uuid.New()
	cid := body.ColumnID.UUID
	var priority *string
	if body.Priority != nil {
		s := string(*body.Priority)
		priority = &s
	}
	var dueAt *time.Time
	if body.DueAt != nil {
		dueAt = &body.DueAt.Time
	}
	var rank string
	err = m.tx(ctx, func(q *db.Queries) error {
		if _, err := m.claim(ctx, q, acc.VaultID, bid, expected); err != nil {
			return err
		}
		col, err := requireColumn(ctx, q, acc.VaultID, cid)
		if err != nil {
			return err
		}
		if col.BoardID != bid {
			return errColumnNotFound
		}
		last, err := optional(q.LastCardRank(ctx, cid))
		if err != nil {
			return err
		}
		rank = Between(last, nil)
		now := m.now()
		return q.InsertCard(ctx, db.InsertCardParams{
			ID: id, TenantID: acc.VaultID, BoardID: bid, ColumnID: cid, Title: body.Title, Body: body.Body,
			Priority: priority, DueAt: dueAt, Rank: rank, CreatedAt: &now,
		})
	})
	if err != nil {
		return err
	}
	actor := auth.UserID(c)
	now := m.now()
	if err := m.q.SetCardAuthors(ctx, db.SetCardAuthorsParams{ID: id, CreatedByUserID: &actor, UpdatedAt: &now}); err != nil {
		return err
	}
	m.record(c, acc, "card.created", "card", &id, &body.Title)
	return wire.JSON(c, http.StatusOK, cardDTO{
		ID: wire.UUID{UUID: id}, ColumnID: body.ColumnID, Title: body.Title, Body: body.Body,
		Priority: priority, DueAt: wireTime(dueAt), Rank: rank, UpdatedAt: wireTime(&now),
		CreatedByUserID: wireUUID(&actor), UpdatedByUserID: wireUUID(&actor),
	})
}

func (m *Module) patchCard(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[cardPatchRequest](c)
	if err != nil {
		return err
	}
	id, err := uuidParam(c, "cardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	var out cardDTO
	err = m.tx(ctx, func(q *db.Queries) error {
		card, err := requireCard(ctx, q, acc.VaultID, id)
		if err != nil {
			return err
		}
		if _, err := m.claim(ctx, q, acc.VaultID, card.BoardID, expected); err != nil {
			return err
		}
		changed := false
		if body.Title != nil {
			card.Title, changed = *body.Title, true
		}
		if body.Body != nil {
			card.Body, changed = body.Body, true
		}
		if body.Priority != nil {
			s := string(*body.Priority)
			card.Priority, changed = &s, true
		}
		if body.DueAt != nil {
			card.DueAt, changed = &body.DueAt.Time, true
		}
		if changed {
			now := m.now()
			card.UpdatedAt = &now
			if err := q.UpdateCard(ctx, db.UpdateCardParams{
				ID: id, Title: card.Title, Body: card.Body, Priority: card.Priority, DueAt: card.DueAt, UpdatedAt: &now,
			}); err != nil {
				return err
			}
		}
		out, err = toCardDTO(db.ListCardsRow(card))
		return err
	})
	if err != nil {
		return err
	}
	m.record(c, acc, "card.updated", "card", &id, &out.Title)
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) deleteCard(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	id, err := uuidParam(c, "cardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	err = m.tx(ctx, func(q *db.Queries) error {
		card, err := requireCard(ctx, q, acc.VaultID, id)
		if err != nil {
			return err
		}
		if _, err := m.claim(ctx, q, acc.VaultID, card.BoardID, expected); err != nil {
			return err
		}
		return q.DeleteCard(ctx, id)
	})
	if err != nil {
		return err
	}
	m.record(c, acc, "card.deleted", "card", &id, nil)
	return c.NoContent(http.StatusNoContent)
}

func (m *Module) moveCard(c *echo.Context) error {
	ctx := c.Request().Context()
	acc, err := m.access.Resolve(c, vaultaccess.Write)
	if err != nil {
		return err
	}
	body, err := wire.Bind[cardMoveRequest](c)
	if err != nil {
		return err
	}
	id, err := uuidParam(c, "cardID")
	if err != nil {
		return err
	}
	expected, err := expectedVersion(c, acc)
	if err != nil {
		return err
	}
	to := body.ToColumnID.UUID
	var out cardDTO
	err = m.tx(ctx, func(q *db.Queries) error {
		card, err := requireCard(ctx, q, acc.VaultID, id)
		if err != nil {
			return err
		}
		if _, err := m.claim(ctx, q, acc.VaultID, card.BoardID, expected); err != nil {
			return err
		}
		col, err := requireColumn(ctx, q, acc.VaultID, to)
		if err != nil {
			return err
		}
		if col.BoardID != card.BoardID {
			return errColumnNotFound
		}
		before, err := siblingCard(ctx, q, body.BeforeID, to)
		if err != nil {
			return err
		}
		after, err := siblingCard(ctx, q, body.AfterID, to)
		if err != nil {
			return err
		}
		now := m.now()
		card.ColumnID, card.Rank, card.UpdatedAt = to, Between(before, after), &now
		if err := q.MoveCard(ctx, db.MoveCardParams{ID: id, ColumnID: to, Rank: card.Rank, UpdatedAt: &now}); err != nil {
			return err
		}
		out, err = toCardDTO(db.ListCardsRow(card))
		return err
	})
	if err != nil {
		return err
	}
	m.record(c, acc, "card.moved", "card", &id, &out.Title)
	return wire.JSON(c, http.StatusOK, out)
}

func siblingCard(ctx context.Context, q *db.Queries, id *wire.UUID, column uuid.UUID) (*string, error) {
	if id == nil {
		return nil, nil //nolint:nilnil // no neighbour = unbounded
	}
	rank, err := q.SiblingCard(ctx, db.SiblingCardParams{ID: id.UUID, ColumnID: column})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errCardNeighbor
	}
	return &rank, err
}

// MARK: - Snapshot and helpers

func (m *Module) respondSnapshot(c *echo.Context, tenant, board uuid.UUID) error {
	snap, err := m.snapshot(c.Request().Context(), tenant, board)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, snap)
}

// snapshot is KanbanService.snapshot: columns by rank, each column's cards by rank.
func (m *Module) snapshot(ctx context.Context, tenant, board uuid.UUID) (boardDTO, error) {
	b, err := m.requireBoard(ctx, m.q, tenant, board)
	if err != nil {
		return boardDTO{}, err
	}
	cols, err := m.q.ListColumns(ctx, board)
	if err != nil {
		return boardDTO{}, err
	}
	out := boardDTO{
		ID: wire.UUID{UUID: b.ID}, Title: b.Title, Version: b.Version, Columns: make([]columnDTO, 0, len(cols)),
		CreatedByUserID: wireUUID(b.CreatedByUserID), UpdatedByUserID: wireUUID(b.UpdatedByUserID),
	}
	for _, col := range cols {
		cards, err := m.q.ListCards(ctx, col.ID)
		if err != nil {
			return boardDTO{}, err
		}
		cd := columnDTO{ID: wire.UUID{UUID: col.ID}, Title: col.Title, Rank: col.Rank, Cards: make([]cardDTO, 0, len(cards))}
		for _, card := range cards {
			dto, err := toCardDTO(card)
			if err != nil {
				return boardDTO{}, err
			}
			cd.Cards = append(cd.Cards, dto)
		}
		out.Columns = append(out.Columns, cd)
	}
	return out, nil
}

// toCardDTO is KanbanService.cardDTO.
func toCardDTO(c db.ListCardsRow) (cardDTO, error) {
	ex, err := decodeExtra(c.Extra)
	if err != nil {
		return cardDTO{}, err
	}
	out := cardDTO{
		ID: wire.UUID{UUID: c.ID}, ColumnID: wire.UUID{UUID: c.ColumnID}, Title: c.Title, Body: c.Body,
		Priority: priorityOut(c.Priority), DueAt: wireTime(c.DueAt), Rank: c.Rank, UpdatedAt: wireTime(c.UpdatedAt),
		CreatedByUserID: wireUUID(c.CreatedByUserID), UpdatedByUserID: wireUUID(c.UpdatedByUserID),
	}
	if ex != nil && ex.Job != nil {
		j := ex.Job
		out.JobConfig = &cardJobConfigDTO{
			Source: j.Source, Cron: j.Cron, RunAt: refTime(j.RunAt), Domain: j.Domain, Prompt: j.Prompt,
			SpaceID: j.SpaceID, JobSlug: j.JobSlug, PromotedAt: refTime(j.PromotedAt),
		}
	}
	return out, nil
}

// claim is KanbanService.claimMutation: bump the board version (only from
// the expected one when given) inside the caller's transaction. No row means
// the board is not the tenant's (404) or the version is stale (409).
func (m *Module) claim(ctx context.Context, q *db.Queries, tenant, board uuid.UUID, expected *int64) (int64, error) {
	var v int64
	var err error
	if expected != nil {
		v, err = q.ClaimExpectedVersion(ctx, db.ClaimExpectedVersionParams{ID: board, TenantID: tenant, Version: *expected})
	} else {
		v, err = q.ClaimVersion(ctx, db.ClaimVersionParams{ID: board, TenantID: tenant})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	if _, err := m.requireBoard(ctx, q, tenant, board); err != nil {
		return 0, err
	}
	return 0, errVersionConflict
}

func (m *Module) requireBoard(ctx context.Context, q *db.Queries, tenant, board uuid.UUID) (db.GetBoardRow, error) {
	b, err := q.GetBoard(ctx, db.GetBoardParams{ID: board, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return b, errBoardNotFound
	}
	return b, err
}

func requireColumn(ctx context.Context, q *db.Queries, tenant, column uuid.UUID) (db.GetColumnRow, error) {
	col, err := q.GetColumn(ctx, db.GetColumnParams{ID: column, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return col, errColumnNotFound
	}
	return col, err
}

func requireCard(ctx context.Context, q *db.Queries, tenant, card uuid.UUID) (db.GetCardRow, error) {
	row, err := q.GetCard(ctx, db.GetCardParams{ID: card, TenantID: tenant})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errCardNotFound
	}
	return row, err
}

// tx runs fn in one transaction, as Swift's db.transaction does.
func (m *Module) tx(ctx context.Context, fn func(q *db.Queries) error) error {
	return pgx.BeginFunc(ctx, m.pool, func(t pgx.Tx) error { return fn(m.q.WithTx(t)) })
}

// record is KanbanController.record: shared vaults only, failures swallowed
// (Swift `try?`). The in-process SSE publish is not ported (see the PR).
func (m *Module) record(c *echo.Context, acc vaultaccess.Access, action, targetType string, targetID *uuid.UUID, title *string) {
	if acc.IsPersonal {
		return
	}
	actor, ok := auth.Identity(c)
	if !ok {
		return
	}
	if err := m.activity.Record(c.Request().Context(), vaultactivity.Event{
		VaultID: acc.VaultID, Actor: actor, Action: action, TargetType: targetType, TargetID: targetID, TargetTitle: title,
	}); err != nil {
		m.log.WarnContext(c.Request().Context(), "vault activity not recorded", "action", action, "error", err)
	}
}

// expectedVersion is KanbanController.expectedVersion: ignored on the
// personal vault; required and an Int64 on a shared one.
func expectedVersion(c *echo.Context, acc vaultaccess.Access) (*int64, error) {
	if acc.IsPersonal {
		return nil, nil //nolint:nilnil // no version check on the personal vault
	}
	raw, ok := c.Request().Header[http.CanonicalHeaderKey(boardVersionHeader)]
	if !ok || len(raw) == 0 {
		return nil, errVersionRequired
	}
	v, err := strconv.ParseInt(raw[0], 10, 64)
	if err != nil {
		return nil, errVersionRequired
	}
	return &v, nil
}

// uuidParam is KanbanController.uuidParam: 400 invalid_<name>.
func uuidParam(c *echo.Context, name string) (uuid.UUID, error) {
	raw := c.Param(name)
	id, err := ids.Parse(raw)
	if err != nil || len(raw) != 36 {
		return uuid.Nil, wire.NewError(http.StatusBadRequest, "invalid_"+name)
	}
	return id, nil
}

// optional turns pgx.ErrNoRows from a :one query into nil.
func optional(s string, err error) (*string, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // no row = nil, like Fluent's first()
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func refTime(d *refDate) *wire.Time {
	if d == nil {
		return nil
	}
	return &wire.Time{Time: d.Time}
}

func wireUUID(id *uuid.UUID) *wire.UUID {
	if id == nil {
		return nil
	}
	return &wire.UUID{UUID: *id}
}
