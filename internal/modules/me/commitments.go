package me

import (
	"encoding/json"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/modules/me/db"
	"lumina/internal/wire"
	"net/http"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// commitmentState is CommitmentState: any string decodes (an unknown one as
// "unknown"); a non-string is a type mismatch on String.
type commitmentState string

func (s *commitmentState) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	*s = commitmentState(knownState(v))
	return nil
}

func knownState(v string) string {
	switch v {
	case "open", "done", "dropped":
		return v
	}
	return "unknown"
}

type commitmentUpdateRequest struct {
	State commitmentState `json:"state"`
}

type commitmentRow struct {
	ID                uuid.UUID
	Text              string
	DueAt             *time.Time
	State             string
	SurfacedCount     int32
	MemoryID          *uuid.UUID
	SourceVaultFileID *uuid.UUID
	CreatedAt         time.Time
	DoneAt            *time.Time
}

func (r commitmentRow) dto() CommitmentDTO {
	return CommitmentDTO{
		Id: wire.UUID{UUID: r.ID}, Text: r.Text, DueAt: wtp(r.DueAt), State: CommitmentState(knownState(r.State)),
		SurfacedCount: int(r.SurfacedCount), MemoryId: wup(r.MemoryID), SourceVaultFileId: wup(r.SourceVaultFileID),
		CreatedAt: wt(r.CreatedAt), DoneAt: wtp(r.DoneAt),
	}
}

func (m *Module) listCommitments(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	filter := wire.Query(c, "state")
	if filter == "" {
		filter = "open"
	}
	var rows []commitmentRow
	switch filter {
	case "all":
		rs, err := m.q.CommitmentsAll(ctx(c), u.id)
		if err != nil {
			return err
		}
		for _, r := range rs {
			rows = append(rows, commitmentRow(r))
		}
	case "open":
		rs, err := m.q.CommitmentsOpen(ctx(c), u.id)
		if err != nil {
			return err
		}
		for _, r := range rs {
			rows = append(rows, commitmentRow(r))
		}
	case "done", "dropped":
		rs, err := m.q.CommitmentsClosed(ctx(c), db.CommitmentsClosedParams{TenantID: u.id, State: filter})
		if err != nil {
			return err
		}
		for _, r := range rs {
			rows = append(rows, commitmentRow(r))
		}
	default:
		return badRequest("state must be open, done, dropped or all")
	}
	items := make([]CommitmentDTO, 0, len(rows))
	for _, r := range rows {
		items = append(items, r.dto())
	}
	return wire.JSON(c, http.StatusOK, CommitmentsResponse{Items: items})
}

func (m *Module) patchCommitment(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	raw := c.Param("id")
	id, err := ids.Parse(raw)
	if err != nil {
		return badRequest("invalid commitment id")
	}
	body, err := wire.Bind[commitmentUpdateRequest](c)
	if err != nil {
		return err
	}
	state := string(body.State)
	if state == "unknown" {
		return badRequest("state must be open, done or dropped")
	}
	if state == "open" {
		_, err := m.q.CommitmentClash(ctx(c), db.CommitmentClashParams{TenantID: u.id, ID: id})
		if err == nil {
			return wire.NewError(http.StatusConflict, "an open commitment with the same text already exists")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	row, err := m.q.UpdateCommitment(ctx(c), db.UpdateCommitmentParams{State: state, Now: m.now(), TenantID: u.id, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("commitment not found")
	}
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, commitmentRow(row).dto())
}
