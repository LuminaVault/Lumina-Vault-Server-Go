package soul

import (
	"encoding/json"
	"lumina/internal/soul"
	"lumina/internal/wire"
	"reflect"
)

// Response is LuminaVaultShared SoulResponse. updatedAt is encodeIfPresent.
type Response struct {
	Markdown  string     `json:"markdown"`
	UpdatedAt *wire.Time `json:"updatedAt,omitempty"`
}

// putRequest is SoulPutRequest.
type putRequest struct {
	Markdown string `json:"markdown"`
}

// composeRequest is SoulComposeRequest (LuminaVaultShared 5.32.0). Every field is
// optional, and its CodingKeys are snake_case on the wire.
type composeRequest struct {
	AgentName     *string         `json:"agent_name,omitempty"`
	Tone          *toneValue      `json:"tone,omitempty"`
	Role          *roleValue      `json:"role,omitempty"`
	Autonomy      *autonomyValue  `json:"autonomy,omitempty"`
	Priorities    []priorityValue `json:"priorities,omitempty"`
	OtherPriority *string         `json:"other_priority,omitempty"`
	Format        *formatValue    `json:"format,omitempty"`
	Length        *lengthValue    `json:"length,omitempty"`
	Emojis        *bool           `json:"emojis,omitempty"`
	VoiceSamples  []string        `json:"voice_samples,omitempty"`
	DryRun        *bool           `json:"dry_run,omitempty"`
}

func (r *composeRequest) input() soul.ComposeInput {
	in := soul.ComposeInput{VoiceSamples: r.VoiceSamples}
	if r.AgentName != nil {
		in.AgentName = *r.AgentName
	}
	if r.Tone != nil {
		in.Tone = string(*r.Tone)
	}
	if r.Role != nil {
		in.Role = string(*r.Role)
	}
	if r.Autonomy != nil {
		in.Autonomy = string(*r.Autonomy)
	}
	for _, p := range r.Priorities {
		in.Priorities = append(in.Priorities, string(p))
	}
	if r.OtherPriority != nil {
		in.OtherPriority = *r.OtherPriority
	}
	if r.Format != nil {
		in.Format = string(*r.Format)
	}
	if r.Length != nil {
		in.Length = string(*r.Length)
	}
	if r.Emojis != nil {
		in.Emojis = *r.Emojis
	}
	return in
}

// decodeEnum is a String-backed Swift enum's init(from:): a non-string is a type
// mismatch, and an unknown raw value is dataCorrupted.
func decodeEnum(b []byte, swiftType string, values map[string]string) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if _, ok := values[s]; !ok {
		return "", &wire.CorruptedError{Message: "Cannot initialize " + swiftType + " from invalid String value " + s}
	}
	return s, nil
}

type (
	toneValue     string
	roleValue     string
	autonomyValue string
	priorityValue string
	formatValue   string
	lengthValue   string
)

func (v *toneValue) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "SoulTone", soul.Tones)
	*v = toneValue(s)
	return err
}

func (v *roleValue) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "SoulRole", soul.Roles)
	*v = roleValue(s)
	return err
}

func (v *autonomyValue) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "SoulAutonomy", soul.Autonomies)
	*v = autonomyValue(s)
	return err
}

func (v *priorityValue) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "SoulPriority", soul.Priorities)
	*v = priorityValue(s)
	return err
}

func (v *formatValue) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "SoulFormat", soul.Formats)
	*v = formatValue(s)
	return err
}

func (v *lengthValue) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "SoulLength", soul.Lengths)
	*v = lengthValue(s)
	return err
}
