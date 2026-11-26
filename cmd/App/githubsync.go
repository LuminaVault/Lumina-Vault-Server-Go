package main

import (
	"log/slog"
	"lumina/internal/config"
	"lumina/internal/diskguard"
	"lumina/internal/githubapp"
	"lumina/internal/githubsync"
	"lumina/internal/httpserver"
	"lumina/internal/jobs"
	"lumina/internal/modules/vault"
	"lumina/internal/ratelimit"
	"lumina/internal/ssrfguard"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultgit"
	"lumina/internal/vaultpaths"
	"time"

	"github.com/google/uuid"
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
//
// files is the vault module's own write pipeline (its path locks are the ones
// every save takes); a pull changes the vault through it and nothing else.
// Nil leaves sync push-only.
func githubSync(pool *pgxpool.Pool, cfg config.Config, paths vaultpaths.Paths, limits *ratelimit.Store, history *vaultHistory, files *vault.Files, disk *diskguard.Guard) (module httpserver.Module, registered []jobs.Job, stop func()) {
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
	opts := githubsync.SyncerOptions{
		Store: store, GitHub: client, Repos: history.committer, RawRoot: paths.Raw,
		GitBaseURL: settings.GitBaseURL, Concurrency: pushConcurrency(pool), Log: log,
		MaxRepoBytes: settings.MaxRepoBytes, PollInterval: settings.PollInterval, PushTimeout: settings.PushTimeout,
	}
	var progress func(uuid.UUID) (int, int, bool)
	if files != nil {
		pullOpts := githubsync.PullOptions{
			Files: files, Store: store, RawRoot: paths.Raw, MaxFileSize: history.committer.MaxFileSize,
			MaxFiles: settings.MaxFiles, Log: log,
		}
		if disk != nil {
			pullOpts.Space = disk.Check
		}
		pull := githubsync.NewPull(pullOpts)
		opts.Pull = pull
		progress = func(v uuid.UUID) (int, int, bool) {
			p, ok := pull.Progress(v)
			return p.Done, p.Total, ok
		}
	}
	syncer := githubsync.NewSyncer(opts)
	history.onCommit(syncer.OnCommit)
	mod := githubsyncmod.New(githubsyncmod.Options{
		WebhookSecret: settings.WebhookSecret, Progress: progress,
		Client: client, Store: store, Access: vaultaccess.New(pool), Paths: paths,
		CallbackURL: settings.CallbackURL, Slug: settings.Slug,
		MaxRepoBytes: settings.MaxRepoBytes, MaxFiles: settings.MaxFiles,
		WebReturnOrigins: cfg.CORSAllowedOrigins, Limits: limits, Sync: syncer, Log: log,
	})
	// Deliveries answered but not yet handed to the syncer finish first; one
	// that does not make it is queued again by the sweep.
	stop = func() {
		mod.Drain()
		syncer.Stop()
	}
	return mod, []jobs.Job{syncer.SweepJob(), syncer.PollJob(), syncer.PurgeJob()}, stop
}

// pushConcurrency is how many pushes may run at once in this process: a
// quarter of the database pool, at least 1 and at most 4. A push pins one
// pool connection for the vault's advisory lock for up to a minute and needs
// a second for its own queries, so pushes must never be able to take the
// pool: the API would wait behind them, and at the pool's size they would
// wait on each other. pgx's default pool is max(4, CPUs), which gives 1.
func pushConcurrency(pool *pgxpool.Pool) int {
	if pool == nil {
		return 1
	}
	return max(1, min(4, int(pool.Config().MaxConns)/4))
}
