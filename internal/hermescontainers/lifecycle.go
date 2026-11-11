package hermescontainers

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrSpawnNotPorted is what EnsureRunning answers once docker itself works: Go
// does not spawn tenant containers (owner decision O1, wave-6-8-plan.md P4). In a
// pod with no docker the network check fails first, with Swift's error.
var ErrSpawnNotPorted = errors.New("hermes tenant container spawn is not ported to Go")

// Containers is the part of HermesContainerManager the wave 6 routes call.
type Containers interface {
	Handle(ctx context.Context, tenant uuid.UUID) (*Handle, error)
	EnsureRunning(ctx context.Context, tenant uuid.UUID) (*Handle, error)
	RecordXaiConnected(ctx context.Context, tenant uuid.UUID, at *time.Time) error
	RecordNousConnected(ctx context.Context, tenant uuid.UUID, at *time.Time) error
	// SeedGatewayConfig answers the number of gateway variables written, or nil
	// when the tenant has no container yet.
	SeedGatewayConfig(ctx context.Context, tenant uuid.UUID) (*int, error)
	ApplyGatewayConfig(ctx context.Context, tenant uuid.UUID) error
}

// Lifecycle is Manager plus the docker-backed operations.
type Lifecycle struct {
	*Manager
	Docker  Docker
	Network string
	Now     func() time.Time
}

var _ Containers = (*Lifecycle)(nil)

// EnsureRunning is ensureRunning up to its first docker call, the network check.
func (l *Lifecycle) EnsureRunning(ctx context.Context, tenant uuid.UUID) (*Handle, error) {
	if err := l.Docker.EnsureNetworkExists(ctx, l.Network); err != nil {
		return nil, err
	}
	return nil, ErrSpawnNotPorted
}

// RecordXaiConnected is recordXaiConnected (at set) and recordXaiDisconnected
// (at nil). A tenant with no row is a no-op.
func (l *Lifecycle) RecordXaiConnected(ctx context.Context, tenant uuid.UUID, at *time.Time) error {
	if at != nil {
		_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET xai_connected_at = $2, last_used_at = $2
			WHERE tenant_id = $1`, tenant, *at)
		return err
	}
	_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET xai_connected_at = NULL WHERE tenant_id = $1`, tenant)
	return err
}

// RecordNousConnected is recordNousConnected / recordNousDisconnected.
func (l *Lifecycle) RecordNousConnected(ctx context.Context, tenant uuid.UUID, at *time.Time) error {
	if at != nil {
		_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET nous_connected_at = $2, last_used_at = $2
			WHERE tenant_id = $1`, tenant, *at)
		return err
	}
	_, err := l.pool.Exec(ctx, `UPDATE hermes_tenant_containers SET nous_connected_at = NULL WHERE tenant_id = $1`, tenant)
	return err
}

// SeedGatewayConfig is seedGatewayConfig: nil with no container row. Writing the
// tenant's config.yaml and .env onto its volume is not ported (P4), so a tenant
// with a row fails the writeEnv step here, where Swift's restart step would fail
// next in a pod with no docker anyway.
func (l *Lifecycle) SeedGatewayConfig(ctx context.Context, tenant uuid.UUID) (*int, error) {
	h, err := l.Handle(ctx, tenant)
	if err != nil || h == nil {
		return nil, err
	}
	return nil, ErrSpawnNotPorted
}

// ApplyGatewayConfig is applyGatewayConfig up to its first docker call.
func (l *Lifecycle) ApplyGatewayConfig(ctx context.Context, tenant uuid.UUID) error {
	if err := l.Docker.EnsureNetworkExists(ctx, l.Network); err != nil {
		return err
	}
	return ErrSpawnNotPorted
}
