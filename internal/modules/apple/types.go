package apple

import (
	"encoding/json"
	"lumina/internal/modules/apple/db"
	"lumina/internal/wire"
	"reflect"
	"strings"
)

// Request bodies are hand-written, not the generated models: wire.Bind checks
// fields in declaration order, and Foundation reports the first failing key in
// the Swift struct's order (LuminaVaultShared APIDTOs.swift), while
// oapi-codegen sorts fields alphabetically.

type consentUpdateRequest struct {
	Domain      domain `json:"domain"`
	Allowed     bool   `json:"allowed"`
	AllowWrites *bool  `json:"allowWrites,omitempty"`
}

type appleReminderInput struct {
	ExternalID      string     `json:"externalID"`
	Title           string     `json:"title"`
	Notes           *string    `json:"notes,omitempty"`
	DueAt           *wire.Time `json:"dueAt,omitempty"`
	Completed       bool       `json:"completed"`
	CompletedAt     *wire.Time `json:"completedAt,omitempty"`
	ListName        *string    `json:"listName,omitempty"`
	Priority        *int64     `json:"priority,omitempty"`
	RemoteUpdatedAt *wire.Time `json:"remoteUpdatedAt,omitempty"`
}

type remindersSyncRequest struct {
	Reminders []appleReminderInput `json:"reminders"`
}

type photoIndexInput struct {
	AssetLocalID string     `json:"assetLocalID"`
	TakenAt      *wire.Time `json:"takenAt,omitempty"`
	IsScreenshot bool       `json:"isScreenshot"`
	OcrText      *string    `json:"ocrText,omitempty"`
	SceneTags    *[]string  `json:"sceneTags,omitempty"`
}

type photoIndexSyncRequest struct {
	Items []photoIndexInput `json:"items"`
}

type deviceCommandResult struct {
	ID      wire.UUID          `json:"id"`
	Ok      bool               `json:"ok"`
	Payload *map[string]string `json:"payload,omitempty"`
	Error   *string            `json:"error,omitempty"`
}

// domain is AppleDataDomain as a request value: an unknown raw value fails the
// way Swift's RawRepresentable decoding does.
type domain string

const (
	domainHealth    domain = "health"
	domainCalendar  domain = "calendar"
	domainReminders domain = "reminders"
	domainPhotos    domain = "photos"
	domainLocation  domain = "location"
	domainFiles     domain = "files"
)

// allDomains is AppleDataDomain.allCases, in declaration order.
var allDomains = []domain{domainHealth, domainCalendar, domainReminders, domainPhotos, domainLocation, domainFiles}

func (d *domain) UnmarshalJSON(b []byte) error {
	s, err := enumString(b, "AppleDataDomain", func(s string) bool {
		for _, v := range allDomains {
			if string(v) == s {
				return true
			}
		}
		return false
	})
	*d = domain(s)
	return err
}

// commandKinds is DeviceCommandKind's raw values.
var commandKinds = map[string]bool{
	"ping": true, "reminder_create": true, "calendar_create": true, "device_fetch": true, "photo_analyze": true,
}

// enumString decodes a String-backed Swift enum. A non-string is a type
// mismatch; an unknown value is Swift's dataCorrupted message.
func enumString(b []byte, swiftType string, valid func(string) bool) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !valid(s) {
		return "", &wire.CorruptedError{Message: "Cannot initialize " + swiftType + " from invalid String value " + s}
	}
	return s, nil
}

// asCommand is QueuedDeviceCommand.asCommand: an unknown kind drops the row,
// an unknown domain becomes nil, an unreadable payload becomes {}.
func asCommand(r db.PendingCommandsRow) (DeviceCommand, bool) {
	if !commandKinds[r.Kind] {
		return DeviceCommand{}, false
	}
	cmd := DeviceCommand{Id: wire.UUID{UUID: r.ID}, Kind: DeviceCommandKind(r.Kind), Payload: map[string]string{}}
	if r.Domain != nil {
		for _, d := range allDomains {
			if string(d) == *r.Domain {
				v := AppleDataDomain(d)
				cmd.Domain = &v
			}
		}
	}
	var args map[string]string
	if json.Unmarshal([]byte(r.Payload), &args) == nil && args != nil {
		cmd.Payload = args
	}
	return cmd, true
}

// trimSpace is Swift's trimmingCharacters(in: .whitespacesAndNewlines).
func trimSpace(s string) string { return strings.TrimSpace(s) }
