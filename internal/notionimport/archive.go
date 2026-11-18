// Package notionimport reads a Notion "Markdown & CSV" export: the ZIP and its
// limits, the path map, the link rewrite, and a database's CSV and row pages.
// It writes nothing to the vault and knows no database; internal/modules/imports
// does the writing (spec 2026-11-18-e7-notion-import).
package notionimport

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// The limits of spec D1. They bound what an upload can put on disk: entries
// are streamed, never held in memory together.
const (
	MaxCompressedBytes   = 200 << 20
	MaxUncompressedBytes = 1 << 30
	MaxEntries           = 20_000
	MaxRatio             = 100
	// ratioFloor exempts small entries from the ratio check: a short, repetitive
	// text file compresses well and is no bomb; the total still bounds them.
	ratioFloor = 1 << 20
)

var (
	// ErrNotZip is an upload that doesn't open as a ZIP.
	ErrNotZip = errors.New("the upload is not a ZIP file")
	// ErrEmpty is a ZIP without a single file.
	ErrEmpty = errors.New("the ZIP is empty")
	// ErrEntryTooLarge is Entry.Read on an entry over its limit.
	ErrEntryTooLarge = errors.New("entry too large")
)

// LimitError is a broken D1 limit; its message names which.
type LimitError struct{ msg string }

func (e *LimitError) Error() string { return e.msg }

func limitf(format string, args ...any) error {
	return &LimitError{msg: fmt.Sprintf(format, args...)}
}

// Entry is one file of the export. Path is its name in the ZIP, forward
// slashes, as Notion wrote it.
type Entry struct {
	Path string
	// Size is the uncompressed size. archive/zip fails a read that yields
	// more, so it is an upper bound on what Open returns.
	Size int64
	file *zip.File
}

// Open streams the entry.
func (e Entry) Open() (io.ReadCloser, error) {
	if e.file == nil {
		return nil, errors.New("notionimport: entry has no data")
	}
	return e.file.Open()
}

// Read returns the whole entry, or ErrEntryTooLarge when it is over limit
// bytes.
func (e Entry) Read(limit int64) ([]byte, error) {
	if e.Size > limit {
		return nil, ErrEntryTooLarge
	}
	rc, err := e.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrEntryTooLarge
	}
	return data, nil
}

// Archive is an opened export. Close it to release the nested ZIPs' temp files.
type Archive struct {
	Entries []Entry

	closers []io.Closer
	temps   []string
	total   int64
	count   int
	seen    map[string]bool
}

// Close closes the nested ZIPs and removes their temp files.
func (a *Archive) Close() {
	for _, c := range a.closers {
		_ = c.Close()
	}
	for _, t := range a.temps {
		_ = os.Remove(t)
	}
	a.closers, a.temps = nil, nil
}

// Unpack opens the ZIP in r and lists its files, checking every D1 limit
// against the central directory. Nested ZIPs at the top level (Notion's
// ExportBlock-…-Part-N.zip) are extracted to tmpDir and listed in place, under
// the same totals; a ZIP inside a nested ZIP stays a plain entry. No entry's
// content is read here except a nested ZIP's.
func Unpack(r io.ReaderAt, size int64, tmpDir string) (*Archive, error) {
	if size == 0 {
		return nil, ErrEmpty
	}
	if size > MaxCompressedBytes {
		return nil, limitf("the ZIP is over %d MB", MaxCompressedBytes>>20)
	}
	zr, err := zip.NewReader(r, size)
	if errors.Is(err, zip.ErrInsecurePath) {
		return nil, limitf("the ZIP has an entry that points outside it")
	}
	if err != nil {
		return nil, ErrNotZip
	}
	a := &Archive{seen: map[string]bool{}}
	if err := a.add(zr.File, 0, tmpDir); err != nil {
		a.Close()
		return nil, err
	}
	if len(a.Entries) == 0 {
		a.Close()
		return nil, ErrEmpty
	}
	return a, nil
}

func (a *Archive) add(files []*zip.File, depth int, tmpDir string) error {
	for _, f := range files {
		name := strings.ReplaceAll(f.Name, `\`, "/")
		if strings.HasSuffix(name, "/") || f.FileInfo().IsDir() {
			continue
		}
		if err := checkEntryPath(name); err != nil {
			return err
		}
		if a.count++; a.count > MaxEntries {
			return limitf("the ZIP has more than %d files", MaxEntries)
		}
		size := f.UncompressedSize64
		if size > MaxUncompressedBytes || a.total+int64(size) > MaxUncompressedBytes {
			return limitf("the ZIP unpacks to more than %d MB", MaxUncompressedBytes>>20)
		}
		a.total += int64(size)
		if size > ratioFloor && (f.CompressedSize64 == 0 || size/f.CompressedSize64 > MaxRatio) {
			return limitf("%q is compressed more than %d times, which looks like a ZIP bomb", name, MaxRatio)
		}
		if depth == 0 && strings.HasSuffix(strings.ToLower(name), ".zip") {
			nested, err := a.openNested(f, tmpDir)
			if err != nil {
				return err
			}
			if nested != nil {
				if err := a.add(nested, 1, tmpDir); err != nil {
					return err
				}
				continue
			}
		}
		if a.seen[name] {
			continue
		}
		a.seen[name] = true
		a.Entries = append(a.Entries, Entry{Path: name, Size: int64(size), file: f}) //nolint:gosec // size is at most MaxUncompressedBytes
	}
	return nil
}

// openNested extracts a nested ZIP to a temp file and opens it. A file named
// .zip that isn't one returns nil: it stays an ordinary entry.
func (a *Archive) openNested(f *zip.File, tmpDir string) ([]*zip.File, error) {
	src, err := f.Open()
	if err != nil {
		return nil, ErrNotZip
	}
	defer func() { _ = src.Close() }()
	tmp, err := os.CreateTemp(tmpDir, "nested-*.zip")
	if err != nil {
		return nil, err
	}
	a.temps = append(a.temps, tmp.Name())
	_, copyErr := io.Copy(tmp, io.LimitReader(src, int64(f.UncompressedSize64)+1)) //nolint:gosec // bounded above
	if closeErr := tmp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		if errors.Is(copyErr, zip.ErrFormat) || errors.Is(copyErr, zip.ErrChecksum) {
			return nil, ErrNotZip
		}
		return nil, copyErr
	}
	zr, err := zip.OpenReader(tmp.Name())
	if errors.Is(err, zip.ErrInsecurePath) {
		if zr != nil {
			_ = zr.Close()
		}
		return nil, limitf("the ZIP has an entry that points outside it")
	}
	if err != nil {
		return nil, nil //nolint:nilerr // not a ZIP after all: keep it as a file
	}
	a.closers = append(a.closers, zr)
	return zr.File, nil
}

// checkEntryPath refuses a name that would leave the export's root.
func checkEntryPath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) ||
		(len(name) > 1 && name[1] == ':') {
		return limitf("the ZIP has an entry with an absolute path: %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return limitf("the ZIP has an entry that points outside it: %q", name)
		}
	}
	return nil
}
