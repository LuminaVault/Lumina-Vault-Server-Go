package quickassistant

import (
	"encoding/json"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/wire"
)

// Context is QuickAssistantContextDTO: where the user is when they open the
// card. wire.Decode makes currentTab required, as the Swift decoder does.
type Context struct {
	CurrentTab    string     `json:"currentTab"`
	ActiveSpaceID *wire.UUID `json:"activeSpaceID,omitempty"`
	TimeZone      *string    `json:"timeZone,omitempty"`
}

// ActionKind is AssistantActionKindDTO.
type ActionKind string

const (
	KindDone            ActionKind = "done"
	KindNeedsConfirm    ActionKind = "needsConfirm"
	KindNeedsPermission ActionKind = "needsPermission"
	KindOpenScreen      ActionKind = "openScreen"
	// KindUnknown is the decoder's fallback (ruling 7), never sent.
	KindUnknown ActionKind = "unknown"
)

// UnmarshalJSON maps a kind this build does not know to KindUnknown.
func (k *ActionKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch v := ActionKind(s); v {
	case KindDone, KindNeedsConfirm, KindNeedsPermission, KindOpenScreen:
		*k = v
	default:
		*k = KindUnknown
	}
	return nil
}

// Screen is AssistantScreenDTO.
type Screen string

const (
	ScreenHome       Screen = "home"
	ScreenSpaces     Screen = "spaces"
	ScreenAI         Screen = "ai"
	ScreenBrain      Screen = "brain"
	ScreenInsights   Screen = "insights"
	ScreenSettings   Screen = "settings"
	ScreenDataAccess Screen = "dataAccess"
	ScreenBrief      Screen = "brief"
	ScreenLearned    Screen = "learned"
	ScreenMemory     Screen = "memory"
	// ScreenUnknown is the decoder's fallback, not a destination.
	ScreenUnknown Screen = "unknown"
)

// screens are the ten Shared cases, in Shared order.
var screens = []Screen{
	ScreenHome, ScreenSpaces, ScreenAI, ScreenBrain, ScreenInsights,
	ScreenSettings, ScreenDataAccess, ScreenBrief, ScreenLearned, ScreenMemory,
}

// ParseScreen reports whether s is a destination. "unknown" is not one.
func ParseScreen(s string) (Screen, bool) {
	for _, c := range screens {
		if string(c) == s {
			return c, true
		}
	}
	return "", false
}

// UnmarshalJSON maps a screen this build does not know to ScreenUnknown.
func (s *Screen) UnmarshalJSON(b []byte) error {
	var raw string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if v, ok := ParseScreen(raw); ok {
		*s = v
	} else {
		*s = ScreenUnknown
	}
	return nil
}

// Action is AssistantActionDTO. Screen, MemoryID and ExpiresAt are omitted when nil.
type Action struct {
	ID        wire.UUID  `json:"id"`
	Kind      ActionKind `json:"kind"`
	Summary   string     `json:"summary"`
	Screen    *Screen    `json:"screen,omitempty"`
	MemoryID  *wire.UUID `json:"memoryID,omitempty"`
	ExpiresAt *wire.Time `json:"expiresAt,omitempty"`
}

// Frame is the stream frame {"type":"assistant_action","payload":{...}}.
func Frame(a Action) memoryagent.StreamEvent {
	return memoryagent.StreamEvent{Type: "assistant_action", Payload: a}
}

// ResolutionStatus is AssistantActionStatusDTO.
type ResolutionStatus string

const (
	StatusConfirmed       ResolutionStatus = "confirmed"
	StatusCancelled       ResolutionStatus = "cancelled"
	StatusFailed          ResolutionStatus = "failed"
	StatusExpired         ResolutionStatus = "expired"
	StatusAlreadyResolved ResolutionStatus = "alreadyResolved"
	// StatusUnknown is the decoder's fallback, never sent.
	StatusUnknown ResolutionStatus = "unknown"
)

// UnmarshalJSON maps a status this build does not know to StatusUnknown.
func (r *ResolutionStatus) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch v := ResolutionStatus(s); v {
	case StatusConfirmed, StatusCancelled, StatusFailed, StatusExpired, StatusAlreadyResolved:
		*r = v
	default:
		*r = StatusUnknown
	}
	return nil
}

// Resolution is AssistantActionResolutionDTO, the reply to confirm and cancel.
type Resolution struct {
	Status  ResolutionStatus `json:"status"`
	Message string           `json:"message"`
}
