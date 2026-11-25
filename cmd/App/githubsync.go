package main

import (
	"log/slog"
	"lumina/internal/config"
	"lumina/internal/githubapp"
	"lumina/internal/githubsync"
	"lumina/internal/httpserver"
	"lumina/internal/jobs"
	"lumina/internal/ratelimit"
	"lumina/internal/ssrfguard"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultpaths"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	githubsyncmod "lumina/internal/modules/githubsync"
)

// githubSync builds GitHub sync (spec 2026-11-19-e4-github-sync, D5): its
// routes, the push worker behind the committer's commit hook, and the
// GitHubSyncSweep job (which runs only when LV_GO_JOBS names it, like every
// job). stop ends pushes in flight; call it on shutdown, before the
// committer's.
//
// Nothing is built, mounted, hooked or registered unless GITHUB_SYNC_ENABLED
// is true; with the switch on but VAULT_GIT_ENABLED off or a GitHub key
// missing or bad, one warning is logged and the same nothing happens, so a
// half-configured server never offers a flow that cannot finish.
//
// Requests to GitHub, REST and git alike, go through the SSRF guard, HTTPS
// only outside development, and never follow a redirect.
func githubSync(pool *pgxpool.Pool, cfg config.Config, paths vaultpaths.Paths, limits *ratelimit.Store, history *vaultHistory) (module httpserver.Module, registered []jobs.Job, stop func()) {
	stop = func() {}
	if !githubsync.Enabled(cfg) {
		return nil, nil, stop
	}
	log := slog.Default().With("label", "lv.github-sync")
	settings, err := githubsync.SettingsFrom(cfg)
	if err != nil {
		log.Warn("GitHub sync is enabled but not configured; not mounting it", "reason", err.Error())
		return nil, nil, stop
	}
	if history == nil {
		log.Warn("GitHub sync is enabled but vault history is not running; not mounting it")
		return nil, nil, stop
	}
	guard := ssrfguard.Guard{RequireHTTPS: !githubsync.IsDev(cfg)}
	client, err := githubapp.New(githubapp.Config{
		AppID: settings.AppID, ClientID: settings.ClientID, ClientSecret: settings.ClientSecret, PrivateKey: settings.PrivateKey,
		APIBaseURL: settings.APIBaseURL, WebBaseURL: settings.GitBaseURL, HTTP: guard.Client(30 * time.Second),
	})
	if err != nil {
		log.Warn("GitHub sync is enabled but its client cannot be built; not mounting it", "reason", err.Error())
		return nil, nil, stop
	}
	// Git's own client: no overall timeout (each push has its deadline), the
	// same dial guard, redirects refused by SetHTTPClient.
	vaultgit.SetHTTPClient(guard.Client(0))
	store := githubsync.NewStore(pool)
	syncer := githubsync.NewSyncer(githubsync.SyncerOptions{
		Store: store, GitHub: client, Repos: history.committer, RawRoot: paths.Raw,
		GitBaseURL: settings.GitBaseURL, Log: log,
	})
	history.onCommit(syncer.OnCommit)
	module = githubsyncmod.New(githubsyncmod.Options{
		Client: client, Store: store, Access: vaultaccess.New(pool), Paths: paths,
		CallbackURL: settings.CallbackURL, Slug: settings.Slug,
		MaxRepoBytes: settings.MaxRepoBytes, MaxFiles: settings.MaxFiles,
		WebReturnOrigins: cfg.CORSAllowedOrigins, Limits: limits, Sync: syncer, Log: log,
	})
	return module, []jobs.Job{syncer.SweepJob()}, syncer.Stop
}
