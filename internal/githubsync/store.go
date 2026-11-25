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
}

// Connection is one github_sync_connections row.
type Connection struct {
	VaultID              uuid.UUID
	InstallationID       int64
	AccountLogin         string
	AccountType          string
	RepoID               int64
	RepoFullName         string
	RepoPrivate          bool
	Branch               string
	State                string
	ErrorCode            *string
	ErrorAt              *time.Time
	RetryCount           int
	NextRetryAt          *time.Time
	RemoteSHA            *string
	LastPushAt           *time.Time
	LastPullAt           *time.Time
	Report               Report
	ConnectedByUserID    *uuid.UUID
	ConnectedGitHubLogin string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Store reads and writes the connections table. The table is Go only: on a
// database migrated by Swift (parity) it is absent, and Get reads that as
// "not connected" rather than failing. Only the absent table (42P01) is read
// that way: a missing column is a broken schema and stays an error.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const columns = `vault_id, installation_id, account_login, account_type, repo_id, repo_full_name, repo_private,
	branch, state, error_code, error_at, retry_count, next_retry_at, remote_sha, last_push_at, last_pull_at,
	report, connected_by_user_id, connected_github_login, created_at, updated_at`

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
		&report, &c.ConnectedByUserID, &c.ConnectedGitHubLogin, &c.CreatedAt, &c.UpdatedAt)
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
		 branch, state, report, connected_by_user_id, connected_github_login)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11, $12)`,
		c.VaultID, c.InstallationID, c.AccountLogin, c.AccountType, c.RepoID, c.RepoFullName, c.RepoPrivate,
		c.Branch, c.State, string(report), c.ConnectedByUserID, c.ConnectedGitHubLogin)
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
