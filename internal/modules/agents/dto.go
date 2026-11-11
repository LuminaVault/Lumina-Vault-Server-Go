package agents

import (
	"encoding/json"
	"lumina/internal/wire"
	"reflect"
)

// The wire types are LuminaVaultShared 5.32.0's (APIDTOs.swift, "Agents page" and
// "Agent rooms"). models.gen.go has the responses; the ones here differ from the
// spec on purpose.

// sessionDTO is AgentSessionDTO. The spec's costUSD is a float, which oapi-codegen
// makes float32; Swift's is a Double.
type sessionDTO struct {
	InstanceID   string       `json:"instanceID"`
	Profile      *string      `json:"profile,omitempty"`
	ID           string       `json:"id"`
	Title        *string      `json:"title,omitempty"`
	Source       string       `json:"source"`
	StartedAt    *wire.Time   `json:"startedAt,omitempty"`
	LastActiveAt *wire.Time   `json:"lastActiveAt,omitempty"`
	MessageCount *int         `json:"messageCount,omitempty"`
	IsActive     bool         `json:"isActive"`
	CostUSD      *wire.Double `json:"costUSD,omitempty"`
}

type sessionsResponse struct {
	Sessions []sessionDTO            `json:"sessions"`
	Errors   []AgentInstanceErrorDTO `json:"errors"`
}

// respondMode is AgentRoomRespondMode as a request decodes it.
type respondMode string

const (
	modeMention      respondMode = "mention"
	modeEveryMessage respondMode = "every_human_message"
)

// UnmarshalJSON is a Swift String enum's init(from:): a non-string is a type
// mismatch against String, an unknown string is dataCorrupted naming the type.
func (m *respondMode) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if s != string(modeMention) && s != string(modeEveryMessage) {
		return &wire.CorruptedError{Message: "Cannot initialize AgentRoomRespondMode from invalid String value " + s}
	}
	*m = respondMode(s)
	return nil
}

// memberRequest is AgentRoomMemberRequest, in declaration order.
type memberRequest struct {
	InstanceID  string       `json:"instanceID"`
	Profile     *string      `json:"profile,omitempty"`
	Handle      *string      `json:"handle,omitempty"`
	DisplayName *string      `json:"displayName,omitempty"`
	RespondMode *respondMode `json:"respondMode,omitempty"`
}

// createRequest is AgentRoomCreateRequest.
type createRequest struct {
	Title   string          `json:"title"`
	Members []memberRequest `json:"members"`
}

// postRequest is AgentRoomPostRequest.
type postRequest struct {
	Body string `json:"body"`
}

// candidate is AgentRoomCandidateDTO with a plain instance id.
type candidate struct {
	InstanceID      string  `json:"instanceID"`
	Profile         *string `json:"profile,omitempty"`
	DisplayName     string  `json:"displayName"`
	SuggestedHandle string  `json:"suggestedHandle"`
}

type candidatesResponse struct {
	Candidates []candidate `json:"candidates"`
}
