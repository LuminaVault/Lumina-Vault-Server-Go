// Package onboardlatch is the Go port of LuminaVaultServer's OnboardingLatches
// (Sources/App/Onboarding/OnboardingLatches.swift): the server-side flip of the
// guided-start completion flags, as a side effect of the real work.
//
// Every call is best effort. Latching is bookkeeping for a progress card and
// must never turn a successful upload or compile into a failed request, so
// Latch has no error to return. It is awaited rather than detached because the
// card polls GET /v1/onboarding right after the action completes.
package onboardlatch

import (
	"context"
	"log/slog"
	"lumina/internal/ids"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Latch names the step a flag completes, not the caller, so a second capture
// surface latches the same one. The values are Swift's raw values.
type Latch string

const (
	FirstCapture       Latch = "firstCapture"
	FirstMemoryCompile Latch = "firstMemoryCompile"
	FirstQuery         Latch = "firstQuery"
)

// timeout bounds a latch on a busy pool. Swift has no bound; here a stuck pool
// loses a progress flag instead of holding up a request that already succeeded.
const timeout = 5 * time.Second

// One statement per latch: a read-then-write would spend a round trip per
// request forever to learn nothing, and race the card's polling. The statements
// are Swift's, verbatim. New rows have signup latched, because reaching a call
// site means the account exists; the conflict branch writes only latch columns
// (a concurrent PATCH can't be clobbered) and keeps the first timestamp.
const (
	upsertFirstCapture = `INSERT INTO onboarding_state
    (id, tenant_id, signup_completed, signup_completed_at,
     first_capture_completed, first_capture_completed_at,
     created_at, updated_at)
VALUES ($1, $2, TRUE, NOW(), TRUE, NOW(), NOW(), NOW())
ON CONFLICT (tenant_id) DO UPDATE
   SET first_capture_completed = TRUE,
       first_capture_completed_at = NOW(),
       updated_at = NOW()
 WHERE NOT onboarding_state.first_capture_completed`

	upsertFirstQuery = `INSERT INTO onboarding_state
    (id, tenant_id, signup_completed, signup_completed_at,
     first_query_completed, first_query_completed_at,
     created_at, updated_at)
VALUES ($1, $2, TRUE, NOW(), TRUE, NOW(), NOW(), NOW())
ON CONFLICT (tenant_id) DO UPDATE
   SET first_query_completed = TRUE,
       first_query_completed_at = NOW(),
       updated_at = NOW()
 WHERE NOT onboarding_state.first_query_completed`

	// memory_compile is the source of truth; the legacy kb_compile pair is
	// dual-written. The conflict branch fires when either pair is unset, which
	// self-heals a row stranded by the pre-M130 bug, and the COALESCE chain
	// keeps the date the user actually compiled on.
	upsertFirstMemoryCompile = `INSERT INTO onboarding_state
    (id, tenant_id, signup_completed, signup_completed_at,
     first_memory_compile_completed, first_memory_compile_completed_at,
     first_kb_compile_completed, first_kb_compile_completed_at,
     created_at, updated_at)
VALUES ($1, $2, TRUE, NOW(),
        TRUE, NOW(), TRUE, NOW(), NOW(), NOW())
ON CONFLICT (tenant_id) DO UPDATE
   SET first_memory_compile_completed = TRUE,
       first_memory_compile_completed_at = COALESCE(
           onboarding_state.first_memory_compile_completed_at,
           onboarding_state.first_kb_compile_completed_at,
           NOW()),
       first_kb_compile_completed = TRUE,
       first_kb_compile_completed_at = COALESCE(
           onboarding_state.first_kb_compile_completed_at,
           NOW()),
       updated_at = NOW()
 WHERE NOT (onboarding_state.first_memory_compile_completed
            AND onboarding_state.first_kb_compile_completed)`
)

// Latches writes the flags. A nil *Latches does nothing.
type Latches struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Latches {
	return &Latches{pool: pool}
}

// Latch idempotently flips latch for tenant and stamps its timestamp. Errors
// are logged and swallowed. A client that hangs up after its upload succeeded
// doesn't cancel it, because the work it records did happen.
func (l *Latches) Latch(ctx context.Context, latch Latch, tenant uuid.UUID) {
	if l == nil {
		return
	}
	var stmt string
	switch latch {
	case FirstCapture:
		stmt = upsertFirstCapture
	case FirstMemoryCompile:
		stmt = upsertFirstMemoryCompile
	case FirstQuery:
		stmt = upsertFirstQuery
	default:
		slog.WarnContext(ctx, "onboarding latch skipped: unknown latch", "latch", string(latch))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if _, err := l.pool.Exec(ctx, stmt, uuid.New(), tenant); err != nil {
		slog.WarnContext(ctx, "onboarding latch failed", "latch", string(latch), "tenant", ids.Upper(tenant), "error", err)
	}
}
