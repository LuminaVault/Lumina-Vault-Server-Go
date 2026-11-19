// Package vaultgittest records vaultgit.Touch calls so writer tests can check
// that a write signals the committer, without git or a debounce.
package vaultgittest

import (
	"lumina/internal/vaultgit"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// Touch is one recorded vaultgit.Touch call.
type Touch struct {
	Tenant uuid.UUID
	Reason string
	Actor  string
	Source string
}

// Recorder is a vaultgit.Toucher that keeps every call.
type Recorder struct {
	mu      sync.Mutex
	touches []Touch
}

// Record installs a Recorder as the package-level Touch target for the rest of
// the test. Tests that use it must not run in parallel with each other.
func Record(t testing.TB) *Recorder {
	t.Helper()
	r := &Recorder{}
	vaultgit.StartToucher(r)
	t.Cleanup(func() { vaultgit.StartToucher(nil) })
	return r
}

// Touch implements vaultgit.Toucher.
func (r *Recorder) Touch(tenant uuid.UUID, reason, actor, source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.touches = append(r.touches, Touch{Tenant: tenant, Reason: reason, Actor: actor, Source: source})
}

// For returns the touches recorded for tenant, oldest first.
func (r *Recorder) For(tenant uuid.UUID) []Touch {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Touch
	for _, t := range r.touches {
		if t.Tenant == tenant {
			out = append(out, t)
		}
	}
	return out
}

// Reasons returns the reasons recorded for tenant, oldest first.
func (r *Recorder) Reasons(tenant uuid.UUID) []string {
	var out []string
	for _, t := range r.For(tenant) {
		out = append(out, t.Reason)
	}
	return out
}
