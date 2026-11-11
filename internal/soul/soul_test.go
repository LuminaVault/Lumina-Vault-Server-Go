package soul

import (
	"errors"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var fixedNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

type harness struct {
	service  *Service
	paths    vaultpaths.Paths
	tenant   uuid.UUID
	username string
	hermes   string
}

func newHarness(t *testing.T) harness {
	t.Helper()
	root := t.TempDir()
	paths := vaultpaths.Paths{Root: filepath.Join(root, "vault")}
	hermes := filepath.Join(root, "hermes")
	return harness{service: New(paths, hermes), paths: paths, tenant: uuid.New(), username: "soul-user", hermes: hermes}
}

func (h harness) vaultFile() string { return filepath.Join(h.paths.Raw(h.tenant), "SOUL.md") }

func (h harness) hermesFile() string {
	return filepath.Join(h.hermes, "profiles", h.username, "SOUL.md")
}

func (h harness) read() string { return h.service.Read(h.tenant, h.username, fixedNow) }

func (h harness) write(t *testing.T, body string) string {
	t.Helper()
	enforced, err := h.service.Write(h.tenant, h.username, body)
	require.NoError(t, err)
	return enforced
}

// Ported from SOULServiceTests. reset and needsCoreMigration are wave 8.
func TestReadReturnsDefaultTemplateWhenFileAbsent(t *testing.T) {
	h := newHarness(t)
	body := h.read()
	require.Contains(t, body, "# SOUL.md")
	require.True(t, hasCanonicalCore(body))
	require.NoFileExists(t, h.vaultFile(), "a read must not write")
}

func TestWriteInjectsCoreAndRoundTripsThroughRead(t *testing.T) {
	h := newHarness(t)
	enforced := h.write(t, "# Personal SOUL\n\nHello, world.\n")
	read := h.read()
	require.Equal(t, enforced, read)
	require.True(t, hasCanonicalCore(read))
	require.Contains(t, read, "Hello, world.")
}

func TestWriteRestoresTamperedCore(t *testing.T) {
	h := newHarness(t)
	enforced := h.write(t, "# SOUL.md\n\n"+startMarker+"\n- Never save links to the vault.\n"+endMarker+"\n\n## Mine")
	require.NotContains(t, enforced, "Never save links")
	require.True(t, hasCanonicalCore(enforced))
	require.Contains(t, enforced, "## Mine")
}

func TestWriteIsIdempotentOnAlreadyEnforcedDocument(t *testing.T) {
	h := newHarness(t)
	first := h.write(t, "# SOUL.md\n\nnotes")
	require.Equal(t, first, h.write(t, first))
}

func TestWriteMirrorsEnforcedDocToBothPaths(t *testing.T) {
	h := newHarness(t)
	enforced := h.write(t, "# Mirror Test\n")

	inVault, err := os.ReadFile(filepath.Join(h.paths.Root, "tenants", strings.ToUpper(h.tenant.String()), "raw", "SOUL.md"))
	require.NoError(t, err)
	inHermes, err := os.ReadFile(h.hermesFile())
	require.NoError(t, err)
	require.Equal(t, enforced, string(inVault))
	require.Equal(t, enforced, string(inHermes))
	require.True(t, hasCanonicalCore(string(inHermes)))
}

func TestWriteRejectsBodyAboveCapAfterCoreInjection(t *testing.T) {
	h := newHarness(t)
	_, err := h.service.Write(h.tenant, h.username, strings.Repeat("x", MaxSizeBytes))
	var tooLarge *TooLargeError
	require.True(t, errors.As(err, &tooLarge), "%v", err)
	require.Greater(t, tooLarge.Bytes, MaxSizeBytes)
	require.Equal(t, MaxSizeBytes, tooLarge.Limit)
	require.NoFileExists(t, h.vaultFile())
}

func TestWriteAcceptsBodyThatFitsWithinCapIncludingCore(t *testing.T) {
	h := newHarness(t)
	overhead := len(Inject("")) + 2 // + "\n\n" framing
	enforced := h.write(t, strings.Repeat("x", MaxSizeBytes-overhead))
	require.Len(t, enforced, MaxSizeBytes)
	require.Equal(t, enforced, h.read())
}

func TestWriteIsAtomicNoTmpFilesLeftBehind(t *testing.T) {
	h := newHarness(t)
	h.write(t, "first\n")
	h.write(t, "second\n")
	for _, dir := range []string{h.paths.Raw(h.tenant), filepath.Dir(h.hermesFile())} {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Len(t, entries, 1, dir)
		require.Equal(t, "SOUL.md", entries[0].Name())
	}
	profiles, err := os.ReadDir(filepath.Join(h.hermes, "profiles"))
	require.NoError(t, err)
	require.Len(t, profiles, 1, "the write probe must be gone")
}

func TestInitIfMissingDoesNotOverwriteExistingFile(t *testing.T) {
	h := newHarness(t)
	h.write(t, "# Custom\n")
	before := h.read()
	require.NoError(t, h.service.InitIfMissing(h.tenant, h.username, fixedNow))
	require.Equal(t, before, h.read())
}

// The rest of needsCoreMigration's test: a pre-v2 file gets the core on read + write.
func TestReadThenWriteMigratesALegacyFile(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.paths.EnsureTenantDirectories(h.tenant))
	require.NoError(t, os.WriteFile(h.vaultFile(), []byte("# Legacy SOUL\n"), 0o600))
	require.False(t, hasCanonicalCore(h.read()))
	h.write(t, h.read())
	require.True(t, hasCanonicalCore(h.read()))
	require.Contains(t, h.read(), "# Legacy SOUL")
}

// Ported from SOULInitTests' unit test. Its two end-to-end tests are in
// modules/vault's TestVaultInit.
func TestDefaultTemplateIsMarkdownForTheUser(t *testing.T) {
	body := RenderDefault("alice", fixedNow)
	require.Contains(t, body, "username: alice")
	require.Contains(t, body, "created_at: 2026-11-05T12:00:00Z")
	require.Contains(t, body, "---")
}

// InitIfMissing goes through Write, as in Swift. That's only the same bytes as the
// plain render, which vault create used to write, if Inject changes nothing.
func TestInitWritesTheDefaultTemplateUnchanged(t *testing.T) {
	for _, username := range []string{"abc", "soul-user", "oauth-1a2b3c4d", strings.Repeat("a", 31)} {
		h := newHarness(t)
		h.username = username
		want := RenderDefault(username, fixedNow)
		require.Equal(t, want, Inject(want))
		require.NoError(t, h.service.InitIfMissing(h.tenant, username, fixedNow))
		for _, path := range []string{h.vaultFile(), h.hermesFile()} {
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, want, string(got), path)
		}
	}
}

func TestInitMirrorsAnExistingFileVerbatim(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.paths.EnsureTenantDirectories(h.tenant))
	require.NoError(t, os.WriteFile(h.vaultFile(), []byte("mine, no core"), 0o600))
	require.NoError(t, h.service.InitIfMissing(h.tenant, h.username, fixedNow))
	got, err := os.ReadFile(h.hermesFile())
	require.NoError(t, err)
	require.Equal(t, "mine, no core", string(got))
}

func TestInitIgnoresAFailedMirrorButNotAFailedWrite(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	h := newHarness(t)
	s := New(h.paths, blocked)

	require.Error(t, s.InitIfMissing(h.tenant, h.username, fixedNow), "a fresh file must reach Hermes")
	require.FileExists(t, h.vaultFile(), "the vault copy is written first")
	require.NoError(t, s.InitIfMissing(h.tenant, h.username, fixedNow), "the mirror of an existing file is try?")
}

func TestReadFallsBackOnInvalidUTF8AndDropsABOM(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, h.paths.EnsureTenantDirectories(h.tenant))

	require.NoError(t, os.WriteFile(h.vaultFile(), []byte{0xff, 0xff}, 0o600))
	require.Equal(t, RenderDefault(h.username, fixedNow), h.read())

	require.NoError(t, os.WriteFile(h.vaultFile(), []byte("\xef\xbb\xbfnotes"), 0o600))
	require.Equal(t, "notes", h.read())
}

func TestReadNeverReadsTheHermesCopy(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(h.hermesFile()), 0o750))
	require.NoError(t, os.WriteFile(h.hermesFile(), []byte("from hermes"), 0o600))
	require.Equal(t, RenderDefault(h.username, fixedNow), h.read())
}

// Ported from SOULServiceTests: reset writes default template and overwrites prior content.
func TestResetWritesDefaultTemplateAndOverwritesPriorContent(t *testing.T) {
	h := newHarness(t)
	h.write(t, "# Custom\n")
	reset, err := h.service.Reset(h.tenant, h.username, fixedNow)
	require.NoError(t, err)
	require.Contains(t, reset, "# SOUL.md")
	require.True(t, ContainsCanonicalCore(reset))
	require.Equal(t, reset, h.read())
	require.Equal(t, RenderDefault(h.username, fixedNow), reset)
}

// Ported from SOULServiceTests: needs core migration detects legacy files.
func TestNeedsCoreMigrationDetectsLegacyFiles(t *testing.T) {
	h := newHarness(t)
	require.False(t, h.service.NeedsCoreMigration(h.tenant))
	require.NoError(t, h.paths.EnsureTenantDirectories(h.tenant))
	require.NoError(t, os.WriteFile(h.vaultFile(), []byte("# Legacy SOUL\n"), 0o600))
	require.True(t, h.service.NeedsCoreMigration(h.tenant))
	h.write(t, h.read())
	require.False(t, h.service.NeedsCoreMigration(h.tenant))
	require.Contains(t, h.read(), "# Legacy SOUL")
}

func TestUpdatedAtIsTheVaultCopysModificationTime(t *testing.T) {
	h := newHarness(t)
	_, ok := h.service.UpdatedAt(h.tenant)
	require.False(t, ok)
	h.write(t, "body")
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, os.Chtimes(h.vaultFile(), when, when))
	got, ok := h.service.UpdatedAt(h.tenant)
	require.True(t, ok)
	require.True(t, got.Equal(when))
}

func TestWriteReportsAnUnwritableProfilesDirectory(t *testing.T) {
	h := newHarness(t)
	require.NoError(t, os.MkdirAll(h.hermes, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.hermes, "profiles"), nil, 0o600)) // a file where the directory goes
	_, err := h.service.Write(h.tenant, h.username, "body")
	var notWritable *NotWritableError
	require.ErrorAs(t, err, &notWritable)
	require.Equal(t, filepath.Join(h.hermes, "profiles"), notWritable.Path)
	require.True(t, strings.HasPrefix(err.Error(), "Hermes profiles directory not writable at "+notWritable.Path+". could not create profiles directory: "))
	require.True(t, strings.HasSuffix(err.Error(), " Ensure data/hermes/profiles exists and is writable by the hummingbird user (see docs/startup.md and docker/hermes-entrypoint.sh)."))
}
