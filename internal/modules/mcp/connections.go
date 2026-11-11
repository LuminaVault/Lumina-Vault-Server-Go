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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
}

// dto is AgentConnection.asDTO. Fluent sets createdAt on save, so it is never
// nil for a row this server wrote.
func (c connection) dto() ConnectionDTO {
	d := ConnectionDTO{
		ID: wire.UUID{UUID: c.ID}, Name: c.Name, ClientKind: c.ClientKind, TokenPrefix: c.TokenPrefix,
		AllowPersonalData: c.AllowPersonalData, Access: c.Access,
	}
	if c.CreatedAt != nil {
		d.CreatedAt = wire.Time{Time: *c.CreatedAt}
	}
	if c.LastUsedAt != nil {
		d.LastUsedAt = &wire.Time{Time: *c.LastUsedAt}
	}
	return d
}

const connectionColumns = `id, tenant_id, name, client_kind, token_prefix, created_at, last_used_at, allow_personal_data, access`

func scanConnection(row pgx.Row) (*connection, error) {
	var c connection
	err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.ClientKind, &c.TokenPrefix, &c.CreatedAt, &c.LastUsedAt, &c.AllowPersonalData, &c.Access)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Connections is AgentConnectionService.
type Connections struct{ pool *pgxpool.Pool }

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
	c, err := scanConnection(s.pool.QueryRow(ctx, `
		INSERT INTO agent_connections (id, tenant_id, name, client_kind, token_hash, token_prefix, created_at, allow_personal_data, access)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), $7, $8)
		RETURNING `+connectionColumns,
		uuid.New(), tenant, trimmed, kind, hash[:], token[:displayPrefixLen], allowPersonal, acc))
	if err != nil {
		return ConnectionDTO{}, "", fmt.Errorf("mcp: issue connection: %w", err)
	}
	return c.dto(), token, nil
}

// List is the tenant's live keys, newest first.
func (s *Connections) List(ctx context.Context, tenant uuid.UUID) ([]ConnectionDTO, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+connectionColumns+` FROM agent_connections
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
	return scanConnection(s.pool.QueryRow(ctx, `SELECT `+connectionColumns+` FROM agent_connections
		WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL LIMIT 1`, tenant, id))
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

// Update applies whichever grants are non-nil.
func (s *Connections) Update(ctx context.Context, tenant, id uuid.UUID, allowPersonal *bool, acc *string) (ConnectionDTO, error) {
	c, err := s.live(ctx, tenant, id)
	if err != nil {
		return ConnectionDTO{}, err
	}
	if c == nil {
		return ConnectionDTO{}, errNotFound
	}
	if allowPersonal != nil {
		c.AllowPersonalData = *allowPersonal
	}
	if acc != nil {
		c.Access = *acc
	}
	if _, err := s.pool.Exec(ctx, `UPDATE agent_connections SET allow_personal_data = $2, access = $3 WHERE id = $1`,
		c.ID, c.AllowPersonalData, c.Access); err != nil {
		return ConnectionDTO{}, err
	}
	return c.dto(), nil
}

// byToken is the live row behind a presented key, or nil.
func (s *Connections) byToken(ctx context.Context, token string) (*connection, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return nil, nil
	}
	hash := hashToken(token)
	return scanConnection(s.pool.QueryRow(ctx, `SELECT `+connectionColumns+` FROM agent_connections
		WHERE token_hash = $1 AND revoked_at IS NULL LIMIT 1`, hash[:]))
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
