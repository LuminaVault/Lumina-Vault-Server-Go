package githubsync

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connection states (D14).
const (
	StateConnecting     = "connecting"
	StateInitialSync    = "initial_sync"
	StateActive         = "active"
	StateNeedsAttention = "needs_attention"
)

var (
	// ErrAlreadyConnected: the vault already has a connection.
	ErrAlreadyConnected = errors.New("githubsync: vault already connected")
	// ErrRepoInUse: another vault already syncs to the repository.
	ErrRepoInUse = errors.New("githubsync: repository already in use")
)

// Conflict is one pair the last pull kept twice.
type Conflict struct {
	Path         string    `json:"path"`
	ConflictPath string    `json:"conflictPath"`
	At           time.Time `json:"at"`
}

// Held is one sample of a path that stays on GitHub only.
type Held struct {
	Path   string     `json:"path"`
	Reason HeldReason `json:"reason"`
}

// Report is the last pull's outcome. It holds paths, never file content.
type Report struct {
	Conflicts []Conflict `json:"conflicts,omitempty"`
	HeldCount int        `json:"heldCount,omitempty"`
	Held      []Held     `json:"held,omitempty"`
	// Kept are files GitHub deleted or renamed that the vault kept, because
	// they have no row to trash or move (D7). They go back to GitHub.
	Kept []string `json:"kept,omitempty"`
}

// Connection is one github_sync_connections row.
type Connection struct {
	VaultID        uuid.UUID
	InstallationID int64
	AccountLogin   string
	AccountType    string
	RepoID         int64
	RepoFullName   string
	RepoPrivate    bool
	Branch         string
	State          string
	ErrorCode      *string
	ErrorAt        *time.Time
	RetryCount     int
	// ConfirmCount is how many attempts running ended with ErrorCode as an
	// answer that is believed only when it repeats (syncer.go, confirmAfter).
	ConfirmCount int
	NextRetryAt  *time.Time
	// ReservedAt is set while NextRetryAt is an attempt's reservation
	// (Reserve) and not the wait after a failure.
	ReservedAt           *time.Time
	RemoteSHA            *string
	LastPushAt           *time.Time
	LastPullAt           *time.Time
	Report               Report
	ConnectedByUserID    *uuid.UUID
	ConnectedGitHubLogin string
	// MergeExisting is the owner's consent, given at connect, to merge a
	// branch that already had files into the vault (D11).
	MergeExisting bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Store reads and writes the connections table. The table is Go only: on a
// database migrated by Swift (parity) it is absent, and Get reads that as
// "not connected" rather than failing. Only the absent table (42P01) is read
// that way: a missing column is a broken schema and stays an error.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const columns = `vault_id, installation_id, account_login, account_type, repo_id, repo_full_name, repo_private,
	branch, state, error_code, error_at, retry_count, next_retry_at, remote_sha, last_push_at, last_pull_at,
	report, connected_by_user_id, connected_github_login, merge_existing, confirm_count, reserved_at, created_at, updated_at`

func missingTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// Get returns the vault's connection, or nil.
func (s *Store) Get(ctx context.Context, vault uuid.UUID) (*Connection, error) {
	var (
		c      Connection
		report []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT `+columns+` FROM github_sync_connections WHERE vault_id = $1`, vault).Scan(
		&c.VaultID, &c.InstallationID, &c.AccountLogin, &c.AccountType, &c.RepoID, &c.RepoFullName, &c.RepoPrivate,
		&c.Branch, &c.State, &c.ErrorCode, &c.ErrorAt, &c.RetryCount, &c.NextRetryAt, &c.RemoteSHA, &c.LastPushAt, &c.LastPullAt,
		&report, &c.ConnectedByUserID, &c.ConnectedGitHubLogin, &c.MergeExisting, &c.ConfirmCount, &c.ReservedAt, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || missingTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(report) > 0 {
		if err := json.Unmarshal(report, &c.Report); err != nil {
			return nil, err
		}
	}
	return &c, nil
}

// Insert saves a new connection. A second connection for the vault is
// ErrAlreadyConnected; a repository another vault uses is ErrRepoInUse.
func (s *Store) Insert(ctx context.Context, c Connection) error {
	report, err := json.Marshal(c.Report)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO github_sync_connections
		(vault_id, installation_id, account_login, account_type, repo_id, repo_full_name, repo_private,
		 branch, state, report, connected_by_user_id, connected_github_login, merge_existing)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12, $13)`,
		c.VaultID, c.InstallationID, c.AccountLogin, c.AccountType, c.RepoID, c.RepoFullName, c.RepoPrivate,
		c.Branch, c.State, string(report), c.ConnectedByUserID, c.ConnectedGitHubLogin, c.MergeExisting)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		if pgErr.ConstraintName == "github_sync_connections_pkey" {
			return ErrAlreadyConnected
		}
		return ErrRepoInUse
	}
	return err
}

// Delete removes the vault's connection and returns it, or nil when there
// was none.
func (s *Store) Delete(ctx context.Context, vault uuid.UUID) (*Connection, error) {
	c, err := s.Get(ctx, vault)
	if err != nil || c == nil {
		return nil, err
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM github_sync_connections WHERE vault_id = $1`, vault)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	return c, nil
}

// ReposInUse reports which of ids another vault than except already syncs to.
func (s *Store) ReposInUse(ctx context.Context, ids []int64, except uuid.UUID) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT repo_id FROM github_sync_connections WHERE repo_id = ANY($1) AND vault_id <> $2`, ids, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// VaultOwner is the vault's owner, read fresh on every call: a personal
// vault's user, or its team's current owner (D13: ownership transfers take
// effect at once). An unknown vault has uuid.Nil.
func (s *Store) VaultOwner(ctx context.Context, vault uuid.UUID) (uuid.UUID, error) {
	var owner *uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(v.personal_owner_user_id, t.owner_user_id)
		FROM vaults v LEFT JOIN teams t ON t.id = v.team_id
		WHERE v.id = $1`, vault).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner == nil) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, err
	}
	return *owner, nil
}

// MARK: the push worker's writes (D6)
//
// Every write names the connection it read as (vault, repository, branch): a
// push that finishes after a disconnect, or after a reconnect to another
// repository, changes nothing. All of them read an absent table (42P01) as
// "no connection".

// Key is the identity of the connection a worker read.
type Key struct {
	Vault  uuid.UUID
	RepoID int64
	Branch string
}

// Key is c's identity for the worker's writes.
func (c *Connection) Key() Key { return Key{Vault: c.VaultID, RepoID: c.RepoID, Branch: c.Branch} }

const whereKey = ` WHERE vault_id = $1 AND repo_id = $2 AND branch = $3`

func (s *Store) update(ctx context.Context, sql string, args ...any) (bool, error) {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if missingTable(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// Due lists the active connections of vaults that are not archived (D13) and
// whose retry time, if any, has passed: the ones a sweep looks at (D6).
// All of them, every time: a row that is in sync is never touched, so no
// ordering would bring a skipped one to the front.
func (s *Store) Due(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.vault_id FROM github_sync_connections c
		JOIN vaults v ON v.id = c.vault_id
		WHERE c.state = $1 AND (c.next_retry_at IS NULL OR c.next_retry_at <= $2) AND v.archived_at IS NULL
		ORDER BY c.vault_id`, StateActive, now)
	if missingTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		if missingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// VaultArchived reports an archived vault (D13: the syncer skips it). An
// unknown vault is archived as far as sync cares.
func (s *Store) VaultArchived(ctx context.Context, vault uuid.UUID) (bool, error) {
	var archived bool
	err := s.pool.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM vaults WHERE id = $1`, vault).Scan(&archived)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return archived, err
}

// MarkPushed records a successful sync: the remote tip, and no error. pushed
// is false when the remote already had the commit and nothing was sent, so
// last_push_at stays.
func (s *Store) MarkPushed(ctx context.Context, k Key, sha string, at time.Time, pushed bool) (bool, error) {
	return s.update(ctx, `UPDATE github_sync_connections SET remote_sha = $4,
		last_push_at = CASE WHEN $6 THEN $5 ELSE last_push_at END,
		error_code = NULL, error_at = NULL, retry_count = 0, confirm_count = 0, next_retry_at = NULL, reserved_at = NULL,
		updated_at = $5`+whereKey,
		k.Vault, k.RepoID, k.Branch, sha, at, pushed)
}

// Reserve writes the retry time of an attempt that is about to push or fetch,
// before it starts: a process killed in the middle (out of memory on a large
// pack) records nothing afterwards, and without this the next start's sweep
// would try again at once, for ever. first is the attempt's first
// reservation; it counts the attempt in retry_count, so each kill waits
// longer than the one before. No error is recorded: nothing has failed yet.
// The attempt's own end replaces the reservation (a push, a recorded
// failure) or takes it back (Release).
func (s *Store) Reserve(ctx context.Context, k Key, at, next time.Time, first bool) (bool, error) {
	bump := 0
	if first {
		bump = 1
	}
	return s.update(ctx, `UPDATE github_sync_connections SET next_retry_at = $4, reserved_at = $5,
		retry_count = retry_count + $6`+whereKey+` AND state = $7`, k.Vault, k.RepoID, k.Branch, next, at, bump, StateActive)
}

// Release takes back a reservation whose attempt ended with nothing to
// record: nothing to push, or a shutdown. It changes nothing when the row
// has since been given another retry time. (No retry time at all is "Sync
// now" having cleared it: the attempt is still taken out of the count.)
func (s *Store) Release(ctx context.Context, k Key, next time.Time) (bool, error) {
	return s.update(ctx, `UPDATE github_sync_connections SET next_retry_at = NULL, reserved_at = NULL,
		retry_count = GREATEST(retry_count - 1, 0)`+whereKey+` AND (next_retry_at IS NULL OR next_retry_at = $4)`,
		k.Vault, k.RepoID, k.Branch, next)
}

// MarkRetry records a failure that is retried: the state stays active, the
// code and time are shown as "last sync failed, retrying". counted says the
// attempt is already in retry_count (Reserve). confirming says the code is
// one that stops retries once it repeats: confirm_count is how many attempts
// running ended with it, and any other failure starts the count again.
func (s *Store) MarkRetry(ctx context.Context, k Key, code string, at, next time.Time, counted, confirming bool) (bool, error) {
	return s.update(ctx, `UPDATE github_sync_connections SET
		confirm_count = CASE WHEN NOT $9 THEN 0 WHEN error_code IS NOT DISTINCT FROM $4 THEN confirm_count + 1 ELSE 1 END,
		error_code = $4, error_at = $5,
		retry_count = retry_count + CASE WHEN $8 THEN 0 ELSE 1 END, next_retry_at = $6, reserved_at = NULL,
		updated_at = $5`+whereKey+` AND state = $7`,
		k.Vault, k.RepoID, k.Branch, code, at, next, StateActive, counted, confirming)
}

// MarkNeedsAttention stops retries: a person has to act (D6's table).
func (s *Store) MarkNeedsAttention(ctx context.Context, k Key, code string, at time.Time) (bool, error) {
	return s.update(ctx, `UPDATE github_sync_connections SET state = $4, error_code = $5, error_at = $6,
		next_retry_at = NULL, reserved_at = NULL, updated_at = $6`+whereKey,
		k.Vault, k.RepoID, k.Branch, StateNeedsAttention, code, at)
}

// SetRepoFullName follows a rename or transfer on GitHub (D6).
func (s *Store) SetRepoFullName(ctx context.Context, k Key, fullName string, at time.Time) (bool, error) {
	return s.update(ctx, `UPDATE github_sync_connections SET repo_full_name = $4, updated_at = $5`+whereKey,
		k.Vault, k.RepoID, k.Branch, fullName, at)
}

// Requeue is "Sync now" (D14): a connection waiting out a retry is due at
// once, and one stopped for a cause in retryable goes back to active with a
// fresh retry count. A wait GitHub itself asked for (holdCode) is not cut
// short. The error stays on the row until a sync succeeds. It reports
// whether anything changed.
func (s *Store) Requeue(ctx context.Context, vault uuid.UUID, retryable []string, holdCode string, at time.Time) (bool, error) {
	return s.update(ctx, `UPDATE github_sync_connections SET
		retry_count = CASE WHEN state = $2 THEN 0 ELSE retry_count END,
		confirm_count = CASE WHEN state = $2 THEN 0 ELSE confirm_count END,
		state = $3, next_retry_at = NULL, reserved_at = NULL, updated_at = $6
		WHERE vault_id = $1 AND ((state = $2 AND error_code = ANY($4))
			OR (state = $3 AND next_retry_at IS NOT NULL AND error_code IS DISTINCT FROM $5))`,
		vault, StateNeedsAttention, StateActive, retryable, holdCode, at)
}

// ConsentToMerge records the owner's answer to "this branch already has
// files: merge them into the vault?" on a connection that stopped for want
// of it (code), and puts it back to work with a fresh retry count. The error
// stays on the row until a sync succeeds, as after Requeue. False: the
// connection is not stopped for that reason.
func (s *Store) ConsentToMerge(ctx context.Context, vault uuid.UUID, code string, at time.Time) (bool, error) {
	return s.update(ctx, `UPDATE github_sync_connections SET merge_existing = TRUE, state = $2,
		retry_count = 0, confirm_count = 0, next_retry_at = NULL, reserved_at = NULL, updated_at = $5
		WHERE vault_id = $1 AND state = $3 AND error_code = $4`,
		vault, StateActive, StateNeedsAttention, code, at)
}

// MARK: the pull worker's writes (D9, D10)

// MarkPulled records a finished pull: the remote tip it merged, when, and
// its report. The error, if any, stays for the push that follows to clear.
func (s *Store) MarkPulled(ctx context.Context, k Key, sha string, at time.Time, report Report) (bool, error) {
	b, err := json.Marshal(report)
	if err != nil {
		return false, err
	}
	return s.update(ctx, `UPDATE github_sync_connections SET remote_sha = $4, last_pull_at = $5, report = $6::jsonb,
		updated_at = $5`+whereKey, k.Vault, k.RepoID, k.Branch, sha, at, string(b))
}

// ByRepo returns the connection that syncs to the repository, or nil.
func (s *Store) ByRepo(ctx context.Context, repoID int64) (*Connection, error) {
	var vault uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT vault_id FROM github_sync_connections WHERE repo_id = $1`, repoID).Scan(&vault)
	if errors.Is(err, pgx.ErrNoRows) || missingTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, vault)
}

func (s *Store) vaults(ctx context.Context, sql string, args ...any) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if missingTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		if missingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// StopInstallation puts every connection of an installation that is not
// already stopped for that reason into needs_attention (D9: the App was
// uninstalled or suspended). It returns how many changed.
func (s *Store) StopInstallation(ctx context.Context, installation int64, code string, at time.Time) (int, error) {
	ids, err := s.vaults(ctx, `UPDATE github_sync_connections SET state = $2, error_code = $3, error_at = $4,
		next_retry_at = NULL, updated_at = $4
		WHERE installation_id = $1 AND (state <> $2 OR error_code IS DISTINCT FROM $3) RETURNING vault_id`,
		installation, StateNeedsAttention, code, at)
	return len(ids), err
}

// ResumeInstallation puts the connections an installation's suspension
// stopped back to work (D9: unsuspend) and returns their vaults. A connection
// stopped for another reason stays stopped.
func (s *Store) ResumeInstallation(ctx context.Context, installation int64, suspendedCode string, at time.Time) ([]uuid.UUID, error) {
	return s.vaults(ctx, `UPDATE github_sync_connections SET state = $2, error_code = NULL, error_at = NULL,
		retry_count = 0, confirm_count = 0, next_retry_at = NULL, reserved_at = NULL, updated_at = $5
		WHERE installation_id = $1 AND state = $3 AND error_code = $4 RETURNING vault_id`,
		installation, StateActive, StateNeedsAttention, suspendedCode, at)
}

// StopRepos puts the connections of an installation to the listed
// repositories into needs_attention (D9: repositories removed from it).
func (s *Store) StopRepos(ctx context.Context, installation int64, repos []int64, code string, at time.Time) (int, error) {
	if len(repos) == 0 {
		return 0, nil
	}
	ids, err := s.vaults(ctx, `UPDATE github_sync_connections SET state = $3, error_code = $4, error_at = $5,
		next_retry_at = NULL, updated_at = $5
		WHERE installation_id = $1 AND repo_id = ANY($2) RETURNING vault_id`,
		installation, repos, StateNeedsAttention, code, at)
	return len(ids), err
}

// MARK: webhook deliveries (D9)

// Delivery statuses.
const (
	DeliveryQueued  = "queued"
	DeliveryDone    = "done"
	DeliveryIgnored = "ignored"
	DeliveryFailed  = "failed"
)

// Delivery is one github_webhook_deliveries row: six fields of the payload
// and nothing else (D2). Removed is not stored; it is the repository ids an
// installation_repositories event took away.
type Delivery struct {
	ID             string
	Event          string
	Action         string
	InstallationID int64
	RepoID         int64
	Ref            string
	After          string
	Removed        []int64
}

func nullInt(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func nullText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// InsertDelivery records a delivery as queued. inserted is false when its id
// was seen before: GitHub delivered it twice, or it is a replay.
func (s *Store) InsertDelivery(ctx context.Context, d Delivery, at time.Time) (inserted bool, err error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO github_webhook_deliveries
		(delivery_id, event, action, installation_id, repo_id, ref, after_sha, status, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT (delivery_id) DO NOTHING`,
		d.ID, d.Event, nullText(d.Action), nullInt(d.InstallationID), nullInt(d.RepoID), nullText(d.Ref), nullText(d.After),
		DeliveryQueued, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetDeliveryStatus closes one delivery.
func (s *Store) SetDeliveryStatus(ctx context.Context, id, status string, at time.Time) error {
	_, err := s.update(ctx, `UPDATE github_webhook_deliveries SET status = $2, processed_at = $3 WHERE delivery_id = $1`, id, status, at)
	return err
}

// DeliveryStatus is a delivery's status, or "" when it is unknown.
func (s *Store) DeliveryStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT status FROM github_webhook_deliveries WHERE delivery_id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) || missingTable(err) {
		return "", nil
	}
	return status, err
}

// DeliveriesDone closes the open push deliveries of a repository's branch
// that arrived no later than upTo: the pull that just finished started after
// them, so it saw what they announced.
func (s *Store) DeliveriesDone(ctx context.Context, repoID int64, ref string, upTo, at time.Time) error {
	_, err := s.update(ctx, `UPDATE github_webhook_deliveries SET status = $4, processed_at = $5
		WHERE repo_id = $1 AND ref = $2 AND status = ANY($3) AND event = 'push' AND received_at <= $6`,
		repoID, ref, openDeliveries, DeliveryDone, at, upTo)
	return err
}

// openDeliveries are the statuses of a delivery nothing has handled yet:
// queued, or failed on the way (a database error while it was looked up).
var openDeliveries = []string{DeliveryQueued, DeliveryFailed}

// StaleDeliveries is the sweep's half of the queue for pushes (D9): push
// deliveries still open that arrived before the given time. Those whose
// repository and branch an active connection syncs are returned as vaults to
// pull, each once; the rest have nothing to wait for and are closed as
// ignored. Deliveries of other events are StaleEvents' to list.
func (s *Store) StaleDeliveries(ctx context.Context, before, at time.Time) ([]uuid.UUID, error) {
	out, err := s.vaults(ctx, `SELECT DISTINCT c.vault_id FROM github_webhook_deliveries d
		JOIN github_sync_connections c ON c.repo_id = d.repo_id AND d.ref = 'refs/heads/' || c.branch
		WHERE d.event = 'push' AND d.status = ANY($1) AND d.received_at < $2 AND c.state = $3`,
		openDeliveries, before, StateActive)
	if err != nil {
		return nil, err
	}
	_, err = s.update(ctx, `UPDATE github_webhook_deliveries d SET status = $3, processed_at = $4
		WHERE d.event = 'push' AND d.status = ANY($1) AND d.received_at < $2 AND NOT EXISTS (
			SELECT 1 FROM github_sync_connections c
			WHERE c.repo_id = d.repo_id AND d.ref = 'refs/heads/' || c.branch AND c.state = $5)`,
		openDeliveries, before, DeliveryIgnored, at, StateActive)
	return out, err
}

// staleEventsBatch is how many deliveries one sweep handles again.
const staleEventsBatch = 100

// StaleEvents is the sweep's half of the queue for everything that is not a
// push (D9): deliveries still open that arrived before the given time,
// oldest first, a batch at a time. The row holds six fields of the payload
// (D2), so the repositories an installation_repositories event took away are
// not among them: Removed is empty, and the sweep asks GitHub instead.
func (s *Store) StaleEvents(ctx context.Context, before time.Time) ([]Delivery, error) {
	rows, err := s.pool.Query(ctx, `SELECT delivery_id, event, COALESCE(action, ''), COALESCE(installation_id, 0),
			COALESCE(repo_id, 0), COALESCE(ref, ''), COALESCE(after_sha, '')
		FROM github_webhook_deliveries
		WHERE event <> 'push' AND status = ANY($1) AND received_at < $2
		ORDER BY received_at, delivery_id LIMIT $3`, openDeliveries, before, staleEventsBatch)
	if missingTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.Event, &d.Action, &d.InstallationID, &d.RepoID, &d.Ref, &d.After); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ByInstallation lists the vaults with an active connection through the
// installation.
func (s *Store) ByInstallation(ctx context.Context, installation int64) ([]uuid.UUID, error) {
	return s.vaults(ctx, `SELECT vault_id FROM github_sync_connections WHERE installation_id = $1 AND state = $2 ORDER BY vault_id`,
		installation, StateActive)
}

// PurgeDeliveries deletes deliveries received before the given time
// (GitHubSyncPurge: 7 days).
func (s *Store) PurgeDeliveries(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM github_webhook_deliveries WHERE received_at < $1`, before)
	if missingTable(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
