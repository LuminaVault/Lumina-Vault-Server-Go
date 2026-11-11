package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/jobs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// DeletedDataPurge is DeletedDataPurgeJob (Sources/App/Account/DeletedDataPurgeJob.swift).
// It runs `rm -r` on user data unattended, so it is ported line for line:
//
//   - only direct children of the roots are candidates, never anything deeper;
//   - a child must be named exactly `_deleted_<1-12 digits>_<identity>`;
//   - a symlink or non-directory candidate is skipped, and removal uses lstat at every
//     level and unlinks symlinks without following them;
//   - age comes from the timestamp in the name, mtime only when that won't parse.
//
// Swift quirk, kept: a root that is itself a symlink is skipped (lstat), even if it
// points at a directory.
type DeletedDataPurge struct {
	Roots     []PurgeRoot
	Retention time.Duration
	Logger    *slog.Logger
}

// PurgeIdentity says what a root's soft-deleted children are named after.
type PurgeIdentity int

const (
	// IdentityTenantUUID is `<vaultRoot>/tenants/`: the tenant UUID.
	IdentityTenantUUID PurgeIdentity = iota
	// IdentityProfileName is `<hermesDataRoot>/profiles/`: a Hermes profile id.
	IdentityProfileName
)

// PurgeRoot is DeletedDataPurgeJob.Root.
type PurgeRoot struct {
	Path     string
	Identity PurgeIdentity
	// Label is the short name for logs (vault, hermes, hermes-legacy).
	Label string
}

// PurgeSummary is DeletedDataPurgeJob.Summary.
type PurgeSummary struct {
	Purged, Retained, Skipped, Failed int
}

const (
	purgePrefix = "_deleted_"
	// DefaultPurgeRetentionDays is account.deletion.purgeAfterDays's default.
	DefaultPurgeRetentionDays = 30
)

// NewDeletedDataPurge is DeletedDataPurgeJob.init: a zero or negative retention would
// purge on the next tick, so it is clamped to one day.
func NewDeletedDataPurge(roots []PurgeRoot, retentionDays int, logger *slog.Logger) *DeletedDataPurge {
	if logger == nil {
		logger = slog.Default()
	}
	return &DeletedDataPurge{Roots: roots, Retention: time.Duration(max(1, retentionDays)) * 24 * time.Hour, Logger: logger}
}

// PurgeRoots is DeletedDataPurgeJob.roots: the vault's tenants/, Hermes profiles/, and
// the bare Hermes data root, where an old deletion renamed `<hermesDataRoot>/<id>`.
func PurgeRoots(vaultRootPath, hermesDataRoot string) []PurgeRoot {
	return []PurgeRoot{
		{Path: filepath.Join(vaultRootPath, "tenants"), Identity: IdentityTenantUUID, Label: "vault"},
		{Path: filepath.Join(hermesDataRoot, "profiles"), Identity: IdentityProfileName, Label: "hermes"},
		{Path: hermesDataRoot, Identity: IdentityTenantUUID, Label: "hermes-legacy"},
	}
}

// PurgeDirectoryName is DeletedDataPurgeJob.directoryName.
func PurgeDirectoryName(tenant uuid.UUID, deletedAt time.Time) string {
	return fmt.Sprintf("%s%d_%s", purgePrefix, deletedAt.Unix(), strings.ToUpper(tenant.String()))
}

// ParsePurgeName is DeletedDataPurgeJob.parse. ok is false for anything that is not
// exactly `_deleted_<1-12 digits>_<identity>`. deletedAt is nil when the digits don't
// form a time; with at most 12 digits they always do, as in Swift.
func ParsePurgeName(name string, identity PurgeIdentity) (deletedAt *time.Time, id string, ok bool) {
	rest, found := strings.CutPrefix(name, purgePrefix)
	if !found {
		return nil, "", false
	}
	digits, id, found := strings.Cut(rest, "_")
	if !found || len(digits) < 1 || len(digits) > 12 {
		return nil, "", false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return nil, "", false
		}
	}
	switch identity {
	case IdentityTenantUUID:
		if len(id) != 36 {
			return nil, "", false
		}
		if _, err := uuid.Parse(id); err != nil {
			return nil, "", false
		}
	case IdentityProfileName:
		if !IsSafeProfileName(id) {
			return nil, "", false
		}
	}
	if seconds, err := strconv.ParseInt(digits, 10, 64); err == nil {
		t := time.Unix(seconds, 0)
		deletedAt = &t
	}
	return deletedAt, id, true
}

// IsSafeProfileName is DeletedDataPurgeJob.isSafeProfileName: ASCII letters, digits,
// `.`, `_` and `-`, at most 128, and never `.` or `..`.
func IsSafeProfileName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// Run is DeletedDataPurgeJob.run.
func (j *DeletedDataPurge) Run(now time.Time) PurgeSummary {
	var summary PurgeSummary
	for _, root := range j.Roots {
		j.purge(root, now, &summary)
	}
	j.Logger.Info("account.deletion.purge_run",
		"purged", summary.Purged, "retained", summary.Retained, "skipped", summary.Skipped, "failed", summary.Failed)
	return summary
}

func (j *DeletedDataPurge) purge(root PurgeRoot, now time.Time, summary *PurgeSummary) {
	rootPath := filepath.Clean(root.Path)
	if entryKind(rootPath) != kindDirectory {
		return
	}
	names, err := readNames(rootPath)
	if err != nil {
		summary.Failed++
		j.Logger.Error("account.deletion.purge_list_failed", "root", root.Label, "error", err.Error())
		return
	}
	for _, name := range names {
		parsedAt, id, ok := ParsePurgeName(name, root.Identity)
		if !ok {
			continue
		}
		path := rootPath + "/" + name
		if entryKind(path) != kindDirectory {
			// A symlink or file wearing the soft-delete name was not made by the
			// deletion service. Leave it for a human.
			summary.Skipped++
			j.Logger.Warn("account.deletion.purge_skipped_not_directory", "root", root.Label, "name", name)
			continue
		}
		deletedAt := parsedAt
		if deletedAt == nil {
			deletedAt = modificationTime(path)
		}
		if deletedAt == nil {
			summary.Skipped++
			continue
		}
		if now.Sub(*deletedAt) < j.Retention {
			summary.Retained++
			continue
		}
		if err := removeTree(path); err != nil {
			summary.Failed++
			j.Logger.Error("account.deletion.purge_failed", "root", root.Label, "id", id, "error", err.Error())
			continue
		}
		summary.Purged++
		// Identifier only, never a file name from inside the tree.
		j.Logger.Info("account.deletion.purged", "root", root.Label, "id", id,
			"deletedAt", deletedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
}

// Job is DeletedDataPurgeService: once a day at 04:00 UTC, an hour after
// LapseArchiverService. A run with failures is reported as an error so the gate
// metric doesn't count it as a success; Swift only logs them.
func (j *DeletedDataPurge) Job() jobs.Job {
	return jobs.Job{
		Name:     "DeletedDataPurgeService",
		Schedule: jobs.DailyUTC(4),
		Run: func(context.Context) error {
			if s := j.Run(time.Now()); s.Failed > 0 {
				return fmt.Errorf("purge: %d failed", s.Failed)
			}
			return nil
		},
	}
}

type fileKind int

const (
	kindMissing fileKind = iota
	kindDirectory
	kindSymlink
	kindOther
)

// entryKind is FilesystemEntry.kind: lstat, never following a link.
func entryKind(path string) fileKind {
	info, err := os.Lstat(path)
	if err != nil {
		return kindMissing
	}
	switch mode := info.Mode(); {
	case mode&os.ModeSymlink != 0:
		return kindSymlink
	case mode.IsDir():
		return kindDirectory
	default:
		return kindOther
	}
}

func modificationTime(path string) *time.Time {
	info, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	t := time.Unix(info.ModTime().Unix(), 0)
	return &t
}

func readNames(dir string) ([]string, error) {
	f, err := os.Open(dir) //nolint:gosec // a purge root or a directory found under one, never request input
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return names, err
}

// removeError is FilesystemEntry.RemoveError.
type removeError struct {
	path  string
	errno syscall.Errno
}

func (e removeError) Error() string {
	return fmt.Sprintf("could not remove %s: errno %d", e.path, int(e.errno))
}

// removeTree is FilesystemEntry.removeTree: removes path and everything under it. A
// symlink anywhere in the tree is unlinked itself; its target is never visited.
func removeTree(path string) error {
	switch entryKind(path) {
	case kindMissing:
		return nil
	case kindDirectory:
		names, err := readNames(path)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := removeTree(path + "/" + name); err != nil {
				return err
			}
		}
		return errnoError(path, syscall.Rmdir(path))
	default:
		return errnoError(path, syscall.Unlink(path))
	}
}

func errnoError(path string, err error) error {
	if err == nil {
		return nil
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return removeError{path: path, errno: errno}
	}
	return err
}
