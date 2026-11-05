// Package soul is the Go port of LuminaVaultServer's SOULCore and SOULService
// (Sources/App/Auth). SOUL.md is the per-user Hermes personality file, and Hermes
// loads it on every turn, so a write lands in two places: the tenant's raw directory
// and the user's Hermes profile.
//
// /v1/soul itself is wave 8. SOULService.reset, updatedAt and needsCoreMigration
// have no caller until then, and the composer's non-default inputs arrive with it.
package soul

import (
	"fmt"
	"lumina/internal/vaultfiles"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// FileName is SOULService.fileName.
	FileName = "SOUL.md"
	// MaxSizeBytes is SOULService.maxSizeBytes. It applies to the enforced document,
	// so the core counts toward it.
	MaxSizeBytes = 64 * 1024
)

// TooLargeError is SOULServiceError.tooLarge.
type TooLargeError struct {
	Bytes int
	Limit int
}

func (e *TooLargeError) Error() string {
	return "soul: " + strconv.Itoa(e.Bytes) + " bytes is over the limit of " + strconv.Itoa(e.Limit)
}

// Service is SOULService.
type Service struct {
	paths      vaultpaths.Paths
	hermesRoot string
}

// New takes the vault layout and the Hermes data root, the directory holding
// profiles/<username>.
func New(paths vaultpaths.Paths, hermesDataRoot string) *Service {
	return &Service{paths: paths, hermesRoot: hermesDataRoot}
}

// RenderDefault is SOULDefaultTemplate.render, which is SOULComposer.render with
// every input at its default. The core is already where Inject puts it.
func RenderDefault(username string, now time.Time) string {
	return strings.Join([]string{
		"---\nversion: 2\nusername: " + username + "\ncreated_at: " + now.UTC().Format("2006-01-02T15:04:05Z") + "\n---",
		"# SOUL.md",
		Core,
		"## Identity",
		"I am Hermes, your second brain — I remember everything so you don't have to. I read this file on every reply, so it defines who I am for " + username + ".",
		"## Chat voice",
		"Warm, encouraging, and plain-spoken. Lead with empathy, stay direct.",
		"- Format: Bullets and short sections over walls of text.\n- Length: Short and scannable by default; go deep only when asked.\n- No emojis.",
		"## Published content voice",
		"When drafting anything that leaves this vault (posts, replies, messages on connected channels), switch to a neutral, professional voice: clear, factual, no inside jokes, no emojis unless the platform calls for them. Publishing itself always requires explicit authorization (see core covenant).",
		"## What matters to me",
		"- Whatever the current mission needs — tell me and update this list.",
		"## Operations",
		"Propose actions and wait for a clear go-ahead before acting. Keep continuity across sessions by leaning on memory rather than re-asking.",
		"<!-- Anything below this line is free-form notes the user wrote about themselves. -->",
	}, "\n\n")
}

func (s *Service) vaultPath(tenant uuid.UUID) string {
	return filepath.Join(s.paths.Raw(tenant), FileName)
}

func (s *Service) hermesPath(username string) string {
	return filepath.Join(s.hermesRoot, "profiles", username, FileName)
}

// Read is SOULService.read: the vault copy, or the default template when that's
// missing or isn't UTF-8. It writes nothing, and it never reads the Hermes copy,
// despite what Swift's doc comment says. Callers building a prompt want
// TrimWhitespacesAndNewlines(Strip(text)), as chat and MCP do in Swift.
func (s *Service) Read(tenant uuid.UUID, username string, now time.Time) string {
	if data, err := os.ReadFile(s.vaultPath(tenant)); err == nil { //nolint:gosec // a fixed name under the tenant's raw directory
		if text, ok := vaultfiles.DecodeUTF8(data); ok {
			return text
		}
	}
	return RenderDefault(username, now)
}

// Write is SOULService.write: it enforces the core, checks the size of the result,
// writes the vault copy and then the Hermes copy, and returns what it persisted. A
// failure on the Hermes copy leaves the vault copy written.
func (s *Service) Write(tenant uuid.UUID, username, body string) (string, error) {
	enforced := Inject(body)
	if len(enforced) > MaxSizeBytes {
		return "", &TooLargeError{Bytes: len(enforced), Limit: MaxSizeBytes}
	}
	data := []byte(enforced)
	if err := s.paths.EnsureTenantDirectories(tenant); err != nil {
		return "", err
	}
	if err := vaultfiles.ReplaceFile(s.vaultPath(tenant), data); err != nil {
		return "", err
	}
	if err := s.ensureProfilesWritable(); err != nil {
		return "", err
	}
	if err := vaultfiles.ReplaceFile(s.hermesPath(username), data); err != nil {
		return "", err
	}
	return enforced, nil
}

// InitIfMissing is SOULService.initIfMissing. A file already there is kept, and only
// mirrored to the Hermes profile, best effort.
func (s *Service) InitIfMissing(tenant uuid.UUID, username string, now time.Time) error {
	if err := s.paths.EnsureTenantDirectories(tenant); err != nil {
		return err
	}
	target := s.vaultPath(tenant)
	if vaultfiles.Exists(target) {
		if data, err := os.ReadFile(target); err == nil { //nolint:gosec // a fixed name under the tenant's raw directory
			_ = vaultfiles.ReplaceFile(s.hermesPath(username), data) // try? in Swift
		}
		return nil
	}
	_, err := s.Write(tenant, username, RenderDefault(username, now))
	return err
}

// ensureProfilesWritable is HermesDataPathService.ensureProfilesDirectoryWritable.
func (s *Service) ensureProfilesWritable() error {
	profiles := filepath.Join(s.hermesRoot, "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil { //nolint:gosec // the mode Foundation's createDirectory leaves; Hermes reads this tree
		return fmt.Errorf("could not create profiles directory: %w", err)
	}
	probe := filepath.Join(profiles, ".write-probe-"+uuid.NewString())
	if err := os.WriteFile(probe, nil, 0o644); err != nil { //nolint:gosec // an empty probe, removed at once
		return fmt.Errorf("profiles directory is not writable: %w", err)
	}
	return os.Remove(probe)
}
