package me

import (
	"bytes"
	"encoding/json"
	"errors"
	"lumina/internal/ids"
	"lumina/internal/modules/me/db"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// reviewLaterInterval is MemoryController.reviewLaterInterval (30 days).
const reviewLaterInterval = 30 * 24 * time.Hour

var briefKinds = map[string]bool{
	"today": true, "openLoops": true, "toRevisit": true, "learned": true,
	"onYourMind": true, "weather": true, "inbox": true, "other": true,
}

var briefRefKinds = map[string]bool{"memory": true, "commitment": true, "event": true, "soulChange": true}

// feedbackAction is BriefCardFeedbackAction: an unknown value is Foundation's
// dataCorrupted error, a non-string a type mismatch on String.
type feedbackAction string

func (a *feedbackAction) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	switch s {
	case "up", "down", "dismiss":
		*a = feedbackAction(s)
		return nil
	}
	return &wire.CorruptedError{Message: "Cannot initialize BriefCardFeedbackAction from invalid String value " + s}
}

type feedbackRequest struct {
	Action feedbackAction `json:"action"`
}

func (m *Module) getBrief(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	day, err := parseBriefDate(c.QueryParam("date"), u.timezone, m.now())
	if err != nil {
		return err
	}
	row, err := m.q.BriefByDate(ctx(c), db.BriefByDateParams{TenantID: u.id, Day: day})
	if errors.Is(err, pgx.ErrNoRows) {
		return wire.JSON(c, http.StatusOK, MeBriefResponse{})
	}
	if err != nil {
		return err
	}
	b := briefDTO(row.ID, row.Date, row.Cards, row.EndNote, row.CreatedAt)
	return wire.JSON(c, http.StatusOK, MeBriefResponse{Brief: &b})
}

// parseBriefDate is MeBriefController.parseDate: a real `yyyy-MM-dd` calendar
// day, or today in the user's timezone when missing.
func parseBriefDate(raw, timezone string, now time.Time) (string, error) {
	if raw == "" {
		return localDay(now, timezone), nil
	}
	t, err := time.Parse("2006-01-02", raw)
	if swiftstr.Count(raw) != 10 || err != nil || t.Format("2006-01-02") != raw || t.Year() < 1 {
		return "", badRequest("date must be yyyy-MM-dd")
	}
	return raw, nil
}

func briefDTO(id uuid.UUID, date, cards, endNote string, createdAt time.Time) BriefDTO {
	return BriefDTO{Id: wire.UUID{UUID: id}, Date: date, Cards: decodeCards(cards), EndNote: endNote, CreatedAt: wt(createdAt)}
}

// decodeCards is `(try? JSONDecoder().decode([BriefCardDTO].self, ...)) ?? []`:
// one malformed card empties the whole list. Unknown kinds decode as
// "unknown"; unknown keys are ignored.
func decodeCards(raw string) []BriefCardDTO {
	var arr []json.RawMessage
	if json.Unmarshal([]byte(raw), &arr) != nil || arr == nil {
		return []BriefCardDTO{}
	}
	out := make([]BriefCardDTO, 0, len(arr))
	for _, el := range arr {
		card, ok := decodeCard(el)
		if !ok {
			return []BriefCardDTO{}
		}
		out = append(out, card)
	}
	return out
}

func decodeCard(raw json.RawMessage) (BriefCardDTO, bool) {
	var obj map[string]json.RawMessage
	if !isObject(raw) || json.Unmarshal(raw, &obj) != nil {
		return BriefCardDTO{}, false
	}
	var card BriefCardDTO
	var kind string
	if !reqString(obj, "id", &card.Id) || !reqString(obj, "kind", &kind) ||
		!reqString(obj, "title", &card.Title) || !reqString(obj, "body", &card.Body) {
		return BriefCardDTO{}, false
	}
	card.Kind = BriefCardKind(knownOr(kind, briefKinds))
	if w, ok := obj["why"]; ok && !isNull(w) {
		var why string
		if json.Unmarshal(w, &why) != nil {
			return BriefCardDTO{}, false
		}
		card.Why = &why
	}
	refsRaw, ok := obj["refs"]
	if !ok || isNull(refsRaw) {
		return BriefCardDTO{}, false
	}
	var refs []json.RawMessage
	if json.Unmarshal(refsRaw, &refs) != nil || refs == nil {
		return BriefCardDTO{}, false
	}
	card.Refs = make([]BriefCardRefDTO, 0, len(refs))
	for _, r := range refs {
		var ro map[string]json.RawMessage
		if !isObject(r) || json.Unmarshal(r, &ro) != nil {
			return BriefCardDTO{}, false
		}
		var ref BriefCardRefDTO
		var rk string
		if !reqString(ro, "kind", &rk) || !reqString(ro, "id", &ref.Id) {
			return BriefCardDTO{}, false
		}
		ref.Kind = BriefCardRefKind(knownOr(rk, briefRefKinds))
		card.Refs = append(card.Refs, ref)
	}
	return card, true
}

func reqString(obj map[string]json.RawMessage, key string, dst *string) bool {
	v, ok := obj[key]
	return ok && !isNull(v) && json.Unmarshal(v, dst) == nil
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

func isObject(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	return len(t) > 0 && t[0] == '{'
}

func knownOr(v string, known map[string]bool) string {
	if known[v] {
		return v
	}
	return "unknown"
}

// postFeedback is MeBriefController.feedback + MeBriefService.feedback.
func (m *Module) postFeedback(c *echo.Context) error {
	u, err := m.loadUser(c)
	if err != nil {
		return err
	}
	briefID, err := ids.Parse(c.Param("briefId"))
	if err != nil || len(c.Param("briefId")) != 36 {
		return badRequest("invalid brief id")
	}
	cardID := c.Param("cardId")
	if cardID == "" || swiftstr.Count(cardID) > 64 {
		return badRequest("invalid card id")
	}
	body, err := wire.Bind[feedbackRequest](c)
	if err != nil {
		return err
	}
	now := m.now()
	row, err := m.q.BriefByID(ctx(c), db.BriefByIDParams{TenantID: u.id, ID: briefID})
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("brief card not found")
	}
	if err != nil {
		return err
	}
	var card *BriefCardDTO
	for _, cd := range decodeCards(row.Cards) {
		if cd.Id == cardID {
			card = &cd
			break
		}
	}
	if card == nil {
		return notFound("brief card not found")
	}
	if err := m.q.UpsertBriefFeedback(ctx(c), db.UpsertBriefFeedbackParams{
		TenantID: u.id, BriefID: briefID, CardID: cardID, CardKind: string(card.Kind), Action: string(body.Action), Now: now,
	}); err != nil {
		return err
	}
	if body.Action == "down" && card.Kind == "toRevisit" {
		actor := u.id
		for _, ref := range card.Refs {
			if ref.Kind != "memory" {
				continue
			}
			memoryID, err := ids.Parse(ref.Id)
			if err != nil || len(ref.Id) != 36 {
				continue
			}
			if err := m.q.SnoozeMemory(ctx(c), db.SnoozeMemoryParams{
				Now: now, NextReviewAt: now.Add(reviewLaterInterval), Actor: &actor,
				UpdatedAt: m.now(), TenantID: u.id, ID: memoryID,
			}); err != nil {
				return err
			}
		}
	}
	return c.NoContent(http.StatusNoContent)
}
