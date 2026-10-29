// Package wire holds the JSON types that must encode exactly as the Swift
// server does. oapi-codegen maps `format: uuid` to UUID and `format: date-time`
// to Time (see the module oapi-codegen.yaml files), so generated DTOs cannot
// emit Go's defaults (lowercase UUIDs, RFC3339Nano timestamps).
package wire

import (
	"bytes"
	"encoding/json"
	"errors"
	"lumina/internal/ids"
	"strings"
	"time"

	"github.com/google/uuid"
)

// swiftISO8601 is Foundation's JSONEncoder .iso8601: whole seconds, UTC, "Z".
// The iOS client decodes with .iso8601 too (LuminaVaultClient
// API/Core/Endpoint.swift), which rejects fractional seconds.
const swiftISO8601 = "2006-01-02T15:04:05Z07:00"

// Time is a time.Time that marshals like Swift's .iso8601 date strategy.
type Time struct{ time.Time }

func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Truncate(time.Second).Format(swiftISO8601))
}

var errFractional = errors.New("wire: fractional seconds are not accepted (Swift .iso8601)")

// UnmarshalJSON accepts what Swift's .iso8601 decoder accepts: internet date
// time with "Z" or a numeric offset, and no fractional seconds. Go's parser
// would silently allow a fraction, so it is rejected explicitly.
func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if i := strings.IndexByte(s, 'T'); i >= 0 && strings.Contains(s[i:], ".") {
		return errFractional
	}
	parsed, err := time.Parse(swiftISO8601, s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// UUID is a uuid.UUID that marshals UPPERCASE, like Swift's UUID.
type UUID struct{ uuid.UUID }

func (u UUID) MarshalJSON() ([]byte, error) {
	return json.Marshal(ids.Upper(u.UUID))
}

func (u *UUID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := ids.Parse(s)
	if err != nil {
		return err
	}
	u.UUID = parsed
	return nil
}

// String returns the Swift form; used for path params and logs.
func (u UUID) String() string { return ids.Upper(u.UUID) }
