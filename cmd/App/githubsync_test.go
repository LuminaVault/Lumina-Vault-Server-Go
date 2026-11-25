package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"lumina/internal/config"
	"lumina/internal/httpserver"
	"lumina/internal/vaultpaths"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func githubSyncStatuses(t *testing.T, cfg config.Config) []int {
	t.Helper()
	var modules []httpserver.Module
	if m := githubSyncModule(nil, cfg, vaultpaths.Paths{Root: t.TempDir()}); m != nil {
		modules = append(modules, m)
	}
	srv := httpserver.New(config.Config{}, httpserver.Deps{Auth: noAuth, Modules: modules})
	var statuses []int
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/v1/vault/github-sync"},
		{http.MethodPost, "/v1/vault/github-sync/connect"},
		{http.MethodPost, "/v1/vault/github-sync/complete"},
		{http.MethodGet, "/v1/vault/github-sync/installations"},
		{http.MethodGet, "/v1/vault/github-sync/repositories"},
		{http.MethodGet, "/v1/vault/github-sync/branches"},
		{http.MethodPut, "/v1/vault/github-sync/connection"},
		{http.MethodDelete, "/v1/vault/github-sync/connection"},
		{http.MethodGet, "/v1/github-sync/callback"},
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(r.method, r.path, nil))
		statuses = append(statuses, rec.Code)
	}
	return statuses
}

func githubSyncConfig(t *testing.T) config.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return config.Config{
		LVEnvironment: "production", GitHubSyncEnabled: "true", VaultGitEnabled: "true",
		GitHubAppID: "1", GitHubAppClientID: "Iv1.x", GitHubAppClientSecret: "s",
		GitHubAppPrivateKey:    string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})),
		GitHubAppWebhookSecret: "w", GitHubAppSlug: "luminavault",
		GitHubSyncCallbackURL: "https://api.lumina.test/v1/github-sync/callback", GitHubSyncPollInterval: "15m",
		GitHubSyncMaxRepoMB: 200, GitHubSyncMaxFiles: 5000,
		GitHubAPIBaseURL: "https://api.github.com", GitHubGitBaseURL: "https://github.com",
	}
}

// D14: with the flag off every route is 404.
func TestGitHubSyncFlagOffRegistersNothing(t *testing.T) {
	cfg := githubSyncConfig(t)
	for _, flag := range []string{"false", "", "0"} {
		cfg.GitHubSyncEnabled = flag
		require.Equal(t, []int{404, 404, 404, 404, 404, 404, 404, 404, 404}, githubSyncStatuses(t, cfg), "flag %q", flag)
	}
}

func TestGitHubSyncFlagOnRegistersRoutes(t *testing.T) {
	// The vault routes sit behind auth; the callback is public and answers a
	// missing state with its error redirect.
	require.Equal(t, []int{401, 401, 401, 401, 401, 401, 401, 401, 302}, githubSyncStatuses(t, githubSyncConfig(t)))
}

// D5: a half-configured server mounts nothing.
func TestGitHubSyncFlagOnWithoutConfigRegistersNothing(t *testing.T) {
	for name, change := range map[string]func(*config.Config){
		"no vault git":  func(c *config.Config) { c.VaultGitEnabled = "false" },
		"no secret":     func(c *config.Config) { c.GitHubAppClientSecret = "" },
		"no key":        func(c *config.Config) { c.GitHubAppPrivateKey = "" },
		"no webhook":    func(c *config.Config) { c.GitHubAppWebhookSecret = "" },
		"http api base": func(c *config.Config) { c.GitHubAPIBaseURL = "http://api.github.com" },
	} {
		cfg := githubSyncConfig(t)
		change(&cfg)
		require.Equal(t, []int{404, 404, 404, 404, 404, 404, 404, 404, 404}, githubSyncStatuses(t, cfg), name)
	}
}
