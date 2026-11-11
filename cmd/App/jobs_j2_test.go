package main

import (
	"lumina/internal/config"
	"lumina/internal/jobs"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func j2Names(cfg config.Config) []string {
	var names []string
	for _, j := range j2Jobs(nil, cfg, nil) {
		names = append(names, j.Name)
	}
	slices.Sort(names)
	return names
}

// J2 registers each job under Swift's start conditions with the Swift type name,
// and LV_GO_JOBS naming all of them boots.
func TestJ2JobsFollowSwiftStartConditions(t *testing.T) {
	all := config.Config{LVEnvironment: "production", LearnedDigestEnabled: "true", MeetingBriefEnabled: "true"}
	want := []string{"LearnedDigestScheduler", "MeetingBriefScheduler"}
	require.Equal(t, want, j2Names(all))

	all.LVGoJobs = want
	_, err := jobs.New(all, j2Jobs(nil, all, nil), jobs.Options{})
	require.NoError(t, err)

	off := all
	off.LearnedDigestEnabled, off.MeetingBriefEnabled = "false", "0"
	require.Empty(t, j2Names(off))
	garbage := all
	garbage.LearnedDigestEnabled = "maybe" // reader.bool falls back to the default
	require.Contains(t, j2Names(garbage), "LearnedDigestScheduler")

	test := all
	test.LVEnvironment = "test"
	require.Empty(t, j2Names(test))
}
