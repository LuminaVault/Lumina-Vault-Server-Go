package hermesprofile_test

import (
	"context"
	"encoding/json"
	"lumina/internal/hermesprofile"
	"lumina/internal/ids"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Ports FilesystemHermesGatewayTests.swift (4).
func TestFilesystemHermesGatewayProvisionsProfileDirectoryAndConfig(t *testing.T) {
	root := t.TempDir()
	tenant := uuid.New()
	id, err := hermesprofile.FilesystemGateway{Root: root}.ProvisionProfile(context.Background(), tenant, "alice")
	require.NoError(t, err)
	require.Equal(t, "alice", id)

	raw, err := os.ReadFile(filepath.Join(root, "profiles", "alice", "profile.json"))
	require.NoError(t, err)
	var cfg map[string]any
	require.NoError(t, json.Unmarshal(raw, &cfg))
	require.Equal(t, "alice", cfg["username"])
	require.Equal(t, ids.Upper(tenant), cfg["tenantID"])
	require.InDelta(t, 1, cfg["schemaVersion"], 0)
}

// FilesystemHermesGatewayTests: "same tenant same username is idempotent".
func TestFilesystemHermesGatewaySameTenantIsIdempotent(t *testing.T) {
	gw := hermesprofile.FilesystemGateway{Root: t.TempDir()}
	tenant := uuid.New()
	_, err := gw.ProvisionProfile(context.Background(), tenant, "bob")
	require.NoError(t, err)
	second, err := gw.ProvisionProfile(context.Background(), tenant, "bob")
	require.NoError(t, err)
	require.Equal(t, "bob", second)
}

// FilesystemHermesGatewayTests: "different tenant same username throws".
func TestFilesystemHermesGatewayDifferentTenantThrows(t *testing.T) {
	gw := hermesprofile.FilesystemGateway{Root: t.TempDir()}
	_, err := gw.ProvisionProfile(context.Background(), uuid.New(), "carol")
	require.NoError(t, err)
	_, err = gw.ProvisionProfile(context.Background(), uuid.New(), "carol")
	require.EqualError(t, err, "usernameCollision")
}

// FilesystemHermesGatewayTests: "delete renames dir". Swift's
// FilesystemHermesGateway.deleteProfile has no caller (only the protocol and the
// test use it), so Go has no deleteProfile. The mechanism that does move a
// profile aside in production is the reconciler's reapOrphans, with the same
// _deleted_<ts>_<name> rename: a provisioned profile no hermes_profiles row
// names is renamed, and the original path is gone.
func TestFilesystemHermesGatewayDeleteRenamesDir(t *testing.T) {
	v := setup(t)
	gw := hermesprofile.FilesystemGateway{Root: v.dataRoot}
	_, err := gw.ProvisionProfile(context.Background(), uuid.New(), "dave")
	require.NoError(t, err)

	s, err := v.reconciler(gw).ReapOrphans(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"dave"}, s.OrphansSoftDeleted)

	_, err = os.Stat(filepath.Join(v.dataRoot, "profiles", "dave"))
	require.ErrorIs(t, err, os.ErrNotExist)
	entries, err := os.ReadDir(filepath.Join(v.dataRoot, "profiles"))
	require.NoError(t, err)
	found := false
	for _, e := range entries {
		found = found || (strings.HasPrefix(e.Name(), "_deleted_") && strings.HasSuffix(e.Name(), "_dave"))
	}
	require.True(t, found, "a _deleted_*_dave entry replaces the profile")
}

// HermesDataPathServiceTests: "ensure creates profiles and accepts writes".
func TestHermesDataPathServiceEnsureCreatesProfiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "hermes")
	require.NoError(t, hermesprofile.EnsureProfilesWritable(root))
	profiles := filepath.Join(root, "profiles")
	require.DirExists(t, profiles)

	probe := filepath.Join(profiles, "alice")
	require.NoError(t, os.MkdirAll(probe, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(probe, "SOUL.md"), []byte("ok"), 0o600))
}

// HermesDataPathServiceTests: "ensure fails when profiles is not writable".
// Root ignores the permission bits, so this skips there, as Swift's does.
func TestHermesDataPathServiceEnsureFailsWhenNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores POSIX permission bits")
	}
	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	require.NoError(t, os.MkdirAll(profiles, 0o755))
	require.NoError(t, os.Chmod(profiles, 0o555))       //nolint:gosec // read-only on purpose
	t.Cleanup(func() { _ = os.Chmod(profiles, 0o755) }) //nolint:gosec // restore so TempDir cleanup can remove it

	err := hermesprofile.EnsureProfilesWritable(root)
	require.Error(t, err)
	require.Contains(t, err.Error(), "profiles")
}
