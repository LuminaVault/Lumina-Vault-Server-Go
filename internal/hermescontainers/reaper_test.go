package hermescontainers

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// HermesContainerReaperServiceTests: "the interval is a quarter of the idle TTL".
func TestReaperIntervalIsAQuarterOfTheIdleTTL(t *testing.T) {
	require.Equal(t, 450, ReaperIntervalSeconds(1800))
	require.Equal(t, 900, ReaperIntervalSeconds(3600))
}

// HermesContainerReaperServiceTests: "the interval never drops below a minute".
func TestReaperIntervalNeverDropsBelowAMinute(t *testing.T) {
	require.Equal(t, 60, ReaperIntervalSeconds(60))
	require.Equal(t, 60, ReaperIntervalSeconds(1))
	require.Equal(t, 60, ReaperIntervalSeconds(0))
}
