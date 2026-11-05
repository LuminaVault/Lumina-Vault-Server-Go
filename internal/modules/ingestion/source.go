package ingestion

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

var errSourceNotFound = wire.NewError(http.StatusNotFound, "ingestion source not found or expired")

// sourceTokenHash is MultimodalIngestionService.sourceTokenHash. The worker mints
// the token; the row holds only this.
func sourceTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// source is the public route BYO Hermes reads a file from.
//
// The path comes from the vault_files row, not from a client, so as in Swift it
// goes through neither vaultfiles.SanitizePath nor ResolveInside.
func (m *Module) source(c *echo.Context) error {
	ctx := c.Request().Context()
	token := c.Param("token")
	if swiftstr.Count(token) != 64 {
		return errSourceNotFound
	}
	hash := sourceTokenHash(token)
	item, err := m.q.ItemByTokenHash(ctx, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return errSourceNotFound
	}
	if err != nil {
		return err
	}
	if item.SourceTokenExpiresAt == nil || !item.SourceTokenExpiresAt.After(m.now()) || !item.VaultFileID.Valid {
		return errSourceNotFound
	}
	row, err := m.files.FindByID(ctx, item.TenantID, item.VaultFileID.Bytes)
	if err != nil {
		return err
	}
	if row == nil {
		return errSourceNotFound
	}
	target := m.paths.Raw(item.TenantID) + "/" + row.Path
	if _, err := os.Stat(target); err != nil {
		return echo.ErrNotFound // HTTPError(.notFound): no message, no body
	}
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	size := info.Size()

	// HEAD ignores Range. A header that is present but empty is still a header, and
	// parseRange refuses it; a refused range carries none of the file's headers.
	head := c.Request().Method == http.MethodHead
	raw, ranged := c.Request().Header["Range"]
	var first, last int64
	if ranged && !head {
		if first, last, err = parseRange(strings.Join(raw, ", "), size); err != nil {
			return err
		}
	}

	h := c.Response().Header()
	h.Set("Content-Type", row.ContentType)
	h.Set("Cache-Control", "private, no-store")
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", `"`+row.SHA256+`"`)
	name := "source"
	if item.FileName != nil {
		name = *item.FileName
	}
	h.Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	switch {
	case head:
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		c.Response().WriteHeader(http.StatusOK)
		return nil
	case ranged:
		h.Set("Content-Length", strconv.FormatInt(last-first+1, 10))
		h.Set("Content-Range", "bytes "+strconv.FormatInt(first, 10)+"-"+strconv.FormatInt(last, 10)+"/"+strconv.FormatInt(size, 10))
		return streamFile(c, target, http.StatusPartialContent, first, last-first+1)
	default:
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		return streamFile(c, target, http.StatusOK, 0, size)
	}
}

func streamFile(c *echo.Context, path string, status int, offset, length int64) error {
	f, err := os.Open(path) //nolint:gosec // vault_files.path under the tenant root, unsanitised as in Swift; see source.
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return echo.ErrNotFound
		}
		return err
	}
	defer func() { _ = f.Close() }()
	w := c.Response()
	w.WriteHeader(status)
	_, err = io.Copy(w, io.NewSectionReader(f, offset, length))
	return err
}

// parseRange is MultimodalIngestionController.parseRange: one range, inclusive,
// clamped to the file, or a bare 416. Int64() accepts a leading "+", as ParseInt
// does.
func parseRange(value string, size int64) (first, last int64, err error) {
	unsatisfiable := echo.NewHTTPError(http.StatusRequestedRangeNotSatisfiable, "")
	spec, ok := strings.CutPrefix(value, "bytes=")
	if size <= 0 || !ok || strings.Contains(value, ",") {
		return 0, 0, unsatisfiable
	}
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return 0, 0, unsatisfiable
	}
	if parts[0] == "" {
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, unsatisfiable
		}
		return max(0, size-suffix), size - 1, nil
	}
	first, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil || first < 0 || first >= size {
		return 0, 0, unsatisfiable
	}
	last = size - 1
	if parts[1] != "" {
		end, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			end = -1 // Int64(parts[1]) ?? -1
		}
		last = min(end, size-1)
	}
	if last < first {
		return 0, 0, unsatisfiable
	}
	return first, last, nil
}
