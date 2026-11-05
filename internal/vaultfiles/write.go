package vaultfiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// filePerm goes through the process umask at open, which gives 0644 under the
// usual 022. That is what Data.write(options: .atomic) leaves on a new file on
// Linux, as far as can be read without running it: the Swift source wasn't
// available when this was written. Both servers run as UID 999 (D3), so the mode
// doesn't decide who can read a file.
const filePerm = 0o666

// WriteAtomic is `data.write(to: path, options: .atomic)`: the bytes go to a
// hidden file beside path and are renamed over it, so a reader sees the old file
// or the new one and never part of either. An existing file at path is replaced,
// a directory at path is an error, and the parent must exist. TodosController and
// LinkCaptureService write this way (after CreateParent); only upload adds the
// remove-and-move of ReplaceFile.
func WriteAtomic(path string, data []byte) error {
	aux := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".aux-"+shortID())
	f, err := os.OpenFile(aux, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return err
	}
	if err := writeAndSync(f, data); err != nil {
		_ = os.Remove(aux)
		return err
	}
	if err := os.Rename(aux, path); err != nil {
		_ = os.Remove(aux)
		return err
	}
	return nil
}

// ReplaceFile is the write in VaultController.upload: create the parent, write
// <target>.tmp-<8 uppercase hex> atomically, remove what is at target, then move
// the temp file there. target comes from ResolveInside.
//
// It is not atomic as a whole, though the name of WriteAtomic might suggest so:
// between the remove and the move there is no file at target, and a failed move
// leaves the old file gone and the temp file behind. Both are Swift's behaviour.
// So is the remove taking a whole directory with it if target names one, because
// FileManager.removeItem is recursive. A dangling symlink at target doesn't
// "exist", so nothing removes it and the move fails.
func ReplaceFile(target string, data []byte) error {
	if err := CreateParent(target); err != nil {
		return err
	}
	tmp := target + ".tmp-" + shortID()
	if err := WriteAtomic(tmp, data); err != nil {
		return err
	}
	if fileExists(target) {
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove %s: %w", target, err)
		}
	}
	return moveItem(tmp, target)
}

// CreateParent is `createDirectory(at: path.deletingLastPathComponent(),
// withIntermediateDirectories: true)`.
func CreateParent(path string) error { return createParent(path) }

func writeAndSync(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// shortID is `UUID().uuidString.prefix(8)`.
func shortID() string { return strings.ToUpper(uuid.NewString()[:8]) }
