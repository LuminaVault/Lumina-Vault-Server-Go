// Package clientcaps is the Go port of Swift's ClientCapabilities
// (Sources/App/HTTP/ClientCapabilities.swift): what the caller declared it can cope
// with. Shipped iOS builds older than LuminaVaultShared 5.16.0 die on an unknown
// chat SSE `type`, and they look the same on the wire as new ones, so a client that
// can skip such a frame says so and everyone else keeps the classic stream.
package clientcaps

import (
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"strings"
)

// HeaderName is the request header clients declare through.
const HeaderName = "x-lv-client-caps"

// Capability is one thing a client can declare.
type Capability string

// ChatHermesRun says the client skips an unknown `type` on the chat SSE stream.
// The server needs it before it may emit the chat run-pointer event.
const ChatHermesRun Capability = "chat.hermes_run"

// Capabilities is a parsed declaration. The zero value declares nothing.
type Capabilities struct {
	declared map[Capability]bool
}

// Supports reports whether the client declared c.
func (c Capabilities) Supports(capability Capability) bool {
	return c.declared[capability]
}

// Parse reads a header value: comma-separated, any order, spaces and case ignored,
// unknown tokens skipped so a newer client isn't downgraded on the tokens it shares
// with this server. Empty or unparseable declares nothing, because the default has
// to be safe for a client that never heard of the header.
func Parse(value string) Capabilities {
	var caps Capabilities
	for token := range strings.SplitSeq(value, ",") {
		if capability := Capability(strings.ToLower(swiftstr.TrimWhitespaces(token))); capability == ChatHermesRun {
			if caps.declared == nil {
				caps.declared = map[Capability]bool{}
			}
			caps.declared[capability] = true
		}
	}
	return caps
}

// FromRequest is Request.clientCapabilities.
func FromRequest(r *http.Request) Capabilities {
	value, _ := wire.HeaderValue(r, HeaderName)
	return Parse(value)
}
