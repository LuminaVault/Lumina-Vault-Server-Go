// Package onboarding serves /v1/onboarding: the Go port of LuminaVaultServer
// Sources/App/Onboarding/OnboardingController.swift (HER-93 resumable
// onboarding, guided-start dismissal).
//
// OnboardingLatches (the server-side firstCapture/firstMemoryCompile/
// firstQuery latches) is not ported: no route here calls it. Its callers
// (vault upload, link capture, memory compile, chat) port it with them.
package onboarding

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/onboarding/oapi-codegen.yaml api/openapi.yaml"

import (
	"context"
	"errors"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/httpserver"
	"lumina/internal/modules/onboarding/db"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
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
	r.GET("/v1/onboarding", m.get, authMW)
	r.PATCH("/v1/onboarding", m.patch, authMW)
}

// stateDTO is LuminaVaultShared's OnboardingStateDTO. The spec makes every
// flag optional, but Swift always encodes the seven Bools; only the nil
// dates are omitted. Field order is the Swift declaration order.
type stateDTO struct {
	SignupCompleted            bool       `json:"signupCompleted"`
	SignupCompletedAt          *wire.Time `json:"signupCompletedAt,omitempty"`
	EmailVerifiedCompleted     bool       `json:"emailVerifiedCompleted"`
	EmailVerifiedCompletedAt   *wire.Time `json:"emailVerifiedCompletedAt,omitempty"`
	SoulConfiguredCompleted    bool       `json:"soulConfiguredCompleted"`
	SoulConfiguredCompletedAt  *wire.Time `json:"soulConfiguredCompletedAt,omitempty"`
	FirstCaptureCompleted      bool       `json:"firstCaptureCompleted"`
	FirstCaptureCompletedAt    *wire.Time `json:"firstCaptureCompletedAt,omitempty"`
	FirstKBCompileCompleted    bool       `json:"firstKBCompileCompleted"`
	FirstKBCompileCompletedAt  *wire.Time `json:"firstKBCompileCompletedAt,omitempty"`
	FirstQueryCompleted        bool       `json:"firstQueryCompleted"`
	FirstQueryCompletedAt      *wire.Time `json:"firstQueryCompletedAt,omitempty"`
	BrainConfiguredCompleted   bool       `json:"brainConfiguredCompleted"`
	BrainConfiguredCompletedAt *wire.Time `json:"brainConfiguredCompletedAt,omitempty"`
	GuidedStartDismissedAt     *wire.Time `json:"guidedStartDismissedAt,omitempty"`
}

// patchRequest is OnboardingPatchRequest in Swift declaration order, which is
// the order Foundation reports the first type mismatch in.
type patchRequest struct {
	SignupCompleted          *bool `json:"signupCompleted,omitempty"`
	EmailVerifiedCompleted   *bool `json:"emailVerifiedCompleted,omitempty"`
	SoulConfiguredCompleted  *bool `json:"soulConfiguredCompleted,omitempty"`
	FirstCaptureCompleted    *bool `json:"firstCaptureCompleted,omitempty"`
	FirstKBCompileCompleted  *bool `json:"firstKBCompileCompleted,omitempty"`
	FirstQueryCompleted      *bool `json:"firstQueryCompleted,omitempty"`
	BrainConfiguredCompleted *bool `json:"brainConfiguredCompleted,omitempty"`
	GuidedStartDismissed     *bool `json:"guidedStartDismissed,omitempty"`
}

type state = db.GetOnboardingStateRow

func (m *Module) get(c *echo.Context) error {
	row, err := m.loadOrCreate(c.Request().Context(), auth.UserID(c))
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, toDTO(row))
}

func (m *Module) patch(c *echo.Context) error {
	ctx, tenant := c.Request().Context(), auth.UserID(c)
	body, err := wire.Bind[patchRequest](c)
	if err != nil {
		return err
	}
	// The seven latches reject false, in this order, before any row exists.
	for _, f := range []struct {
		v    *bool
		name string
	}{
		{body.SignupCompleted, "signupCompleted"},
		{body.EmailVerifiedCompleted, "emailVerifiedCompleted"},
		{body.SoulConfiguredCompleted, "soulConfiguredCompleted"},
		{body.FirstCaptureCompleted, "firstCaptureCompleted"},
		{body.FirstKBCompileCompleted, "firstKBCompileCompleted"},
		{body.FirstQueryCompleted, "firstQueryCompleted"},
		{body.BrainConfiguredCompleted, "brainConfiguredCompleted"},
	} {
		if f.v != nil && !*f.v {
			return wire.NewError(http.StatusBadRequest, f.name+": onboarding flags are one-way; only true is accepted")
		}
	}

	now := m.now()
	row, err := m.loadOrCreate(ctx, tenant)
	if err != nil {
		return err
	}
	// Fluent writes only the fields that were assigned (and updated_at when
	// any was), so a concurrent server-side latch on another column survives.
	set := map[string]any{}
	latch := func(want *bool, flag *bool, at **time.Time, col string) {
		if want != nil && *want && !*flag {
			*flag, *at = true, &now
			set[col+"_completed"], set[col+"_completed_at"] = true, now
		}
	}
	latch(body.SignupCompleted, &row.SignupCompleted, &row.SignupCompletedAt, "signup")
	latch(body.EmailVerifiedCompleted, &row.EmailVerifiedCompleted, &row.EmailVerifiedCompletedAt, "email_verified")
	latch(body.SoulConfiguredCompleted, &row.SoulConfiguredCompleted, &row.SoulConfiguredCompletedAt, "soul_configured")
	latch(body.FirstCaptureCompleted, &row.FirstCaptureCompleted, &row.FirstCaptureCompletedAt, "first_capture")
	if body.FirstKBCompileCompleted != nil && *body.FirstKBCompileCompleted && !row.FirstMemoryCompileCompleted {
		// HER-240: memory_compile is the source of truth; the legacy kb pair
		// is dual-written.
		row.FirstMemoryCompileCompleted, row.FirstMemoryCompileCompletedAt = true, &now
		row.FirstKbCompileCompleted, row.FirstKbCompileCompletedAt = true, &now
		set["first_memory_compile_completed"], set["first_memory_compile_completed_at"] = true, now
		set["first_kb_compile_completed"], set["first_kb_compile_completed_at"] = true, now
	}
	latch(body.FirstQueryCompleted, &row.FirstQueryCompleted, &row.FirstQueryCompletedAt, "first_query")
	latch(body.BrainConfiguredCompleted, &row.BrainConfiguredCompleted, &row.BrainConfiguredCompletedAt, "brain_configured")
	if body.GuidedStartDismissed != nil {
		switch {
		case *body.GuidedStartDismissed && row.GuidedStartDismissedAt == nil:
			row.GuidedStartDismissedAt = &now
			set["guided_start_dismissed_at"] = now
		case !*body.GuidedStartDismissed:
			// Assigned even when already nil: Fluent counts that as a change.
			row.GuidedStartDismissedAt = nil
			set["guided_start_dismissed_at"] = nil
		}
	}
	if len(set) > 0 {
		set["updated_at"] = now
		if err := m.update(ctx, row.ID, set); err != nil {
			return err
		}
	}
	return wire.JSON(c, http.StatusOK, toDTO(row))
}

// columns is the closed set update may name, in a fixed order.
var columns = []string{
	"signup_completed", "signup_completed_at",
	"email_verified_completed", "email_verified_completed_at",
	"soul_configured_completed", "soul_configured_completed_at",
	"first_capture_completed", "first_capture_completed_at",
	"first_kb_compile_completed", "first_kb_compile_completed_at",
	"first_query_completed", "first_query_completed_at",
	"first_memory_compile_completed", "first_memory_compile_completed_at",
	"brain_configured_completed", "brain_configured_completed_at",
	"guided_start_dismissed_at", "updated_at",
}

func (m *Module) update(ctx context.Context, id uuid.UUID, set map[string]any) error {
	var parts []string
	args := []any{id}
	for _, col := range columns {
		v, ok := set[col]
		if !ok {
			continue
		}
		args = append(args, v)
		parts = append(parts, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	// Column names come from the constant list above, never from input.
	_, err := m.pool.Exec(ctx, "UPDATE onboarding_state SET "+strings.Join(parts, ", ")+" WHERE id = $1", args...)
	return err
}

// loadOrCreate mirrors Swift: the tenant's row, or a new one with signup
// latched (OnboardingState(tenantID:signupCompleted: true)).
func (m *Module) loadOrCreate(ctx context.Context, tenant uuid.UUID) (state, error) {
	row, err := m.q.GetOnboardingState(ctx, tenant)
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return state{}, err
	}
	signupAt := m.now()
	created := signupAt
	row = state{ID: uuid.New(), TenantID: tenant, SignupCompleted: true, SignupCompletedAt: &signupAt}
	err = m.q.InsertOnboardingState(ctx, db.InsertOnboardingStateParams{
		ID: row.ID, TenantID: tenant, SignupCompletedAt: &signupAt, CreatedAt: &created,
	})
	return row, err
}

func at(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

func toDTO(r state) stateDTO {
	return stateDTO{
		SignupCompleted:            r.SignupCompleted,
		SignupCompletedAt:          at(r.SignupCompletedAt),
		EmailVerifiedCompleted:     r.EmailVerifiedCompleted,
		EmailVerifiedCompletedAt:   at(r.EmailVerifiedCompletedAt),
		SoulConfiguredCompleted:    r.SoulConfiguredCompleted,
		SoulConfiguredCompletedAt:  at(r.SoulConfiguredCompletedAt),
		FirstCaptureCompleted:      r.FirstCaptureCompleted,
		FirstCaptureCompletedAt:    at(r.FirstCaptureCompletedAt),
		FirstKBCompileCompleted:    r.FirstMemoryCompileCompleted,
		FirstKBCompileCompletedAt:  at(r.FirstMemoryCompileCompletedAt),
		FirstQueryCompleted:        r.FirstQueryCompleted,
		FirstQueryCompletedAt:      at(r.FirstQueryCompletedAt),
		BrainConfiguredCompleted:   r.BrainConfiguredCompleted,
		BrainConfiguredCompletedAt: at(r.BrainConfiguredCompletedAt),
		GuidedStartDismissedAt:     at(r.GuidedStartDismissedAt),
	}
}
