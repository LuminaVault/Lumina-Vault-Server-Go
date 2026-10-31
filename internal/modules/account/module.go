// Package account serves DELETE /v1/account (HER-92, GDPR erasure): the Go
// port of LuminaVaultServer Sources/App/Account/AccountController.swift,
// AccountDeletionService.swift and TenantDataEraser.swift.
//
// DeletedDataPurgeService (the daily `rm -r` of soft-deleted folders) stays in
// Swift (jobs.md). This module only produces what it purges: the tenant vault
// and Hermes profile folders renamed to `_deleted_<unix seconds>_<id>`.
package account

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/account/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/modules/account/db"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/bcrypt"
)

// freshAuthWindow is how old a token may be to stand in for the password.
const freshAuthWindow = 5 * time.Minute

// maxBody mirrors Hummingbird's default request body limit (2 MiB).
const maxBody = 2 << 20

var (
	errPasswordIncorrect = wire.NewError(http.StatusUnauthorized, "password incorrect")
	errReAuthRequired    = wire.NewError(http.StatusUnauthorized,
		fmt.Sprintf("account deletion requires password or a JWT issued within %d seconds", int(freshAuthWindow.Seconds())))
)

type Module struct {
	pool           *pgxpool.Pool
	q              *db.Queries
	keys           auth.Keyset
	vault          vaultpaths.Paths
	hermesDataRoot string
	now            func() time.Time
}

func New(pool *pgxpool.Pool, keys auth.Keyset, vault vaultpaths.Paths, hermesDataRoot string) *Module {
	return &Module{pool: pool, q: db.New(pool), keys: keys, vault: vault, hermesDataRoot: hermesDataRoot, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.DELETE("/v1/account", m.delete, authMW)
}

func (m *Module) delete(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	password := decodePassword(c.Request().Body)
	iat := m.tokenIssuedAt(c.Request().Header.Get("Authorization"))
	now := m.now()

	if err := m.verifyReAuth(ctx, tenant, password, iat, now); err != nil {
		return err
	}

	// Read before the cascade wipes the row: it names the Hermes folder.
	hpid, err := m.q.HermesProfileID(ctx, tenant)
	hasProfile := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	if err := pgx.BeginFunc(ctx, m.pool, func(tx pgx.Tx) error {
		q := m.q.WithTx(tx)
		if err := Erase(ctx, q, tenant); err != nil {
			return err
		}
		return q.DeleteUser(ctx, tenant)
	}); err != nil {
		return err
	}

	suffix := DirectoryName(tenant, now)
	m.softRename(ctx, m.vault.TenantRoot(tenant), suffix)
	if hasProfile && IsSafeProfileName(hpid) {
		m.softRename(ctx, filepath.Join(m.hermesDataRoot, "profiles", hpid), suffix)
	}
	// PII-safe audit: the tenant id only.
	slog.InfoContext(ctx, "account.deleted", "tenantID", ids.Upper(tenant))
	return c.NoContent(http.StatusNoContent)
}

// decodePassword is decodeBodyAllowingEmpty: any body that does not decode as
// AccountDeleteRequest (empty, malformed, wrong type) means "no password".
func decodePassword(body io.Reader) *string {
	raw, err := io.ReadAll(io.LimitReader(body, maxBody))
	if err != nil {
		return nil
	}
	// A map, not a struct: encoding/json matches keys case-insensitively and
	// Foundation does not ("PASSWORD" is no password in Swift).
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var password *string
	if v, ok := obj["password"]; !ok || json.Unmarshal(v, &password) != nil {
		return nil
	}
	return password
}

// tokenIssuedAt re-verifies the bearer token for its iat; any failure means
// the fresh-JWT path is unavailable.
func (m *Module) tokenIssuedAt(header string) *time.Time {
	claims, err := auth.Verify(m.keys, header)
	if err != nil || claims.IssuedAt == nil {
		return nil
	}
	t := claims.IssuedAt.Time
	return &t
}

func (m *Module) verifyReAuth(ctx context.Context, tenant uuid.UUID, password *string, iat *time.Time, now time.Time) error {
	if password != nil && *password != "" {
		hash, err := m.q.UserPasswordHash(ctx, tenant)
		if err != nil {
			return err
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(*password)) != nil {
			return errPasswordIncorrect
		}
		return nil
	}
	if iat != nil {
		if age := now.Sub(*iat); age <= freshAuthWindow && age >= 0 {
			return nil
		}
	}
	return errReAuthRequired
}

// softRename moves path to a sibling named siblingName. Best effort, as in
// Swift: the rows are already gone, so a failure is logged (it needs an
// operator: the purge job never touches a live-named folder), not returned.
func (m *Module) softRename(ctx context.Context, path, siblingName string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	target := filepath.Join(filepath.Dir(path), siblingName)
	err := error(nil)
	if _, statErr := os.Lstat(target); statErr == nil {
		// FileManager.moveItem refuses an existing destination; os.Rename
		// would replace an empty directory.
		err = fmt.Errorf("%s already exists", target)
	} else {
		err = os.Rename(path, target)
	}
	if err != nil {
		slog.ErrorContext(ctx, "account.deletion.rename_failed", "path", path, "error", err)
	}
}

// DirectoryName is DeletedDataPurgeJob.directoryName: `_deleted_<unix
// seconds>_<UPPERCASE tenant UUID>`, for both the vault and the Hermes folder.
func DirectoryName(tenant uuid.UUID, deletedAt time.Time) string {
	return fmt.Sprintf("_deleted_%d_%s", deletedAt.Unix(), ids.Upper(tenant))
}

// IsSafeProfileName is DeletedDataPurgeJob.isSafeProfileName: ASCII letters,
// digits, ".", "_" and "-", at most 128, never "." or "..".
func IsSafeProfileName(name string) bool {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return false
	}
	allowed := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
	}
	for _, r := range name {
		if !allowed(r) {
			return false
		}
	}
	return true
}
