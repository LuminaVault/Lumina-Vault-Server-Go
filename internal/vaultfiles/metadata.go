package vaultfiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/wire"
	"math"
	"time"

	"github.com/google/uuid"
)

// referenceDateUnix is 2001-01-01T00:00:00Z, the epoch of Swift's Date.
const referenceDateUnix = 978307200

// Metadata is VaultFileMetadata, the JSON sidecar in vault_files.metadata.
// Every field is optional and a missing key is nil. Tags keeps Swift's
// distinction between nil (no key) and empty ("tags":[]).
//
// MarshalJSON and UnmarshalJSON are the column's codec. Fluent stores it with a
// bare JSONEncoder (PostgresNIO _defaultJSONEncoder), so a date is a number of
// seconds since 2001-01-01 and a UUID is uppercase. The X-Vault-Metadata header
// has its own format: see ParseSidecar.
type Metadata struct {
	EnrichmentStatus *string
	Title            *string
	Tags             []string
	IsTodo           *bool
	Done             *bool
	DueAt            *time.Time
	ProjectID        *uuid.UUID
	Provenance       *string
	SuggestedSpaceID *uuid.UUID
}

type metadataJSON struct {
	EnrichmentStatus *string     `json:"enrichmentStatus,omitempty"`
	Title            *string     `json:"title,omitempty"`
	Tags             *[]string   `json:"tags,omitempty"`
	IsTodo           *bool       `json:"isTodo,omitempty"`
	Done             *bool       `json:"done,omitempty"`
	DueAt            *float64    `json:"dueAt,omitempty"`
	ProjectID        *wire.UUID  `json:"projectID,omitempty"`
	Provenance       *string     `json:"provenance,omitempty"`
	SuggestedSpaceID *wire.UUID  `json:"suggestedSpaceID,omitempty"`
}

// MarshalJSON writes what Swift's JSONEncoder writes, up to key order and
// string escaping, which a jsonb column doesn't keep.
func (m Metadata) MarshalJSON() ([]byte, error) {
	out := metadataJSON{
		EnrichmentStatus: m.EnrichmentStatus,
		Title:            m.Title,
		IsTodo:           m.IsTodo,
		Done:             m.Done,
		Provenance:       m.Provenance,
		ProjectID:        wireUUID(m.ProjectID),
		SuggestedSpaceID: wireUUID(m.SuggestedSpaceID),
	}
	if m.Tags != nil {
		out.Tags = &m.Tags
	}
	if m.DueAt != nil {
		seconds := referenceSeconds(*m.DueAt)
		out.DueAt = &seconds
	}
	return json.Marshal(out)
}

// UnmarshalJSON decodes the column as Swift's JSONDecoder does, and that is
// stricter than encoding/json: keys are case sensitive and a null inside tags
// is an error. Unknown keys are ignored, so re-saving a row drops them, as in
// Swift. A failure fails the query that read the row, as Fluent's does.
func (m *Metadata) UnmarshalJSON(b []byte) error {
	return m.decode(b, func(raw json.RawMessage) (*time.Time, error) {
		var seconds float64
		if err := json.Unmarshal(raw, &seconds); err != nil {
			return nil, fmt.Errorf("dueAt: %w", err)
		}
		t := fromReferenceSeconds(seconds)
		return &t, nil
	})
}

// ParseSidecar decodes the X-Vault-Metadata upload header. Here dates are
// ISO-8601 strings (VaultController.metadataDecoder), and anything that fails
// to decode is nil: Swift ignores a bad sidecar and the upload goes on as a
// plain file. "{}" is not a failure. It's an empty sidecar, and MergeMetadata
// will clear the row's note fields with it.
func ParseSidecar(header string) *Metadata {
	var m Metadata
	err := m.decode([]byte(header), func(raw json.RawMessage) (*time.Time, error) {
		var t wire.Time
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		return &t.Time, nil
	})
	if err != nil {
		return nil
	}
	return &m
}

// MergeMetadata is VaultController.mergeMetadata, for a re-upload that carries
// a sidecar. title, tags, isTodo, done and dueAt always take the new value,
// even when that's nil. enrichmentStatus and projectID keep the old value when
// the new one is nil.
//
// QUIRK, as in Swift: provenance and suggestedSpaceID are lost whichever side
// has them, because Swift builds the result with an initializer that doesn't
// set suggestedSpaceID and a call that doesn't pass provenance.
func MergeMetadata(old *Metadata, updated Metadata) Metadata {
	merged := Metadata{
		EnrichmentStatus: updated.EnrichmentStatus,
		Title:            updated.Title,
		Tags:             updated.Tags,
		IsTodo:           updated.IsTodo,
		Done:             updated.Done,
		DueAt:            updated.DueAt,
		ProjectID:        updated.ProjectID,
	}
	if old != nil {
		if merged.EnrichmentStatus == nil {
			merged.EnrichmentStatus = old.EnrichmentStatus
		}
		if merged.ProjectID == nil {
			merged.ProjectID = old.ProjectID
		}
	}
	return merged
}

func (m *Metadata) decode(b []byte, dueAt func(json.RawMessage) (*time.Time, error)) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("metadata: expected an object, found null")
	}
	*m = Metadata{}
	for _, f := range []struct {
		key string
		set func(json.RawMessage) error
	}{
		{"enrichmentStatus", func(r json.RawMessage) error { return json.Unmarshal(r, &m.EnrichmentStatus) }},
		{"title", func(r json.RawMessage) error { return json.Unmarshal(r, &m.Title) }},
		{"tags", func(r json.RawMessage) error { return decodeTags(r, &m.Tags) }},
		{"isTodo", func(r json.RawMessage) error { return json.Unmarshal(r, &m.IsTodo) }},
		{"done", func(r json.RawMessage) error { return json.Unmarshal(r, &m.Done) }},
		{"dueAt", func(r json.RawMessage) (err error) { m.DueAt, err = dueAt(r); return err }},
		{"projectID", func(r json.RawMessage) (err error) { m.ProjectID, err = decodeUUID(r); return err }},
		{"provenance", func(r json.RawMessage) error { return json.Unmarshal(r, &m.Provenance) }},
		{"suggestedSpaceID", func(r json.RawMessage) (err error) { m.SuggestedSpaceID, err = decodeUUID(r); return err }},
	} {
		raw, ok := fields[f.key]
		if !ok || bytes.Equal(raw, []byte("null")) {
			continue
		}
		if err := f.set(raw); err != nil {
			return fmt.Errorf("metadata %s: %w", f.key, err)
		}
	}
	return nil
}

func decodeTags(raw json.RawMessage, tags *[]string) error {
	var elements []*string
	if err := json.Unmarshal(raw, &elements); err != nil {
		return err
	}
	*tags = make([]string, len(elements))
	for i, e := range elements {
		if e == nil {
			return errors.New("null element")
		}
		(*tags)[i] = *e
	}
	return nil
}

func decodeUUID(raw json.RawMessage) (*uuid.UUID, error) {
	var u wire.UUID
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	return &u.UUID, nil
}

func wireUUID(id *uuid.UUID) *wire.UUID {
	if id == nil {
		return nil
	}
	return &wire.UUID{UUID: *id}
}

// referenceSeconds is Date.timeIntervalSinceReferenceDate. Near today a Double
// resolves about 119 ns, so a value read from the column comes back as the same
// Double through time.Time's nanoseconds.
func referenceSeconds(t time.Time) float64 {
	return float64(t.Unix()-referenceDateUnix) + float64(t.Nanosecond())/1e9
}

func fromReferenceSeconds(seconds float64) time.Time {
	whole := math.Floor(seconds)
	return time.Unix(int64(whole)+referenceDateUnix, int64(math.Round((seconds-whole)*1e9))).UTC()
}
