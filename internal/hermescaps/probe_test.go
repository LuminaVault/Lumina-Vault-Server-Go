package hermescaps

import (
	"context"
	"errors"
	"lumina/internal/hermesdash"
	"lumina/internal/hermesdash/hermesdashtest"
	"lumina/internal/ssrfguard"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ported from HermesDashboardProbeTests: the probe classifies the three auth
// outcomes and gates every dashboard capability on bearer.

func probeClient(stub *hermesdashtest.Stub) *hermesdash.Dashboard {
	return hermesdash.NewDashboard("http://127.0.0.1:9119", "t", ssrfguard.Guard{AllowPrivateRanges: true, AllowTailnetHTTP: true}, stub)
}

func TestProbeBearerModeOnALoopbackDashboardEnablesEveryCapability(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/status", 200, `{"version":"0.21.0","auth_required":false}`)
	stub.Respond("GET", "/api/fs/default-cwd", 200, `{"cwd":"/home/hermes"}`)
	stub.Respond("GET", "/api/skills", 200, "[]")
	version := "0.21.0"
	assert.Equal(t, &Dashboard{Reachable: true, AuthMode: Bearer, SkillsWrite: true, Cron: true, FS: true, Sessions: true, Version: &version},
		ProbeDashboard(context.Background(), probeClient(stub)))
}

func TestProbeGatedDashboardAnswering401ReportsOAuthOnlyWithNothingUsable(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/status", 200, `{"version":"0.21.0","auth_required":true}`)
	stub.Respond("GET", "/api/skills", 401, `{"detail":"Unauthorized"}`)
	probe := ProbeDashboard(context.Background(), probeClient(stub))
	assert.True(t, probe.Reachable)
	assert.Equal(t, OAuthOnly, probe.AuthMode)
	assert.False(t, probe.SkillsWrite)
	assert.False(t, probe.Cron)
	assert.False(t, probe.FS)
	assert.False(t, probe.Sessions)
}

func TestProbeLoginRedirectReportsOAuthOnlyEvenWhenStatusOmitsAuthRequired(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Respond("GET", "/api/status", 200, `{"version":"0.21.0"}`)
	stub.Respond("GET", "/api/skills", 302, "", "Location", "/login")
	assert.Equal(t, OAuthOnly, ProbeDashboard(context.Background(), probeClient(stub)).AuthMode)
}

func TestProbeUnreachableDashboardReportsUnreachable(t *testing.T) {
	stub := hermesdashtest.New()
	stub.Failure = errors.New("cannot connect to host")
	assert.Equal(t, &Dashboard{AuthMode: Unreachable}, ProbeDashboard(context.Background(), probeClient(stub)))
}

func TestAttachKeepsEveryGatewayFieldAndAddsTheDashboard(t *testing.T) {
	dashboard := &Dashboard{Reachable: true, AuthMode: Bearer, SkillsWrite: true, Cron: true, FS: true, Sessions: true}
	merged := Attach(dashboard, ManagedDefault())
	assert.False(t, merged.IsUserOverride)
	assert.Equal(t, Managed, merged.Chat)
	assert.Equal(t, ManagedDefault().IngestionMaxSourceBytes, merged.IngestionMaxSourceBytes)
	assert.Equal(t, dashboard, merged.Dashboard)
}

func TestCachedCapabilitiesWithoutADashboardFieldStillDecode(t *testing.T) {
	legacy := `{"isUserOverride":true,"chat":"live","sessions":"live","jobs":"live","skills":"read_only","soul":"unsupported","gateways":"unsupported","memory":"unsupported","providers":"read_only"}`
	var decoded Capabilities
	require.NoError(t, decodeCapabilities([]byte(legacy), &decoded))
	assert.Nil(t, decoded.Dashboard)
	assert.Equal(t, ReadOnly, decoded.Skills)
}
