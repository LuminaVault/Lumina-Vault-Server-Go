package main

import (
	"lumina/internal/config"
	"lumina/internal/hermescontainers"
	"testing"

	"github.com/stretchr/testify/require"
)

// HermesContainerReaperServiceTests: the reaper job exists only where Swift
// builds HermesContainerReaperService (a container manager, so a SecretBox, and
// not the test environment), under Swift's type name for LV_GO_JOBS.
func TestContainerReaperJobGates(t *testing.T) {
	require.Empty(t, (&agent{cfg: config.Config{}}).containerReaperJob(), "no SecretBox, no container manager")

	lc := &hermescontainers.Lifecycle{}
	require.Empty(t, (&agent{cfg: config.Config{LVEnvironment: "test"}, containers: lc}).containerReaperJob())

	got := (&agent{cfg: config.Config{LVEnvironment: "production", HermesPerTenantIdleTTLSeconds: 1800}, containers: lc}).containerReaperJob()
	require.Len(t, got, 1)
	require.Equal(t, "HermesContainerReaperService", got[0].Name)
}
