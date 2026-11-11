package ws

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"lumina/internal/ids"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// PingInterval is Hummingbird's default autoPing (WebSocketServerConfiguration,
// hummingbird-websocket 2.7.0): a ping every 30 s, and a socket that hasn't
// answered within the next period is closed.
const PingInterval = 30 * time.Second

// Accept upgrades the request with Swift's server settings: no Origin check
// (Hummingbird has none, and coder/websocket's default refuses cross-origin
// upgrades) and no compression extension (Hummingbird's default list is empty).
// Authentication happens before this, in the route's middleware.
func Accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
}

// connWriter adapts a coder/websocket connection to Writer.
type connWriter struct{ c *websocket.Conn }

func (w connWriter) Write(ctx context.Context, message string) error {
	return w.c.Write(ctx, websocket.MessageText, []byte(message))
}

// Serve is the body of Swift's `/v1/ws` onUpgrade closure. It registers conn for
// tenant, relays each inbound text frame that passes Evaluate to every socket of
// the tenant (the sender's own included), logs each rejection at warning, and
// removes the socket when the peer goes away or ctx ends. Binary frames are
// ignored. A message over MaxMessageBytes closes the socket with 1009, as
// Hummingbird's `messages(maxSize:)` does. Serve closes conn before returning.
func (m *Manager) Serve(ctx context.Context, conn *websocket.Conn, tenant uuid.UUID, username string) {
	tenantID := ids.Upper(tenant)
	conn.SetReadLimit(MaxMessageBytes)
	id := m.Register(tenantID, username, connWriter{conn})
	defer m.Remove(tenantID, id)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go keepAlive(ctx, conn, cancel)

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			slog.DebugContext(ctx, "websocket closed", "error", err)
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		message := string(data)
		switch d := Evaluate(message); d.Reason {
		case Allow:
			m.Broadcast(ctx, tenantID, message)
		case RejectOversize:
			slog.WarnContext(ctx, "ws.broadcast rejected oversize", "tenant_id", tenantID, "bytes", d.ByteCount)
		default:
			slog.WarnContext(ctx, "ws.broadcast rejected "+d.Reason.String()+" message", "tenant_id", tenantID)
		}
	}
}

// keepAlive pings every PingInterval and cancels the socket's context when a
// pong doesn't arrive within one interval. Ping needs Serve's concurrent Read.
func keepAlive(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, PingInterval)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
}
