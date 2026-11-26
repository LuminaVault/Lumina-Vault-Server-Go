package vault

import (
	"context"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/vaultgit"
	"lumina/internal/wire"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// HeldPathError is the 409 a save or a move gets when its path cannot be a
// vault file because GitHub sync holds an entry of the connected repository
// in its way (spec 2026-11-19-e4-github-sync D8): GitHub has a link, a
// submodule or a file where the path needs a folder, or files inside what the
// path would make a file. Git cannot hold both under one name, so the file
// could never reach GitHub, and committed beside the entry it would break
// every later commit of the vault. The code is "path_held".
type HeldPathError struct {
	// Held is the entry in the way, as a path in the repository.
	Held string
}

func (e *HeldPathError) Error() string { return "path_held" }

// StatusCode is 409.
func (*HeldPathError) StatusCode() int { return http.StatusConflict }

// WriteResponse implements wire.ResponseError.
func (e *HeldPathError) WriteResponse(c *echo.Context) error {
	type body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	return wire.JSON(c, http.StatusConflict, struct {
		Error body `json:"error"`
	}{Error: body{
		Code:    "path_held",
		Message: "The connected GitHub repository has something at \"" + e.Held + "\" that LuminaVault cannot hold, so nothing can be saved there. Pick another name or folder.",
	}})
}

type syncWriteKey struct{}

// SyncContext marks ctx as GitHub sync's own pull. A pull writes what the
// remote tree it fetched has, and replaces the held list when it ends: until
// then the list on disk is the one of the tree before, and must not refuse
// the files of the new one.
func SyncContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, syncWriteKey{}, true)
}

// heldGuard refuses a path a held entry is in the way of (HeldPathError). A
// vault that never synced has no held list, and this is then one failed open
// of a file and nothing else. A list that cannot be read refuses nothing:
// saves do not depend on sync being well.
func (f *Files) heldGuard(ctx context.Context, tenant uuid.UUID, safe string) error {
	if ctx.Value(syncWriteKey{}) != nil {
		return nil
	}
	held, err := vaultgit.HeldAt(f.paths.Raw(tenant), safe)
	if err != nil {
		slog.WarnContext(ctx, "vault: held paths not read", "tenant", ids.Upper(tenant), "error", err)
		return nil
	}
	if held != "" {
		return &HeldPathError{Held: held}
	}
	return nil
}
