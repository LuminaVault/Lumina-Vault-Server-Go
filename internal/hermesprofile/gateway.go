package hermesprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/wire"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Gateway is HermesGateway's provisioning half. It returns the Hermes profile id.
type Gateway interface {
	ProvisionProfile(ctx context.Context, tenant uuid.UUID, username string) (string, error)
}

// NewGateway is makeHermesGateway: "filesystem", otherwise the logging gateway, with
// a warning if kind is one it doesn't know.
func NewGateway(kind, dataRoot string) Gateway {
	switch strings.ToLower(kind) {
	case "filesystem":
		return FilesystemGateway{Root: dataRoot}
	case "logging":
	default:
		slog.Warn("unknown hermes.gatewayKind; falling back to LoggingHermesGateway", "kind", kind)
	}
	return LoggingGateway{}
}

// LoggingGateway is LoggingHermesGateway: nothing is provisioned, for dev only.
type LoggingGateway struct{}

// ProvisionProfile returns a stable, readable id.
func (LoggingGateway) ProvisionProfile(ctx context.Context, tenant uuid.UUID, username string) (string, error) {
	slog.WarnContext(ctx, "hermes (dev only): provisionProfile", "tenantID", ids.Upper(tenant), "username", username)
	return "hermes-" + username, nil
}

// errUsernameCollision is HermesGatewayError.usernameCollision, spelled as
// String(describing:) writes it into hermes_profiles.last_error.
var errUsernameCollision = errors.New("usernameCollision")

// ioFailure is HermesGatewayError.ioFailure as String(describing:) writes it.
func ioFailure(err error) error { return fmt.Errorf("ioFailure(%q)", err.Error()) }

// FilesystemGateway is FilesystemHermesGateway: profiles/<username>/profile.json on
// the volume Hermes also mounts. Swift's replicas write the same file while both
// servers run, so its bytes have to match theirs.
type FilesystemGateway struct {
	Root string
}

// ProvisionProfile returns username, which is the profile id here. A profile.json
// for another tenant is a collision, and one for this tenant is left alone.
func (g FilesystemGateway) ProvisionProfile(_ context.Context, tenant uuid.UUID, username string) (string, error) {
	if err := EnsureProfilesWritable(g.Root); err != nil {
		return "", ioFailure(err)
	}
	dir := filepath.Join(g.Root, profilesDir, username)
	path := filepath.Join(dir, "profile.json")

	if existing, err := os.ReadFile(path); err == nil { //nolint:gosec // the path is built from the server's data root and a stored username
		owner, err := profileOwner(existing)
		if err != nil {
			return "", err
		}
		if owner != tenant {
			return "", errUsernameCollision
		}
		return username, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	payload, err := encodeProfile(username, tenant, time.Now())
	if err != nil {
		return "", ioFailure(err)
	}
	if err := writeAtomic(dir, path, payload); err != nil {
		return "", ioFailure(err)
	}
	return username, nil
}

func writeAtomic(dir, path string, payload []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the mode Foundation's createDirectory leaves; Hermes reads this tree
		return err
	}
	tmp := filepath.Join(dir, "profile.json.tmp")
	if err := os.WriteFile(tmp, payload, 0o644); err != nil { //nolint:gosec // the mode Data.write leaves; Hermes reads this file
		return err
	}
	return os.Rename(tmp, path)
}

const profilesDir = "profiles"

// EnsureProfilesWritable is HermesDataPathService.ensureProfilesDirectoryWritable.
// Swift runs it at boot for the filesystem gateway and fails the boot on an error.
// So should whichever main mounts Middleware.
func EnsureProfilesWritable(root string) error {
	profiles := filepath.Join(root, profilesDir)
	if err := os.MkdirAll(profiles, 0o755); err != nil { //nolint:gosec // the mode Foundation's createDirectory leaves; Hermes reads this tree
		return fmt.Errorf("could not create profiles directory: %w", err)
	}
	probe := filepath.Join(profiles, ".write-probe-"+ids.Upper(uuid.New()))
	if err := os.WriteFile(probe, nil, 0o644); err != nil { //nolint:gosec // an empty probe, removed at once
		return fmt.Errorf("profiles directory is not writable: %w", err)
	}
	return os.Remove(probe)
}

// encodeProfile is HermesProfileConfig through JSONEncoder with
// [.prettyPrinted, .sortedKeys] and .iso8601 dates: `" : "`, two spaces, no newline
// at the end.
func encodeProfile(username string, tenant uuid.UUID, createdAt time.Time) ([]byte, error) {
	name, err := wire.MarshalFoundation(username)
	if err != nil {
		return nil, err
	}
	created, err := wire.MarshalFoundation(wire.Time{Time: createdAt})
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "{\n  \"createdAt\" : %s,\n  \"schemaVersion\" : 1,\n  \"tenantID\" : %q,\n  \"username\" : %s\n}",
		created, ids.Upper(tenant), name), nil
}

// profileOwner decodes an existing profile.json. Every field is required, as the
// Swift struct's are, and a file that won't decode fails the provisioning.
func profileOwner(raw []byte) (uuid.UUID, error) {
	var cfg struct {
		Username      *string    `json:"username"`
		TenantID      *string    `json:"tenantID"`
		CreatedAt     *wire.Time `json:"createdAt"`
		SchemaVersion *int       `json:"schemaVersion"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return uuid.Nil, err
	}
	if cfg.Username == nil || cfg.TenantID == nil || cfg.CreatedAt == nil || cfg.SchemaVersion == nil {
		return uuid.Nil, errors.New("profile.json is missing a field")
	}
	return ids.Parse(*cfg.TenantID)
}
