package apple_test

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/dbtest"
	"lumina/internal/embedding"
	"lumina/internal/httpserver"
	"lumina/internal/migrations"
	"lumina/internal/modules/apple"
	"lumina/internal/posthog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// posthogStub is a stand-in PostHog; these tests never reach the real one.
func posthogStub(t *testing.T, status int) (*posthog.Client, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	events := []map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			APIKey string           `json:"api_key"`
			Batch  []map[string]any `json:"batch"`
		}
		if err := json.Unmarshal(b, &body); err == nil && body.APIKey == "phc_stub" {
			mu.Lock()
			events = append(events, body.Batch...)
			mu.Unlock()
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	client := posthog.New("phc_stub", srv.URL, posthog.Options{})
	return client, func() []map[string]any {
		client.Flush(context.Background())
		mu.Lock()
		defer mu.Unlock()
		return events
	}
}

func setupAnalytics(t *testing.T, analytics *posthog.Client) env {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	seed, err := os.ReadFile("../../../parity/seed/base.sql")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, string(seed))
	require.NoError(t, err)
	e := httpserver.New(config.Config{}, httpserver.Deps{
		Auth: auth.Middleware(keys, auth.PGUsers(pool)),
		Modules: []httpserver.Module{apple.New(pool, apple.Options{
			Embedder:  embedding.New(pool, defaultOpts, nil),
			Analytics: analytics,
		})},
	})
	return env{t: t, e: e, pool: pool}
}

const aliceDistinctID = "A11CE000-0000-4000-8000-000000000001"

func requireEvent(t *testing.T, got map[string]any, name string, props map[string]any) {
	t.Helper()
	want := map[string]any{"$lib": "luminavault-server"}
	for k, v := range props {
		want[k] = v
	}
	require.Equal(t, name, got["event"])
	require.Equal(t, aliceDistinctID, got["distinct_id"])
	require.Equal(t, want, got["properties"])
}

func counts(inserted, updated, skipped float64) map[string]any {
	return map[string]any{"inserted_count": inserted, "updated_count": updated, "skipped_count": skipped}
}

// Swift AppleConsentController.swift:53: every accepted PUT, with
// `allowWrites ?? false` reported as writes_allowed.
func TestConsentCapturesUpdate(t *testing.T) {
	client, captured := posthogStub(t, http.StatusOK)
	v := setupAnalytics(t, client)
	for _, body := range []string{
		`{"domain":"reminders","allowed":true,"allowWrites":true}`,
		`{"domain":"calendar","allowed":true}`,
		`{"domain":"photos","allowed":false}`,
	} {
		r := v.do(alice, http.MethodPut, "/v1/apple/consent", body)
		require.Equal(t, http.StatusOK, r.code, string(r.body))
	}
	events := captured()
	require.Len(t, events, 3)
	requireEvent(t, events[0], "apple_data_consent_updated", map[string]any{"domain": "reminders", "allowed": true, "writes_allowed": true})
	requireEvent(t, events[1], "apple_data_consent_updated", map[string]any{"domain": "calendar", "allowed": true, "writes_allowed": false})
	requireEvent(t, events[2], "apple_data_consent_updated", map[string]any{"domain": "photos", "allowed": false, "writes_allowed": false})
}

func TestConsentCapturesNothingOnABadBody(t *testing.T) {
	client, captured := posthogStub(t, http.StatusOK)
	v := setupAnalytics(t, client)
	r := v.do(alice, http.MethodPut, "/v1/apple/consent", `{"domain":"nope","allowed":true}`)
	require.Equal(t, http.StatusBadRequest, r.code, string(r.body))
	require.Empty(t, captured())
}

// Swift AppleRemindersController.swift:105: one event per accepted batch, even
// when every item was skipped.
func TestRemindersSyncCaptures(t *testing.T) {
	client, captured := posthogStub(t, http.StatusOK)
	v := setupAnalytics(t, client)
	v.setConsent(alice, "reminders", true)
	require.Len(t, captured(), 1, "the consent PUT")
	now := time.Now()
	v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-1", "A", now), reminderItem("r-2", "B", now), reminderItem(" ", "skipped", now)), http.StatusOK)
	v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-1", "A again", now.Add(time.Minute))), http.StatusOK)
	v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem(" ", "skipped", now)), http.StatusOK)
	events := captured()[1:]
	require.Len(t, events, 3)
	requireEvent(t, events[0], "apple_reminders_synced", counts(2, 0, 1))
	requireEvent(t, events[1], "apple_reminders_synced", counts(0, 1, 0))
	requireEvent(t, events[2], "apple_reminders_synced", counts(0, 0, 1))
}

func TestRemindersSyncCapturesNothingWhenRejected(t *testing.T) {
	client, captured := posthogStub(t, http.StatusOK)
	v := setupAnalytics(t, client)
	v.post(alice, "/v1/reminders/sync", remindersBody(reminderItem("r-1", "A", time.Now())), http.StatusForbidden)
	require.Empty(t, captured())
	v.setConsent(alice, "reminders", true)
	v.post(alice, "/v1/reminders/sync", remindersBody(), http.StatusBadRequest)
	require.Len(t, captured(), 1, "only the consent PUT")
}

// Swift PhotoIndexController.swift:96: unlike reminders it has no empty check,
// so an empty batch is an event with zero counts.
func TestPhotosIndexCaptures(t *testing.T) {
	client, captured := posthogStub(t, http.StatusOK)
	v := setupAnalytics(t, client)
	v.setConsent(alice, "photos", true)
	require.Len(t, captured(), 1, "the consent PUT")
	v.post(alice, "/v1/photos/index", `{"items":[{"assetLocalID":"p-1","isScreenshot":false},{"assetLocalID":"p-2","isScreenshot":false},{"assetLocalID":" ","isScreenshot":false}]}`, http.StatusOK)
	v.post(alice, "/v1/photos/index", `{"items":[{"assetLocalID":"p-1","isScreenshot":true}]}`, http.StatusOK)
	v.post(alice, "/v1/photos/index", `{"items":[]}`, http.StatusOK)
	events := captured()[1:]
	require.Len(t, events, 3)
	requireEvent(t, events[0], "photos_indexed", counts(2, 0, 1))
	requireEvent(t, events[1], "photos_indexed", counts(0, 1, 0))
	requireEvent(t, events[2], "photos_indexed", counts(0, 0, 0))
}

func TestPhotosIndexCapturesNothingWhenForbidden(t *testing.T) {
	client, captured := posthogStub(t, http.StatusOK)
	v := setupAnalytics(t, client)
	v.post(alice, "/v1/photos/index", `{"items":[]}`, http.StatusForbidden)
	require.Empty(t, captured())
}

// PostHog failing never touches the response; setup's nil client works the same.
func TestConsentUnaffectedByPostHog(t *testing.T) {
	client, captured := posthogStub(t, http.StatusInternalServerError)
	v := setupAnalytics(t, client)
	v.setConsent(alice, "reminders", true)
	require.Len(t, captured(), 1, "PostHog was asked and said 500")
}
