package workers

import (
	"fmt"
	"log/slog"
	"lumina/internal/modules/account"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ports Tests/AppTests/Account/DeletedDataPurgeJobTests.swift. The job runs `rm -r`
// on user data unattended, so these pin what it must not touch as much as what it
// removes.

const day = 24 * time.Hour

var purgeNow = time.Unix(1_800_000_000, 0)

// sandbox is `<base>/vault/tenants`, `<base>/hermes/profiles`, and `<base>/outside`,
// which nothing may ever delete from.
type sandbox struct{ base string }

func newSandbox(t *testing.T) sandbox {
	t.Helper()
	s := sandbox{base: t.TempDir()}
	for _, dir := range []string{s.tenants(), s.profiles(), s.outside()} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(s.outside(), "precious.txt"), []byte("keep me"), 0o644))
	return s
}

func (s sandbox) tenants() string  { return filepath.Join(s.base, "vault/tenants") }
func (s sandbox) hermes() string   { return filepath.Join(s.base, "hermes") }
func (s sandbox) profiles() string { return filepath.Join(s.base, "hermes/profiles") }
func (s sandbox) outside() string  { return filepath.Join(s.base, "outside") }

func (s sandbox) job(retentionDays int) *DeletedDataPurge {
	return NewDeletedDataPurge(PurgeRoots(filepath.Join(s.base, "vault"), s.hermes()), retentionDays, slog.New(slog.DiscardHandler))
}

func makeDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "raw"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "raw/note.md"), []byte("note"), 0o644))
	return dir
}

func exists(path string) bool { return entryKind(path) != kindMissing }

func stamp(daysAgo float64) int64 {
	return purgeNow.Unix() - int64(daysAgo*float64(day/time.Second))
}

func upper(id uuid.UUID) string { return strings.ToUpper(id.String()) }

// Swift: "purges only correctly named directories past retention".
func TestPurgesOnlyCorrectlyNamedDirectoriesPastRetention(t *testing.T) {
	box := newSandbox(t)
	old := makeDir(t, box.tenants(), PurgeDirectoryName(uuid.New(), purgeNow.Add(-31*day)))
	recent := makeDir(t, box.tenants(), PurgeDirectoryName(uuid.New(), purgeNow.Add(-29*day)))
	live := makeDir(t, box.tenants(), upper(uuid.New()))
	notUUID := makeDir(t, box.tenants(), fmt.Sprintf("_deleted_%d_not-a-uuid", stamp(400)))
	noDigits := makeDir(t, box.tenants(), "_deleted_abc_"+upper(uuid.New()))
	extraSuffix := makeDir(t, box.tenants(), fmt.Sprintf("_deleted_%d_%s_x", stamp(400), upper(uuid.New())))
	prefixed := makeDir(t, box.tenants(), fmt.Sprintf("x_deleted_%d_%s", stamp(400), upper(uuid.New())))

	summary := box.job(30).Run(purgeNow)

	assert.False(t, exists(old))
	for _, kept := range []string{recent, live, notUUID, noDigits, extraSuffix, prefixed} {
		assert.True(t, exists(kept), "%s must survive", filepath.Base(kept))
	}
	assert.Equal(t, 1, summary.Purged)
	assert.Equal(t, 1, summary.Retained)
}

// Swift: "purges old Hermes profile soft-deletes and leaves live profiles".
func TestPurgesOldHermesProfileSoftDeletes(t *testing.T) {
	box := newSandbox(t)
	deletedAccount := makeDir(t, box.profiles(), PurgeDirectoryName(uuid.New(), purgeNow.Add(-60*day)))
	reaped := makeDir(t, box.profiles(), fmt.Sprintf("_deleted_%d_hermes-alice", stamp(45)))
	live := makeDir(t, box.profiles(), "hermes-bob")
	traversal := makeDir(t, box.profiles(), fmt.Sprintf("_deleted_%d_..", stamp(45)))

	summary := box.job(30).Run(purgeNow)

	assert.False(t, exists(deletedAccount))
	assert.False(t, exists(reaped))
	assert.True(t, exists(live))
	assert.True(t, exists(traversal))
	assert.Equal(t, 2, summary.Purged)
}

// Swift: "never follows a symlink out of the root".
func TestPurgeNeverFollowsSymlinkOutOfRoot(t *testing.T) {
	box := newSandbox(t)
	// 1. A symlink wearing a valid soft-delete name, pointing outside.
	trap := filepath.Join(box.tenants(), PurgeDirectoryName(uuid.New(), purgeNow.Add(-90*day)))
	require.NoError(t, os.Symlink(box.outside(), trap))
	// 2. A real, purgeable directory that contains a symlink to outside.
	purgeable := makeDir(t, box.tenants(), PurgeDirectoryName(uuid.New(), purgeNow.Add(-90*day)))
	require.NoError(t, os.Symlink(box.outside(), filepath.Join(purgeable, "raw/escape")))
	// 3. A plain file wearing a valid name.
	file := filepath.Join(box.tenants(), PurgeDirectoryName(uuid.New(), purgeNow.Add(-90*day)))
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	summary := box.job(30).Run(purgeNow)

	assert.Equal(t, kindSymlink, entryKind(trap), "a symlinked candidate is skipped, not removed")
	assert.True(t, exists(file), "a non-directory candidate is skipped")
	assert.False(t, exists(purgeable))
	assert.FileExists(t, filepath.Join(box.outside(), "precious.txt"), "nothing outside the root may be deleted")
	assert.Equal(t, 1, summary.Purged)
	assert.Equal(t, 2, summary.Skipped)
}

// Swift: "retention is configurable and never below one day".
func TestPurgeRetentionNeverBelowOneDay(t *testing.T) {
	box := newSandbox(t)
	tenDays := makeDir(t, box.tenants(), PurgeDirectoryName(uuid.New(), purgeNow.Add(-10*day)))
	hourOld := makeDir(t, box.tenants(), PurgeDirectoryName(uuid.New(), purgeNow.Add(-time.Hour)))

	box.job(0).Run(purgeNow)

	assert.False(t, exists(tenDays))
	assert.True(t, exists(hourOld), "a zero setting must not remove the recovery window")
}

// Swift: "missing roots are a no-op".
func TestPurgeMissingRootsAreNoOp(t *testing.T) {
	job := NewDeletedDataPurge(PurgeRoots("/nonexistent-"+uuid.NewString(), "/nonexistent-"+uuid.NewString()), DefaultPurgeRetentionDays, slog.New(slog.DiscardHandler))
	assert.Equal(t, PurgeSummary{}, job.Run(purgeNow))
}

// Swift: "parses exactly the deletion service's name".
func TestParsePurgeNameExactly(t *testing.T) {
	tenant := uuid.New()
	at := time.Unix(1_700_000_000, 0)
	deletedAt, id, ok := ParsePurgeName(PurgeDirectoryName(tenant, at), IdentityTenantUUID)
	require.True(t, ok)
	assert.Equal(t, upper(tenant), id)
	require.NotNil(t, deletedAt)
	assert.True(t, deletedAt.Equal(at))
	for name, identity := range map[string]PurgeIdentity{
		"_deleted__" + upper(tenant):              IdentityTenantUUID,
		"_deleted_1234567890123_" + upper(tenant): IdentityTenantUUID,
		"_deleted_1_a/b":                          IdentityProfileName,
		"_deleted_1_.":                            IdentityProfileName,
	} {
		_, _, ok := ParsePurgeName(name, identity)
		assert.False(t, ok, name)
	}
}

// Go only: the account module and this job agree on the soft-delete name, and a
// lowercase UUID parses as Swift's UUID(uuidString:) does.
func TestPurgeNameEdges(t *testing.T) {
	tenant := uuid.New()
	_, _, ok := ParsePurgeName(fmt.Sprintf("_deleted_5_%s", tenant.String()), IdentityTenantUUID)
	assert.True(t, ok)
	_, _, ok = ParsePurgeName("_deleted_5_hermés", IdentityProfileName)
	assert.False(t, ok, "non-ASCII profile names are refused")
	_, _, ok = ParsePurgeName("_deleted_5_"+strings.Repeat("a", 129), IdentityProfileName)
	assert.False(t, ok)
	assert.True(t, IsSafeProfileName("hermes_bob-1.x"))
	at := time.Unix(1_700_000_000, 0)
	assert.Equal(t, account.DirectoryName(tenant, at), PurgeDirectoryName(tenant, at))
}
