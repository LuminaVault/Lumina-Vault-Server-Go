package selfimprovement

import (
	"encoding/json"
	"lumina/internal/modules/selfimprovement/db"
	"lumina/internal/wire"
	"reflect"
	"time"
)

// settingsRequest is ImprovementSettingsDTO as a request, field by field in Swift's
// declaration order: wire.Bind reports the first failing key, and the generated
// model is alphabetical. Every key is required, because the synthesized decoder
// ignores the memberwise init's defaults.
type settingsRequest struct {
	Enabled                 bool      `json:"enabled"`
	CuratorEnabled          bool      `json:"curatorEnabled"`
	IntervalHours           int       `json:"intervalHours"`
	MinimumIdleHours        int       `json:"minimumIdleHours"`
	Consolidate             bool      `json:"consolidate"`
	PruneBuiltins           bool      `json:"pruneBuiltins"`
	BackupKeep              int       `json:"backupKeep"`
	SoulReviewEnabled       bool      `json:"soulReviewEnabled"`
	ReviewComplexSessions   bool      `json:"reviewComplexSessions"`
	SoulReviewWindowDays    int       `json:"soulReviewWindowDays"`
	SoulReviewCooldownHours int       `json:"soulReviewCooldownHours"`
	ModelMode               modelMode `json:"modelMode"`
}

type settingsUpdateRequest struct {
	Settings settingsRequest `json:"settings"`
}

// modelMode decodes ImprovementModelMode: a non-string is a type mismatch and an
// unknown value Swift's dataCorrupted message.
type modelMode string

func (m *modelMode) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !ImprovementModelMode(s).Valid() {
		return &wire.CorruptedError{Message: "Cannot initialize ImprovementModelMode from invalid String value " + s}
	}
	*m = modelMode(s)
	return nil
}

// runRequest is ImprovementRunRequest. dryRun is required on the wire too: a body
// without it fails to decode, which the handler reads as the default.
type runRequest struct {
	DryRun bool `json:"dryRun"`
}

// soulReviewRequest is SoulReviewRequest, with the enum read by hand because any
// failure to decode means the default.
type soulReviewRequest struct {
	Trigger string `json:"trigger"`
}

type pinRequest struct {
	Pinned bool `json:"pinned"`
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func settingsDTO(row db.SelfImprovementSetting) ImprovementSettingsDTO {
	mode := ImprovementModelMode(row.ModelMode)
	if !mode.Valid() {
		mode = Economy
	}
	return ImprovementSettingsDTO{
		Enabled:                 row.Enabled,
		CuratorEnabled:          row.CuratorEnabled,
		IntervalHours:           int(row.IntervalHours),
		MinimumIdleHours:        int(row.MinimumIdleHours),
		Consolidate:             row.Consolidate,
		PruneBuiltins:           row.PruneBuiltins,
		BackupKeep:              int(row.BackupKeep),
		SoulReviewEnabled:       row.SoulReviewEnabled,
		ReviewComplexSessions:   row.ReviewComplexSessions,
		SoulReviewWindowDays:    int(row.SoulReviewWindowDays),
		SoulReviewCooldownHours: int(row.SoulReviewCooldownHours),
		ModelMode:               mode,
	}
}

// triggerDTO and the fallbacks below are the ?? in ImprovementRun.toDTO and
// ImprovementChange.toDTO: a value this version doesn't know still reads.
func triggerDTO(raw string) ImprovementTrigger {
	if t := ImprovementTrigger(raw); t.Valid() {
		return t
	}
	return Manual
}

func runDTO(row db.SelfImprovementRun) ImprovementRunDTO {
	kind := ImprovementChangeKind(row.Kind)
	if !kind.Valid() {
		kind = Curator
	}
	status := ImprovementRunStatus(row.Status)
	if !status.Valid() {
		status = ImprovementRunStatusFailed
	}
	return ImprovementRunDTO{
		Id:             wire.UUID{UUID: row.ID},
		Kind:           kind,
		Status:         status,
		Trigger:        triggerDTO(row.Trigger),
		DryRun:         row.DryRun,
		ModelUsed:      row.ModelUsed,
		ReportMarkdown: row.ReportMarkdown,
		ActionsApplied: int(row.ActionsApplied),
		ActionsSkipped: int(row.ActionsSkipped),
		StartedAt:      wireTime(row.StartedAt),
		EndedAt:        wireTime(row.EndedAt),
		CreatedAt:      wire.Time{Time: row.CreatedAt},
		FailureReason:  row.FailureReason,
	}
}

func changeDTO(row db.SelfImprovementChange) ImprovementChangeDTO {
	kind := ImprovementChangeKind(row.Kind)
	if !kind.Valid() {
		kind = Soul
	}
	state := ImprovementChangeState(row.State)
	if !state.Valid() {
		state = ImprovementChangeStateFailed
	}
	return ImprovementChangeDTO{
		Id:             wire.UUID{UUID: row.ID},
		Kind:           kind,
		State:          state,
		Trigger:        triggerDTO(row.Trigger),
		Title:          row.Title,
		Summary:        row.Summary,
		Patch:          row.Patch,
		BaseSHA256:     row.BaseSha256,
		ReportMarkdown: row.ReportMarkdown,
		FailureReason:  row.FailureReason,
		CreatedAt:      wire.Time{Time: row.CreatedAt},
		DecidedAt:      wireTime(row.DecidedAt),
		AppliedAt:      wireTime(row.AppliedAt),
	}
}
