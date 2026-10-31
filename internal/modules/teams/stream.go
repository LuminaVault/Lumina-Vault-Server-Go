package teams

import (
	"bytes"
	"encoding/json"
	"lumina/internal/vaultaccess"
	"lumina/internal/vaultactivity"
	"net/http"

	"github.com/labstack/echo/v5"
)

// activityStream is GET /v1/vaults/{vaultID}/activity/stream, Swift's
// VaultActivitySSEResponse: no initial frame, then one frame per event
// recorded in this process for the vault, until the client goes away.
//
// In-process only, as in Swift: an event recorded by the other server (Swift
// while both run) never reaches this stream. See internal/vaultactivity.
func (m *Module) activityStream(c *echo.Context) error {
	vaultID, err := uuidParam(c, "vaultID")
	if err != nil {
		return err
	}
	if _, err := m.resolve(c, vaultID, vaultaccess.Read); err != nil {
		return err
	}
	events, cancel := m.hub.Subscribe(vaultID)
	defer cancel()

	w := c.Response()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return nil //nolint:nilerr // headers are sent; nothing left to report
	}
	done := c.Request().Context().Done()
	for {
		select {
		case <-done:
			return nil
		case ev := <-events:
			frame, err := sseFrame(ev)
			if err != nil {
				return nil //nolint:nilerr // the response is committed
			}
			if _, err := w.Write(frame); err != nil {
				return nil //nolint:nilerr // client gone
			}
			if err := rc.Flush(); err != nil {
				return nil //nolint:nilerr // client gone
			}
		}
	}
}

// sseFrame is "id: <ID>\nevent: activity\ndata: <json>\n\n" with the JSON as
// Foundation's JSONEncoder writes it: compact, keys in declaration order, no
// HTML escaping, and "/" escaped as "\/".
func sseFrame(ev vaultactivity.Activity) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ev); err != nil {
		return nil, err
	}
	data := bytes.ReplaceAll(bytes.TrimSuffix(buf.Bytes(), []byte("\n")), []byte("/"), []byte(`\/`))
	out := make([]byte, 0, len(data)+64)
	out = append(out, "id: "...)
	out = append(out, ev.ID.String()...)
	out = append(out, "\nevent: activity\ndata: "...)
	out = append(out, data...)
	out = append(out, "\n\n"...)
	return out, nil
}
