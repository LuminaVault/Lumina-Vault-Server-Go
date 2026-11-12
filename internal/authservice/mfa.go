package authservice

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MFA is DefaultMFAService: email OTP challenges in mfa_challenges.
type MFA struct {
	pool   *pgxpool.Pool
	sender Sender
	codes  CodeGenerator
	now    func() time.Time
}

const (
	// mfaChallengeLifetime is DefaultMFAService.challengeLifetime.
	mfaChallengeLifetime = 5 * time.Minute
	// mfaMaxFailedAttempts is DefaultMFAService.maxFailedAttempts.
	mfaMaxFailedAttempts = 5
)

// NewMFA returns the MFA service. now defaults to time.Now.
func NewMFA(pool *pgxpool.Pool, sender Sender, codes CodeGenerator, now func() time.Time) *MFA {
	if now == nil {
		now = time.Now
	}
	return &MFA{pool: pool, sender: sender, codes: codes, now: now}
}

// Issue is DefaultMFAService.issue: a new challenge row, then the email. The
// row stays when the send fails.
func (m *MFA) Issue(ctx context.Context, u *User, purpose string) (uuid.UUID, error) {
	code := m.codes.Generate()
	id, now := uuid.New(), m.now()
	_, err := m.pool.Exec(ctx, `INSERT INTO mfa_challenges
		(id, tenant_id, purpose, channel, destination, code_hash, expires_at, consumed_at, failed_attempts, resend_count, last_sent_at, created_at)
		VALUES ($1, $2, $3, 'email', $4, $5, $6, NULL, 0, 0, $7, $7)`,
		id, u.ID, purpose, u.Email, sha256Hex(code), now.Add(mfaChallengeLifetime), now)
	if err != nil {
		return uuid.Nil, err
	}
	if err := m.sender.Send(ctx, code, u.Email, purpose); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

type challenge struct {
	tenant     uuid.UUID
	codeHash   string
	expiresAt  time.Time
	consumedAt *time.Time
	failed     int64
}

func (m *MFA) find(ctx context.Context, id uuid.UUID) (*challenge, error) {
	var c challenge
	err := m.pool.QueryRow(ctx, `SELECT tenant_id, code_hash, expires_at, consumed_at, failed_attempts FROM mfa_challenges WHERE id = $1`, id).
		Scan(&c.tenant, &c.codeHash, &c.expiresAt, &c.consumedAt, &c.failed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Verify is DefaultMFAService.verify: false for an unknown, consumed, expired
// or exhausted challenge; a wrong code counts a failure and the fifth burns the
// challenge. Swift weakness, kept: the hashes are compared with ==, not in
// constant time, and nothing ties the challenge to the client that started it.
func (m *MFA) Verify(ctx context.Context, id uuid.UUID, code string) (bool, error) {
	c, err := m.find(ctx, id)
	if err != nil || c == nil {
		return false, err
	}
	now := m.now()
	if c.consumedAt != nil || c.expiresAt.Before(now) || c.failed >= mfaMaxFailedAttempts {
		return false, nil
	}
	if c.codeHash == sha256Hex(code) {
		_, err := m.pool.Exec(ctx, `UPDATE mfa_challenges SET consumed_at = $2 WHERE id = $1`, id, now)
		return err == nil, err
	}
	failed := c.failed + 1
	var burnt *time.Time
	if failed >= mfaMaxFailedAttempts {
		burnt = &now
	}
	// Fluent writes the incremented count it read, so concurrent failures can
	// lose one, as in Swift.
	_, err = m.pool.Exec(ctx, `UPDATE mfa_challenges SET failed_attempts = $2, consumed_at = COALESCE($3, consumed_at) WHERE id = $1`,
		id, failed, burnt)
	return false, err
}
