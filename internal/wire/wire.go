// Package wire holds the JSON types that must encode exactly as the Swift
// server does. oapi-codegen maps `format: uuid` to UUID and `format: date-time`
// to Time (see the module oapi-codegen.yaml files), so generated DTOs cannot
// emit Go's defaults (lowercase UUIDs, RFC3339Nano timestamps).
package wire

import (
	"bytes"
	"encoding/json"
	"lumina/internal/ids"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// swiftISO8601 is Foundation's JSONEncoder .iso8601 output: whole seconds,
// UTC, "Z" (golden: testdata/golden/todos-http.json). The iOS client decodes
// with .iso8601 too (LuminaVaultClient API/Core/Endpoint.swift).
const swiftISO8601 = "2006-01-02T15:04:05Z07:00"

// CorruptedError is Foundation's DecodingError.dataCorrupted: the value had
// the right JSON type but could not be parsed. Bind renders it as
// "The given data was not valid input: <Message>".
type CorruptedError struct{ Message string }

func (e *CorruptedError) Error() string { return e.Message }

var (
	errBadDate = &CorruptedError{"Expected date string to be ISO8601-formatted."}
	errBadUUID = &CorruptedError{"Attempted to decode UUID from invalid UUID string."}
)

// Time is a time.Time that marshals like Swift's .iso8601 date strategy.
type Time struct{ time.Time }

func (t Time) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Truncate(time.Second).Format(swiftISO8601))
}

// UnmarshalJSON accepts what the Swift server accepts (golden errors-http.json):
// "Z" or a numeric offset, with or without fractional seconds. The fraction is
// kept; it only disappears on output, as in Swift.
func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		return nil
	}
	s, err := jsonString(b)
	if err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return errBadDate
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
	s, err := jsonString(b)
	if err != nil {
		return err
	}
	parsed, err := ids.Parse(s)
	if err != nil || len(s) != 36 {
		return errBadUUID
	}
	u.UUID = parsed
	return nil
}

// jsonString decodes a JSON string; a non-string is a type mismatch against
// String, which is what Swift reports for a Date or UUID given a number.
func jsonString(b []byte) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	return s, nil
}

// String returns the Swift form; used for path params and logs.
func (u UUID) String() string { return ids.Upper(u.UUID) }
