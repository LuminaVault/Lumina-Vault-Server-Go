package skillscron

import (
	"encoding/json"
	"errors"
	"lumina/internal/hermesdash"
	"lumina/internal/ssrfguard"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The golden files are CronBridgeService's own parseSchedule, parseTimeOfDay,
// deriveName and parse, compiled out of the Swift source and run on Swift 6.4.1
// (Darwin Foundation) over the inputs they list.

type scheduleGolden struct {
	Text  string  `json:"text"`
	Name  string  `json:"name"`
	Cron  *string `json:"cron"`
	Human *string `json:"human"`
	TOD   *string `json:"tod"`
}

func TestDeterministicScheduleMatchesSwift(t *testing.T) {
	raw, err := os.ReadFile("testdata/swift-schedule-golden.json")
	require.NoError(t, err)
	var rows []scheduleGolden
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.NotEmpty(t, rows)
	for _, row := range rows {
		assert.Equal(t, row.Name, DeriveName(row.Text), "deriveName(%q)", row.Text)
		parsed, ok := ParseSchedule(row.Text)
		if row.Cron == nil {
			assert.False(t, ok, "parseSchedule(%q) = %+v, Swift: nil", row.Text, parsed)
		} else if assert.True(t, ok, "parseSchedule(%q): Swift %q", row.Text, *row.Cron) {
			assert.Equal(t, *row.Cron, parsed.Cron, "cron of %q", row.Text)
			assert.Equal(t, *row.Human, parsed.Human, "human of %q", row.Text)
		}
		h, m, ok := ParseTimeOfDay(swiftstr.Lowercased(row.Text))
		if row.TOD == nil {
			assert.False(t, ok, "parseTimeOfDay(%q)", row.Text)
		} else if assert.True(t, ok, "parseTimeOfDay(%q)", row.Text) {
			assert.Equal(t, *row.TOD, strconv.Itoa(h)+":"+strconv.Itoa(m), "parseTimeOfDay(%q)", row.Text)
		}
	}
}

type parseGolden struct {
	Input string `json:"input"`
	Jobs  string `json:"jobs"`
}

func TestParseMatchesSwift(t *testing.T) {
	raw, err := os.ReadFile("testdata/swift-parse-golden.json")
	require.NoError(t, err)
	var rows []parseGolden
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.NotEmpty(t, rows)
	for _, row := range rows {
		got, err := json.Marshal(Parse([]byte(row.Input)))
		require.NoError(t, err)
		assert.JSONEq(t, row.Jobs, string(got), "parse(%s)", row.Input)
	}
}

func TestFormatHuman(t *testing.T) {
	for _, c := range []struct {
		h, m int
		want string
	}{{0, 0, "12:00 AM"}, {9, 5, "9:05 AM"}, {12, 0, "12:00 PM"}, {13, 30, "1:30 PM"}, {23, 59, "11:59 PM"}, {7, 99, "7:99 AM"}} {
		assert.Equal(t, c.want, formatHuman(c.h, c.m))
	}
}

func TestDeliverKeywords(t *testing.T) {
	for text, want := range map[string]string{
		"post it on Telegram and Discord": "telegram",
		"DISCORD please":                  "discord",
		"slack signal":                    "slack",
		"Signal me":                       "signal",
		"e-mail me":                       "email",
		"email":                           "email",
		"just do it":                      "origin",
	} {
		assert.Equal(t, want, deliverTarget(text), text)
	}
}

// Ports HermesDashboardClientTests' `cron bridge keeps its legacy BYO error
// codes`, plus the cases byoError's switch has beyond it.
func TestByoErrorKeepsLegacyCodes(t *testing.T) {
	code := func(err error) (int, string) {
		var we *wire.Error
		require.ErrorAs(t, err, &we)
		return we.Status, we.Message
	}
	status, msg := code(ByoError(hermesdash.HTTPStatus(500, "/api/cron/jobs"), "http"))
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Equal(t, "byo_cron_http_500", msg)
	_, msg = code(ByoError(hermesdash.ErrUnauthorized, "create"))
	assert.Equal(t, "byo_cron_create_401", msg)
	_, msg = code(ByoError(hermesdash.ErrAuthModeUnsupported, "http"))
	assert.Equal(t, "hermes_dashboard_auth_mode_unsupported", msg)
	_, msg = code(ByoError(hermesdash.DashboardUnreachable("/api/cron/jobs"), "http"))
	assert.Equal(t, "hermes_dashboard_unreachable", msg)
	_, msg = code(ByoError(hermesdash.BodyTooLarge("/api/cron/jobs", 1), "http"))
	assert.Equal(t, "hermes_mirror_body_too_large", msg)

	// Not a transport error: passes through untouched (Swift's catch is typed).
	rejection := &ssrfguard.Rejection{Kind: ssrfguard.Loopback, Value: "127.0.0.1"}
	assert.Same(t, rejection, ByoError(rejection, "http"))
	plain := errors.New("boom")
	assert.Same(t, plain, ByoError(plain, "http"))
}
