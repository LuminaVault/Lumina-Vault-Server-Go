package authservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"lumina/internal/auth"
	"lumina/internal/hermesprofile"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuthService's constants.
const (
	maxFailedLogins       = 5
	lockoutDuration       = 15 * time.Minute
	minPasswordLength     = 12
	resetCodeLifetime     = 15 * time.Minute
	maxResetFailures      = 5
	resetLockoutDuration  = 30 * time.Minute
	verifyCodeLifetime    = 24 * time.Hour
	maxVerifyFailures     = 5
	verifyLockoutDuration = 30 * time.Minute
	// trialDays is applyTrialDefaults' default.
	trialDays = 14
)

// Profiles is the part of hermesprofile.Service issueTokens and register use.
type Profiles interface {
	Ensure(ctx context.Context, tenant uuid.UUID) error
	Find(ctx context.Context, tenant uuid.UUID) (*hermesprofile.Profile, error)
}

// Analytics is PostHogAnalytics.capture (*posthog.Client; nil-safe).
type Analytics interface {
	Capture(name, distinctID string, props map[string]any)
}

// Options are DefaultAuthService's collaborators.
type Options struct {
	Keys     auth.Keyset
	Profiles Profiles
	// Analytics receives issueTokens' `$set is_internal`. Nil sends nothing.
	Analytics Analytics
	// TierOverride is TierOverrideAllowlist.override(forEmail:)
	// (billing.TierOverrides.Override). Nil is the empty allowlist.
	TierOverride func(email string) (string, bool)
	// MFASender, ResetSender and VerifySender are the three EmailOTPSenders.
	MFASender, ResetSender, VerifySender Sender
	// MFACodes, ResetCodes and VerifyCodes default to DefaultCodeGenerator.
	MFACodes, ResetCodes, VerifyCodes CodeGenerator
	// PasswordCost defaults to auth.PasswordCost (12); tests lower it.
	PasswordCost int
	Now          func() time.Time
	Log          *slog.Logger
}

// Service is DefaultAuthService.
type Service struct {
	pool *pgxpool.Pool
	opt  Options
	mfa  *MFA
}

// New returns the service. Senders default to the logging sender.
func New(pool *pgxpool.Pool, opt Options) *Service {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Log == nil {
		opt.Log = slog.Default().With("label", "lv.auth")
	}
	if opt.PasswordCost == 0 {
		opt.PasswordCost = auth.PasswordCost
	}
	for _, s := range []*Sender{&opt.MFASender, &opt.ResetSender, &opt.VerifySender} {
		if *s == nil {
			*s = LoggingSender{Log: opt.Log}
		}
	}
	for _, g := range []*CodeGenerator{&opt.MFACodes, &opt.ResetCodes, &opt.VerifyCodes} {
		if *g == nil {
			*g = DefaultCodeGenerator{}
		}
	}
	return &Service{pool: pool, opt: opt, mfa: NewMFA(pool, opt.MFASender, opt.MFACodes, opt.Now)}
}

// User is the users row as AuthService reads it.
type User struct {
	ID                  uuid.UUID
	Email               string
	Username            string
	PasswordHash        string
	IsVerified          bool
	FailedLoginAttempts int64
	LockoutUntil        *time.Time
	TierOverride        string
	VaultInitialized    bool
}

const userColumns = `id, email, username, password_hash, is_verified, failed_login_attempts, lockout_until, tier_override, vault_initialized`

func (s *Service) findUser(ctx context.Context, where string, arg any) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE `+where+` LIMIT 1`, arg).
		Scan(&u.ID, &u.Email, &u.Username, &u.PasswordHash, &u.IsVerified, &u.FailedLoginAttempts, &u.LockoutUntil, &u.TierOverride, &u.VaultInitialized)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// FindUserByEmail is DatabaseAuthRepository.findUser(byEmail:): an exact match
// on the lowercased email, nil when there is none.
func (s *Service) FindUserByEmail(ctx context.Context, email string) (*User, error) {
	return s.findUser(ctx, `email = $1`, swiftstr.Lowercased(email))
}

// FindUserByUsername is findUser(byUsername:).
func (s *Service) FindUserByUsername(ctx context.Context, username string) (*User, error) {
	return s.findUser(ctx, `username = $1`, swiftstr.Lowercased(username))
}

// FindUserByID is findUser(byID:).
func (s *Service) FindUserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return s.findUser(ctx, `id = $1`, id)
}

// CreateUser is DatabaseAuthRepository.createUser followed by
// applyTrialDefaults and ensurePersonalVault, as register and the OAuth
// upsert run them: the User init's defaults (email lowercased, username
// trimmed of whitespace and lowercased), a 14-day trial, and the personal vault
// whose id is the user's (M90).
func (s *Service) CreateUser(ctx context.Context, email, username, passwordHash string) (*User, error) {
	u := &User{
		ID: uuid.New(), Email: swiftstr.Lowercased(email),
		Username:     swiftstr.Lowercased(trimWhitespaces(username)),
		PasswordHash: passwordHash, TierOverride: "none",
	}
	now := s.opt.Now()
	if _, err := s.pool.Exec(ctx, `INSERT INTO users (id, email, username, password_hash, is_verified, failed_login_attempts,
		lockout_until, tier, tier_expires_at, tier_override, revenuecat_user_id, context_routing, privacy_no_cn_origin, timezone,
		is_admin, vault_initialized, auto_save_links, mnemosyne_enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, false, 0, NULL, 'trial', NULL, 'none', NULL, false, false, 'UTC', false, false, true, true, $5, $5)`,
		u.ID, u.Email, u.Username, u.PasswordHash, now); err != nil {
		return nil, err
	}
	// applyTrialDefaults: its own save, as in Swift.
	trialNow := s.opt.Now()
	if _, err := s.pool.Exec(ctx, `UPDATE users SET tier = 'trial', tier_expires_at = $2, updated_at = $3 WHERE id = $1`,
		u.ID, trialNow.Add(trialDays*24*time.Hour), trialNow); err != nil {
		return nil, err
	}
	if err := s.ensurePersonalVault(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// trimWhitespaces is `trimmingCharacters(in: .whitespaces)` for User.init.
func trimWhitespaces(s string) string { return swiftstr.TrimWhitespaces(s) }

// ensurePersonalVault is DefaultAuthService.ensurePersonalVault.
func (s *Service) ensurePersonalVault(ctx context.Context, u *User) error {
	var available bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.vaults') IS NOT NULL AS available`).Scan(&available); err != nil {
		return err
	}
	if !available {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at)
			VALUES ($1, $1, $2, NOW(), NOW()) ON CONFLICT (id) DO NOTHING`, u.ID, u.Username+"'s Vault"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO vault_memberships
			(id, vault_id, user_id, role, can_use_ai, created_by_user_id, created_at, updated_at)
			VALUES (gen_random_uuid(), $1, $1, 'admin', TRUE, $1, NOW(), NOW())
			ON CONFLICT (vault_id, user_id) DO NOTHING`, u.ID)
		return err
	})
}

// EnsureSoft is HermesProfileService.ensureSoft: provisioning failures are
// logged and never fail the caller (HER-29).
func (s *Service) EnsureSoft(ctx context.Context, u *User) {
	if s.opt.Profiles == nil {
		return
	}
	if err := s.opt.Profiles.Ensure(ctx, u.ID); err != nil {
		s.opt.Log.Warn(fmt.Sprintf("hermes profile provisioning degraded for %s: %v", u.Username, err))
	}
}

// Register is DefaultAuthService.register. Swift weakness, kept: "email
// already registered" and "username already taken" let anyone probe which
// accounts exist; only registerByIP limits it.
func (s *Service) Register(ctx context.Context, email, rawUsername, password string) (*AuthResponse, error) {
	if swiftstr.CharacterCount(password) < minPasswordLength {
		return nil, ErrWeakPassword
	}
	username, err := ValidateUsername(rawUsername)
	if err != nil {
		return nil, err
	}
	if existing, err := s.FindUserByEmail(ctx, email); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrEmailExists
	}
	if existing, err := s.FindUserByUsername(ctx, username); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrUsernameTaken
	}
	hash, err := auth.HashPassword(password, s.opt.PasswordCost)
	if err != nil {
		return nil, err
	}
	// A concurrent register of the same email or username loses on the unique
	// index: a 500, as Fluent's error is in Swift.
	u, err := s.CreateUser(ctx, email, username, hash)
	if err != nil {
		return nil, err
	}
	s.EnsureSoft(ctx, u)
	return s.IssueTokens(ctx, u)
}

// Login is DefaultAuthService.login. requireMFA is the mfa-auth-v1 header.
//
// Swift weaknesses, kept: an unknown email answers at once while a known one
// pays for bcrypt (a timing oracle), and a locked account answers 423 before
// the password is checked, which confirms the account exists. The lockout
// counter is not reset when a lock expires, so the next failure locks again.
func (s *Service) Login(ctx context.Context, email, password string, requireMFA bool) (*AuthResponse, error) {
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalidCredentials
	}
	if u.LockoutUntil != nil && u.LockoutUntil.After(s.opt.Now()) {
		return nil, ErrAccountLocked
	}
	if !auth.VerifyPassword(password, u.PasswordHash) {
		var lockoutAt *time.Time
		if u.FailedLoginAttempts+1 >= maxFailedLogins {
			t := s.opt.Now().Add(lockoutDuration)
			lockoutAt = &t
		}
		if err := s.incrementFailedLogin(ctx, u.ID, lockoutAt); err != nil {
			return nil, err
		}
		return nil, ErrInvalidCredentials
	}
	if err := s.resetFailedLogin(ctx, u.ID); err != nil {
		return nil, err
	}
	u.FailedLoginAttempts, u.LockoutUntil = 0, nil
	if requireMFA {
		id, err := s.mfa.Issue(ctx, u, "login")
		if err != nil {
			return nil, err
		}
		return MFAPendingResponse(u, id), nil
	}
	return s.IssueTokens(ctx, u)
}

// incrementFailedLogin re-reads the user and writes count + 1, as Fluent does:
// not an atomic increment, so concurrent failures can lose one (Swift
// weakness, kept). A nil lockoutAt clears an expired lock.
func (s *Service) incrementFailedLogin(ctx context.Context, id uuid.UUID, lockoutAt *time.Time) error {
	u, err := s.FindUserByID(ctx, id)
	if err != nil || u == nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE users SET failed_login_attempts = $2, lockout_until = $3, updated_at = $4 WHERE id = $1`,
		id, u.FailedLoginAttempts+1, lockoutAt, s.opt.Now())
	return err
}

func (s *Service) resetFailedLogin(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET failed_login_attempts = 0, lockout_until = NULL, updated_at = $2 WHERE id = $1`,
		id, s.opt.Now())
	return err
}

// VerifyMFA is DefaultAuthService.verifyMFA.
func (s *Service) VerifyMFA(ctx context.Context, challengeID uuid.UUID, code string) (*AuthResponse, error) {
	ok, err := s.mfa.Verify(ctx, challengeID, code)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrMFAInvalid
	}
	c, err := s.mfa.find(ctx, challengeID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrMFAInvalid
	}
	u, err := s.FindUserByID(ctx, c.tenant)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrMFAInvalid
	}
	return s.IssueTokens(ctx, u)
}

// ResendMFA is DefaultAuthService.resendMFA. Swift weaknesses, kept: it needs
// only an email, so anyone can mail a code to any account (and an unknown email
// answers 401, which tells the caller it has no account). It also issues a new
// challenge whose id is never returned, so the client's challenge never
// matches the resent code.
func (s *Service) ResendMFA(ctx context.Context, email string) error {
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrInvalidCredentials
	}
	_, err = s.mfa.Issue(ctx, u, "login")
	return err
}

// ForgotPassword is DefaultAuthService.forgotPassword: silent for an unknown
// email. Swift weakness, kept: a failed send answers 5xx only for a known one.
func (s *Service) ForgotPassword(ctx context.Context, email string) error {
	return s.resetCode(ctx, email)
}

// ResendReset is DefaultAuthService.resendReset, the same as forgotPassword.
func (s *Service) ResendReset(ctx context.Context, email string) error {
	return s.resetCode(ctx, email)
}

func (s *Service) resetCode(ctx context.Context, email string) error {
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil || u == nil {
		return err
	}
	code := s.opt.ResetCodes.Generate()
	now := s.opt.Now()
	if _, err := s.pool.Exec(ctx, `INSERT INTO password_reset_tokens (id, tenant_id, code_hash, expires_at, used_at, failed_attempts, locked_until, created_at)
		VALUES ($1, $2, $3, $4, NULL, 0, NULL, $5)`, uuid.New(), u.ID, sha256Hex(code), now.Add(resetCodeLifetime), now); err != nil {
		return err
	}
	return s.opt.ResetSender.Send(ctx, code, u.Email, "reset")
}

// otpRow is a password_reset_tokens or email_verification_tokens row.
type otpRow struct {
	id          uuid.UUID
	codeHash    string
	expiresAt   time.Time
	failed      int64
	lockedUntil *time.Time
}

// latestUnused is `query(tenantID:).filter(usedAt == nil).sort(createdAt, desc).first()`.
func (s *Service) latestUnused(ctx context.Context, table string, tenant uuid.UUID) (*otpRow, error) {
	var r otpRow
	err := s.pool.QueryRow(ctx, `SELECT id, code_hash, expires_at, failed_attempts, locked_until FROM `+table+`
		WHERE tenant_id = $1 AND used_at IS NULL ORDER BY created_at DESC LIMIT 1`, tenant).
		Scan(&r.id, &r.codeHash, &r.expiresAt, &r.failed, &r.lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// failOTP counts a wrong code on the row and locks it at max, as Fluent saves
// the incremented value it read.
func (s *Service) failOTP(ctx context.Context, table string, r *otpRow, maxFailures int64, lockout time.Duration) error {
	failed := r.failed + 1
	var locked *time.Time
	if failed >= maxFailures {
		t := s.opt.Now().Add(lockout)
		locked = &t
	}
	_, err := s.pool.Exec(ctx, `UPDATE `+table+` SET failed_attempts = $2, locked_until = COALESCE($3, locked_until) WHERE id = $1`,
		r.id, failed, locked)
	return err
}

// ResetPassword is DefaultAuthService.resetPassword: the newest unused code,
// then a new hash, the login lockout cleared, every refresh token revoked and a
// new session. Access tokens already issued stay valid until they expire.
//
// Swift weaknesses, kept: the lock is on the code row, so a new forgot-password
// starts a fresh five attempts; the hashes are compared with ==; and an unknown
// email gives the same 401 as a wrong code only after skipping the work.
func (s *Service) ResetPassword(ctx context.Context, email, code, newPassword string) (*AuthResponse, error) {
	if swiftstr.CharacterCount(newPassword) < minPasswordLength {
		return nil, ErrWeakPassword
	}
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrResetCodeInvalid
	}
	row, err := s.latestUnused(ctx, "password_reset_tokens", u.ID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrResetCodeInvalid
	}
	now := s.opt.Now()
	if row.lockedUntil != nil && row.lockedUntil.After(now) {
		return nil, ErrResetLocked
	}
	if row.expiresAt.Before(now) {
		return nil, ErrResetCodeInvalid
	}
	if row.codeHash != sha256Hex(code) {
		if err := s.failOTP(ctx, "password_reset_tokens", row, maxResetFailures, resetLockoutDuration); err != nil {
			return nil, err
		}
		return nil, ErrResetCodeInvalid
	}
	if _, err := s.pool.Exec(ctx, `UPDATE password_reset_tokens SET used_at = $2 WHERE id = $1`, row.id, s.opt.Now()); err != nil {
		return nil, err
	}
	hash, err := auth.HashPassword(newPassword, s.opt.PasswordCost)
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE users SET password_hash = $2, failed_login_attempts = 0, lockout_until = NULL, updated_at = $3 WHERE id = $1`,
		u.ID, hash, s.opt.Now()); err != nil {
		return nil, err
	}
	u.PasswordHash, u.FailedLoginAttempts, u.LockoutUntil = hash, 0, nil
	if _, err := s.pool.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = $2 WHERE tenant_id = $1 AND revoked_at IS NULL`,
		u.ID, s.opt.Now()); err != nil {
		return nil, err
	}
	return s.IssueTokens(ctx, u)
}

// SendVerification is DefaultAuthService.sendVerification: silent for an
// unknown or already verified email.
func (s *Service) SendVerification(ctx context.Context, email string) error {
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil || u == nil || u.IsVerified {
		return err
	}
	code := s.opt.VerifyCodes.Generate()
	now := s.opt.Now()
	if _, err := s.pool.Exec(ctx, `INSERT INTO email_verification_tokens (id, tenant_id, code_hash, expires_at, used_at, failed_attempts, locked_until, created_at)
		VALUES ($1, $2, $3, $4, NULL, 0, NULL, $5)`, uuid.New(), u.ID, sha256Hex(code), now.Add(verifyCodeLifetime), now); err != nil {
		return err
	}
	return s.opt.VerifySender.Send(ctx, code, u.Email, "verify")
}

// ConfirmEmail is DefaultAuthService.confirmEmail. An already verified user
// succeeds whatever the code. Swift weaknesses, kept: an unknown email answers
// 401 while a verified one answers 204, so the route tells the two apart; the
// lock is per code row, as for reset.
func (s *Service) ConfirmEmail(ctx context.Context, email, code string) error {
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrVerifyCodeInvalid
	}
	if u.IsVerified {
		return nil
	}
	row, err := s.latestUnused(ctx, "email_verification_tokens", u.ID)
	if err != nil {
		return err
	}
	if row == nil {
		return ErrVerifyCodeInvalid
	}
	now := s.opt.Now()
	if row.lockedUntil != nil && row.lockedUntil.After(now) {
		return ErrVerifyLocked
	}
	if row.expiresAt.Before(now) {
		return ErrVerifyCodeInvalid
	}
	if row.codeHash != sha256Hex(code) {
		if err := s.failOTP(ctx, "email_verification_tokens", row, maxVerifyFailures, verifyLockoutDuration); err != nil {
			return err
		}
		return ErrVerifyCodeInvalid
	}
	if _, err := s.pool.Exec(ctx, `UPDATE email_verification_tokens SET used_at = $2 WHERE id = $1`, row.id, s.opt.Now()); err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE users SET is_verified = true, updated_at = $2 WHERE id = $1`, u.ID, s.opt.Now())
	return err
}

// Refresh is DefaultAuthService.refresh: rotate (revoke, then issue). Not
// transactional and no reuse detection, as in Swift (wave-6-8-plan.md).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*AuthResponse, error) {
	tenant, err := auth.ConsumeRefreshToken(ctx, s.pool, refreshToken, s.opt.Now())
	if errors.Is(err, auth.ErrInvalidRefresh) {
		return nil, ErrInvalidRefresh
	}
	if err != nil {
		return nil, err
	}
	u, err := s.FindUserByID(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalidRefresh
	}
	return s.IssueTokens(ctx, u)
}

// RevokeRefresh is DefaultAuthService.revokeRefresh.
func (s *Service) RevokeRefresh(ctx context.Context, refreshToken string) error {
	return auth.RevokeRefreshToken(ctx, s.pool, refreshToken, s.opt.Now())
}

// AuthResponse is LuminaVaultShared.AuthResponse, keys in declaration order;
// the nil optionals are left out, as JSONEncoder does.
type AuthResponse struct {
	UserID           wire.UUID  `json:"userId"`
	Email            string     `json:"email"`
	AccessToken      string     `json:"accessToken"`
	RefreshToken     string     `json:"refreshToken"`
	ExpiresIn        int        `json:"expiresIn"`
	MFARequired      *bool      `json:"mfaRequired,omitempty"`
	MFAChallengeID   *wire.UUID `json:"mfaChallengeId,omitempty"`
	VaultInitialized bool       `json:"vaultInitialized"`
}

// MFAPendingResponse is mfaPendingResponse: no tokens, just the challenge.
func MFAPendingResponse(u *User, challengeID uuid.UUID) *AuthResponse {
	yes := true
	id := wire.UUID{UUID: challengeID}
	return &AuthResponse{
		UserID: wire.UUID{UUID: u.ID}, Email: u.Email, MFARequired: &yes, MFAChallengeID: &id,
		VaultInitialized: u.VaultInitialized,
	}
}

// tierOverrideRank is TierOverride(rawValue:) ?? .none: anything unknown is none.
func knownOverride(v string) string {
	switch v {
	case "pro", "ultimate":
		return v
	}
	return "none"
}

// IssueTokens is DefaultAuthService.issueTokens, which every session passes
// through: the BILLING_TIER_OVERRIDE_EMAILS stamp, `$set is_internal`, then an
// access token (hpid only for a ready Hermes profile) and a new refresh token.
func (s *Service) IssueTokens(ctx context.Context, u *User) (*AuthResponse, error) {
	if err := s.applyTierOverrideAllowlist(ctx, u); err != nil {
		return nil, err
	}
	if s.opt.Analytics != nil {
		s.opt.Analytics.Capture("$set", ids.Upper(u.ID), map[string]any{
			"$set": map[string]any{"is_internal": knownOverride(u.TierOverride) != "none"},
		})
	}
	hpid := ""
	if s.opt.Profiles != nil {
		profile, err := s.opt.Profiles.Find(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		if profile != nil && profile.Status == "ready" {
			hpid = profile.HermesProfileID
		}
	}
	access, err := s.opt.Keys.MintAccess(u.ID, s.opt.Now(), hpid)
	if err != nil {
		return nil, err
	}
	refresh, err := auth.IssueRefreshToken(ctx, s.pool, u.ID, s.opt.Now())
	if err != nil {
		return nil, err
	}
	return &AuthResponse{
		UserID: wire.UUID{UUID: u.ID}, Email: u.Email, AccessToken: access, RefreshToken: refresh,
		ExpiresIn: int(auth.AccessTokenLifetime / time.Second), VaultInitialized: u.VaultInitialized,
	}, nil
}

// applyTierOverrideAllowlist only raises: a listed email is stamped unless the
// row already matches or already holds ultimate.
func (s *Service) applyTierOverrideAllowlist(ctx context.Context, u *User) error {
	if s.opt.TierOverride == nil {
		return nil
	}
	granted, ok := s.opt.TierOverride(u.Email)
	if !ok {
		return nil
	}
	current := knownOverride(u.TierOverride)
	if current == granted || current == "ultimate" {
		return nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE users SET tier_override = $2, updated_at = $3 WHERE id = $1`, u.ID, granted, s.opt.Now()); err != nil {
		return err
	}
	u.TierOverride = granted
	s.opt.Log.Info("tier override stamped from allowlist", "user_id", ids.Upper(u.ID), "tier_override", granted)
	return nil
}
