package admin

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Ports MetricsSecretMiddlewareTests.swift (5).
func TestMetricsSecretMiddleware(t *testing.T) {
	serve := func(secret, authorization string) *httptest.ResponseRecorder {
		e := echo.New()
		m := &Module{opt: Options{MetricsSecret: secret}}
		e.GET("/internal/metrics", func(c *echo.Context) error { return c.String(http.StatusOK, "ok") }, m.requireMetricsSecret)
		req := httptest.NewRequest(http.MethodGet, "/internal/metrics", nil)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}
	t.Run("missing bearer is rejected", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, serve("s3cret", "").Code)
	})
	t.Run("wrong bearer is rejected", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, serve("s3cret", "Bearer nope").Code)
	})
	t.Run("bare token without Bearer prefix is rejected", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, serve("s3cret", "s3cret").Code)
	})
	t.Run("correct bearer is accepted", func(t *testing.T) {
		rec := serve("s3cret", "Bearer s3cret")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "ok", rec.Body.String())
	})
	t.Run("empty configured secret disables the route", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, serve("", "Bearer ").Code)
	})
	t.Run("the scheme is case-sensitive, as hasPrefix is", func(t *testing.T) {
		require.Equal(t, http.StatusUnauthorized, serve("s3cret", "bearer s3cret").Code)
	})
}

// AdminTokenMiddleware: Swift's tests of it live with llmadmin and community; this
// module carries its own copy.
func TestAdminToken(t *testing.T) {
	serve := func(token, header string) int {
		e := echo.New()
		m := &Module{opt: Options{AdminToken: token}}
		e.POST("/x", func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) }, m.requireAdminToken)
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		if header != "" {
			req.Header.Set("X-Admin-Token", header)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}
	require.Equal(t, http.StatusNotFound, serve("", "anything"))
	require.Equal(t, http.StatusUnauthorized, serve("tok", ""))
	require.Equal(t, http.StatusUnauthorized, serve("tok", "nope"))
	require.Equal(t, http.StatusNoContent, serve("tok", "tok"))
}

// Ports UsageEventsRetentionTests.swift (6).
func TestUsageEventsRetentionPolicy(t *testing.T) {
	t.Run("zero days disables retention", func(t *testing.T) {
		require.False(t, NewRetentionPolicy(0, DefaultRetentionBatchSize).Enabled())
		require.False(t, NewRetentionPolicy(-30, DefaultRetentionBatchSize).Enabled())
	})
	t.Run("a positive window enables retention", func(t *testing.T) {
		require.True(t, NewRetentionPolicy(90, DefaultRetentionBatchSize).Enabled())
	})
	t.Run("the cutoff is the window measured back from now", func(t *testing.T) {
		now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
		cutoff := NewRetentionPolicy(90, DefaultRetentionBatchSize).Cutoff(now)
		require.NotNil(t, cutoff)
		require.Equal(t, "2026-06-14T00:00:00Z", cutoff.UTC().Format(time.RFC3339))
	})
	t.Run("a disabled policy has no cutoff", func(t *testing.T) {
		require.Nil(t, NewRetentionPolicy(0, DefaultRetentionBatchSize).Cutoff(time.Now()))
	})
	t.Run("the batch size is bounded and positive", func(t *testing.T) {
		p := NewRetentionPolicy(90, DefaultRetentionBatchSize)
		require.Positive(t, p.BatchSize)
		require.LessOrEqual(t, p.BatchSize, MaxRetentionBatchSize)
	})
	t.Run("an absurd configured batch size is clamped", func(t *testing.T) {
		require.Equal(t, 10000, NewRetentionPolicy(90, 10_000_000).BatchSize)
		require.Equal(t, 1, NewRetentionPolicy(90, 0).BatchSize)
	})
	t.Run("a disabled summary leaves cutoff out", func(t *testing.T) {
		b, err := json.Marshal(RetentionSummary{RetentionDays: 0})
		require.NoError(t, err)
		require.JSONEq(t, `{"enabled":false,"retentionDays":0,"deleted":0,"moreRemaining":false}`, string(b))
	})
}

// Ports MemoryScoringTests.swift (5).
func TestMemoryScoring(t *testing.T) {
	cfg := DefaultScoringConfig
	now := time.Now()
	t.Run("fresh untouched memory scores one", func(t *testing.T) {
		require.InDelta(t, 1.0, ComputeScore(0, 0, &now, now, cfg), 1e-9)
	})
	t.Run("recency decays at halflife", func(t *testing.T) {
		ago := now.Add(-30 * 24 * time.Hour)
		require.InDelta(t, math.Exp(-1), ComputeScore(0, 0, &ago, now, cfg), 1e-6)
	})
	t.Run("access and query hits dominate old row", func(t *testing.T) {
		old := now.Add(-365 * 24 * time.Hour)
		require.Greater(t, ComputeScore(50, 20, &old, now, cfg), 15.0)
	})
	t.Run("nil created at treated as now", func(t *testing.T) {
		require.InDelta(t, 1.0, ComputeScore(0, 0, nil, now, cfg), 1e-9)
	})
	t.Run("zero age never negative", func(t *testing.T) {
		future := now.Add(24 * time.Hour)
		require.LessOrEqual(t, ComputeScore(0, 0, &future, now, cfg), 1.0+1e-9)
	})
}

func TestSwiftDouble(t *testing.T) {
	for in, want := range map[float64]string{
		2: "2.0", 3: "3.0", 2592000: "2592000.0", 0.2: "0.2", 8420: "8420.0", 72.5: "72.5",
		0.00001: "1e-05", 1e16: "1e+16", 0: "0.0", -4: "-4.0",
	} {
		require.Equal(t, want, swiftDouble(in), "%v", in)
	}
}

// Ports HealthCorrelationServiceTests.swift's prompt test; the other five need a
// database and are in postgres_test.go.
func TestCorrelationPrompt(t *testing.T) {
	t.Run("prompt includes health and memory sections", func(t *testing.T) {
		now := time.Now()
		at := now.Add(-time.Hour)
		value, unit := 8420.0, "count"
		prompt := BuildCorrelationPrompt(
			[]HealthEvent{{EventType: "steps", ValueNumeric: &value, Unit: &unit, RecordedAt: at}},
			[]PromptMemory{{Content: "Slept poorly. Skipped the morning run.", CreatedAt: &at}}, now)
		for _, want := range []string{"## Health events", "steps", "8420", "## Memories", "Slept poorly"} {
			require.Contains(t, prompt, want)
		}
	})
	t.Run("the exact shape", func(t *testing.T) {
		now := time.Date(2026, 11, 11, 12, 0, 0, 500, time.UTC)
		at := time.Date(2026, 11, 10, 8, 30, 0, 0, time.UTC)
		n, text := 7.5, "ok"
		long := strings.Repeat("é", 300) + "\nnext"
		prompt := BuildCorrelationPrompt(
			[]HealthEvent{
				{EventType: "sleep_hours", ValueNumeric: &n, RecordedAt: at},
				{EventType: "mood", ValueText: &text, RecordedAt: at},
				{EventType: "blank", RecordedAt: at},
			},
			[]PromptMemory{{Content: "a\nb"}, {Content: long, CreatedAt: &at}}, now)
		require.Equal(t, "Window ending 2026-11-11T12:00:00Z (last 7 days).\n\n"+
			"## Health events (3)\n"+
			"- [2026-11-10T08:30:00Z] sleep_hours: 7.5\n"+
			"- [2026-11-10T08:30:00Z] mood: ok\n"+
			"- [2026-11-10T08:30:00Z] blank: ?\n"+
			"\n## Memories (2)\n"+
			"- [?] a b\n"+
			"- [2026-11-10T08:30:00Z] "+strings.Repeat("é", 280)+"\n"+
			"\nFind correlations. Respond per system instructions.", prompt)
	})
}

func TestDecodeFirstContent(t *testing.T) {
	got, err := decodeFirstContent([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	require.NoError(t, err)
	require.Equal(t, "hi", got)
	got, err = decodeFirstContent([]byte(`{"choices":[{"message":{"role":"assistant","content":null}}]}`))
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = decodeFirstContent([]byte(`{"choices":[]}`))
	require.NoError(t, err)
	require.Empty(t, got)
	for _, bad := range []string{`{}`, `{"choices":[{}]}`, `{"choices":[{"message":{"content":"x"}}]}`, `nope`} {
		_, err := decodeFirstContent([]byte(bad))
		require.Error(t, err, bad)
	}
}
