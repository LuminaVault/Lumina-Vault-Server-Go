package kanban

import (
	"bytes"
	"encoding/json"
	"errors"
	"lumina/internal/wire"
	"math"
	"time"
)

// cardExtra is the kanban_cards.extra JSONB (Models/CardExtra.swift) as
// Fluent writes it: camelCase keys, nil keys omitted, UUIDs uppercase, and
// dates as Foundation's default .deferredToDate (seconds since 2001-01-01).
type cardExtra struct {
	Job *cardJobConfig `json:"job,omitempty"`
}

type cardJobConfig struct {
	Source     string     `json:"source"`
	Cron       *string    `json:"cron,omitempty"`
	RunAt      *refDate   `json:"runAt,omitempty"`
	Domain     *string    `json:"domain,omitempty"`
	Prompt     *string    `json:"prompt,omitempty"`
	SpaceID    *wire.UUID `json:"spaceID,omitempty"`
	JobSlug    *string    `json:"jobSlug,omitempty"`
	PromotedAt *refDate   `json:"promotedAt,omitempty"`
}

// referenceDate is Foundation's 2001-01-01T00:00:00Z.
var referenceDate = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// refDate is a Date encoded with .deferredToDate: a JSON number of seconds
// since referenceDate (Date.timeIntervalSinceReferenceDate).
type refDate struct{ time.Time }

func (d refDate) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Sub(referenceDate).Seconds())
}

func (d *refDate) UnmarshalJSON(b []byte) error {
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return err
	}
	whole, frac := math.Modf(secs)
	d.Time = referenceDate.Add(time.Duration(whole) * time.Second).Add(time.Duration(math.Round(frac * 1e9)))
	return nil
}

var errMissingSource = errors.New("kanban: extra.job has no source")

// decodeExtra loads the column as Swift's Codable does: a missing `source`
// in a present `job` fails the decode (Swift then fails the whole request).
func decodeExtra(raw []byte) (*cardExtra, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil //nolint:nilnil // NULL column = no extra, as in Swift
	}
	var probe struct {
		Job *map[string]json.RawMessage `json:"job"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	if probe.Job != nil {
		if src, ok := (*probe.Job)["source"]; !ok || bytes.Equal(bytes.TrimSpace(src), []byte("null")) {
			return nil, errMissingSource
		}
	}
	var ex cardExtra
	if err := json.Unmarshal(raw, &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}
