package calendar_test

import (
	"context"
	"encoding/json"
	"io"
	"lumina/internal/ids"
	"lumina/internal/posthog"
	"net/http"
	"net/http/httptest"
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

func requireEvent(t *testing.T, got map[string]any, name, distinctID string, props map[string]any) {
	t.Helper()
	want := map[string]any{"$lib": "luminavault-server"}
	for k, v := range props {
		want[k] = v
	}
	require.Equal(t, name, got["event"])
	require.Equal(t, distinctID, got["distinct_id"])
	require.Equal(t, want, got["properties"])
}

func TestAnalytics(t *testing.T) {
	pool := startDB(t)
	newAnalyticsEnv := func(t *testing.T, o opts) (env, func() []map[string]any) {
		t.Helper()
		client, captured := posthogStub(t, http.StatusOK)
		o.analytics = client
		return newEnv(t, pool, o), captured
	}

	// Swift CalendarController.swift:67 fires on a built consent URL, whatever
	// comes of the consent. GmailController.swift:49 likewise.
	for _, c := range []struct{ path, event string }{
		{"/v1/calendar/connect", "calendar_connected"},
		{"/v1/mail/gmail/connect", "gmail_connect_started"},
	} {
		t.Run(c.event, func(t *testing.T) {
			v, captured := newAnalyticsEnv(t, opts{})
			u := v.user("trial")
			r := v.do(u, http.MethodPost, c.path, "")
			require.Equal(t, http.StatusOK, r.code, r.String())
			events := captured()
			require.Len(t, events, 1)
			requireEvent(t, events[0], c.event, ids.Upper(u), nil)
		})
		t.Run(c.event+" not on a rejected return", func(t *testing.T) {
			v, captured := newAnalyticsEnv(t, opts{})
			r := v.do(v.user("trial"), http.MethodPost, c.path+"?returnTo=https%3A%2F%2Fevil.example%2F", "")
			require.Equal(t, http.StatusBadRequest, r.code, r.String())
			require.Empty(t, captured())
		})
		t.Run(c.event+" not when Google is not configured", func(t *testing.T) {
			v, captured := newAnalyticsEnv(t, opts{unconfigured: true})
			r := v.do(v.user("trial"), http.MethodPost, c.path, "")
			require.Equal(t, http.StatusServiceUnavailable, r.code, r.String())
			require.Empty(t, captured())
		})
	}

	// Swift CalendarController.swift:118: `!(body.attendees ?? []).isEmpty`, on
	// the attendees as sent.
	t.Run("calendar_event_created", func(t *testing.T) {
		v, captured := newAnalyticsEnv(t, opts{})
		u := v.user("trial")
		v.account(u, scopeCalendar, "connected", "rt", "at", time.Now().Add(time.Hour))
		var got map[string]any
		v.google.set(func(s *stubGoogle) { s.insert = insertOK(t, &got) })
		for _, body := range []string{
			createBody,
			`{"title":"x","startsAt":"2099-01-01T10:00:00Z","endsAt":"2099-01-01T12:00:00Z"}`,
			`{"title":"x","startsAt":"2099-01-01T10:00:00Z","endsAt":"2099-01-01T12:00:00Z","attendees":[]}`,
			`{"title":"x","startsAt":"2099-01-01T10:00:00Z","endsAt":"2099-01-01T12:00:00Z","attendees":[""]}`,
		} {
			r := v.do(u, http.MethodPost, "/v1/calendar/events", body)
			require.Equal(t, http.StatusOK, r.code, r.String())
		}
		events := captured()
		require.Len(t, events, 4)
		for i, want := range []bool{true, false, false, true} {
			requireEvent(t, events[i], "calendar_event_created", ids.Upper(u), map[string]any{"has_attendees": want})
		}
	})

	t.Run("calendar_event_created only on success", func(t *testing.T) {
		v, captured := newAnalyticsEnv(t, opts{})
		notConnected := v.user("trial")
		r := v.do(notConnected, http.MethodPost, "/v1/calendar/events", createBody)
		require.Equal(t, http.StatusConflict, r.code, r.String())
		r = v.do(notConnected, http.MethodPost, "/v1/calendar/events", `{"title":" ","startsAt":"2099-01-01T10:00:00Z","endsAt":"2099-01-01T12:00:00Z"}`)
		require.Equal(t, http.StatusBadRequest, r.code, r.String())

		googleDown := v.user("trial")
		v.account(googleDown, scopeCalendar, "connected", "rt", "at", time.Now().Add(time.Hour))
		v.google.set(func(s *stubGoogle) {
			s.insert = func(map[string]any) (int, string) { return http.StatusBadGateway, "boom" }
		})
		r = v.do(googleDown, http.MethodPost, "/v1/calendar/events", createBody)
		require.Equal(t, http.StatusInternalServerError, r.code, r.String())
		require.Empty(t, captured())
	})

	// Swift AppleCalendarController.swift:142: one event per batch, even when
	// every item was skipped.
	t.Run("apple_calendar_synced", func(t *testing.T) {
		v, captured := newAnalyticsEnv(t, opts{})
		u := v.user("trial")
		v.consent(u)
		require.Equal(t, http.StatusOK, v.do(u, http.MethodPost, "/v1/calendar/sync", syncBody(ev("a", "A"), ev("b", "B"), ev("  ", "skipped"))).code)
		require.Equal(t, http.StatusOK, v.do(u, http.MethodPost, "/v1/calendar/sync", syncBody(ev("a", "A again"))).code)
		require.Equal(t, http.StatusOK, v.do(u, http.MethodPost, "/v1/calendar/sync", syncBody(ev("  ", "skipped"))).code)
		events := captured()
		require.Len(t, events, 3)
		for i, want := range [][3]float64{{2, 0, 1}, {0, 1, 0}, {0, 0, 1}} {
			requireEvent(t, events[i], "apple_calendar_synced", ids.Upper(u), map[string]any{
				"inserted_count": want[0], "updated_count": want[1], "skipped_count": want[2],
			})
		}
	})

	t.Run("apple_calendar_synced not on a rejected batch", func(t *testing.T) {
		v, captured := newAnalyticsEnv(t, opts{})
		u := v.user("trial")
		require.Equal(t, http.StatusForbidden, v.do(u, http.MethodPost, "/v1/calendar/sync", syncBody(ev("a", "A"))).code)
		v.consent(u)
		require.Equal(t, http.StatusBadRequest, v.do(u, http.MethodPost, "/v1/calendar/sync", syncBody()).code)
		require.Empty(t, captured())
	})

	// PostHog failing never touches the response, and an unset token (a nil
	// client, which is what the other tests use) works the same.
	t.Run("PostHog failing changes nothing", func(t *testing.T) {
		client, captured := posthogStub(t, http.StatusInternalServerError)
		v := newEnv(t, pool, opts{analytics: client})
		u := v.user("trial")
		v.consent(u)
		r := v.do(u, http.MethodPost, "/v1/calendar/sync", syncBody(ev("a", "A")))
		require.Equal(t, http.StatusOK, r.code, r.String())
		require.Len(t, captured(), 1, "PostHog was asked and said 500")
	})
}
