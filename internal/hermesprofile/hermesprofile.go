// Package hermesprofile is the Go port of Swift's HermesProfileMiddleware and the
// parts of HermesProfileService it calls (HER-273): which persona a chat request runs
// as, and the X-Hermes-Session-Key that keeps each persona's memory apart on one
// Hermes container.
//
// It writes on the request path, which isn't obvious from the middleware's name. A
// tenant with no persona row gets one. A tenant whose hermes_profiles row isn't ready
// is provisioned on the spot: the row, the tenant's vault directories and, with the
// filesystem gateway, profile.json on the volume Hermes mounts. A failure there
// degrades the request rather than failing it.
//
// Personas themselves are served by modules/profiles.
package hermesprofile

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultpaths"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const (
	ctxSlug       = "lv.hermes.activeProfileSlug"
	ctxSessionKey = "lv.hermes.activeSessionKey"

	statusProvisioning = "provisioning"
	statusReady        = "ready"
	statusError        = "error"

	// DefaultSeedPrompt is HermesProfileService.defaultSeedPrompt.
	DefaultSeedPrompt = `You are Hermes — a personal assistant. Tone: warm, concise.

## Behavior
- Save every link mentioned in chat to the user's vault without asking first — link capture is part of the vault contract, whatever the autonomy setting. Confirm the save in your reply with the destination filename.`
)

// ErrUnknownProfile is the 404 for an X-Hermes-Profile with no such persona.
var ErrUnknownProfile = wire.NewError(http.StatusNotFound, "unknown_profile")

// Service is HermesProfileService.
type Service struct {
	pool    *pgxpool.Pool
	gateway Gateway
	paths   vaultpaths.Paths
	now     func() time.Time
}

// New returns a Service. Build gateway with NewGateway.
func New(pool *pgxpool.Pool, gateway Gateway, paths vaultpaths.Paths) *Service {
	return &Service{pool: pool, gateway: gateway, paths: paths, now: time.Now}
}

// Profile is the hermes_profiles row: the tenant's one Hermes container slot.
type Profile struct {
	ID              uuid.UUID
	HermesProfileID string
	Status          string
	LastError       *string
}

// SessionKey returns the key Middleware composed, if it ran for an identified caller.
func SessionKey(c *echo.Context) (string, bool) {
	key, ok := c.Get(ctxSessionKey).(string)
	return key, ok
}

// ActiveSlug returns the persona slug Middleware resolved, if it ran for an
// identified caller.
func ActiveSlug(c *echo.Context) (string, bool) {
	slug, ok := c.Get(ctxSlug).(string)
	return slug, ok
}

// Middleware is HermesProfileMiddleware. Mount it after auth and after
// hermesresolve's middleware. With no identity it does nothing and leaves the 401 to
// what comes next.
//
// The session key is "<hermesProfileID>:<slug>" for whatever hermes_profiles row
// exists, whatever its status: a row whose provisioning failed still gives
// "pending-<UPPERCASE TENANT>:<slug>" from its sentinel id. Only a tenant left with no
// row at all takes the same shape from the fallback, which Hermes treats as a fresh
// session.
func (s *Service) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			tenant, ok := auth.Identity(c)
			if !ok {
				return next(c)
			}
			ctx := c.Request().Context()
			profile, err := s.EnsureSoftAndFind(ctx, tenant)
			if err != nil {
				return err
			}
			slug, err := s.resolveSlug(c, tenant)
			if err != nil {
				return err
			}
			key := "pending-" + ids.Upper(tenant) + ":" + slug
			if profile != nil {
				key = profile.HermesProfileID + ":" + slug
			}
			c.Set(ctxSlug, slug)
			c.Set(ctxSessionKey, key)
			return next(c)
		}
	}
}

func (s *Service) resolveSlug(c *echo.Context, tenant uuid.UUID) (string, error) {
	ctx := c.Request().Context()
	raw, _ := wire.HeaderValue(c.Request(), "X-Hermes-Profile")
	slug := strings.ToLower(swiftstr.TrimWhitespacesAndNewlines(raw))
	if slug == "" {
		return s.EnsureDefaultPersona(ctx, tenant)
	}
	var found string
	err := s.pool.QueryRow(ctx, `SELECT slug FROM user_hermes_profiles WHERE tenant_id = $1 AND slug = $2 LIMIT 1`, tenant, slug).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnknownProfile
	}
	return found, err
}

// EnsureDefaultPersona is ensureDefaultPersona and returns the slug of the tenant's
// default persona. With no default row it promotes the oldest persona or seeds
// "default", in a transaction. Two first touches at once can both reach the insert;
// the partial unique index then fails one with an error, a 500, as in Swift.
func (s *Service) EnsureDefaultPersona(ctx context.Context, tenant uuid.UUID) (string, error) {
	var slug string
	err := s.pool.QueryRow(ctx, `SELECT slug FROM user_hermes_profiles WHERE tenant_id = $1 AND is_default = true LIMIT 1`, tenant).Scan(&slug)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return slug, err
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.now()
		var id uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id, slug FROM user_hermes_profiles WHERE tenant_id = $1 ORDER BY created_at ASC LIMIT 1`, tenant).Scan(&id, &slug)
		switch {
		case err == nil:
			_, err = tx.Exec(ctx, `UPDATE user_hermes_profiles SET is_default = true, updated_at = $2 WHERE id = $1`, id, now)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		slug = "default"
		_, err = tx.Exec(ctx, `INSERT INTO user_hermes_profiles (id, tenant_id, slug, label, system_prompt, is_default, skills_enabled, created_at, updated_at)
			VALUES ($1, $2, $3, 'Default', $4, true, '{}', $5, $5)`, uuid.New(), tenant, slug, DefaultSeedPrompt, now)
		return err
	})
	return slug, err
}

// EnsureSoftAndFind is ensureSoftAndFind: a ready row as is, otherwise one attempt at
// provisioning, then whatever row there is, or nil. A failed attempt is logged and
// isn't an error; only a failed read is.
func (s *Service) EnsureSoftAndFind(ctx context.Context, tenant uuid.UUID) (*Profile, error) {
	existing, err := s.Find(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.Status == statusReady {
		return existing, nil
	}
	if err := s.Ensure(ctx, tenant); err != nil {
		slog.WarnContext(ctx, "hermes profile provisioning degraded", "tenantID", ids.Upper(tenant), "error", err)
	}
	return s.Find(ctx, tenant)
}

// Ensure is HermesProfileService.ensure(for:): reserve the row before calling the gateway,
// so a hang or a crash leaves something inspectable for the reconciler, then flip it
// to ready, or to error with last_error.
func (s *Service) Ensure(ctx context.Context, tenant uuid.UUID) error {
	row, err := s.Find(ctx, tenant)
	if err != nil {
		return err
	}
	if row != nil && row.Status == statusReady {
		return nil
	}
	id, err := s.reserve(ctx, tenant, row)
	if err != nil {
		return err
	}
	hermesID, err := s.provision(ctx, tenant)
	if err != nil {
		// Swift's try?: the row keeps saying provisioning if this fails too.
		if _, saveErr := s.pool.Exec(ctx, `UPDATE hermes_profiles SET status = $2, last_error = $3, updated_at = $4 WHERE id = $1`,
			id, statusError, err.Error(), s.now()); saveErr != nil {
			slog.WarnContext(ctx, "hermes profile error not saved", "tenantID", ids.Upper(tenant), "error", saveErr)
		}
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE hermes_profiles SET hermes_profile_id = $2, status = $3, last_error = NULL, updated_at = $4 WHERE id = $1`,
		id, hermesID, statusReady, s.now())
	return err
}

// reserve is reserveProvisioningRow. Fluent writes nothing when no field changed, so
// neither does this. The sentinel id can't collide, being unique per tenant.
func (s *Service) reserve(ctx context.Context, tenant uuid.UUID, row *Profile) (uuid.UUID, error) {
	if row != nil {
		if row.Status == statusProvisioning && row.LastError == nil {
			return row.ID, nil
		}
		_, err := s.pool.Exec(ctx, `UPDATE hermes_profiles SET status = $2, last_error = NULL, updated_at = $3 WHERE id = $1`,
			row.ID, statusProvisioning, s.now())
		return row.ID, err
	}
	id, now := uuid.New(), s.now()
	_, err := s.pool.Exec(ctx, `INSERT INTO hermes_profiles (id, tenant_id, hermes_profile_id, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)`, id, tenant, "pending-"+ids.Upper(tenant), statusProvisioning, now)
	return id, err
}

func (s *Service) provision(ctx context.Context, tenant uuid.UUID) (string, error) {
	if err := s.paths.EnsureTenantDirectories(tenant); err != nil {
		return "", err
	}
	var username string
	if err := s.pool.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, tenant).Scan(&username); err != nil {
		return "", err
	}
	return s.gateway.ProvisionProfile(ctx, tenant, username)
}

// Find is HermesProfileService.find(for:): the tenant's row, or nil.
func (s *Service) Find(ctx context.Context, tenant uuid.UUID) (*Profile, error) {
	var p Profile
	err := s.pool.QueryRow(ctx, `SELECT id, hermes_profile_id, status, last_error FROM hermes_profiles WHERE tenant_id = $1 LIMIT 1`, tenant).
		Scan(&p.ID, &p.HermesProfileID, &p.Status, &p.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
