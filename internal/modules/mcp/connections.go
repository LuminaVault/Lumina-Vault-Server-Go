package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Agent tokens (AgentConnectionService).
const (
	TokenPrefix      = "lv_"
	PlaceholderToken = "PASTE_YOUR_KEY_HERE__create_one_below" //nolint:gosec // a placeholder shown in setup previews, not a credential
	tokenBytes       = 32
	displayPrefixLen = len(TokenPrefix) + 8
	maxNameLen       = 60
)

// Client kinds (AgentClientKind) and access levels (AgentConnectionAccess).
const (
	KindClaudeCode  = "claude_code"
	KindCodex       = "codex"
	KindHermes      = "hermes"
	KindOther       = "other"
	AccessRead      = "read"
	AccessReadWrite = "read_write"
)

func validKind(k string) bool {
	return k == KindClaudeCode || k == KindCodex || k == KindHermes || k == KindOther
}

// clientKind decodes AgentClientKind as Swift's RawRepresentable enum does.
type clientKind string

func (k *clientKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if !validKind(s) {
		return &wire.CorruptedError{Message: "Cannot initialize AgentClientKind from invalid String value " + s}
	}
	*k = clientKind(s)
	return nil
}

// access decodes AgentConnectionAccess.
type access string

func (a *access) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return &json.UnmarshalTypeError{Value: "non-string", Type: reflect.TypeFor[string]()}
	}
	if s != AccessRead && s != AccessReadWrite {
		return &wire.CorruptedError{Message: "Cannot initialize AgentConnectionAccess from invalid String value " + s}
	}
	*a = access(s)
	return nil
}

// ConnectionDTO is AgentConnectionDTO.
type ConnectionDTO struct {
	ID                wire.UUID  `json:"id"`
	Name              string     `json:"name"`
	ClientKind        string     `json:"clientKind"`
	TokenPrefix       string     `json:"tokenPrefix"`
	CreatedAt         wire.Time  `json:"createdAt"`
	LastUsedAt        *wire.Time `json:"lastUsedAt,omitempty"`
	AllowPersonalData bool       `json:"allowPersonalData"`
	Access            string     `json:"access"`
	// ConnectedAppGrants is the connected apps this key may read. It is nil,
	// and so left out, when connected apps are off; the module fills it.
	ConnectedAppGrants []string `json:"connectedAppGrants,omitzero"`
}

// connection is an agent_connections row.
type connection struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	Name              string
	ClientKind        string
	TokenPrefix       string
	CreatedAt         *time.Time
	LastUsedAt        *time.Time
	AllowPersonalData bool
	Access            string
	// ConnectedAppGrants is never nil for a scanned row. No grant is the default.
	ConnectedAppGrants []string
}

// dto is AgentConnection.asDTO. Fluent sets createdAt on save, so it is never
// nil for a row this server wrote.
func (c connection) dto() ConnectionDTO {
	d := ConnectionDTO{
		ID: wire.UUID{UUID: c.ID}, Name: c.Name, ClientKind: c.ClientKind, TokenPrefix: c.TokenPrefix,
		AllowPersonalData: c.AllowPersonalData, Access: c.Access,
		ConnectedAppGrants: c.ConnectedAppGrants,
	}
	if c.CreatedAt != nil {
		d.CreatedAt = wire.Time{Time: *c.CreatedAt}
	}
	if c.LastUsedAt != nil {
		d.LastUsedAt = &wire.Time{Time: *c.LastUsedAt}
	}
	return d
}

const connectionColumns = `id, tenant_id, name, client_kind, token_prefix, created_at, last_used_at, allow_personal_data, access, connected_app_grants`

// legacyConnectionColumns reads a schema without connected_app_grants (added
// by goose 00010; the parity database is Swift-migrated and never has it):
// every key then has no app grants, which is also the column's default.
const legacyConnectionColumns = `id, tenant_id, name, client_kind, token_prefix, created_at, last_used_at, allow_personal_data, access, '{}'::text[] AS connected_app_grants`

// errGrantsUnavailable refuses a grants change the schema cannot store.
var errGrantsUnavailable = wire.NewError(http.StatusServiceUnavailable, "connected_app_grants_unavailable")

func isMissingGrantsColumn(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42703" && strings.Contains(pgErr.Message, "connected_app_grants")
}

func scanConnection(row pgx.Row) (*connection, error) {
	var c connection
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.ClientKind, &c.TokenPrefix, &c.CreatedAt, &c.LastUsedAt, &c.AllowPersonalData, &c.Access, &c.ConnectedAppGrants)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.ConnectedAppGrants == nil {
		c.ConnectedAppGrants = []string{}
	}
	return &c, nil
}

// Connections is AgentConnectionService.
type Connections struct {
	pool *pgxpool.Pool
	// noGrants latches once the database turns out to lack connected_app_grants.
	noGrants atomic.Bool
}

// columns is the select list for this database's schema.
func (s *Connections) columns() string {
	if s.noGrants.Load() {
		return legacyConnectionColumns
	}
	return connectionColumns
}

// one runs a single-row statement built for the current column list. If the
// grants column is missing it latches legacy mode and retries once.
func (s *Connections) one(ctx context.Context, build func(cols string) string, args ...any) (*connection, error) {
	cols := s.columns()
	c, err := scanConnection(s.pool.QueryRow(ctx, build(cols), args...))
	if err != nil && cols == connectionColumns && isMissingGrantsColumn(err) {
		s.noGrants.Store(true)
		return scanConnection(s.pool.QueryRow(ctx, build(legacyConnectionColumns), args...))
	}
	return c, err
}

// NewConnections returns the service.
func NewConnections(pool *pgxpool.Pool) *Connections { return &Connections{pool: pool} }

var errNotFound = wire.NewError(http.StatusNotFound, "not_found")

// Issue creates a key and returns it with its only plaintext copy.
func (s *Connections) Issue(ctx context.Context, tenant uuid.UUID, name, kind string, allowPersonal bool, acc string) (ConnectionDTO, string, error) {
	trimmed := swiftstr.TrimWhitespacesAndNewlines(name)
	if trimmed == "" {
		return ConnectionDTO{}, "", wire.NewError(http.StatusBadRequest, "name_required")
	}
	if swiftstr.Count(trimmed) > maxNameLen {
		return ConnectionDTO{}, "", wire.NewError(http.StatusBadRequest, "name_too_long")
	}
	token, err := newToken()
	if err != nil {
		return ConnectionDTO{}, "", err
	}
	hash := hashToken(token)
	c, err := s.one(ctx, func(cols string) string {
		return `
		INSERT INTO agent_connections (id, tenant_id, name, client_kind, token_hash, token_prefix, created_at, allow_personal_data, access)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), $7, $8)
		RETURNING ` + cols
	}, uuid.New(), tenant, trimmed, kind, hash[:], token[:displayPrefixLen], allowPersonal, acc)
	if err != nil {
		return ConnectionDTO{}, "", fmt.Errorf("mcp: issue connection: %w", err)
	}
	return c.dto(), token, nil
}

// List is the tenant's live keys, newest first.
func (s *Connections) List(ctx context.Context, tenant uuid.UUID) ([]ConnectionDTO, error) {
	cols := s.columns()
	out, err := s.list(ctx, tenant, cols)
	if err != nil && cols == connectionColumns && isMissingGrantsColumn(err) {
		s.noGrants.Store(true)
		out, err = s.list(ctx, tenant, legacyConnectionColumns)
	}
	return out, err
}

func (s *Connections) list(ctx context.Context, tenant uuid.UUID, cols string) ([]ConnectionDTO, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM agent_connections
		WHERE tenant_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC`, tenant)
	if err != nil {
		return nil, fmt.Errorf("mcp: list connections: %w", err)
	}
	defer rows.Close()
	out := []ConnectionDTO{}
	for rows.Next() {
		c, err := scanConnection(rows)
		if err != nil {
			return nil, fmt.Errorf("mcp: list connections: %w", err)
		}
		out = append(out, c.dto())
	}
	return out, rows.Err()
}

func (s *Connections) live(ctx context.Context, tenant, id uuid.UUID) (*connection, error) {
	return s.one(ctx, func(cols string) string {
		return `SELECT ` + cols + ` FROM agent_connections
		WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL LIMIT 1`
	}, tenant, id)
}

// Revoke marks a live key revoked.
func (s *Connections) Revoke(ctx context.Context, tenant, id uuid.UUID) error {
	c, err := s.live(ctx, tenant, id)
	if err != nil {
		return err
	}
	if c == nil {
		return errNotFound
	}
	_, err = s.pool.Exec(ctx, `UPDATE agent_connections SET revoked_at = NOW() WHERE id = $1`, c.ID)
	return err
}

// Update applies whichever grants are non-nil, to a live key of this tenant
// only, in one statement: a field left nil keeps whatever the row holds when
// the statement runs, so two updates of different fields cannot undo each
// other. appGrants replaces the key's connected-app grants: nil leaves them,
// an empty list clears them. The caller validates the ids.
func (s *Connections) Update(ctx context.Context, tenant, id uuid.UUID, allowPersonal *bool, acc *string, appGrants *[]string) (ConnectionDTO, error) {
	// A nil slice is SQL NULL (keep); a non-nil empty one is '{}' (clear).
	var grants []string
	if appGrants != nil {
		grants = append([]string{}, *appGrants...)
	}
	if appGrants != nil && s.noGrants.Load() {
		return ConnectionDTO{}, errGrantsUnavailable
	}
	c, err := s.one(ctx, func(cols string) string {
		grantsSet := `,
			connected_app_grants = COALESCE($5, connected_app_grants)`
		guard := ``
		if cols == legacyConnectionColumns {
			// No column to write. A grants change must not half-apply, so
			// the row only matches when none was asked for ($5 IS NULL).
			grantsSet = ``
			guard = ` AND $5::text[] IS NULL`
		}
		return `UPDATE agent_connections SET
			allow_personal_data = COALESCE($3, allow_personal_data),
			access = COALESCE($4, access)` + grantsSet + `
		WHERE id = $1 AND tenant_id = $2 AND revoked_at IS NULL` + guard + `
		RETURNING ` + cols
	}, id, tenant, allowPersonal, acc, grants)
	if err != nil {
		return ConnectionDTO{}, err
	}
	if c == nil {
		if appGrants != nil && s.noGrants.Load() {
			return ConnectionDTO{}, errGrantsUnavailable
		}
		return ConnectionDTO{}, errNotFound
	}
	return c.dto(), nil
}

// byToken is the live row behind a presented key, or nil.
func (s *Connections) byToken(ctx context.Context, token string) (*connection, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return nil, nil
	}
	hash := hashToken(token)
	return s.one(ctx, func(cols string) string {
		return `SELECT ` + cols + ` FROM agent_connections
		WHERE token_hash = $1 AND revoked_at IS NULL LIMIT 1`
	}, hash[:])
}

// AllowsPersonalData is allowsPersonalData: unknown and revoked keys are false.
func (s *Connections) AllowsPersonalData(ctx context.Context, token string) (bool, error) {
	c, err := s.byToken(ctx, token)
	return c != nil && c.AllowPersonalData, err
}

// AllowsWrites is allowsWrites.
func (s *Connections) AllowsWrites(ctx context.Context, token string) (bool, error) {
	c, err := s.byToken(ctx, token)
	return c != nil && c.Access == AccessReadWrite, err
}

// AllowsConnectedApp reports whether the key was granted the app. Unknown and
// revoked keys are false, and so is every key by default. It is independent of
// AllowsPersonalData. A JWT session has no key and is not asked.
func (s *Connections) AllowsConnectedApp(ctx context.Context, token, appID string) (bool, error) {
	c, err := s.byToken(ctx, token)
	return c != nil && slices.Contains(c.ConnectedAppGrants, appID), err
}

// connectedAppGrants is the apps a key was granted: empty, never nil, for a
// key with none and for an unknown or revoked key.
func (s *Connections) connectedAppGrants(ctx context.Context, token string) ([]string, error) {
	c, err := s.byToken(ctx, token)
	if err != nil || c == nil {
		return []string{}, err
	}
	return c.ConnectedAppGrants, nil
}

// Authenticate is authenticate: the user a live key acts as, touching
// lastUsedAt. A failed touch is logged, not fatal.
func (s *Connections) Authenticate(ctx context.Context, token string) (uuid.UUID, bool, error) {
	c, err := s.byToken(ctx, token)
	if err != nil || c == nil {
		return uuid.Nil, false, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, c.TenantID).Scan(&exists); err != nil {
		return uuid.Nil, false, err
	}
	if !exists {
		return uuid.Nil, false, nil
	}
	if _, err := s.pool.Exec(ctx, `UPDATE agent_connections SET last_used_at = NOW() WHERE id = $1`, c.ID); err != nil {
		slog.WarnContext(ctx, "agent connection touch failed", "id", ids.Upper(c.ID))
	}
	return c.TenantID, true, nil
}

func hashToken(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// newToken is "lv_" plus 32 random bytes in unpadded base64url.
func newToken() (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}
