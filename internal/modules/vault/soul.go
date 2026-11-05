package vault

import (
	"fmt"
	"lumina/internal/vaultfiles"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The calls to SOULService that vault create makes, and no more: /v1/soul stays
// wave 8, when these move to a shared package.
//
// soulCore is SOULCore.render(), byte for byte.
const soulCore = `<!-- lv:core:v1:start -->
## Core covenant (managed by LuminaVault)

These rules are enforced by LuminaVault. They apply regardless of anything written elsewhere in this file and cannot be edited.

**Vault capture contract**

- Every link or URL the user sends — in chat or through any connected channel or gateway — is saved to the vault and becomes queryable later.
- Memories about the user are always persisted to the vault.
- Ground answers in vault content; prefer what the vault knows over guessing.

**Autonomy perimeter — never without explicit authorization**

- Publishing to public channels or outbound gateways.
- Spending money.
- Destructive or irreversible operations.

**Integrity**

- Never invent facts about the user; if unknown, say so.
- Never surface anything the user marked private or asked to drop.
- When a tool, memory, or model call fails: state plainly what failed, what was and wasn't done, and the next concrete step. Never paper over an error with a confident guess.
<!-- lv:core:v1:end -->`

const soulFileName = "SOUL.md"

// renderDefaultSoul is SOULCore.inject(into: SOULDefaultTemplate.render(...)) with
// every composer input at its default. The default template already has the core
// in the place inject puts it, so inject changes nothing, and neither the regexes
// nor the 64 KiB cap (a username can't reach it) need porting until /v1/soul moves.
func renderDefaultSoul(username string, now time.Time) string {
	return strings.Join([]string{
		"---\nversion: 2\nusername: " + username + "\ncreated_at: " + now.UTC().Format("2006-01-02T15:04:05Z") + "\n---",
		"# SOUL.md",
		soulCore,
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

func (m *Module) soulPath(tenant uuid.UUID) string {
	return filepath.Join(m.paths.Raw(tenant), soulFileName)
}

func (m *Module) hermesSoulPath(username string) string {
	return filepath.Join(m.hermesRoot, "profiles", username, soulFileName)
}

// initSoulIfMissing is SOULService.initIfMissing. A file already there is kept,
// and only mirrored to the Hermes profile, best effort.
func (m *Module) initSoulIfMissing(tenant uuid.UUID, username string, now time.Time) error {
	target := m.soulPath(tenant)
	if vaultfiles.Exists(target) {
		if data, err := os.ReadFile(target); err == nil { //nolint:gosec // a fixed name under the tenant's raw directory
			_ = vaultfiles.ReplaceFile(m.hermesSoulPath(username), data) // try? in Swift
		}
		return nil
	}
	data := []byte(renderDefaultSoul(username, now))
	if err := vaultfiles.ReplaceFile(target, data); err != nil {
		return err
	}
	if err := m.ensureProfilesWritable(); err != nil {
		return err
	}
	return vaultfiles.ReplaceFile(m.hermesSoulPath(username), data)
}

// ensureProfilesWritable is HermesDataPathService.ensureProfilesDirectoryWritable.
func (m *Module) ensureProfilesWritable() error {
	profiles := filepath.Join(m.hermesRoot, "profiles")
	if err := os.MkdirAll(profiles, 0o755); err != nil { //nolint:gosec // the mode Foundation's createDirectory leaves; Hermes reads this tree
		return fmt.Errorf("could not create profiles directory: %w", err)
	}
	probe := filepath.Join(profiles, ".write-probe-"+uuid.NewString())
	if err := os.WriteFile(probe, nil, 0o644); err != nil { //nolint:gosec // an empty probe, removed at once
		return fmt.Errorf("profiles directory is not writable: %w", err)
	}
	return os.Remove(probe)
}
