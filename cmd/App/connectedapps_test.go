package main

import (
	"lumina/internal/config"
	"lumina/internal/connectedapps"
	"lumina/internal/httpserver"
	"lumina/internal/modules/mcp"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// 32 zero bytes, base64: a test-only master key.
const connectedAppsTestKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// noAuth stands in for the JWT middleware: these tests only ask whether a
// route exists.
func noAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(*echo.Context) error { return echo.ErrUnauthorized }
}

func connectedAppsStatuses(t *testing.T, cfg config.Config) (svcNil bool, statuses []int) {
	t.Helper()
	module, svc := connectedAppsModule(nil, cfg)
	var modules []httpserver.Module
	if module != nil {
		modules = append(modules, module)
	}
	srv := httpserver.New(config.Config{}, httpserver.Deps{Auth: noAuth, Modules: modules})
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/v1/me/connected-apps"},
		{http.MethodPost, "/v1/me/connected-apps/norviq/connect"},
		{http.MethodDelete, "/v1/me/connected-apps/norviq"},
		{http.MethodPost, "/v1/me/connected-apps/aaru/complete"},
		{http.MethodGet, "/v1/connected-apps/oauth/callback"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(r.method, r.path, nil))
		statuses = append(statuses, rec.Code)
	}
	return svc == nil, statuses
}

func TestDisabledFlagRegistersNothing(t *testing.T) {
	for _, flag := range []string{"false", "", "0"} {
		svcNil, statuses := connectedAppsStatuses(t, config.Config{
			ConnectedAppsEnabled: flag, LVSecretMasterKey: connectedAppsTestKey, MCPPublicBaseURL: "https://api.lumina.test",
		})
		require.True(t, svcNil, "flag %q", flag)
		require.Equal(t, []int{404, 404, 404, 404, 404}, statuses, "flag %q", flag)
	}
}

func TestEnabledFlagRegistersRoutes(t *testing.T) {
	svcNil, statuses := connectedAppsStatuses(t, config.Config{
		ConnectedAppsEnabled: "TRUE", LVSecretMasterKey: connectedAppsTestKey, MCPPublicBaseURL: "https://api.lumina.test",
	})
	require.False(t, svcNil)
	// The four settings routes sit behind auth; the callback is public and
	// answers a missing state with its error redirect.
	require.Equal(t, []int{401, 401, 401, 401, 302}, statuses)
}

// Tokens cannot be sealed without the master key, so the feature stays off.
func TestEnabledFlagWithoutMasterKeyRegistersNothing(t *testing.T) {
	svcNil, statuses := connectedAppsStatuses(t, config.Config{ConnectedAppsEnabled: "true"})
	require.True(t, svcNil)
	require.Equal(t, []int{404, 404, 404, 404, 404}, statuses)
}

func TestConnectedAppsCallbackURL(t *testing.T) {
	require.Equal(t, "https://mcp.test/v1/connected-apps/oauth/callback",
		connectedAppsCallbackURL(config.Config{MCPPublicBaseURL: "https://mcp.test/", IngestionPublicBaseURL: "https://in.test"}))
	require.Equal(t, "https://in.test/v1/connected-apps/oauth/callback",
		connectedAppsCallbackURL(config.Config{IngestionPublicBaseURL: "https://in.test"}))
	require.Empty(t, connectedAppsCallbackURL(config.Config{}))
}

// /v1/mcp gets connected apps only when the service exists. A nil service
// must leave the interface nil, not hold a typed nil that passes `!= nil`.
func TestMCPOptionsConnectedApps(t *testing.T) {
	off := (&agent{}).mcpOptions(mcp.Options{})
	require.True(t, off.ConnectedApps == nil)
	require.Empty(t, off.ConnectedAppIDs)

	on := (&agent{connectedApps: connectedapps.NewService(connectedapps.Registry{}, nil, nil, nil, nil)}).mcpOptions(mcp.Options{})
	require.NotNil(t, on.ConnectedApps)
	require.Equal(t, []string{"norviq", "aaru", "khepri", "loci"}, on.ConnectedAppIDs)
}
