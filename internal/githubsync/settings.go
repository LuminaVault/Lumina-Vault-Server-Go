// Package githubsync keeps a vault and a GitHub repository in sync (spec
// 2026-11-19-e4-github-sync). This first part holds the settings, the
// connection table and the rules for which remote files a vault can hold;
// the push (E4.8) and pull (E4.9) workers build on them.
package githubsync

import (
	"crypto/rsa"
	"errors"
	"lumina/internal/config"
	"lumina/internal/githubapp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Settings is the validated GitHub sync configuration (D16).
type Settings struct {
	AppID         int64
	ClientID      string
	ClientSecret  string
	PrivateKey    *rsa.PrivateKey
	WebhookSecret string
	Slug          string
	CallbackURL   string
	PollInterval  time.Duration
	MaxRepoBytes  int64
	MaxFiles      int
	APIBaseURL    string
	GitBaseURL    string
}

// Enabled is GITHUB_SYNC_ENABLED read the way the other Go-only switches are.
func Enabled(cfg config.Config) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.GitHubSyncEnabled), "true")
}

// swiftBool is how cmd/App reads VAULT_GIT_ENABLED (swiftBoolDefaultFalse),
// so history and sync never disagree on whether it is on.
func swiftBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "yes", "1", "true":
		return true
	}
	return false
}

// SettingsFrom checks the GitHub keys. It is called only with the switch on:
// with it off nothing reads them. The error names the first missing or bad
// key and never quotes a value.
func SettingsFrom(cfg config.Config) (Settings, error) {
	if !swiftBool(cfg.VaultGitEnabled) {
		return Settings{}, errors.New("githubsync: needs VAULT_GIT_ENABLED=true")
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(cfg.GitHubAppID), 10, 64)
	if err != nil || appID <= 0 {
		return Settings{}, errors.New("githubsync: GITHUB_APP_ID is missing or not a number")
	}
	for _, kv := range []struct{ key, val string }{
		{"GITHUB_APP_CLIENT_ID", cfg.GitHubAppClientID},
		{"GITHUB_APP_CLIENT_SECRET", cfg.GitHubAppClientSecret},
		{"GITHUB_APP_WEBHOOK_SECRET", cfg.GitHubAppWebhookSecret},
		{"GITHUB_APP_SLUG", cfg.GitHubAppSlug},
		{"GITHUB_SYNC_CALLBACK_URL", cfg.GitHubSyncCallbackURL},
	} {
		if strings.TrimSpace(kv.val) == "" {
			return Settings{}, errors.New("githubsync: " + kv.key + " is missing")
		}
	}
	key, err := githubapp.ParsePrivateKey(cfg.GitHubAppPrivateKey)
	if err != nil {
		return Settings{}, err
	}
	poll, err := time.ParseDuration(strings.TrimSpace(cfg.GitHubSyncPollInterval))
	if err != nil || poll < time.Minute {
		return Settings{}, errors.New("githubsync: GITHUB_SYNC_POLL_INTERVAL must be a duration of at least 1m")
	}
	if cfg.GitHubSyncMaxRepoMB <= 0 || cfg.GitHubSyncMaxFiles <= 0 {
		return Settings{}, errors.New("githubsync: GITHUB_SYNC_MAX_REPO_MB and GITHUB_SYNC_MAX_FILES must be positive")
	}
	// Outside development every URL GitHub or the browser is sent to is HTTPS.
	// Staging's callback is the port-forward on localhost (D15), which is http.
	dev := cfg.LVEnvironment == "" || cfg.LVEnvironment == "dev" || cfg.LVEnvironment == "development" || cfg.LVEnvironment == "test"
	for _, kv := range []struct{ key, val string }{
		{"GITHUB_API_BASE_URL", cfg.GitHubAPIBaseURL},
		{"GITHUB_GIT_BASE_URL", cfg.GitHubGitBaseURL},
		{"GITHUB_SYNC_CALLBACK_URL", cfg.GitHubSyncCallbackURL},
	} {
		u, err := url.Parse(strings.TrimSpace(kv.val))
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return Settings{}, errors.New("githubsync: " + kv.key + " is not an absolute URL")
		}
		localCallback := kv.key == "GITHUB_SYNC_CALLBACK_URL" && u.Hostname() == "localhost"
		if u.Scheme != "https" && !dev && !localCallback {
			return Settings{}, errors.New("githubsync: " + kv.key + " must be https")
		}
	}
	return Settings{
		AppID: appID, ClientID: strings.TrimSpace(cfg.GitHubAppClientID), ClientSecret: strings.TrimSpace(cfg.GitHubAppClientSecret),
		PrivateKey: key, WebhookSecret: strings.TrimSpace(cfg.GitHubAppWebhookSecret), Slug: strings.TrimSpace(cfg.GitHubAppSlug),
		CallbackURL: strings.TrimSpace(cfg.GitHubSyncCallbackURL), PollInterval: poll,
		MaxRepoBytes: int64(cfg.GitHubSyncMaxRepoMB) << 20, MaxFiles: cfg.GitHubSyncMaxFiles,
		APIBaseURL: strings.TrimSpace(cfg.GitHubAPIBaseURL), GitBaseURL: strings.TrimSpace(cfg.GitHubGitBaseURL),
	}, nil
}
