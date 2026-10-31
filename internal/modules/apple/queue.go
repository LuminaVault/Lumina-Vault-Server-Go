package apple

// The enqueue half of Swift's DeviceCommandQueue (a socket write that timed
// out is stored and announced with a push) runs inside PersonalDataTools,
// which MCP and Skills call; it stays in Swift until those modules move. Go
// serves the read (pending) and the result routes over the same
// device_command_queue table. These two helpers are ported with their tests
// so the enqueue port has them.

// Queueable is DeviceCommandQueue.queueable: only writes queue; a read the
// phone missed is simply unavailable.
var Queueable = []string{"reminder_create", "calendar_create"}

// Announcement is DeviceCommandQueue.announcement(for:), the push body.
func Announcement(kind string, payload map[string]string) string {
	title := payload["title"]
	if title == "" {
		title = "an item"
	}
	switch kind {
	case "reminder_create":
		return "Open LuminaVault to add the reminder “" + title + "”."
	case "calendar_create":
		return "Open LuminaVault to add “" + title + "” to your calendar."
	default:
		return "Open LuminaVault to finish a request from Lumina."
	}
}
