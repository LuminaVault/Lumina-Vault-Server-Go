package kanban

import (
	"encoding/json"
	"lumina/internal/wire"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// MARK: - CardExtraTests (3)

func TestJobConfigRoundTripsThroughJSON(t *testing.T) {
	spaceID := uuid.New()
	cron, domain, prompt, slug := "0 9 * * 1", "stocks", "Report AAPL", "job-report-aapl"
	original := cardExtra{Job: &cardJobConfig{
		Source: "vault", Cron: &cron, Domain: &domain, Prompt: &prompt,
		SpaceID: &wire.UUID{UUID: spaceID}, JobSlug: &slug,
		PromotedAt: &refDate{time.Unix(1_700_000_000, 0)},
	}}
	raw, err := json.Marshal(original)
	require.NoError(t, err)
	// Fluent's JSONEncoder form: reference-date seconds, uppercase UUID, nil omitted.
	require.Contains(t, string(raw), `"promotedAt":721692800`)
	require.Contains(t, string(raw), strings.ToUpper(spaceID.String()))
	require.NotContains(t, string(raw), "runAt")

	decoded, err := decodeExtra(raw)
	require.NoError(t, err)
	require.Equal(t, "0 9 * * 1", *decoded.Job.Cron)
	require.Equal(t, spaceID, decoded.Job.SpaceID.UUID)
	require.Equal(t, "job-report-aapl", *decoded.Job.JobSlug)
	require.True(t, decoded.Job.PromotedAt.Equal(time.Unix(1_700_000_000, 0)))
}

func TestEmptyExtraRoundTripsWithNilJob(t *testing.T) {
	raw, err := json.Marshal(cardExtra{})
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(raw))
	decoded, err := decodeExtra(raw)
	require.NoError(t, err)
	require.Nil(t, decoded.Job)
	none, err := decodeExtra(nil)
	require.NoError(t, err)
	require.Nil(t, none)
}

// `source defaults to vault` is CardJobConfig's init default; the Go port
// sets it where Swift calls CardJobConfig() (promote), covered by
// TestPromoteAppliesInlineRequestConfig. Decoding still requires it, as
// synthesized Codable does.
func TestSourceIsRequiredOnDecode(t *testing.T) {
	_, err := decodeExtra([]byte(`{"job":{"cron":"0 9 * * *"}}`))
	require.Error(t, err)
	_, err = decodeExtra([]byte(`{"job":{"source":null}}`))
	require.Error(t, err)
	ex, err := decodeExtra([]byte(`{"job":null}`))
	require.NoError(t, err)
	require.Nil(t, ex.Job)
}

func TestRefDateFraction(t *testing.T) {
	var d refDate
	require.NoError(t, json.Unmarshal([]byte(`815216400.5`), &d))
	require.Equal(t, time.Date(2026, 11, 1, 9, 0, 0, 500_000_000, time.UTC), d.UTC())
	b, err := json.Marshal(d)
	require.NoError(t, err)
	require.Equal(t, "815216400.5", string(b))
}

// MARK: - CronExpression parse (validation only)

func TestValidCron(t *testing.T) {
	for _, s := range []string{
		"0 9 * * 1", "  0 9 * * 1 ", "*/5 * * * *", "0,30 9-17/2 1-31 */3 1-5", "0 0 1 1 0",
		"1,,2 * * * *", ", * * * *", "/5 * * * *", "+5 * * * *", "0\t9 * *\n*",
	} {
		require.True(t, validCron(s), s)
	}
	for _, s := range []string{
		"not a cron", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 7",
		"5-1 * * * *", "*/0 * * * *", "1/2/3 * * * *", "1-2-3 * * * *", "-5 * * * *", "0 9 * *", "0 9 * * 1 2",
		"a * * * *", "/ * * * *",
	} {
		require.False(t, validCron(s), s)
	}
}

// MARK: - JobAuthoring helpers

func TestJobSlug(t *testing.T) {
	require.Equal(t, "job-weekly-digest", jobSlug("Weekly Digest"))
	require.Equal(t, "job-one-shot", jobSlug("One Shot!"))
	require.Equal(t, "job-daily-stock-prices", jobSlug("  Daily -- Stock   Prices "))
	require.Equal(t, "job-a-very-long-card-title-that-keeps-going-", jobSlug("  A very long card title that keeps going, and going — and going on  "))
	require.Equal(t, "job-résumé-café", jobSlug("Résumé Café"))
	random := jobSlug("!!!")
	require.Regexp(t, `^job-[0-9a-f]{8}$`, random)
}

func TestSkillMarkdown(t *testing.T) {
	cron, domain := "0 9 * * 1", "life"
	md := skillMarkdown("job-x", "X", &cron, &domain, "Do it")
	require.Equal(t, `---
name: job-x
description: X
license: MIT
allowed-tools: session_search vault_read weather_forecast mail_inbox_recent
metadata:
  capability: medium
  schedule: "0 9 * * 1"
  on_event: []
  daily_run_cap: { trial: 1, pro: 6, ultimate: 24 }
  maxInputTokens: 8000
  outputs:
    - kind: chat_message
---
You are a LuminaVault job: "X".
Domain: life.

Each run, do exactly this and present the result for the user:
Do it

Output a SINGLE JSON object: {"markdown":"<short summary>","blocks":[ ... ]}
using the Lumina block schema (statCard, lineChart, barChart, list, table,
keyValue, heading, paragraph, badge). Choose the blocks that best present
this domain's data — e.g. statCard + lineChart for metrics, table/list for
collections, paragraph for prose. Keep it concise and scannable. Output
only the JSON, no code fence.`, md)
	oneShot := skillMarkdown("job-x", "X", nil, nil, "Do it")
	require.NotContains(t, oneShot, "schedule:")
	require.Contains(t, oneShot, "  capability: medium\n  on_event: []\n")
	require.Contains(t, oneShot, "You are a LuminaVault job: \"X\".\n\n\nEach run")
}

func TestTrimSets(t *testing.T) {
	require.Equal(t, "0 9 * * 1", trimSwiftWhitespace("\t 0 9 * * 1 "))
	require.Equal(t, "\nx\n", trimSwiftWhitespace(" \nx\n "), ".whitespaces keeps newlines")
	require.Empty(t, trimSwiftWhitespaceAndNewlines("  \n  "))
}
