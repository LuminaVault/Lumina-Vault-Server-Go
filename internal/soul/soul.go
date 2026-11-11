// Package soul is the Go port of LuminaVaultServer's SOULCore and SOULService
// (Sources/App/Auth). SOUL.md is the per-user Hermes personality file, and Hermes
// loads it on every turn, so a write lands in two places: the tenant's raw directory
// and the user's Hermes profile.
//
// /v1/soul (wave 8) is internal/modules/soul. needsCoreMigration has no caller in
// Go: SOULCoreMigrationJob stays in Swift.
package soul

import (
	"fmt"
	"lumina/internal/ids"
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
	return Compose(ComposeInput{}, username, now)
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

// Reset is SOULService.reset: the default template through Write.
func (s *Service) Reset(tenant uuid.UUID, username string, now time.Time) (string, error) {
	return s.Write(tenant, username, RenderDefault(username, now))
}

// UpdatedAt is SOULService.updatedAt: the vault copy's modification time, and
// false when there's no file.
func (s *Service) UpdatedAt(tenant uuid.UUID) (time.Time, bool) {
	info, err := os.Stat(s.vaultPath(tenant))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

// NeedsCoreMigration is SOULService.needsCoreMigration: the vault copy exists and
// is UTF-8 but lacks the canonical core.
func (s *Service) NeedsCoreMigration(tenant uuid.UUID) bool {
	data, err := os.ReadFile(s.vaultPath(tenant)) //nolint:gosec // a fixed name under the tenant's raw directory
	if err != nil {
		return false
	}
	text, ok := vaultfiles.DecodeUTF8(data)
	return ok && !ContainsCanonicalCore(text)
}

// ContainsCanonicalCore is SOULCore.containsCanonicalCore.
func ContainsCanonicalCore(markdown string) bool { return strings.Contains(markdown, Core) }

// NotWritableError is HermesDataError.notWritable. The controller answers it with a
// 503 carrying Error(). Reason embeds the OS error, which Foundation words
// differently, so the text can't match Swift's byte for byte.
type NotWritableError struct {
	Path   string
	Reason string
}

func (e *NotWritableError) Error() string {
	return "Hermes profiles directory not writable at " + e.Path + ". " + e.Reason +
		" Ensure data/hermes/profiles exists and is writable by the hummingbird user (see docs/startup.md and docker/hermes-entrypoint.sh)."
}

// ensureProfilesWritable is HermesDataPathService.ensureProfilesDirectoryWritable.
func (s *Service) ensureProfilesWritable() error {
	profiles := filepath.Join(s.hermesRoot, "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil { //nolint:gosec // the mode Foundation's createDirectory leaves; Hermes reads this tree
		return &NotWritableError{Path: profiles, Reason: fmt.Sprintf("could not create profiles directory: %v", err)}
	}
	probe := filepath.Join(profiles, ".write-probe-"+ids.Upper(uuid.New()))
	if err := os.WriteFile(probe, nil, 0o644); err != nil { //nolint:gosec // an empty probe, removed at once
		return &NotWritableError{Path: profiles, Reason: fmt.Sprintf("profiles directory is not writable by the app process: %v", err)}
	}
	if err := os.Remove(probe); err != nil {
		return &NotWritableError{Path: profiles, Reason: fmt.Sprintf("profiles directory is not writable by the app process: %v", err)}
	}
	return nil
}
