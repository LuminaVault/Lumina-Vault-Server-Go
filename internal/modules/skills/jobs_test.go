package skills

import (
	"lumina/internal/jobauthor"
	"lumina/internal/skills"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// JobsTests: the classifier parse, the slug, and SKILL.md authoring, with no
// database or model. The authoring cases parse the generated manifest back
// through the real parser, to prove a created job is a valid vault skill.

// JobsTests: "classifier parses a recurring job".
func TestJobsClassifierParsesARecurringJob(t *testing.T) {
	p := parseProposal(`{"isJob":true,"title":"AAPL watch","cron":"0 8 * * *","scheduleHuman":"Every day at 8 AM","domain":"stocks","spec":"Report AAPL + TSLA price"}`)
	require.True(t, p.IsJob)
	require.Equal(t, "0 8 * * *", *p.Cron)
	require.Equal(t, "stocks", *p.Domain)
	require.Equal(t, "AAPL watch", *p.Title)
}

// JobsTests: "classifier tolerates a json fence, rejects non-jobs and garbage".
func TestJobsClassifierToleratesAFenceAndRejectsGarbage(t *testing.T) {
	require.True(t, parseProposal("```json\n{\"isJob\":true,\"cron\":\"0 9 * * 1\"}\n```").IsJob)
	require.False(t, parseProposal(`{"isJob":false}`).IsJob)
	require.False(t, parseProposal("not json at all").IsJob)
}

// JobsTests: "slug is namespaced and filesystem-safe".
func TestJobsSlugIsNamespacedAndFilesystemSafe(t *testing.T) {
	require.Equal(t, "job-daily-stock-prices", jobauthor.Slug("Daily Stock Prices!"))
	require.True(t, strings.HasPrefix(jobauthor.Slug("AI / Papers"), "job-"))
}

// JobsTests: "authored SKILL.md parses as a valid scheduled vault skill".
func TestJobsAuthoredSkillParsesAsAScheduledVaultSkill(t *testing.T) {
	cron, domain := "0 8 * * *", "stocks"
	md := jobauthor.Markdown("job-x", "Stock Watch", &cron, &domain, "Report prices")
	mf, err := skills.Parse(skills.SourceVault, md)
	require.NoError(t, err)
	require.Equal(t, "job-x", mf.Name)
	require.NotNil(t, mf.Schedule)
	require.Equal(t, "0 8 * * *", *mf.Schedule)
}

// JobsTests: "one-shot SKILL.md omits the schedule (run_at lives on skills_state)".
func TestJobsOneShotSkillOmitsTheSchedule(t *testing.T) {
	md := jobauthor.Markdown("job-y", "One Off", nil, nil, "Do it once")
	require.NotContains(t, md, "schedule:")
	mf, err := skills.Parse(skills.SourceVault, md)
	require.NoError(t, err)
	require.Equal(t, "job-y", mf.Name)
	require.Nil(t, mf.Schedule)
}
