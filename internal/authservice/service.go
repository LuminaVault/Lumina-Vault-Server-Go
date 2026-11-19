package authservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/hermesprofile"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	// dummyHash is a bcrypt hash at the service's cost, checked when a login names
	// no account so that an unknown email costs as much as a wrong password.
	dummyHash func() string
	// families probes once for refresh_tokens.family_id (goose 00009); a
	// database Swift migrated (parity) has none. Call it before opening a
	// transaction: it reads through the pool.
	families func() bool
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
	cost := opt.PasswordCost
	dummy := sync.OnceValue(func() string {
		h, err := auth.HashPassword("lv-no-such-account", cost)
		if err != nil {
			return ""
		}
		return h
	})
	log := opt.Log
	families := sync.OnceValue(func() bool {
		ok, err := auth.RefreshFamilyColumn(context.Background(), pool)
		if err != nil {
			log.Warn("could not probe refresh_tokens.family_id; refresh reuse revokes the whole tenant", "error", err)
			return false
		}
		return ok
	})
	return &Service{pool: pool, opt: opt, mfa: NewMFA(pool, opt.MFASender, opt.MFACodes, opt.Now), dummyHash: dummy, families: families}
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
	return findUserIn(ctx, s.pool, where, arg)
}

// findUserIn is findUser through db, which may be a transaction.
func findUserIn(ctx context.Context, db auth.DB, where string, arg any) (*User, error) {
	var u User
	err := db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE `+where+` LIMIT 1`, arg).
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
	// Hummingbird runs a handler to the end after the client leaves; Go cancels
	// the request context, which would abort pgx mid-sequence and leave a user
	// with no trial expiry or no personal vault. Finish the writes as Swift does.
	ctx = context.WithoutCancel(ctx)
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
// Post-archive fixes (backlog: auth weaknesses kept by #85). Swift answered an
// unknown email at once while a known one paid for bcrypt, answered 423 for a
// locked account before checking the password, kept the failure count across
// an expired lock, and counted failures read-then-write. Now:
//   - an unknown email checks the password against a dummy hash, so it costs
//     the same as a wrong password;
//   - the password is checked first; a locked account answers 423 only to the
//     right password, and a wrong one is the usual 401 without counting;
//   - a failure after the lock has expired starts the count afresh;
//   - the count and the lock are one atomic UPDATE.
func (s *Service) Login(ctx context.Context, email, password string, requireMFA bool) (*AuthResponse, error) {
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if u == nil {
		auth.VerifyPassword(password, s.dummyHash())
		return nil, ErrInvalidCredentials
	}
	now := s.opt.Now()
	locked := u.LockoutUntil != nil && u.LockoutUntil.After(now)
	if !auth.VerifyPassword(password, u.PasswordHash) {
		if !locked {
			if err := s.countFailedLogin(ctx, u.ID, now); err != nil {
				return nil, err
			}
		}
		return nil, ErrInvalidCredentials
	}
	if locked {
		return nil, ErrAccountLocked
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

// countFailedLogin adds one failure in a single UPDATE, so concurrent failures
// all count: Postgres re-reads the row a concurrent update changed. A lock that
// has expired starts the count at one, and the maxFailedLogins-th failure locks
// for lockoutDuration.
func (s *Service) countFailedLogin(ctx context.Context, id uuid.UUID, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET
		failed_login_attempts = CASE WHEN lockout_until <= $2 THEN 1 ELSE failed_login_attempts + 1 END,
		lockout_until = CASE
			WHEN (CASE WHEN lockout_until <= $2 THEN 1 ELSE failed_login_attempts + 1 END) >= $3 THEN $4::timestamptz
			ELSE NULL END,
		updated_at = $2
		WHERE id = $1`, id, now, maxFailedLogins, now.Add(lockoutDuration))
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

// ResendMFA is DefaultAuthService.resendMFA. An unknown email answers like a
// known one (202), as forgot-password does; Swift answered 401, which told the
// caller the address had no account (fixed after the archive). Swift weaknesses,
// kept: it needs only an email, so anyone can mail a code to any account, and it
// issues a new challenge whose id is never returned, so the client's challenge
// never matches the resent code.
func (s *Service) ResendMFA(ctx context.Context, email string) error {
	u, err := s.FindUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if u == nil {
		return nil
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
	if err := s.insertOTP(ctx, "password_reset_tokens", u.ID, code, now.Add(resetCodeLifetime), now); err != nil {
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

// insertOTP stores a new code row. It carries over the tenant's newest unused
// row's failure count (unless that row's lock has expired) and any lock still
// running, so asking for a new code neither resets the attempts nor lifts a
// lock. Swift's new row started at zero and unlocked, a fresh five attempts per
// request; fixed after the archive.
func (s *Service) insertOTP(ctx context.Context, table string, tenant uuid.UUID, code string, expiresAt, now time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO `+table+` (id, tenant_id, code_hash, expires_at, used_at, failed_attempts, locked_until, created_at)
		VALUES ($1, $2, $3, $4, NULL,
			COALESCE((SELECT CASE WHEN locked_until <= $5 THEN 0 ELSE failed_attempts END FROM `+table+`
				WHERE tenant_id = $2 AND used_at IS NULL ORDER BY created_at DESC LIMIT 1), 0),
			(SELECT max(locked_until) FROM `+table+` WHERE tenant_id = $2 AND used_at IS NULL AND locked_until > $5),
			$5)`, uuid.New(), tenant, sha256Hex(code), expiresAt, now)
	return err
}

// failOTP counts a wrong code on the row in one atomic UPDATE and locks it at
// maxFailures. A failure after the row's lock has expired starts the count at
// one. Swift saved the value it had read, so concurrent failures could be lost;
// fixed after the archive.
func (s *Service) failOTP(ctx context.Context, table string, r *otpRow, maxFailures int64, lockout time.Duration) error {
	now := s.opt.Now()
	_, err := s.pool.Exec(ctx, `UPDATE `+table+` SET
		failed_attempts = CASE WHEN locked_until <= $2 THEN 1 ELSE failed_attempts + 1 END,
		locked_until = CASE
			WHEN (CASE WHEN locked_until <= $2 THEN 1 ELSE failed_attempts + 1 END) >= $3 THEN $4::timestamptz
			WHEN locked_until <= $2 THEN NULL
			ELSE locked_until END
		WHERE id = $1`, r.id, now, maxFailures, now.Add(lockout))
	return err
}

// ResetPassword is DefaultAuthService.resetPassword: the newest unused code,
// then a new hash, the login lockout cleared, every refresh token revoked, the
// access tokens issued so far invalidated, and a new session. Swift left the
// access tokens valid until they expired; fixed after the archive.
//
// A new forgot-password carries the attempts and any lock over to the new code
// row, and the hashes are compared in constant time (both fixed after the
// archive; Swift started each row afresh and compared with ==). Swift weakness,
// kept: an unknown email gives the same 401 as a wrong code only after skipping
// the work.
func (s *Service) ResetPassword(ctx context.Context, email, code, newPassword string) (*AuthResponse, error) {
	// As Register: the code is marked used before the bcrypt hash, so a client
	// that leaves mid-request must not strand the reset half done.
	ctx = context.WithoutCancel(ctx)
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
	if !codeMatches(row.codeHash, code) {
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
	if err := s.invalidateSessions(ctx, u.ID); err != nil {
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
	if err := s.insertOTP(ctx, "email_verification_tokens", u.ID, code, now.Add(verifyCodeLifetime), now); err != nil {
		return err
	}
	return s.opt.VerifySender.Send(ctx, code, u.Email, "verify")
}

// ConfirmEmail is DefaultAuthService.confirmEmail. An already verified user
// succeeds whatever the code. Swift weaknesses, kept: an unknown email answers
// 401 while a verified one answers 204, so the route tells the two apart. A new
// code carries the attempts and lock over, as for reset (fixed after the archive).
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
	if !codeMatches(row.codeHash, code) {
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

// Refresh is DefaultAuthService.refresh: rotate (revoke, then issue), in one
// transaction, so a failure part way leaves the old token usable rather than
// the user signed out. A token reused after revocation revokes its login chain,
// or every refresh token of the tenant when it has none
// (auth.ConsumeRefreshTokenFamily), and is the usual 401. Swift was not
// transactional and had no reuse detection; fixed after the archive.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*AuthResponse, error) {
	// Whatever reads through the pool runs before the transaction opens, and
	// the rest goes through tx. A transaction that waits for a second pool
	// connection never gets one once every connection is held by a refresh of
	// the same token waiting on this one's row lock.
	hpid, err := s.refreshProfileID(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	families := s.families()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	tenant, family, err := auth.ConsumeRefreshTokenFamily(ctx, tx, refreshToken, s.opt.Now(), families)
	if errors.Is(err, auth.ErrRefreshReused) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		scope := "tenant"
		if family != uuid.Nil {
			scope = "login chain"
		}
		s.opt.Log.Warn("refresh token reused after revocation; revoked its refresh tokens", "tenant_id", ids.Upper(tenant), "scope", scope)
		return nil, ErrInvalidRefresh
	}
	if errors.Is(err, auth.ErrInvalidRefresh) {
		return nil, ErrInvalidRefresh
	}
	if err != nil {
		return nil, err
	}
	u, err := findUserIn(ctx, tx, `id = $1`, tenant)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, ErrInvalidRefresh
	}
	resp, err := s.issueTokensFor(ctx, tx, u, hpid, family, families)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return resp, nil
}

// refreshProfileID is the ready Hermes profile id of the tenant a refresh token
// belongs to, "" when there is none or the token is unknown. It only reads;
// ConsumeRefreshToken still decides whether the token is good.
func (s *Service) refreshProfileID(ctx context.Context, refreshToken string) (string, error) {
	if s.opt.Profiles == nil {
		return "", nil
	}
	tenant, ok, err := auth.RefreshTokenTenant(ctx, s.pool, refreshToken)
	if err != nil || !ok {
		return "", err
	}
	return s.hermesProfileID(ctx, tenant)
}

// hermesProfileID is the hpid claim: the tenant's Hermes profile id, only when
// the profile is ready.
func (s *Service) hermesProfileID(ctx context.Context, tenant uuid.UUID) (string, error) {
	if s.opt.Profiles == nil {
		return "", nil
	}
	profile, err := s.opt.Profiles.Find(ctx, tenant)
	if err != nil {
		return "", err
	}
	if profile != nil && profile.Status == "ready" {
		return profile.HermesProfileID, nil
	}
	return "", nil
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
	return s.issueTokens(ctx, s.pool, u)
}

// issueTokens is IssueTokens with its writes going through db.
func (s *Service) issueTokens(ctx context.Context, db auth.DB, u *User) (*AuthResponse, error) {
	hpid, err := s.hermesProfileID(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	// A sign-in: uuid.Nil starts a new login chain.
	return s.issueTokensFor(ctx, db, u, hpid, uuid.Nil, s.families())
}

// issueTokensFor is issueTokens with the hpid already looked up. Every query
// goes through db, so Refresh can run it in its transaction without taking a
// second pool connection.
func (s *Service) issueTokensFor(ctx context.Context, db auth.DB, u *User, hpid string, family uuid.UUID, families bool) (*AuthResponse, error) {
	if err := s.applyTierOverrideAllowlist(ctx, db, u); err != nil {
		return nil, err
	}
	if s.opt.Analytics != nil {
		s.opt.Analytics.Capture("$set", ids.Upper(u.ID), map[string]any{
			"$set": map[string]any{"is_internal": knownOverride(u.TierOverride) != "none"},
		})
	}
	access, err := s.opt.Keys.MintAccess(u.ID, s.opt.Now(), hpid)
	if err != nil {
		return nil, err
	}
	refresh, err := auth.IssueRefreshTokenInFamily(ctx, db, u.ID, family, families, s.opt.Now())
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
func (s *Service) applyTierOverrideAllowlist(ctx context.Context, db auth.DB, u *User) error {
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
	if _, err := db.Exec(ctx, `UPDATE users SET tier_override = $2, updated_at = $3 WHERE id = $1`, u.ID, granted, s.opt.Now()); err != nil {
		return err
	}
	u.TierOverride = granted
	s.opt.Log.Info("tier override stamped from allowlist", "user_id", ids.Upper(u.ID), "tier_override", granted)
	return nil
}

// invalidateSessions stamps users.sessions_valid_after, so auth.Identify refuses
// the session tokens issued before now. A database Swift migrated (parity) has
// no such column; there the stamp is skipped, as the check is.
func (s *Service) invalidateSessions(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET sessions_valid_after = $2 WHERE id = $1`, id, s.opt.Now())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42703" { // undefined_column
		return nil
	}
	return err
}
