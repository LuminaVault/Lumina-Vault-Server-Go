package authservice_test

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/authservice"
	"lumina/internal/dbtest"
	"lumina/internal/hermesprofile"
	"lumina/internal/migrations"
	"lumina/internal/vaultpaths"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// keys is the Swift harness's HMAC key ("test-kid").
var keys = auth.NewKeyset(auth.Key{Kid: "test-kid", Secret: []byte("test-secret-do-not-use-in-prod-32chars")})

const password = "CorrectHorseBatteryStaple1!"

// recorder is the Swift tests' MFAChallengeRecorder: an EmailOTPSender that
// keeps the last code and destination and sends nothing.
type recorder struct {
	mu                 sync.Mutex
	lastCode, lastDest string
	sent               int
}

func (r *recorder) Send(_ context.Context, code, to, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastCode, r.lastDest = code, to
	r.sent++
	return nil
}

func (r *recorder) last() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastCode, r.lastDest
}

// events records PostHog captures.
type events struct {
	mu  sync.Mutex
	got []event
}

type event struct {
	name, distinct string
	props          map[string]any
}

func (e *events) Capture(name, distinctID string, props map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, event{name, distinctID, props})
}

func (e *events) all() []event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]event(nil), e.got...)
}

// failingGateway is AlwaysFailingHermesGateway.
type failingGateway struct{}

func (failingGateway) ProvisionProfile(context.Context, uuid.UUID, string) (string, error) {
	return "", errors.New("hermes gateway is intentionally down for lifecycle test")
}

type harness struct {
	svc                *authservice.Service
	pool               *pgxpool.Pool
	mfa, reset, verify *recorder
	analytics          *events
	tierOverride       func(string) (string, bool)
	gateway            hermesprofile.Gateway
	paths              vaultpaths.Paths
}

// newPool is one migrated database per Swift suite: the suites run their cases
// as subtests on it with random emails and usernames, as Swift's do on a
// shared schema.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(context.Background(), pool, migrations.SQL()))
	return pool
}

// build is makeHarness: fixed codes 123456 (MFA), 654321 (reset) and 789012
// (verify), the logging Hermes gateway unless one is given.
func build(t *testing.T, pool *pgxpool.Pool, h harness) harness {
	t.Helper()
	h.pool = pool
	h.mfa, h.reset, h.verify, h.analytics = &recorder{}, &recorder{}, &recorder{}, &events{}
	if h.gateway == nil {
		h.gateway = hermesprofile.LoggingGateway{}
	}
	h.paths = vaultpaths.Paths{Root: t.TempDir()}
	h.svc = authservice.New(pool, authservice.Options{
		Keys:         keys,
		Profiles:     hermesprofile.New(pool, h.gateway, h.paths),
		Analytics:    h.analytics,
		TierOverride: h.tierOverride,
		MFASender:    h.mfa, ResetSender: h.reset, VerifySender: h.verify,
		MFACodes:     authservice.FixedCodeGenerator{Code: "123456"},
		ResetCodes:   authservice.FixedCodeGenerator{Code: "654321"},
		VerifyCodes:  authservice.FixedCodeGenerator{Code: "789012"},
		PasswordCost: 4,
	})
	return h
}

func randomSuffix() string { return strings.ToLower(uuid.NewString()[:8]) }

func randomEmail(prefix string) string { return prefix + "-" + randomSuffix() + "@test.luminavault" }

func randomUsername(prefix string) string { return prefix + "-" + randomSuffix() }

type userRow struct {
	id                 uuid.UUID
	email, username    string
	tier, tierOverride string
	tierExpires        *time.Time
	isVerified         bool
	revenuecat         *string
	failed             int64
}

func (h harness) user(t *testing.T, where string, arg any) *userRow {
	t.Helper()
	var u userRow
	err := h.pool.QueryRow(context.Background(), `SELECT id, email, username, tier, tier_override, tier_expires_at, is_verified,
		revenuecat_user_id, failed_login_attempts FROM users WHERE `+where, arg).
		Scan(&u.id, &u.email, &u.username, &u.tier, &u.tierOverride, &u.tierExpires, &u.isVerified, &u.revenuecat, &u.failed)
	if err != nil {
		return nil
	}
	return &u
}
