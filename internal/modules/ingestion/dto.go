package ingestion

import (
	"encoding/json"
	"lumina/internal/wire"
	"reflect"
)

// The response DTOs are LuminaVaultShared's, by hand rather than models.gen.go: the
// generator sorts keys and turns `type: number` into float32, and Swift writes
// Doubles in declaration order. Nil optionals are omitted (encodeIfPresent).

// sourceKind is IngestionSourceKindDTO on the way in: an unknown value is
// Foundation's dataCorrupted error, a non-string a type mismatch on String.
type sourceKind string

const (
	kindFile sourceKind = "file"
	kindURL  sourceKind = "url"
)

func (k *sourceKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if s != string(kindFile) && s != string(kindURL) {
		return &wire.CorruptedError{Message: "Cannot initialize IngestionSourceKindDTO from invalid String value " + s}
	}
	*k = sourceKind(s)
	return nil
}

// createItemRequest is IngestionCreateItemRequest.
type createItemRequest struct {
	Kind        sourceKind  `json:"kind"`
	FileName    *string     `json:"fileName,omitempty"`
	ContentType *string     `json:"contentType,omitempty"`
	SizeBytes   *wire.Int64 `json:"sizeBytes,omitempty"`
	SHA256      *string     `json:"sha256,omitempty"`
	URL         *string     `json:"url,omitempty"`
}

func (r createItemRequest) sizeBytes() *int64 {
	if r.SizeBytes == nil {
		return nil
	}
	size := int64(*r.SizeBytes)
	return &size
}

// createRequest is IngestionCreateRequest.
type createRequest struct {
	SpaceID *wire.UUID          `json:"spaceID,omitempty"`
	Items   []createItemRequest `json:"items"`
}

type credibilityDTO struct {
	Score      *int     `json:"score,omitempty"`
	Confidence float64  `json:"confidence"`
	Signals    []string `json:"signals"`
	Rationale  string   `json:"rationale"`
	Version    string   `json:"version"`
}

type entityDTO struct {
	ID         wire.UUID `json:"id"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Confidence float64   `json:"confidence"`
}

type relationshipDTO struct {
	ID              wire.UUID `json:"id"`
	SubjectEntityID wire.UUID `json:"subjectEntityID"`
	Predicate       string    `json:"predicate"`
	ObjectEntityID  wire.UUID `json:"objectEntityID"`
	Confidence      float64   `json:"confidence"`
	Evidence        *string   `json:"evidence,omitempty"`
}

type itemDTO struct {
	ID               wire.UUID         `json:"id"`
	BatchID          wire.UUID         `json:"batchID"`
	Kind             string            `json:"kind"`
	State            string            `json:"state"`
	FileName         *string           `json:"fileName,omitempty"`
	ContentType      *string           `json:"contentType,omitempty"`
	SizeBytes        *int64            `json:"sizeBytes,omitempty"`
	UploadedBytes    int64             `json:"uploadedBytes"`
	URL              *string           `json:"url,omitempty"`
	VaultFileID      *wire.UUID        `json:"vaultFileID,omitempty"`
	MemoryID         *wire.UUID        `json:"memoryID,omitempty"`
	Summary          *string           `json:"summary,omitempty"`
	Error            *string           `json:"error,omitempty"`
	Credibility      *credibilityDTO   `json:"credibility,omitempty"`
	Entities         []entityDTO       `json:"entities"`
	Relationships    []relationshipDTO `json:"relationships"`
	ContentSHA256    *string           `json:"contentSHA256,omitempty"`
	PipelineVersion  *string           `json:"pipelineVersion,omitempty"`
	ReusedFromItemID *wire.UUID        `json:"reusedFromItemID,omitempty"`
	GraphReadyAt     *wire.Time        `json:"graphReadyAt,omitempty"`
	CreatedAt        *wire.Time        `json:"createdAt,omitempty"`
	UpdatedAt        *wire.Time        `json:"updatedAt,omitempty"`
}

type batchDTO struct {
	ID             wire.UUID  `json:"id"`
	State          string     `json:"state"`
	Total          int        `json:"total"`
	Completed      int        `json:"completed"`
	Failed         int        `json:"failed"`
	ChunkSizeBytes int        `json:"chunkSizeBytes"`
	Items          []itemDTO  `json:"items"`
	CreatedAt      *wire.Time `json:"createdAt,omitempty"`
	UpdatedAt      *wire.Time `json:"updatedAt,omitempty"`
}

type batchListDTO struct {
	Batches []batchDTO `json:"batches"`
}

// eventDTO is IngestionEventDTO, the data of one stream frame.
type eventDTO struct {
	ID            int64      `json:"id"`
	BatchID       wire.UUID  `json:"batchID"`
	ItemID        *wire.UUID `json:"itemID,omitempty"`
	Type          string     `json:"type"`
	State         *string    `json:"state,omitempty"`
	UploadedBytes *int64     `json:"uploadedBytes,omitempty"`
	CreatedAt     wire.Time  `json:"createdAt"`
}

// itemState is IngestionItemStateDTO(rawValue:): ok is false for unknown text.
func itemState(raw string) (string, bool) {
	switch raw {
	case stateAwaitingUpload, stateQueued, "extracting", "analyzing", "saving",
		stateBlockedCapability, stateCompleted, stateFailed, stateCancelled:
		return raw, true
	}
	return "", false
}

// eventType is IngestionEventTypeDTO(rawValue:).
func eventType(raw string) bool {
	switch raw {
	case "snapshot", eventStateChanged, eventProgress, "terminal", "heartbeat":
		return true
	}
	return false
}
