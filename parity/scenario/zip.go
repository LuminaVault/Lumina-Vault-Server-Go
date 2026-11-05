package scenario

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"lumina/parity/normalize"
)

// zipBody is what a `zip` step compares. Sizes are of the raw bytes, so a value
// that normalizes away still has to be as long on both servers. The CRC isn't
// compared, because an embedded timestamp changes it, but reading an entry checks
// it. A body that isn't a readable archive reports why, which shows up as a
// difference.
func zipBody(raw []byte, norm *normalize.Normalizer) any {
	r, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return map[string]any{"size": len(raw), "error": err.Error()}
	}
	entries := make([]any, 0, len(r.File))
	for _, f := range r.File {
		entry := map[string]any{
			"name":             f.Name,
			"method":           f.Method,
			"flags":            f.Flags,
			"compressedSize":   f.CompressedSize64,
			"uncompressedSize": f.UncompressedSize64,
		}
		// The local header's flags can differ from the central directory's.
		if off, err := f.DataOffset(); err == nil {
			if at := off - 30 - int64(len(f.Name)); at >= 0 && at+8 <= int64(len(raw)) {
				entry["localFlags"] = binary.LittleEndian.Uint16(raw[at+6:])
			}
		}
		rc, err := f.Open()
		if err != nil {
			entry["error"] = err.Error()
			entries = append(entries, entry)
			continue
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			entry["error"] = err.Error()
		}
		entry["content"] = digest(norm.Text(string(content)))
		entries = append(entries, entry)
	}
	return map[string]any{"size": len(raw), "entries": entries}
}
