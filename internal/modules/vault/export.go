package vault

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/modules/vault/db"
	"lumina/internal/soul"
	"lumina/internal/vaultfiles"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v5"
)

// VaultExportService: a streaming, uncompressed zip of the tenant's vault, with
// SOUL.md and memories.json at the root and every file under raw/. Each entry uses
// the data descriptor flag, so a file's CRC and size follow its body and only one
// chunk is ever in memory. No zip64: over 4 GiB is an error.
const (
	exportChunkSize      = 64 * 1024
	exportMemoryPageSize = 200
	maxZipBytes          = 4*1024*1024*1024 - 1

	zipVersion          = 20
	zipFlagDescriptor   = 0x0008
	sigLocalHeader      = 0x04034b50
	sigCentralHeader    = 0x02014b50
	sigDataDescriptor   = 0x08074b50
	sigEndOfCentralDir  = 0x06054b50
	maxZipEntries       = 0xffff
	swiftISO8601Seconds = "2006-01-02T15:04:05Z"
)

// export is VaultController.export. It always serves the caller's personal vault:
// unlike the file routes it doesn't read X-Vault-ID, so a team vault can't be
// exported. As in Swift the 200 is on the wire before the first byte of the body,
// so a later failure can only cut the connection, and Go must really cut it, or a
// client would take a truncated chunked body for a whole archive.
func (m *Module) export(c *echo.Context) error {
	id, err := requireIdentity(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	user, err := m.q.User(ctx, id)
	if err != nil {
		return userError(err)
	}
	since := parseISODate(wire.Query(c, "since"))

	w := c.Response()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="vault-`+ids.Upper(id)+`.zip"`)
	w.WriteHeader(http.StatusOK)
	stream := &zipStream{w: w}
	if err := m.streamExport(ctx, stream, user, since); err != nil {
		slog.ErrorContext(ctx, "vault export aborted", "tenant", ids.Upper(id), "error", err)
		panic(http.ErrAbortHandler)
	}
	slog.InfoContext(ctx, "vault export", "tenant", ids.Upper(id), "entries", len(stream.entries), "bytes", stream.offset)
	return nil
}

func (m *Module) streamExport(ctx context.Context, z *zipStream, user db.UserRow, since *time.Time) error {
	exportedAt := m.now()
	rawRoot := m.paths.Raw(user.ID)

	// Prefer the file on disk. Accounts older than SOULService have none, and get
	// a manifest built here.
	soulData, soulTime := m.soulForExport(rawRoot, user, exportedAt)
	if err := z.storedEntry(soul.FileName, soulData, soulTime); err != nil {
		return err
	}
	if err := m.memoriesEntry(ctx, z, user.ID, since, exportedAt); err != nil {
		return err
	}
	if vaultfiles.Exists(rawRoot) {
		files, err := enumerateRawFiles(rawRoot, since)
		if err != nil {
			return err
		}
		for _, f := range files {
			if err := z.fileEntry("raw/"+f.relativePath, f.path, f.size, f.modTime); err != nil {
				return err
			}
		}
	}
	return z.finish()
}

func (m *Module) soulForExport(rawRoot string, user db.UserRow, exportedAt time.Time) ([]byte, time.Time) {
	path := filepath.Join(rawRoot, soul.FileName)
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // a fixed name under the tenant's raw directory
		if info, err := os.Stat(path); err == nil {
			return data, info.ModTime()
		}
		return data, exportedAt
	}
	modTime := exportedAt
	if user.UpdatedAt != nil {
		modTime = *user.UpdatedAt
	}
	return []byte(buildSoulManifest(user, exportedAt)), modTime
}

// buildSoulManifest is VaultExportService.buildSoulMarkdown.
func buildSoulManifest(u db.UserRow, exportedAt time.Time) string {
	iso := func(t time.Time) string { return t.UTC().Format(swiftISO8601Seconds) }
	lines := []string{
		"# SOUL",
		"",
		"Identity manifest exported from LuminaVault on " + iso(exportedAt) + ".",
		"",
		"- tenant_id: `" + ids.Upper(u.ID) + "`",
		"- email: `" + u.Email + "`",
		"- username: `" + u.Username + "`",
		"- tier: `" + u.Tier + "`",
	}
	if u.TierExpiresAt != nil {
		lines = append(lines, "- tier_expires_at: `"+iso(*u.TierExpiresAt)+"`")
	}
	if u.CreatedAt != nil {
		lines = append(lines, "- created_at: `"+iso(*u.CreatedAt)+"`")
	}
	lines = append(lines,
		"- verified: `"+strconv.FormatBool(u.IsVerified)+"`",
		"",
		"This file is generated at export time. Re-import is not yet supported.",
	)
	return strings.Join(lines, "\n") + "\n"
}

// memoriesEntry streams the tenant's memories as one JSON array, a page at a time.
func (m *Module) memoriesEntry(ctx context.Context, z *zipStream, tenant uuid.UUID, since *time.Time, exportedAt time.Time) error {
	entry, err := z.beginEntry("memories.json", exportedAt)
	if err != nil {
		return err
	}
	if err := entry.write([]byte("[\n")); err != nil {
		return err
	}
	first := true
	for page := int32(0); ; page += exportMemoryPageSize {
		rows, err := m.q.ExportMemories(ctx, db.ExportMemoriesParams{TenantID: tenant, Since: sinceParam(since), Offset: page, Limit: exportMemoryPageSize})
		if err != nil {
			return err
		}
		for _, row := range rows {
			line := "  " + encodeExportMemory(row)
			if !first {
				line = ",\n" + line
			}
			first = false
			if err := entry.write([]byte(line)); err != nil {
				return err
			}
		}
		if len(rows) < exportMemoryPageSize {
			break
		}
	}
	if !first {
		if err := entry.write([]byte("\n")); err != nil {
			return err
		}
	}
	if err := entry.write([]byte("]\n")); err != nil {
		return err
	}
	return entry.end()
}

func sinceParam(since *time.Time) pgtype.Timestamptz {
	if since == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *since, Valid: true}
}

// encodeExportMemory is what Swift's JSONEncoder writes for MemoryExportRow with
// sortedKeys, withoutEscapingSlashes and iso8601 dates: nil keys left out.
func encodeExportMemory(row db.ExportMemoriesRow) string {
	var b strings.Builder
	b.WriteString(`{"content":`)
	writeSwiftJSONString(&b, row.Content)
	if row.CreatedAt != nil {
		b.WriteString(`,"createdAt":"` + row.CreatedAt.UTC().Format(swiftISO8601Seconds) + `"`)
	}
	b.WriteString(`,"id":"` + ids.Upper(row.ID) + `"`)
	if row.Tags != nil {
		b.WriteString(`,"tags":[`)
		for i, tag := range row.Tags {
			if i > 0 {
				b.WriteByte(',')
			}
			writeSwiftJSONString(&b, tag)
		}
		b.WriteByte(']')
	}
	b.WriteByte('}')
	return b.String()
}

// writeSwiftJSONString escapes as Foundation does: quote, backslash, control
// characters. encoding/json can't, because it also escapes U+2028 and U+2029. The
// entry's size is part of what parity compares.
func writeSwiftJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

type rawFile struct {
	path, relativePath string
	size               int64
	modTime            time.Time
}

// enumerateRawFiles is the enumerator with .skipsHiddenFiles: hidden directories
// aren't entered. Only regular files count, so a symlink is never followed out of
// the vault. Soft-deleted files and raw/SOUL.md stay out, the latter because it's
// already at the root. Sorted by relative path.
func enumerateRawFiles(rawRoot string, since *time.Time) ([]rawFile, error) {
	var out []rawFile
	err := filepath.WalkDir(rawRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// The enumerator has no error handler, so it carries on.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == rawRoot {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if since != nil && info.ModTime().Before(*since) {
			return nil
		}
		rel, err := filepath.Rel(rawRoot, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, "_deleted_") || rel == soul.FileName {
			return nil
		}
		out = append(out, rawFile{path: path, relativePath: rel, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b rawFile) int { return strings.Compare(a.relativePath, b.relativePath) })
	return out, nil
}

// zipStream writes the archive and remembers each entry for the central directory.
type zipStream struct {
	w       io.Writer
	offset  uint64
	entries []zipEntry
}

type zipEntry struct {
	name             string
	crc              uint32
	size             uint32
	offset           uint32
	dosTime, dosDate uint16
	descriptor       bool
}

func (z *zipStream) write(b []byte) error {
	z.offset += uint64(len(b))
	_, err := z.w.Write(b)
	return err
}

// storedEntry writes a synthesized entry whose size is known up front, so its CRC
// and sizes sit in the local header and no descriptor follows.
//
// QUIRK, as in Swift: an empty entry has CRC and size zero, so its local header
// claims a descriptor that doesn't exist, and the central directory says otherwise.
func (z *zipStream) storedEntry(name string, data []byte, modTime time.Time) error {
	if int64(len(data)) > maxZipBytes {
		return fmt.Errorf("synthesized entry exceeds zip32 limit: %s", name)
	}
	dosTime, dosDate := dosDateTime(modTime)
	sum := crc32.ChecksumIEEE(data)
	entry := zipEntry{
		name: name, crc: sum, size: uint32(len(data)), offset: clampUint32(z.offset), //nolint:gosec // checked above
		dosTime: dosTime, dosDate: dosDate,
	}
	if err := z.write(localHeader(name, dosTime, dosDate, sum, entry.size)); err != nil {
		return err
	}
	if err := z.write(data); err != nil {
		return err
	}
	z.entries = append(z.entries, entry)
	return nil
}

// openEntry is an entry being streamed: the header is out, and the CRC and size
// follow its body in a data descriptor.
type openEntry struct {
	z       *zipStream
	entry   zipEntry
	crc     uint32
	written int64
}

func (z *zipStream) beginEntry(name string, modTime time.Time) (*openEntry, error) {
	dosTime, dosDate := dosDateTime(modTime)
	e := &openEntry{z: z, entry: zipEntry{
		name: name, offset: clampUint32(z.offset), dosTime: dosTime, dosDate: dosDate, descriptor: true,
	}}
	return e, z.write(localHeader(name, dosTime, dosDate, 0, 0))
}

func (e *openEntry) write(b []byte) error {
	e.written += int64(len(b))
	if e.written > maxZipBytes {
		return errors.New("synthesized entry exceeds zip32 limit")
	}
	e.crc = crc32.Update(e.crc, crc32.IEEETable, b)
	return e.z.write(b)
}

func (e *openEntry) end() error {
	e.entry.crc = e.crc
	e.entry.size = clampUint32(uint64(e.written)) //nolint:gosec // written is never negative
	descriptor := make([]byte, 0, 16)
	descriptor = binary.LittleEndian.AppendUint32(descriptor, sigDataDescriptor)
	descriptor = binary.LittleEndian.AppendUint32(descriptor, e.entry.crc)
	descriptor = binary.LittleEndian.AppendUint32(descriptor, e.entry.size)
	descriptor = binary.LittleEndian.AppendUint32(descriptor, e.entry.size)
	if err := e.z.write(descriptor); err != nil {
		return err
	}
	e.z.entries = append(e.z.entries, e.entry)
	return nil
}

// fileEntry streams one file. A file that grew by more than a chunk since it was
// listed stops the export rather than make a corrupt archive.
func (z *zipStream) fileEntry(name, path string, size int64, modTime time.Time) error {
	if size > maxZipBytes {
		return fmt.Errorf("vault entry exceeds 4 GiB zip32 limit: %s", name)
	}
	entry, err := z.beginEntry(name, modTime)
	if err != nil {
		return err
	}
	f, err := os.Open(path) //nolint:gosec // path comes from walking the tenant's raw directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, exportChunkSize)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if entry.written+int64(n) > size+exportChunkSize {
				return fmt.Errorf("file size changed during export: %s", name)
			}
			if err := entry.write(buf[:n]); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	return entry.end()
}

// finish writes the central directory and the end record. More entries than the
// format holds traps in Swift (UInt16(entries.count)), taking the process down.
// Here only this export fails.
func (z *zipStream) finish() error {
	if len(z.entries) > maxZipEntries {
		return fmt.Errorf("vault export has %d entries, over the zip32 limit", len(z.entries))
	}
	start := z.offset
	for _, e := range z.entries {
		if err := z.write(e.centralHeader()); err != nil {
			return err
		}
	}
	size := z.offset - start
	if z.offset > maxZipBytes {
		return errors.New("vault export exceeds 4 GiB zip32 limit")
	}
	end := make([]byte, 0, 22)
	end = binary.LittleEndian.AppendUint32(end, sigEndOfCentralDir)
	end = binary.LittleEndian.AppendUint16(end, 0)                      // disk
	end = binary.LittleEndian.AppendUint16(end, 0)                      // disk where the directory starts
	end = binary.LittleEndian.AppendUint16(end, uint16(len(z.entries))) //nolint:gosec // checked above
	end = binary.LittleEndian.AppendUint16(end, uint16(len(z.entries))) //nolint:gosec // checked above
	end = binary.LittleEndian.AppendUint32(end, uint32(size))           //nolint:gosec // checked above
	end = binary.LittleEndian.AppendUint32(end, uint32(start))          //nolint:gosec // checked above
	end = binary.LittleEndian.AppendUint16(end, 0)                      // comment length
	return z.write(end)
}

func localHeader(name string, dosTime, dosDate uint16, crc, size uint32) []byte {
	flags := uint16(0)
	if crc == 0 && size == 0 {
		flags = zipFlagDescriptor
	}
	out := make([]byte, 0, 30+len(name))
	out = binary.LittleEndian.AppendUint32(out, sigLocalHeader)
	out = binary.LittleEndian.AppendUint16(out, zipVersion)
	out = binary.LittleEndian.AppendUint16(out, flags)
	out = binary.LittleEndian.AppendUint16(out, 0) // method: store
	out = binary.LittleEndian.AppendUint16(out, dosTime)
	out = binary.LittleEndian.AppendUint16(out, dosDate)
	out = binary.LittleEndian.AppendUint32(out, crc)
	out = binary.LittleEndian.AppendUint32(out, size)
	out = binary.LittleEndian.AppendUint32(out, size)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(name))) //nolint:gosec // a path, far below 64 KiB
	out = binary.LittleEndian.AppendUint16(out, 0)                 // extra length
	return append(out, name...)
}

func (e zipEntry) centralHeader() []byte {
	flags := uint16(0)
	if e.descriptor {
		flags = zipFlagDescriptor
	}
	out := make([]byte, 0, 46+len(e.name))
	out = binary.LittleEndian.AppendUint32(out, sigCentralHeader)
	out = binary.LittleEndian.AppendUint16(out, zipVersion) // version made by
	out = binary.LittleEndian.AppendUint16(out, zipVersion) // version needed
	out = binary.LittleEndian.AppendUint16(out, flags)
	out = binary.LittleEndian.AppendUint16(out, 0) // method: store
	out = binary.LittleEndian.AppendUint16(out, e.dosTime)
	out = binary.LittleEndian.AppendUint16(out, e.dosDate)
	out = binary.LittleEndian.AppendUint32(out, e.crc)
	out = binary.LittleEndian.AppendUint32(out, e.size)
	out = binary.LittleEndian.AppendUint32(out, e.size)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(e.name))) //nolint:gosec // a path, far below 64 KiB
	out = binary.LittleEndian.AppendUint16(out, 0)                   // extra length
	out = binary.LittleEndian.AppendUint16(out, 0)                   // comment length
	out = binary.LittleEndian.AppendUint16(out, 0)                   // disk start
	out = binary.LittleEndian.AppendUint16(out, 0)                   // internal attributes
	out = binary.LittleEndian.AppendUint32(out, 0)                   // external attributes
	out = binary.LittleEndian.AppendUint32(out, e.offset)
	return append(out, e.name...)
}

// dosDateTime is in the server's time zone, as Calendar(identifier: .gregorian) is.
// Production and parity run in UTC.
func dosDateTime(t time.Time) (dosTime, dosDate uint16) {
	t = t.Local()
	year := max(1980, min(2107, t.Year()))
	dosTime = uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2)  //nolint:gosec // each field fits its bits
	dosDate = uint16((year-1980)<<9 | int(t.Month())<<5 | t.Day()) //nolint:gosec // each field fits its bits
	return dosTime, dosDate
}

// clampUint32 is UInt32(clamping:).
func clampUint32(v uint64) uint32 {
	if v > 0xffffffff {
		return 0xffffffff
	}
	return uint32(v)
}
