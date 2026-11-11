package billing_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/billing"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Ports RevenueCatWebhookTests.swift (11), SubscriptionCatalogTests.swift (6),
// TierOverrideAllowlistTests.swift (5), UsageMetricsTests.swift (2) and the
// /me/billing cases of EnforcementTests.swift.

const secret = "rc-test-secret"

var keys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

func setupWith(t *testing.T, webhookSecret string, enforcement bool) env {
	return setupAdmin(t, webhookSecret, enforcement, "billing-admin")
}

func setupAdmin(t *testing.T, webhookSecret string, enforcement bool, adminToken string) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{billing.New(pool, billing.Options{WebhookSecret: webhookSecret, EnforcementEnabled: enforcement, AdminToken: adminToken})},
	})
	return env{t: t, e: e, pool: pool}
}

func setup(t *testing.T) env { return setupWith(t, secret, false) }

func (v env) user(tier, override string, expires *time.Time) uuid.UUID {
	v.t.Helper()
	id := uuid.New()
	_, err := v.pool.Exec(context.Background(), `INSERT INTO users (id, email, password_hash, is_verified, username, tier, tier_override, tier_expires_at, created_at, updated_at)
		VALUES ($1, $2, 'x', true, $3, $4, $5, $6, now(), now())`, id, id.String()+"@t.test", "u"+id.String()[:8], tier, override, expires)
	require.NoError(v.t, err)
	return id
}

func (v env) tier(id uuid.UUID) (string, *time.Time, *string) {
	v.t.Helper()
	var tier string
	var exp *time.Time
	var rc *string
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT tier, tier_expires_at, revenuecat_user_id FROM users WHERE id = $1`, id).Scan(&tier, &exp, &rc))
	return tier, exp, rc
}

func (v env) logRows(eventID string) int {
	v.t.Helper()
	var n int
	require.NoError(v.t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM billing_event_logs WHERE event_id = $1`, eventID).Scan(&n))
	return n
}

func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func event(id, typ, appUser string, extra string) string {
	return fmt.Sprintf(`{"event":{"id":%q,"type":%q,"app_user_id":%q%s}}`, id, typ, appUser, extra)
}

func (v env) post(body string, hdr ...string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/billing/revenuecat-webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func (v env) signed(body string) *httptest.ResponseRecorder {
	return v.post(body, "X-RevenueCat-Signature", sign(body))
}

func (v env) get(user uuid.UUID, path string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	tok, err := scenario.Mint(keys, user)
	require.NoError(v.t, err)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func future(d time.Duration) string {
	return fmt.Sprintf(`,"expiration_at_ms":%d`, time.Now().Add(d).UnixMilli())
}

// --- RevenueCatWebhookTests

func TestBadSignatureReturns401(t *testing.T) {
	v := setup(t)
	body := event("e1", "INITIAL_PURCHASE", uuid.NewString(), `,"product_id":"pro_monthly_14_99"`)
	r := v.post(body, "X-RevenueCat-Signature", "deadbeef")
	require.Equal(t, 401, r.Code)
	require.Empty(t, r.Body.String())
	require.Equal(t, 401, v.post(body, "Authorization", "Bearer wrong").Code)
	require.Equal(t, 401, v.post(body).Code)
}

func TestInitialPurchaseUpgradesTierToProAndSetsExpiry(t *testing.T) {
	v := setup(t)
	id := v.user("trial", "none", nil)
	r := v.signed(event("e1", "INITIAL_PURCHASE", id.String(), `,"product_id":"pro_monthly_14_99","expiration_at_ms":1900000000000`))
	require.Equal(t, 200, r.Code, r.Body.String())
	require.Empty(t, r.Body.String())
	tier, exp, rc := v.tier(id)
	require.Equal(t, "pro", tier)
	require.NotNil(t, exp)
	require.Equal(t, int64(1900000000000), exp.UnixMilli())
	require.Equal(t, id.String(), *rc)
}

func TestInitialPurchaseUltimateSetsUltimateTier(t *testing.T) {
	v := setup(t)
	id := v.user("trial", "none", nil)
	require.Equal(t, 200, v.signed(event("e1", "INITIAL_PURCHASE", id.String(), `,"product_id":"ultimate_yearly_299_99"`+future(time.Hour))).Code)
	tier, _, _ := v.tier(id)
	require.Equal(t, "ultimate", tier)
}

func TestExpirationDropsTierToLapsed(t *testing.T) {
	v := setup(t)
	id := v.user("pro", "none", nil)
	require.Equal(t, 200, v.signed(event("e1", "EXPIRATION", id.String(), "")).Code)
	tier, _, _ := v.tier(id)
	require.Equal(t, "lapsed", tier)
}

func TestReplayedEventIDIsIdempotent(t *testing.T) {
	v := setup(t)
	id := v.user("trial", "none", nil)
	body := event("dup", "INITIAL_PURCHASE", id.String(), `,"product_id":"pro_monthly_14_99"`)
	require.Equal(t, 200, v.signed(body).Code)
	require.Equal(t, 200, v.signed(body).Code)
	require.Equal(t, 1, v.logRows("dup"))
}

func TestUnknownRCUserIDReturns200(t *testing.T) {
	v := setup(t)
	require.Equal(t, 200, v.signed(event("e1", "INITIAL_PURCHASE", "$RCAnonymousID:abc", `,"product_id":"pro_monthly_14_99"`)).Code)
	require.Equal(t, 1, v.logRows("e1"))
	var userID *uuid.UUID
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT user_id FROM billing_event_logs WHERE event_id = 'e1'`).Scan(&userID))
	require.Nil(t, userID)
}

func TestBearerAuthHeaderAcceptedAsAlternativeToHMAC(t *testing.T) {
	v := setup(t)
	id := v.user("trial", "none", nil)
	r := v.post(event("e1", "INITIAL_PURCHASE", id.String(), `,"product_id":"pro_monthly_14_99"`), "Authorization", "Bearer "+secret)
	require.Equal(t, 200, r.Code)
	// A bare secret works too: "Bearer " is a plain string replace.
	require.Equal(t, 200, v.post(event("e2", "RENEWAL", id.String(), `,"product_id":"pro_monthly_14_99"`), "Authorization", secret).Code)
}

func TestAnUnmappedProductFailsInsteadOfSilentlyGrantingNothing(t *testing.T) {
	v := setup(t)
	id := v.user("trial", "none", nil)
	r := v.signed(event("e1", "INITIAL_PURCHASE", id.String(), `,"product_id":"promo_monthly"`))
	require.Equal(t, 500, r.Code)
	tier, _, _ := v.tier(id)
	require.Equal(t, "trial", tier)
	require.Equal(t, 0, v.logRows("e1"))
	// No product at all is the same.
	require.Equal(t, 500, v.signed(event("e2", "RENEWAL", id.String(), "")).Code)
}

func TestARenewalRestoresTheTierOfALapsedSubscriber(t *testing.T) {
	v := setup(t)
	id := v.user("lapsed", "none", nil)
	require.Equal(t, 200, v.signed(event("e1", "RENEWAL", id.String(), `,"product_id":"pro_monthly_14_99"`+future(30*24*time.Hour))).Code)
	tier, exp, _ := v.tier(id)
	require.Equal(t, "pro", tier)
	require.True(t, exp.After(time.Now()))
}

func TestABillingIssueHoldsTheTierThroughTheRetryWindow(t *testing.T) {
	v := setup(t)
	past := time.Now().Add(-time.Hour)
	id := v.user("pro", "none", &past)
	require.Equal(t, 200, v.signed(event("e1", "BILLING_ISSUE", id.String(), "")).Code)
	tier, exp, _ := v.tier(id)
	require.Equal(t, "pro", tier, "must not demote on a retryable failure")
	require.True(t, exp.After(time.Now().Add(7*24*time.Hour)))

	// A later expiry is not pulled back.
	far := time.Now().Add(60 * 24 * time.Hour).Truncate(time.Second)
	id2 := v.user("pro", "none", &far)
	require.Equal(t, 200, v.signed(event("e2", "BILLING_ISSUE", id2.String(), "")).Code)
	_, exp2, _ := v.tier(id2)
	require.True(t, exp2.Equal(far))
}

func TestACancellationWithTimeRemainingKeepsTheTier(t *testing.T) {
	v := setup(t)
	id := v.user("pro", "none", nil)
	require.Equal(t, 200, v.signed(event("e1", "CANCELLATION", id.String(), future(10*24*time.Hour))).Code)
	tier, _, _ := v.tier(id)
	require.Equal(t, "pro", tier)

	require.Equal(t, 200, v.signed(event("e2", "CANCELLATION", id.String(), future(10*24*time.Hour)+`,"is_refund":true`)).Code)
	tier, _, _ = v.tier(id)
	require.Equal(t, "lapsed", tier)

	id2 := v.user("pro", "none", nil)
	require.Equal(t, 200, v.signed(event("e3", "CANCELLATION", id2.String(), "")).Code, "no expiry is already past")
	tier, _, _ = v.tier(id2)
	require.Equal(t, "lapsed", tier)
}

func TestWebhookGuards(t *testing.T) {
	v := setupWith(t, "", false)
	require.Equal(t, 500, v.post(`{}`, "Authorization", "x").Code)

	v = setup(t)
	big := strings.Repeat(" ", 1<<20+1)
	require.Equal(t, 413, v.post(big, "Authorization", secret).Code)
	require.Equal(t, 400, v.signed(`{nope`).Code)
	require.Equal(t, 400, v.signed(`{"event":{"id":"x","type":"RENEWAL"}}`).Code)

	// Subscriber alias and unknown types only stamp revenuecat_user_id.
	id := v.user("trial", "none", nil)
	require.NoError(t, func() error {
		_, err := v.pool.Exec(context.Background(), `UPDATE users SET revenuecat_user_id = 'rc-1' WHERE id = $1`, id)
		return err
	}())
	require.Equal(t, 200, v.signed(event("e1", "SUBSCRIBER_ALIAS", "rc-1", "")).Code)
	require.Equal(t, 200, v.signed(event("e2", "TEST", "rc-1", "")).Code)
	tier, _, rc := v.tier(id)
	require.Equal(t, "trial", tier)
	require.Equal(t, "rc-1", *rc)
}

// --- SubscriptionCatalogTests

func TestSubscriptionCatalog(t *testing.T) {
	for id, want := range map[string]string{
		"pro_monthly_14_99": "pro", "pro_yearly_149_99": "pro",
		"ultimate_monthly_29_99": "ultimate", "ultimate_yearly_299_99": "ultimate",
	} {
		got, ok := billing.TierForProduct(id)
		require.True(t, ok, id)
		require.Equal(t, want, got)
	}
	for _, id := range []string{"promo_monthly", "professional", "product", "ultimate", "pro", "", "PRO_MONTHLY_14_99", " pro_monthly_14_99"} {
		_, ok := billing.TierForProduct(id)
		require.False(t, ok, id)
	}
	seen := map[string]bool{}
	prices := map[string]string{}
	for _, p := range billing.Products {
		require.False(t, seen[p.ID])
		seen[p.ID] = true
		prices[p.ID] = p.PriceUSD
		require.Contains(t, []string{"pro", "ultimate"}, p.Tier)
	}
	require.Equal(t, map[string]string{
		"pro_monthly_14_99": "14.99", "pro_yearly_149_99": "149.99",
		"ultimate_monthly_29_99": "29.99", "ultimate_yearly_299_99": "299.99",
	}, prices)
}

// --- TierOverrideAllowlistTests

func TestTierOverrideAllowlist(t *testing.T) {
	require.Empty(t, billing.ParseTierOverrides("", nil))

	l := billing.ParseTierOverrides("Founder@Example.com=ultimate,tester@example.com=PRO", nil)
	got, ok := l.Override(" founder@EXAMPLE.com ")
	require.True(t, ok)
	require.Equal(t, "ultimate", got)
	got, _ = l.Override("tester@example.com")
	require.Equal(t, "pro", got)
	_, ok = l.Override("nobody@example.com")
	require.False(t, ok)

	got, _ = billing.ParseTierOverrides("bare@example.com", nil).Override("bare@example.com")
	require.Equal(t, "ultimate", got)

	l = billing.ParseTierOverrides("a@x.com=gold,b@x.com=none,c@x.com=pro,=pro", nil)
	require.Len(t, l, 1)
	got, _ = l.Override("c@x.com")
	require.Equal(t, "pro", got)

	l = billing.ParseTierOverrides("  a@x.com = pro ,, \n b@x.com\t", nil)
	require.Equal(t, billing.TierOverrides{"a@x.com": "pro", "b@x.com": "ultimate"}, l)
}

// --- UsageMetricsTests

func TestMeUsageReportsCurrentMonthStorageAndTokenTotals(t *testing.T) {
	v := setup(t)
	id := v.user("trial", "none", nil)
	ctx := context.Background()
	_, err := v.pool.Exec(ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'Personal', now(), now())`, id)
	require.NoError(t, err)
	_, err = v.pool.Exec(ctx, `INSERT INTO vault_files (id, tenant_id, path, content_type, size_bytes, sha256, created_at, updated_at)
		VALUES ($1, $2, 'usage.md', 'text/markdown', 18, 'x', now(), now())`, uuid.New(), id)
	require.NoError(t, err)
	_, err = v.pool.Exec(ctx, `INSERT INTO usage_meter (tenant_id, day, model, mtok_in, mtok_out, chars_out)
		VALUES ($1, current_date, 'a', 100, 50, 300), ($1, current_date, 'b', 20, 30, 50),
		       ($1, current_date - interval '400 days', 'a', 999, 999, 999)`, id)
	require.NoError(t, err)

	r := v.get(id, "/v1/auth/me/usage")
	require.Equal(t, 200, r.Code, r.Body.String())
	var u map[string]any
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &u))
	require.Equal(t, "trial", u["tier"])
	require.InDelta(t, 18, u["storageBytes"], 0)
	require.InDelta(t, 120, u["tokensIn"], 0)
	require.InDelta(t, 80, u["tokensOut"], 0)
	require.InDelta(t, 200, u["tokensTotal"], 0)
	require.InDelta(t, 350, u["ttsCharacters"], 0)
	require.InDelta(t, 0, u["compileRuns"], 0)
	daily, _ := u["daily"].([]any)
	require.Len(t, daily, 1)
}

func TestMeUsageReportsCurrentMonthCompileEvents(t *testing.T) {
	v := setup(t)
	id := v.user("pro", "none", nil)
	_, err := v.pool.Exec(context.Background(), `INSERT INTO usage_events (tenant_id, metric, amount, source, idempotency_key, metadata)
		VALUES ($1, 'memory_compile_run', 1, 'memory_compile', 'r', '{}'), ($1, 'memory_compile_file', 5, 'memory_compile', 'f', '{}')`, id)
	require.NoError(t, err)
	r := v.get(id, "/v1/auth/me/usage")
	require.Equal(t, 200, r.Code, r.Body.String())
	var u map[string]any
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &u))
	require.InDelta(t, 1, u["compileRuns"], 0)
	require.InDelta(t, 5, u["compileFiles"], 0)
}

// --- EnforcementTests: me billing

func TestMeBillingReportsTheEffectiveTierNotTheStoredColumn(t *testing.T) {
	v := setupWith(t, secret, true)
	id := v.user("lapsed", "ultimate", nil)
	r := v.get(id, "/v1/auth/me/billing")
	require.Equal(t, 200, r.Code, r.Body.String())
	require.JSONEq(t, `{"tier":"ultimate","tierOverride":"ultimate","inTrial":false,"daysRemaining":0,"enforcementEnabled":true}`, r.Body.String())
}

func TestMeBillingReportsFreeWithoutATrialCountdown(t *testing.T) {
	v := setup(t)
	id := v.user("free", "none", nil)
	r := v.get(id, "/v1/auth/me/billing")
	require.JSONEq(t, `{"tier":"free","tierOverride":"none","inTrial":false,"daysRemaining":0,"enforcementEnabled":false}`, r.Body.String())

	exp := time.Now().Add(5*24*time.Hour + time.Hour)
	got := billing.MeBilling("trial", "none", &exp, time.Now(), false)
	require.Equal(t, 5, got.DaysRemaining)
	require.True(t, got.InTrial)
	past := time.Now().Add(-48 * time.Hour)
	got = billing.MeBilling("trial", "bogus", &past, time.Now(), false)
	require.Equal(t, 0, got.DaysRemaining)
	require.False(t, got.InTrial)
	require.Equal(t, "bogus", got.TierOverride)
}

func TestMeRoutesNeedABearer(t *testing.T) {
	v := setup(t)
	for _, p := range []string{"/v1/auth/me/billing", "/v1/auth/me/usage"} {
		rec := httptest.NewRecorder()
		v.e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		require.Equal(t, 401, rec.Code)
	}
}

// --- EnforcementTests: admin tier override

func (v env) put(path, body string, hdr ...string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return rec
}

func TestAdminCanSetTierOverride(t *testing.T) {
	v := setup(t)
	id := v.user("lapsed", "none", nil)
	r := v.put("/v1/admin/users/"+id.String()+"/tier-override", `{"tierOverride":"ultimate"}`, "X-Admin-Token", "billing-admin")
	require.Equal(t, 200, r.Code, r.Body.String())
	require.JSONEq(t, `{"id":"`+strings.ToUpper(id.String())+`","tier":"lapsed","tierOverride":"ultimate"}`, r.Body.String())
}

func TestAdminTierOverrideRejectsInvalidEnum(t *testing.T) {
	v := setup(t)
	id := v.user("lapsed", "none", nil)
	path := "/v1/admin/users/" + id.String() + "/tier-override"
	r := v.put(path, `{"tierOverride":"godmode"}`, "X-Admin-Token", "billing-admin")
	require.Equal(t, 400, r.Code)
	require.JSONEq(t, `{"error":{"message":"tierOverride must be one of: none, pro, ultimate"}}`, r.Body.String())
	require.Equal(t, 401, v.put(path, `{"tierOverride":"pro"}`, "X-Admin-Token", "nope").Code)
	require.Equal(t, 401, v.put(path, `{"tierOverride":"pro"}`).Code)
	require.Equal(t, 400, v.put("/v1/admin/users/nope/tier-override", `{"tierOverride":"pro"}`, "X-Admin-Token", "billing-admin").Code)
	require.Equal(t, 404, v.put("/v1/admin/users/"+uuid.NewString()+"/tier-override", `{"tierOverride":"pro"}`, "X-Admin-Token", "billing-admin").Code)
	disabled := setupAdmin(t, secret, false, "")
	require.Equal(t, 404, disabled.put(path, `{"tierOverride":"pro"}`, "X-Admin-Token", "").Code)
}
