// Package profiles serves /v1/profiles: the Go port of LuminaVaultServer
// Sources/App/Profiles/ProfilesController.swift (HER-273 Hermes personas).
//
// Personas are rows in user_hermes_profiles and nothing else: no route here
// calls Hermes. "Activate" only moves the tenant's is_default flag; the chat
// path (HermesProfileMiddleware, still Swift) reads that flag later.
package profiles

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/profiles/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/modules/profiles/db"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

const maxSystemPromptBytes = 8 * 1024

// slugRe is Swift's `^[a-z0-9][a-z0-9-]{0,31}$` evaluated by ICU
// (NSRegularExpression), where `$` also matches before one line terminator at
// the very end of the input. So "abc\n" is a valid slug in Swift, and is
// stored with its newline.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}(?:\r\n|[\n\v\f\r\x{85}\x{2028}\x{2029}])?\z`)

// Swift's ErrorCode raw values, sent as the HTTPError message.
var (
	errInvalidSlug       = wire.NewError(http.StatusBadRequest, "invalid_slug")
	errInvalidLabel      = wire.NewError(http.StatusBadRequest, "invalid_label")
	errPromptTooLong     = wire.NewError(http.StatusBadRequest, "system_prompt_too_long")
	errTemplateNotFound  = wire.NewError(http.StatusBadRequest, "template_not_found")
	errSlugExists        = wire.NewError(http.StatusConflict, "slug_already_exists")
	errNotFound          = wire.NewError(http.StatusNotFound, "profile_not_found")
	errCannotDeleteDflt  = wire.NewError(http.StatusConflict, "cannot_delete_default")
	errSkillsNotAnArray  = wire.NewError(http.StatusBadRequest, "Type mismatch for `skillsEnabled` key, expected `Array<Any>` type.")
	skillsElemNotFound   = "Value not found for `skillsEnabled.%d` key."
	skillsElemNotAString = "Type mismatch for `skillsEnabled.%d` key, expected `String` type."
)

type Module struct {
	pool *pgxpool.Pool
	q    *db.Queries
	now  func() time.Time
}

func New(pool *pgxpool.Pool) *Module {
	return &Module{pool: pool, q: db.New(pool), now: time.Now}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	r.GET("/v1/profiles", m.list, authMW)
	r.POST("/v1/profiles", m.create, authMW)
	r.GET("/v1/profiles/:slug", m.get, authMW)
	r.PATCH("/v1/profiles/:slug", m.patch, authMW)
	r.DELETE("/v1/profiles/:slug", m.delete, authMW)
	r.POST("/v1/profiles/:slug/activate", m.activate, authMW)
}

// createRequest is HermesProfileCreateRequest with the fields in Swift's
// declaration order: the synthesized decoder reports the first failing key in
// that order (oapi-codegen's struct is alphabetical).
type createRequest struct {
	Slug          string           `json:"slug"`
	Label         string           `json:"label"`
	SystemPrompt  *string          `json:"systemPrompt,omitempty"`
	TemplateSlug  *string          `json:"templateSlug,omitempty"`
	SkillsEnabled *json.RawMessage `json:"skillsEnabled,omitempty"`
}

// patchRequest is HermesProfilePatchRequest, in declaration order.
type patchRequest struct {
	Label         *string          `json:"label,omitempty"`
	SystemPrompt  *string          `json:"systemPrompt,omitempty"`
	SkillsEnabled *json.RawMessage `json:"skillsEnabled,omitempty"`
}

func (m *Module) list(c *echo.Context) error {
	rows, err := m.q.ListProfiles(c.Request().Context(), auth.UserID(c))
	if err != nil {
		return err
	}
	now := m.now()
	out := HermesProfilesListResponse{Items: make([]HermesProfileDTO, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, toDTO(r, now))
		if r.IsDefault && out.ActiveSlug == nil {
			slug := r.Slug
			out.ActiveSlug = &slug
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) create(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	body, err := wire.Bind[createRequest](c)
	if err != nil {
		return err
	}
	skills, err := decodeSkills(body.SkillsEnabled)
	if err != nil {
		return err
	}
	if !slugRe.MatchString(body.Slug) {
		return errInvalidSlug
	}
	if err := validateLabel(body.Label); err != nil {
		return err
	}
	prompt, err := resolveSystemPrompt(body.SystemPrompt, body.TemplateSlug)
	if err != nil {
		return err
	}
	if _, err := m.q.GetProfile(ctx, db.GetProfileParams{TenantID: tenant, Slug: body.Slug}); err == nil {
		return errSlugExists
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// First profile becomes default automatically.
	n, err := m.q.CountProfiles(ctx, tenant)
	if err != nil {
		return err
	}
	if skills == nil {
		skills = []string{}
	}
	now := m.now()
	row := db.UserHermesProfile{
		ID: uuid.New(), TenantID: tenant, Slug: body.Slug, Label: body.Label, SystemPrompt: prompt,
		IsDefault: n == 0, SkillsEnabled: skills, CreatedAt: &now, UpdatedAt: &now,
	}
	if err := m.q.CreateProfile(ctx, db.CreateProfileParams{
		ID: row.ID, TenantID: tenant, Slug: row.Slug, Label: row.Label, SystemPrompt: row.SystemPrompt,
		IsDefault: row.IsDefault, SkillsEnabled: row.SkillsEnabled, CreatedAt: &now,
	}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(row, now))
}

func (m *Module) get(c *echo.Context) error {
	row, err := m.load(c.Request().Context(), auth.UserID(c), c.Param("slug"))
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(row, m.now()))
}

// patch mirrors Swift: decode, load (404), then validate and assign each
// non-nil field in order. Fluent's save writes (and stamps updated_at) only
// when at least one field was assigned, even to its current value.
func (m *Module) patch(c *echo.Context) error {
	ctx := c.Request().Context()
	slug := c.Param("slug")
	body, err := wire.Bind[patchRequest](c)
	if err != nil {
		return err
	}
	skills, err := decodeSkills(body.SkillsEnabled)
	if err != nil {
		return err
	}
	row, err := m.load(ctx, auth.UserID(c), slug)
	if err != nil {
		return err
	}
	changed := false
	if body.Label != nil {
		if err := validateLabel(*body.Label); err != nil {
			return err
		}
		row.Label, changed = *body.Label, true
	}
	if body.SystemPrompt != nil {
		if len(*body.SystemPrompt) > maxSystemPromptBytes {
			return errPromptTooLong
		}
		row.SystemPrompt, changed = *body.SystemPrompt, true
	}
	if body.SkillsEnabled != nil {
		row.SkillsEnabled, changed = skills, true
	}
	now := m.now()
	if changed {
		if err := m.q.UpdateProfile(ctx, db.UpdateProfileParams{
			ID: row.ID, Label: row.Label, SystemPrompt: row.SystemPrompt, SkillsEnabled: row.SkillsEnabled, UpdatedAt: &now,
		}); err != nil {
			return err
		}
		row.UpdatedAt = &now
	}
	return wire.JSON(c, http.StatusOK, toDTO(row, now))
}

func (m *Module) delete(c *echo.Context) error {
	ctx := c.Request().Context()
	row, err := m.load(ctx, auth.UserID(c), c.Param("slug"))
	if err != nil {
		return err
	}
	if row.IsDefault {
		return errCannotDeleteDflt
	}
	if err := m.q.DeleteProfile(ctx, row.ID); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// activate flips the default inside one transaction, as Swift does. An
// already-default target is a no-op that answers with its stored updated_at.
func (m *Module) activate(c *echo.Context) error {
	ctx, tenant, slug := c.Request().Context(), auth.UserID(c), c.Param("slug")
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := m.q.WithTx(tx)
	target, err := q.GetProfile(ctx, db.GetProfileParams{TenantID: tenant, Slug: slug})
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	now := m.now()
	activatedAt := now
	if target.IsDefault {
		if target.UpdatedAt != nil {
			activatedAt = *target.UpdatedAt
		}
	} else {
		if err := q.ClearDefault(ctx, db.ClearDefaultParams{TenantID: tenant, UpdatedAt: &now}); err != nil {
			return err
		}
		if err := q.SetDefault(ctx, db.SetDefaultParams{ID: target.ID, UpdatedAt: &now}); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, HermesProfileActivateResponse{Slug: slug, ActivatedAt: wire.Time{Time: activatedAt}})
}

func (m *Module) load(ctx context.Context, tenant uuid.UUID, slug string) (db.UserHermesProfile, error) {
	row, err := m.q.GetProfile(ctx, db.GetProfileParams{TenantID: tenant, Slug: slug})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, errNotFound
	}
	return row, err
}

// validateLabel: 1...80 Swift Characters after trimming whitespace and
// newlines. The label itself is stored untrimmed.
func validateLabel(label string) error {
	if n := swiftstr.Count(swiftstr.TrimWhitespacesAndNewlines(label)); n < 1 || n > 80 {
		return errInvalidLabel
	}
	return nil
}

// resolveSystemPrompt: a non-empty templateSlug wins over systemPrompt; an
// unknown template is an error even when systemPrompt is valid.
func resolveSystemPrompt(explicit, templateSlug *string) (string, error) {
	body := defaultSeed
	switch {
	case templateSlug != nil && *templateSlug != "":
		t, ok := builtInTemplates[*templateSlug]
		if !ok {
			return "", errTemplateNotFound
		}
		body = t
	case explicit != nil:
		body = *explicit
	}
	if len(body) > maxSystemPromptBytes {
		return "", errPromptTooLong
	}
	return body, nil
}

// decodeSkills decodes `skillsEnabled: [String]?` with Foundation's error
// wording, whose key path for an element is `skillsEnabled.<index>`
// (Hummingbird joins the coding path with dots).
func decodeSkills(raw *json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(*raw, &elems); err != nil {
		return nil, errSkillsNotAnArray
	}
	out := make([]string, len(elems))
	for i, e := range elems {
		if string(e) == "null" {
			return nil, wire.NewError(http.StatusBadRequest, fmt.Sprintf(skillsElemNotFound, i))
		}
		if err := json.Unmarshal(e, &out[i]); err != nil {
			return nil, wire.NewError(http.StatusBadRequest, fmt.Sprintf(skillsElemNotAString, i))
		}
	}
	return out, nil
}

func toDTO(r db.UserHermesProfile, now time.Time) HermesProfileDTO {
	skills := r.SkillsEnabled
	if skills == nil {
		skills = []string{}
	}
	return HermesProfileDTO{
		Id:            wire.UUID{UUID: r.ID},
		Slug:          r.Slug,
		Label:         r.Label,
		SystemPrompt:  r.SystemPrompt,
		IsDefault:     r.IsDefault,
		SkillsEnabled: skills,
		CreatedAt:     wire.Time{Time: orNow(r.CreatedAt, now)},
		UpdatedAt:     wire.Time{Time: orNow(r.UpdatedAt, now)},
	}
}

// orNow is Swift's `row.createdAt ?? Date()` for rows with NULL timestamps.
func orNow(t *time.Time, now time.Time) time.Time {
	if t == nil {
		return now
	}
	return *t
}
