// Package hermescontainers is the read side of LuminaVaultServer's
// HermesContainerManager (Sources/App/Hermes): a tenant's container row, with its
// API key decrypted. It writes nothing, starts nothing and needs no Docker. The
// lifecycle (ensureRunning, eviction, reprovision) is wave 6, and extends this.
package hermescontainers

import (
	"context"
	"errors"
	"lumina/internal/secretbox"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Handle is HermesContainerHandle.
type Handle struct {
	TenantID uuid.UUID
	// ContainerName is the Docker name, hermes-tenant-<tenant>.
	ContainerName string
	// Port is the host port mapped to the container's API server.
	Port int
	// APIServerKey is the plain API_SERVER_KEY, sent as a bearer token on every
	// request to the container. Never log it.
	APIServerKey    string
	XaiConnectedAt  *time.Time
	NousConnectedAt *time.Time
}

// BaseURL is how the server reaches the container over the shared Docker network.
func (h *Handle) BaseURL() string {
	return "http://" + h.ContainerName + ":8642"
}

// Manager is the read-only part of HermesContainerManager.
type Manager struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
}

func New(pool *pgxpool.Pool, box *secretbox.Box) *Manager {
	return &Manager{pool: pool, box: box}
}

// Handle is HermesContainerManager.handle: the tenant's handle, or nil with no error
// when the tenant has no row. It doesn't bump last_used_at. A key that won't open is
// secretbox.ErrDecryptionFailed, so one bad row answers an error, not "no container".
func (m *Manager) Handle(ctx context.Context, tenant uuid.UUID) (*Handle, error) {
	h := Handle{TenantID: tenant}
	var sealed secretbox.Sealed
	err := m.pool.QueryRow(ctx, `SELECT container_name, port, api_server_key_ciphertext, api_server_key_nonce, xai_connected_at, nous_connected_at
		FROM hermes_tenant_containers
		WHERE tenant_id = $1
		LIMIT 1`, tenant).
		Scan(&h.ContainerName, &h.Port, &sealed.Ciphertext, &sealed.Nonce, &h.XaiConnectedAt, &h.NousConnectedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // Swift's Optional: no row is not an error
	}
	if err != nil {
		return nil, err
	}
	h.APIServerKey, err = m.box.Open(sealed, tenant)
	if err != nil {
		return nil, err
	}
	return &h, nil
}
