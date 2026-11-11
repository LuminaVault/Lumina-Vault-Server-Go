package main

import (
	"lumina/internal/config"
	"lumina/internal/embedding"
	"lumina/internal/jobs"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func j2Names(cfg config.Config, kind embedding.Kind) []string {
	var names []string
	for _, j := range j2Jobs(nil, cfg, nil, kind, nil) {
		names = append(names, j.Name)
	}
	slices.Sort(names)
	return names
}

// J2 registers each job under Swift's start conditions with the Swift type name,
// and LV_GO_JOBS naming all of them boots.
func TestJ2JobsFollowSwiftStartConditions(t *testing.T) {
	all := config.Config{
		LVEnvironment: "production", LearnedDigestEnabled: "true", MeetingBriefEnabled: "true",
		EmbeddingReembedEnabled: "false", EmbeddingHealEnabled: "true",
		KnowledgeGraphWorkerEnabled: "TRUE", SynthesisWorkerEnabled: "true",
	}
	want := []string{"EmbeddingReembedService", "KnowledgeExtractionWorker", "LearnedDigestScheduler", "MeetingBriefScheduler", "SynthesisWorker"}
	require.Equal(t, want, j2Names(all, embedding.KindOpenRouter))

	all.LVGoJobs = want
	_, err := jobs.New(all, j2Jobs(nil, all, nil, embedding.KindOpenRouter, nil), jobs.Options{})
	require.NoError(t, err)

	off := all
	off.LearnedDigestEnabled, off.MeetingBriefEnabled, off.EmbeddingHealEnabled = "false", "0", "no"
	off.KnowledgeGraphWorkerEnabled = "1" // Swift compares the string with "true", so "1" is off
	off.SynthesisWorkerEnabled = ""       // off by default
	require.Empty(t, j2Names(off, embedding.KindOpenRouter))
	garbage := all
	garbage.LearnedDigestEnabled = "maybe" // reader.bool falls back to the default
	require.Contains(t, j2Names(garbage, embedding.KindOpenRouter), "LearnedDigestScheduler")

	// Heal defaults on only with a real provider; production runs deterministic,
	// so the service is not registered there unless the re-embed is turned on.
	require.NotContains(t, j2Names(all, embedding.KindDeterministic), "EmbeddingReembedService")
	reembed := all
	reembed.EmbeddingReembedEnabled = "true"
	require.Contains(t, j2Names(reembed, embedding.KindDeterministic), "EmbeddingReembedService")

	// SynthesisWorker is the one J2 job Swift starts under LV_ENVIRONMENT=test.
	test := all
	test.LVEnvironment = "test"
	require.Equal(t, []string{"SynthesisWorker"}, j2Names(test, embedding.KindOpenRouter))
}
