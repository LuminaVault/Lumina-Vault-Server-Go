package skills

import (
	"encoding/json"
	"lumina/internal/wire"
	"reflect"
)

// Wire shapes from LuminaVaultShared/APIDTOs.swift, fields in declaration order.
// Optionals are omitted when nil, as Swift's synthesized encodeIfPresent does.

// Run statuses (SkillRunStatus).
const (
	statusPending = "pending"
	statusRunning = "running"
	statusSuccess = "success"
	statusError   = "error"
)

func validRunStatus(s string) bool {
	return s == statusPending || s == statusRunning || s == statusSuccess || s == statusError
}

// APNS categories (APNSCategory).
var apnsCategories = map[string]bool{"chat": true, "nudge": true, "digest": true, "approval": true, "runCompleted": true}

// apnsCategory decodes APNSCategory as Swift's RawRepresentable enum does.
type apnsCategory string

func (a *apnsCategory) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !apnsCategories[s] {
		return &wire.CorruptedError{Message: "Cannot initialize APNSCategory from invalid String value " + s}
	}
	*a = apnsCategory(s)
	return nil
}

// JobOriginDTO; an unknown stored kind encodes as "unknown".
type JobOriginDTO struct {
	Kind string    `json:"kind"`
	ID   wire.UUID `json:"id"`
}

func originKind(raw string) string {
	if raw == "conversation" || raw == "card" {
		return raw
	}
	return "unknown"
}

// SkillDTO is LuminaVaultShared.SkillDTO.
type SkillDTO struct {
	ID               string        `json:"id"`
	Source           string        `json:"source"`
	Name             string        `json:"name"`
	Title            string        `json:"title"`
	DescriptionText  string        `json:"descriptionText"`
	Capability       string        `json:"capability"`
	Schedule         *string       `json:"schedule,omitempty"`
	ScheduleOverride *string       `json:"scheduleOverride,omitempty"`
	Enabled          bool          `json:"enabled"`
	LastRunAt        *wire.Time    `json:"lastRunAt,omitempty"`
	LastStatus       *string       `json:"lastStatus,omitempty"`
	LastError        *string       `json:"lastError,omitempty"`
	DailyRunCount    int           `json:"dailyRunCount"`
	DailyRunCap      int           `json:"dailyRunCap"`
	APNSCategory     *string       `json:"apnsCategory,omitempty"`
	BodyExcerpt      string        `json:"bodyExcerpt"`
	Origin           *JobOriginDTO `json:"origin,omitempty"`
}

// SkillListResponse is the GET /v1/skills body.
type SkillListResponse struct {
	Skills []SkillDTO `json:"skills"`
}

// skillPatchRequest is SkillPatchRequest.
type skillPatchRequest struct {
	Enabled          *bool         `json:"enabled"`
	ScheduleOverride *string       `json:"scheduleOverride"`
	APNSCategory     *apnsCategory `json:"apnsCategory"`
}

// LuminaChartPoint, LuminaSeries, LuminaKeyValue and LuminaBlock: decoded from
// skill_run_log.blocks and re-encoded, so anything Swift would not decode drops
// the whole array, and unknown keys disappear.
type luminaChartPoint struct {
	X string      `json:"x"`
	Y wire.Double `json:"y"`
}

type luminaSeries struct {
	Name   string             `json:"name"`
	Points []luminaChartPoint `json:"points"`
}

type luminaKeyValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type luminaBlock struct {
	Type    string            `json:"type"`
	Text    *string           `json:"text,omitempty"`
	Level   *int              `json:"level,omitempty"`
	Label   *string           `json:"label,omitempty"`
	Value   *string           `json:"value,omitempty"`
	Delta   *string           `json:"delta,omitempty"`
	Trend   *string           `json:"trend,omitempty"`
	Items   *[]string         `json:"items,omitempty"`
	Columns *[]string         `json:"columns,omitempty"`
	Rows    *[][]string       `json:"rows,omitempty"`
	Series  *[]luminaSeries   `json:"series,omitempty"`
	Pairs   *[]luminaKeyValue `json:"pairs,omitempty"`
	URL     *string           `json:"url,omitempty"`
}

// SkillRunDTO is LuminaVaultShared.SkillRunDTO.
type SkillRunDTO struct {
	ID        wire.UUID      `json:"id"`
	StartedAt wire.Time      `json:"startedAt"`
	EndedAt   *wire.Time     `json:"endedAt,omitempty"`
	Status    string         `json:"status"`
	Error     *string        `json:"error,omitempty"`
	ModelUsed *string        `json:"modelUsed,omitempty"`
	MtokIn    int            `json:"mtokIn"`
	MtokOut   int            `json:"mtokOut"`
	Markdown  *string        `json:"markdown,omitempty"`
	Blocks    *[]luminaBlock `json:"blocks,omitempty"`
}

// SkillSparklinePoint is one day of the runs sparkline.
type SkillSparklinePoint struct {
	Day   wire.Time `json:"day"`
	Count int       `json:"count"`
}

// SkillRunsResponse is the GET /v1/skills/{name}/runs body. nextCursor is always nil.
type SkillRunsResponse struct {
	Runs      []SkillRunDTO         `json:"runs"`
	Sparkline []SkillSparklinePoint `json:"sparkline"`
}

// SkillOutputDTO is LuminaVaultShared.SkillOutputDTO. memoryID and memoID are always nil.
type SkillOutputDTO struct {
	ID            wire.UUID `json:"id"`
	SkillName     string    `json:"skillName"`
	Source        string    `json:"source"`
	Kind          string    `json:"kind"`
	Headline      string    `json:"headline"`
	Body          string    `json:"body"`
	CreatedAt     wire.Time `json:"createdAt"`
	VaultFilePath *string   `json:"vaultFilePath,omitempty"`
}

// SkillOutputListResponse is the GET /v1/skills/outputs body.
type SkillOutputListResponse struct {
	Outputs    []SkillOutputDTO `json:"outputs"`
	StreakDays int              `json:"streakDays"`
	ActiveRun  bool             `json:"activeRun"`
	NextCursor *string          `json:"nextCursor,omitempty"`
}

// JobProposalDTO is the POST /v1/jobs/detect body.
type JobProposalDTO struct {
	IsJob         bool    `json:"isJob"`
	Title         *string `json:"title,omitempty"`
	Cron          *string `json:"cron,omitempty"`
	ScheduleHuman *string `json:"scheduleHuman,omitempty"`
	Domain        *string `json:"domain,omitempty"`
	Spec          *string `json:"spec,omitempty"`
}

type detectRequest struct {
	Text string `json:"text"`
}

// jobCreateRequest is JobCreateRequest.
type jobCreateRequest struct {
	Title          string     `json:"title"`
	Cron           string     `json:"cron"`
	Domain         *string    `json:"domain"`
	Spec           string     `json:"spec"`
	SpaceID        *wire.UUID `json:"spaceId"`
	ConversationID *wire.UUID `json:"conversationId"`
}
