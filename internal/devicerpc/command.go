// Package devicerpc is the Go port of LuminaVaultServer's server→device RPC:
// DeviceCommandBroker (Sources/App/Apple/DeviceCommandBroker.swift) and the
// enqueue half of DeviceCommandQueue (Apple/DeviceCommandQueue.swift), with the
// LuminaVaultShared 5.32.0 DTOs they send.
//
// A caller (MCP's and Skills' personal-data tools) sends a Command to the
// tenant's sockets with Broker.Request and waits up to 12 s for the app to post
// the Result to POST /v1/devices/command/{id}/result, whose handler calls
// Broker.Resolve. The waiters live in this process's memory, as in Swift: the
// socket, the result POST and the caller must all be on the same server.
package devicerpc

import (
	"encoding/json"
	"lumina/internal/wire"

	"github.com/google/uuid"
)

// Kind is DeviceCommandKind.
type Kind string

const (
	KindPing           Kind = "ping"
	KindReminderCreate Kind = "reminder_create"
	KindCalendarCreate Kind = "calendar_create"
	KindDeviceFetch    Kind = "device_fetch"
	KindPhotoAnalyze   Kind = "photo_analyze"
)

// Command is DeviceCommand. Domain is an AppleDataDomain raw value; "" is nil.
type Command struct {
	ID      uuid.UUID
	Kind    Kind
	Domain  string
	Payload map[string]string
}

// NewCommand is DeviceCommand.init with a fresh id.
func NewCommand(kind Kind, domain string, payload map[string]string) Command {
	return Command{ID: uuid.New(), Kind: kind, Domain: domain, Payload: payload}
}

// MarshalJSON encodes as Swift's JSONEncoder does: upper-case id, domain
// omitted when nil, payload always an object.
func (c Command) MarshalJSON() ([]byte, error) {
	payload := c.Payload
	if payload == nil {
		payload = map[string]string{}
	}
	return json.Marshal(struct {
		ID      wire.UUID         `json:"id"`
		Kind    Kind              `json:"kind"`
		Domain  string            `json:"domain,omitempty"`
		Payload map[string]string `json:"payload"`
	}{wire.UUID{UUID: c.ID}, c.Kind, c.Domain, payload})
}

// envelope is DeviceCommandEnvelope, the socket message. `type` lets the app
// tell device commands apart from other socket traffic.
type envelope struct {
	Type    string  `json:"type"`
	Command Command `json:"command"`
}

// EnvelopeType is DeviceCommandEnvelope.type.
const EnvelopeType = "device_command"

// Result is DeviceCommandResult, as the app posts it.
type Result struct {
	ID      uuid.UUID
	OK      bool
	Payload map[string]string // nil when the app sent none
	Error   *string
}
