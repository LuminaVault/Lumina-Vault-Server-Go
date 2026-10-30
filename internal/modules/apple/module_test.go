package apple_test

import (
	"context"
	"encoding/json"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/apple"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

var (
	alice = uuid.MustParse("A11CE000-0000-4000-8000-000000000001")
	bob   = uuid.MustParse("B0B00000-0000-4000-8000-000000000002")
	keys  = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})
)

type env struct {
	t    *testing.T
	e    *echo.Echo
	pool *pgxpool.Pool
}

// setup migrates a fresh database and loads base.sql plus, with fixtures,
// the module's parity seed.
func setup(t *testing.T, fixtures bool, opts apple.Options) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	files := []string{"../../../parity/seed/base.sql"}
	if fixtures {
		files = append(files, "../../../parity/seed/apple.sql")
	}
	for _, f := range files {
		seed, err := os.ReadFile(f)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(seed))
		require.NoError(t, err)
	}
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth:    auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{apple.New(pool, opts)},
	})
	return env{t: t, e: e, pool: pool}
}

var defaultOpts = apple.Options{EmbeddingProvider: "deterministic", EmbeddingMonthlyCap: 2_000_000}

type resp struct {
	code int
	ct   string
	body []byte
}

func (v env) do(user uuid.UUID, method, path, body string) resp {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		tok, err := scenario.Mint(keys, user)
		require.NoError(v.t, err)
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	v.e.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Header().Get("Content-Type"), rec.Body.Bytes()}
}

func (v env) setConsent(user uuid.UUID, domain string, allowed bool) {
	v.t.Helper()
	r := v.do(user, http.MethodPut, "/v1/apple/consent", fmt.Sprintf(`{"domain":%q,"allowed":%t}`, domain, allowed))
	require.Equal(v.t, http.StatusOK, r.code, string(r.body))
}

type syncCounts struct{ Inserted, Updated, Skipped int }

func (v env) post(user uuid.UUID, path, body string, want int) syncCounts {
	v.t.Helper()
	r := v.do(user, http.MethodPost, path, body)
	require.Equal(v.t, want, r.code, string(r.body))
	var out syncCounts
	if r.code == http.StatusOK {
		require.NoError(v.t, json.Unmarshal(r.body, &out))
	}
	return out
}

func reminderItem(externalID, title string, remote time.Time) string {
	return fmt.Sprintf(`{"externalID":%q,"title":%q,"completed":false,"remoteUpdatedAt":%q}`, externalID, title, remote.UTC().Format(time.RFC3339))
}

func remindersBody(items ...string) string {
	return `{"reminders":[` + strings.Join(items, ",") + `]}`
}

// MARK: AppleRemindersControllerTests

func TestRemindersSync403WithoutConsent(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-1", "Buy milk", time.Now())), http.StatusForbidden)
	r := v.do(alice, http.MethodPost, "/v1/reminders/sync", "not json")
	require.Equal(t, http.StatusForbidden, r.code, "the gate runs before decoding")
	require.JSONEq(t, `{"error":{"message":"reminders access not allowed by the user"}}`, string(r.body))
}

func TestRemindersSyncHappyPathInsertsTwo(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "reminders", true)
	now := time.Now()
	got := v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-1", "Buy milk", now), reminderItem("r-2", "Call mom", now)), http.StatusOK)
	require.Equal(t, syncCounts{2, 0, 0}, got)
}

func TestRemindersSyncIdempotentResyncUpdates(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "reminders", true)
	t0 := time.Now()
	require.Equal(t, 2, v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-1", "Buy milk", t0), reminderItem("r-2", "Call mom", t0)), http.StatusOK).Inserted)
	t1 := t0.Add(time.Minute)
	got := v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-1", "Buy oat milk", t1), reminderItem("r-2", "Call mom back", t1)), http.StatusOK)
	require.Equal(t, syncCounts{0, 2, 0}, got)
}

func TestRemindersSyncLastWriterWins(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "reminders", true)
	base := time.Now()
	require.Equal(t, 1, v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-lww", "Original", base)), http.StatusOK).Inserted)
	older := v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-lww", "Stale edit", base.Add(-time.Hour))), http.StatusOK)
	require.Equal(t, 0, older.Updated)
	require.Equal(t, 1, older.Skipped)
	newer := v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-lww", "Fresh edit", base.Add(time.Hour))), http.StatusOK)
	require.Equal(t, 1, newer.Updated)
	require.Equal(t, 0, newer.Skipped)
	var title string
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT title FROM apple_reminders WHERE external_id = 'r-lww'`).Scan(&title))
	require.Equal(t, "Fresh edit", title)
}

func TestRemindersSyncSkipsEmptyExternalID(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "reminders", true)
	now := time.Now()
	got := v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("  ", "No id", now), reminderItem("r-ok", "Valid", now)), http.StatusOK)
	require.Equal(t, 1, got.Inserted)
	require.Equal(t, 1, got.Skipped)
}

func TestRemindersSyncTenantIsolation(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "reminders", true)
	v.setConsent(bob, "reminders", true)
	now := time.Now()
	a := v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("shared-1", "A one", now), reminderItem("shared-2", "A two", now)), http.StatusOK)
	require.Equal(t, 2, a.Inserted)
	b := v.post(bob, "/v1/reminders/sync", remindersBody(reminderItem("shared-1", "B one", now), reminderItem("shared-2", "B two", now)), http.StatusOK)
	require.Equal(t, 2, b.Inserted)
	require.Equal(t, 0, b.Updated)
	again := v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("shared-1", "A one v2", now.Add(2*time.Minute))), http.StatusOK)
	require.Equal(t, syncCounts{0, 1, 0}, again)
}

func TestRemindersSyncValidation(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "reminders", true)
	items := make([]string, 1001)
	for i := range items {
		items[i] = fmt.Sprintf(`{"externalID":"b-%d","title":"t","completed":false}`, i)
	}
	cases := []struct {
		name, body string
		status     int
		message    string
	}{
		{"empty", `{"reminders":[]}`, 400, "reminders array required"},
		{"too many", remindersBody(items...), 413, "batch exceeds 1000 reminders"},
		{"nested missing key names only the key", `{"reminders":[{"externalID":"x","title":"t"}]}`, 400, "Coding key `completed` not found."},
		{"nested type mismatch names the path", `{"reminders":[{"externalID":"x","title":"t","completed":false},{"externalID":"y","title":"u","completed":false,"priority":"high"}]}`, 400, "Type mismatch for `reminders.1.priority` key, expected `Int` type."},
		{"nested null", `{"reminders":[{"externalID":"x","title":null,"completed":false}]}`, 400, "Value not found for `reminders.0.title` key."},
		{"element not an object", `{"reminders":[1]}`, 400, "Type mismatch for `reminders.0` key, expected `Dictionary<String, Any>` type."},
		{"not an array", `{"reminders":{}}`, 400, "Type mismatch for `reminders` key, expected `Array<Any>` type."},
	}
	for _, tc := range cases {
		r := v.do(alice, http.MethodPost, "/v1/reminders/sync", tc.body)
		require.Equal(t, tc.status, r.code, tc.name)
		require.JSONEq(t, `{"error":{"message":`+mustJSON(tc.message)+`}}`, string(r.body), tc.name)
	}
}

// MARK: PhotoIndexControllerTests

func TestEmbedSource(t *testing.T) {
	src := func(ocr *string, tags []string) *string {
		s, ok := apple.EmbedSource(ocr, tags)
		if !ok {
			return nil
		}
		return &s
	}
	str := func(s string) *string { return &s }
	require.Nil(t, src(nil, nil))
	require.Nil(t, src(str("   "), []string{}))
	require.Nil(t, src(str(""), nil))
	require.Equal(t, "Flight AB123 gate 7", *src(str("Flight AB123 gate 7"), nil))
	require.Equal(t, "Scene: receipt, document", *src(nil, []string{"receipt", "document"}))
	require.Equal(t, "Total $42.10\nScene: receipt", *src(str("Total $42.10"), []string{"receipt"}))
}

func TestPhotos403WithoutConsent(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.post(alice, "/v1/photos/index", `{"items":[{"assetLocalID":"asset-1","isScreenshot":true,"ocrText":"hi"}]}`, http.StatusForbidden)
}

func TestPhotosHappyPathInsertsTwo(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "photos", true)
	now := time.Now().UTC().Format(time.RFC3339)
	got := v.post(alice, "/v1/photos/index", `{"items":[
		{"assetLocalID":"asset-1","takenAt":"`+now+`","isScreenshot":true,"ocrText":"Flight AB123"},
		{"assetLocalID":"asset-2","takenAt":"`+now+`","isScreenshot":false}]}`, http.StatusOK)
	require.Equal(t, syncCounts{2, 0, 0}, got)
	// No OCR / tags ⇒ stored with a NULL embedding, but still upserted.
	var withVec, withoutVec int
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE embedding IS NOT NULL), count(*) FILTER (WHERE embedding IS NULL) FROM photo_index`).Scan(&withVec, &withoutVec))
	require.Equal(t, 1, withVec)
	require.Equal(t, 1, withoutVec)
}

func TestPhotosIdempotentReindex(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "photos", true)
	require.Equal(t, 1, v.post(alice, "/v1/photos/index", `{"items":[{"assetLocalID":"asset-x","isScreenshot":true,"ocrText":"v1"}]}`, http.StatusOK).Inserted)
	got := v.post(alice, "/v1/photos/index", `{"items":[{"assetLocalID":"asset-x","isScreenshot":true,"ocrText":"v2","sceneTags":["receipt"]}]}`, http.StatusOK)
	require.Equal(t, syncCounts{0, 1, 0}, got)
}

func TestPhotosSkipsEmptyAssetID(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "photos", true)
	got := v.post(alice, "/v1/photos/index", `{"items":[{"assetLocalID":"  ","isScreenshot":false,"ocrText":"no id"},{"assetLocalID":"asset-ok","isScreenshot":true,"ocrText":"ok"}]}`, http.StatusOK)
	require.Equal(t, 1, got.Inserted)
	require.Equal(t, 1, got.Skipped)
}

func TestPhotosTenantIsolation(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "photos", true)
	v.setConsent(bob, "photos", true)
	require.Equal(t, 1, v.post(alice, "/v1/photos/index", `{"items":[{"assetLocalID":"shared-asset","isScreenshot":true,"ocrText":"A"}]}`, http.StatusOK).Inserted)
	b := v.post(bob, "/v1/photos/index", `{"items":[{"assetLocalID":"shared-asset","isScreenshot":true,"ocrText":"B"}]}`, http.StatusOK)
	require.Equal(t, 1, b.Inserted)
	require.Equal(t, 0, b.Updated)
}

// The stored vector is DeterministicEmbeddingService's, and each embed adds
// max(1, (chars+3)/4) to this month's embedding_usage.
func TestPhotosDeterministicVectorAndUsage(t *testing.T) {
	v := setup(t, false, defaultOpts)
	v.setConsent(alice, "photos", true)
	v.post(alice, "/v1/photos/index", `{"items":[{"assetLocalID":"a","isScreenshot":true,"ocrText":"AB"},{"assetLocalID":"b","isScreenshot":true,"sceneTags":["x"]}]}`, http.StatusOK)
	var first, second float32
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT (embedding::real[])[1], (embedding::real[])[2] FROM photo_index WHERE asset_local_id = 'a'`).Scan(&first, &second))
	require.InDelta(t, float32('A')/127.5-1, first, 1e-7)
	require.InDelta(t, float32('B')/127.5-1, second, 1e-7)
	var tokens int64
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT tokens_used FROM embedding_usage WHERE tenant_id = $1`, alice).Scan(&tokens))
	require.Equal(t, apple.EstimateTokens("AB")+apple.EstimateTokens("Scene: x"), tokens)
}

func TestPhotosCapExceededKeepsEarlierRows(t *testing.T) {
	v := setup(t, true, defaultOpts) // the seed puts bob at the cap
	v.setConsent(bob, "photos", true)
	r := v.do(bob, http.MethodPost, "/v1/photos/index", `{"items":[{"assetLocalID":"b-1","isScreenshot":false},{"assetLocalID":"b-2","isScreenshot":false,"ocrText":"embed me"}]}`)
	require.Equal(t, http.StatusInternalServerError, r.code)
	require.Empty(t, r.body)
	var n int
	require.NoError(t, v.pool.QueryRow(context.Background(), `SELECT count(*) FROM photo_index WHERE tenant_id = $1`, bob).Scan(&n))
	require.Equal(t, 1, n, "b-1 was written before b-2's embed failed")
}

func TestPhotosRealProviderIsRefused(t *testing.T) {
	v := setup(t, false, apple.Options{EmbeddingProvider: "openrouter", EmbeddingMonthlyCap: 2_000_000})
	v.setConsent(alice, "photos", true)
	r := v.do(alice, http.MethodPost, "/v1/photos/index", `{"items":[{"assetLocalID":"a","isScreenshot":true,"ocrText":"x"}]}`)
	require.Equal(t, http.StatusInternalServerError, r.code, "no fake vectors when a real provider is configured")
}

// MARK: consent

func TestConsentSnapshotAndPurge(t *testing.T) {
	v := setup(t, true, defaultOpts)
	r := v.do(alice, http.MethodGet, "/v1/apple/consent", "")
	require.Equal(t, http.StatusOK, r.code)
	require.JSONEq(t, `{"consents":[
		{"domain":"health","allowed":false,"allowWrites":false},
		{"domain":"calendar","allowed":true,"allowWrites":true,"lastSyncAt":"2026-01-05T00:00:00Z"},
		{"domain":"reminders","allowed":false,"allowWrites":false},
		{"domain":"photos","allowed":false,"allowWrites":false},
		{"domain":"location","allowed":false,"allowWrites":false},
		{"domain":"files","allowed":false,"allowWrites":false}]}`, string(r.body))

	r = v.do(alice, http.MethodPut, "/v1/apple/consent", `{"allowWrites":true,"allowed":true,"domain":"reminders"}`)
	require.Contains(t, string(r.body), `{"allowWrites":true,"allowed":true,"domain":"reminders"}`)
	r = v.do(alice, http.MethodPut, "/v1/apple/consent", `{"domain":"reminders","allowed":true}`)
	require.Contains(t, string(r.body), `{"allowWrites":false,"allowed":true,"domain":"reminders"}`, "omitted allowWrites is false, not unchanged")

	ctx := context.Background()
	v.setConsent(alice, "photos", false)
	var photos int
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT count(*) FROM photo_index WHERE tenant_id = $1`, alice).Scan(&photos))
	require.Zero(t, photos, "revoking photos purges the index")

	v.setConsent(alice, "location", false)
	var place *string
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT last_location_place FROM users WHERE id = $1`, alice).Scan(&place))
	require.Nil(t, place, "revoking location clears the last known location")

	r = v.do(alice, http.MethodPut, "/v1/apple/consent", `{"domain":"bogus","allowed":true}`)
	require.Equal(t, http.StatusBadRequest, r.code)
	require.JSONEq(t, `{"error":{"message":"The given data was not valid input: Cannot initialize AppleDataDomain from invalid String value bogus"}}`, string(r.body))
}

// MARK: DeviceCommandQueueTests

func TestQueuedWriteIsPendingForOwnerUntilResult(t *testing.T) {
	v := setup(t, true, defaultOpts)
	pending := func(user uuid.UUID) []map[string]any {
		r := v.do(user, http.MethodGet, "/v1/devices/commands/pending", "")
		require.Equal(t, http.StatusOK, r.code)
		var out struct{ Commands []map[string]any }
		require.NoError(t, json.Unmarshal(r.body, &out))
		return out.Commands
	}
	got := pending(alice)
	require.Len(t, got, 3, "expired, delivered and unknown-kind rows are not handed out")
	require.Equal(t, "C0000000-0000-4000-8000-000000000001", got[0]["id"])
	require.Equal(t, "reminder_create", got[0]["kind"])
	require.Equal(t, "reminders", got[0]["domain"])
	require.Equal(t, map[string]any{"title": "Call mom", "due": ""}, got[0]["payload"])
	require.NotContains(t, got[2], "domain", "an unknown domain is omitted")
	require.Equal(t, map[string]any{}, got[2]["payload"], "an unreadable payload is {}")
	require.Len(t, pending(bob), 1)

	result := func(user uuid.UUID, body string) {
		r := v.do(user, http.MethodPost, "/v1/devices/command/anything/result", body)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
		require.Empty(t, r.body)
		require.Empty(t, r.ct)
	}
	// Bob posting a result for Alice's command id changes nothing.
	result(bob, `{"id":"C0000000-0000-4000-8000-000000000001","ok":true}`)
	require.Len(t, pending(alice), 3)

	result(alice, `{"id":"C0000000-0000-4000-8000-000000000001","ok":false,"error":"denied"}`)
	require.Len(t, pending(alice), 2)
	var ok *bool
	var reason *string
	require.NoError(t, v.pool.QueryRow(context.Background(),
		`SELECT result_ok, result_error FROM device_command_queue WHERE id = 'c0000000-0000-4000-8000-000000000001'`).Scan(&ok, &reason))
	require.False(t, *ok)
	require.Equal(t, "denied", *reason)
}

func TestExpiredWriteIsNeverHandedOut(t *testing.T) {
	v := setup(t, false, defaultOpts)
	_, err := v.pool.Exec(context.Background(), `INSERT INTO device_command_queue (id, tenant_id, kind, domain, payload, created_at, expires_at)
		VALUES (gen_random_uuid(), $1, 'calendar_create', 'calendar', '{"title":"Dentist"}', now(), now() - interval '60 seconds')`, alice)
	require.NoError(t, err)
	r := v.do(alice, http.MethodGet, "/v1/devices/commands/pending", "")
	require.JSONEq(t, `{"commands":[]}`, string(r.body))
}

func TestOnlyWritesQueueAndThePushSaysWhatIsWaiting(t *testing.T) {
	require.Equal(t, []string{"reminder_create", "calendar_create"}, apple.Queueable)
	require.Equal(t, "Open LuminaVault to add the reminder “Call mom”.", apple.Announcement("reminder_create", map[string]string{"title": "Call mom"}))
	require.Equal(t, "Open LuminaVault to add “an item” to your calendar.", apple.Announcement("calendar_create", map[string]string{}))
	require.Equal(t, "Open LuminaVault to finish a request from Lumina.", apple.Announcement("device_fetch", nil))
}

// Every success body must validate against api/openapi.yaml (decisions.md D2).
func TestResponsesConformToSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	require.NoError(t, err)
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	require.NoError(t, err)

	v := setup(t, true, defaultOpts)
	v.setConsent(alice, "reminders", true)
	v.setConsent(alice, "photos", true)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/apple/consent", ""},
		{http.MethodPut, "/v1/apple/consent", `{"domain":"health","allowed":true}`},
		{http.MethodPost, "/v1/reminders/sync", `{"reminders":[{"externalID":"x","title":"t","completed":false}]}`},
		{http.MethodPost, "/v1/photos/index", `{"items":[{"assetLocalID":"x","isScreenshot":false,"ocrText":"o"}]}`},
		{http.MethodGet, "/v1/devices/commands/pending", ""},
		{http.MethodPost, "/v1/devices/command/C0000000-0000-4000-8000-000000000002/result", `{"id":"C0000000-0000-4000-8000-000000000002","ok":true}`},
	} {
		r := v.do(alice, c.method, c.path, c.body)
		require.Equal(t, http.StatusOK, r.code, "%s %s: %s", c.method, c.path, r.body)
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		route, params, err := router.FindRoute(req)
		require.NoError(t, err, c.path)
		in := &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		out := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: in, Status: r.code,
			Header: http.Header{"Content-Type": {r.ct}},
		}
		out.SetBodyBytes(r.body)
		require.NoError(t, openapi3filter.ValidateResponse(ctx, out), "%s %s: %s", c.method, c.path, r.body)
	}
}

func TestFormatVectorMatchesSwiftFloatDescription(t *testing.T) {
	require.Equal(t, "[0.0,-1.0,0.5,0.003921628,1e-05]", apple.FormatVector([]float32{0, -1, 0.5, 0.003921628, 1e-05}))
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
