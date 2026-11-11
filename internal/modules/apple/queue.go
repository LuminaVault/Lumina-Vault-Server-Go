package apple

import "lumina/internal/devicerpc"

// The enqueue half of Swift's DeviceCommandQueue is devicerpc.Queue, which the
// personal-data tools (MCP, Skills) call when a socket write times out. Go
// serves the read (pending) and the result routes over the same
// device_command_queue table. These two names stay for this module's tests.

// Queueable is DeviceCommandQueue.queueable, as strings.
var Queueable = func() []string {
	out := make([]string, len(devicerpc.Queueable))
	for i, k := range devicerpc.Queueable {
		out[i] = string(k)
	}
	return out
}()

// Announcement is devicerpc.Announcement for a raw kind.
func Announcement(kind string, payload map[string]string) string {
	return devicerpc.Announcement(devicerpc.Kind(kind), payload)
}
