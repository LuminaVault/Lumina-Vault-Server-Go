package kanban

import (
	"encoding/json"
	"lumina/internal/wire"
	"reflect"
)

// Hand-written wire types. The generated models (models.gen.go) follow the
// spec, which lacks fields Swift sends (createdByUserId, jobConfig, …) and
// orders request fields alphabetically. Request field order here is the
// Swift declaration order (LuminaVaultShared APIDTOs.swift), because Swift
// reports the first failing key in that order and wire.Bind walks fields in
// struct order.

type boardDTO struct {
	ID              wire.UUID   `json:"id"`
	Title           string      `json:"title"`
	Version         int64       `json:"version"`
	Columns         []columnDTO `json:"columns"`
	CreatedByUserID *wire.UUID  `json:"createdByUserId,omitempty"`
	UpdatedByUserID *wire.UUID  `json:"updatedByUserId,omitempty"`
}

type columnDTO struct {
	ID    wire.UUID `json:"id"`
	Title string    `json:"title"`
	Rank  string    `json:"rank"`
	Cards []cardDTO `json:"cards"`
}

type cardDTO struct {
	ID              wire.UUID         `json:"id"`
	ColumnID        wire.UUID         `json:"columnID"`
	Title           string            `json:"title"`
	Body            *string           `json:"body,omitempty"`
	Priority        *string           `json:"priority,omitempty"`
	DueAt           *wire.Time        `json:"dueAt,omitempty"`
	Rank            string            `json:"rank"`
	UpdatedAt       *wire.Time        `json:"updatedAt,omitempty"`
	JobConfig       *cardJobConfigDTO `json:"jobConfig,omitempty"`
	CreatedByUserID *wire.UUID        `json:"createdByUserId,omitempty"`
	UpdatedByUserID *wire.UUID        `json:"updatedByUserId,omitempty"`
}

type cardJobConfigDTO struct {
	Source     string     `json:"source"`
	Cron       *string    `json:"cron,omitempty"`
	RunAt      *wire.Time `json:"runAt,omitempty"`
	Domain     *string    `json:"domain,omitempty"`
	Prompt     *string    `json:"prompt,omitempty"`
	SpaceID    *wire.UUID `json:"spaceID,omitempty"`
	JobSlug    *string    `json:"jobSlug,omitempty"`
	PromotedAt *wire.Time `json:"promotedAt,omitempty"`
}

type boardCreateRequest struct {
	Title string `json:"title"`
}

type boardPatchRequest struct {
	Title    *string `json:"title"`
	Archived *bool   `json:"archived"`
}

type columnCreateRequest struct {
	Title string `json:"title"`
}

type columnPatchRequest struct {
	Title string `json:"title"`
}

type columnReorderRequest struct {
	ColumnID wire.UUID  `json:"columnID"`
	BeforeID *wire.UUID `json:"beforeID"`
	AfterID  *wire.UUID `json:"afterID"`
}

type cardCreateRequest struct {
	ColumnID wire.UUID     `json:"columnID"`
	Title    string        `json:"title"`
	Body     *string       `json:"body"`
	Priority *cardPriority `json:"priority"`
	DueAt    *wire.Time    `json:"dueAt"`
}

type cardPatchRequest struct {
	Title    *string       `json:"title"`
	Body     *string       `json:"body"`
	Priority *cardPriority `json:"priority"`
	DueAt    *wire.Time    `json:"dueAt"`
}

type cardMoveRequest struct {
	ToColumnID wire.UUID  `json:"toColumnID"`
	BeforeID   *wire.UUID `json:"beforeID"`
	AfterID    *wire.UUID `json:"afterID"`
}

type cardPromoteRequest struct {
	Cron    *string    `json:"cron"`
	RunAt   *wire.Time `json:"runAt"`
	Domain  *string    `json:"domain"`
	Prompt  *string    `json:"prompt"`
	SpaceID *wire.UUID `json:"spaceID"`
}

// cardPriority is the CardPriority String enum. An unknown value is
// Foundation's dataCorrupted error, a non-string a type mismatch on String.
type cardPriority string

var validPriorities = map[string]bool{"low": true, "medium": true, "high": true, "urgent": true}

func (p *cardPriority) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !validPriorities[s] {
		return &wire.CorruptedError{Message: "Cannot initialize CardPriority from invalid String value " + s}
	}
	*p = cardPriority(s)
	return nil
}

// priorityOut is CardPriority(rawValue:) on the stored text: unknown → nil.
func priorityOut(raw *string) *string {
	if raw == nil || !validPriorities[*raw] {
		return nil
	}
	return raw
}
