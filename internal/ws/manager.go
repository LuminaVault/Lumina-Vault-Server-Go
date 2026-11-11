// Package ws is the Go port of LuminaVaultServer's WebSocket fan-out:
// ConnectionManager (Sources/App/WebSockets/ConnectionManager.swift), the
// WebSocketBroadcastGuard, and the body of the `/v1/ws` upgrade closure
// (App+build.swift:3836-3880).
//
// One Manager per process, built in cmd/App. Publishers (memory compile
// progress, the device-RPC broker) call Broadcast; the `/v1/ws` route hands each
// upgraded socket to Serve. The map is per process, as in Swift: a message
// published here reaches only sockets connected to this process.
package ws

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// WriteTimeout bounds one broadcast write. Swift has none; a socket that stops
// reading would otherwise hold a publisher (a compile, a device command) forever.
const WriteTimeout = 10 * time.Second

// Writer sends one text message to one socket.
type Writer interface {
	Write(ctx context.Context, message string) error
}

// Connection is ConnectionManager.Connection.
type Connection struct {
	ID          uuid.UUID
	TenantID    string
	Username    string
	ConnectedAt time.Time
	out         Writer
}

// Manager is ConnectionManager: the open sockets of each tenant, several per
// tenant. Tenant keys are UUID strings, compared upper-cased (Swift keys by
// `UUID.uuidString`, which is upper case).
type Manager struct {
	mu       sync.Mutex
	byTenant map[string]map[uuid.UUID]*Connection
	now      func() time.Time
}

// NewManager returns an empty Manager.
func NewManager() *Manager {
	return &Manager{byTenant: map[string]map[uuid.UUID]*Connection{}, now: time.Now}
}

func key(tenantID string) string { return strings.ToUpper(tenantID) }

// Register adds a socket for tenantID and returns its connection ID.
func (m *Manager) Register(tenantID, username string, out Writer) uuid.UUID {
	id := uuid.New()
	k := key(tenantID)
	m.mu.Lock()
	conns := m.byTenant[k]
	if conns == nil {
		conns = map[uuid.UUID]*Connection{}
		m.byTenant[k] = conns
	}
	conns[id] = &Connection{ID: id, TenantID: k, Username: username, ConnectedAt: m.now(), out: out}
	m.mu.Unlock()
	slog.Info("websocket connected", "tenant_id", k, "username", username, "connection_id", strings.ToUpper(id.String()))
	return id
}

// Remove drops a socket. Removing an unknown one is a no-op.
func (m *Manager) Remove(tenantID string, id uuid.UUID) {
	k := key(tenantID)
	m.mu.Lock()
	m.drop(k, id)
	m.mu.Unlock()
	slog.Info("websocket disconnected", "tenant_id", k, "connection_id", strings.ToUpper(id.String()))
}

func (m *Manager) drop(k string, id uuid.UUID) {
	if conns := m.byTenant[k]; conns != nil {
		delete(conns, id)
		if len(conns) == 0 {
			delete(m.byTenant, k)
		}
	}
}

// Broadcast sends message to every socket of tenantID, one after another. A
// tenant with no sockets is a no-op. A socket whose write fails is logged and
// dropped from the map (Swift drops it too and doesn't close it). The signature
// is memorycompile.BroadcastPublisher's Broadcast field.
//
// The caller's cancellation doesn't reach the writes: a coder/websocket write
// whose context ends closes the socket, and a publisher's request ending must
// not disconnect the user's device. Each write gets WriteTimeout instead.
func (m *Manager) Broadcast(ctx context.Context, tenantID, message string) {
	k := key(tenantID)
	m.mu.Lock()
	conns := make([]*Connection, 0, len(m.byTenant[k]))
	for _, c := range m.byTenant[k] {
		conns = append(conns, c)
	}
	m.mu.Unlock()

	base := context.WithoutCancel(ctx)
	for _, c := range conns {
		wctx, cancel := context.WithTimeout(base, WriteTimeout)
		err := c.out.Write(wctx, message)
		cancel()
		if err != nil {
			slog.WarnContext(ctx, "websocket broadcast failed", "tenant_id", k, "connection_id", strings.ToUpper(c.ID.String()), "error", err)
			m.mu.Lock()
			if m.byTenant[k][c.ID] == c {
				m.drop(k, c.ID)
			}
			m.mu.Unlock()
		}
	}
}

// Connections is ConnectionManager.listConnections: a snapshot, in no order.
func (m *Manager) Connections(tenantID string) []Connection {
	m.mu.Lock()
	defer m.mu.Unlock()
	conns := m.byTenant[key(tenantID)]
	out := make([]Connection, 0, len(conns))
	for _, c := range conns {
		out = append(out, *c)
	}
	return out
}
