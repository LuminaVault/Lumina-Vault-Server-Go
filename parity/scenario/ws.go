package scenario

import (
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455's accept hash, not a security boundary here
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"lumina/parity/normalize"
	"net/http"
)

// WS is a WebSocket step. The handshake is compared like any response, headers
// included. After it, Send goes out in order, then the Trigger, then up to Frames
// messages are read, within FramesTimeout. Fewer by then is not an error, as with
// SSE, so a frame a server refuses to relay shows as a shorter list.
//
// This is a small RFC 6455 client on net/http's protocol switch, enough to compare
// a server's frames: text, binary, close, and answering pings. It offers no
// extensions and no subprotocols, as iOS doesn't. It keeps the harness free of a
// dependency that port code hasn't vetted (D-deps).
//
// What it can't show is fan-out to a second socket or a dropped connection's
// cleanup: those are Go tests (wave-5-plan.md, "Streaming").
type WS struct {
	Send []string `yaml:"send"` // text messages, with {{vars}} substituted
}

const (
	wsGUID      = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	opContinue  = 0x0
	opText      = 0x1
	opBinary    = 0x2
	opClose     = 0x8
	opPing      = 0x9
	opPong      = 0xA
	wsMaxHeader = 14
)

// prepareUpgrade asks for a WebSocket on req and returns the key it sent.
func prepareUpgrade(req *http.Request) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := base64.StdEncoding.EncodeToString(raw)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	return key, nil
}

// upgradeClient drops the client timeout: net/http wraps the body of a response
// under a timeout, which hides the writer a protocol switch gives it. The request
// context still bounds the handshake, and runSocket bounds the rest.
func upgradeClient(c *http.Client) *http.Client {
	out := *c
	out.Timeout = 0
	return &out
}

// runSocket drives an accepted upgrade and returns the messages it read.
func runSocket(
	stepCtx, ctx context.Context,
	st Step,
	t Target,
	client *http.Client,
	resp *http.Response,
	key string,
	vars map[string]string,
	norm *normalize.Normalizer,
	ignore []string,
) (any, int, error) {
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), acceptFor(key); got != want {
		return nil, 0, fmt.Errorf("bad Sec-WebSocket-Accept %q, want %q", got, want)
	}
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return nil, 0, errors.New("the upgraded connection can't be written to")
	}
	defer context.AfterFunc(stepCtx, func() { _ = rw.Close() })()
	conn := &wsConn{rw: rw}

	for _, msg := range st.WS.Send {
		if err := conn.write(opText, []byte(substitute(msg, vars))); err != nil {
			return nil, 0, fmt.Errorf("send: %w", err)
		}
	}
	triggerStatus := 0
	// Smoke checks statuses on a deployed server, so it doesn't need the write.
	if st.Trigger != nil && t.Token == "" {
		var err error
		if triggerStatus, err = sendTrigger(ctx, t, client, *st.Trigger, vars); err != nil {
			return nil, 0, fmt.Errorf("trigger: %w", err)
		}
	}
	messages := []any{}
	for len(messages) < st.Frames {
		op, payload, err := conn.next()
		if err != nil {
			break // deadline or the connection dropped; compare what was read
		}
		if op == opClose {
			messages = append(messages, closeValue(payload))
			break
		}
		v, err := messageValue(op, payload, norm, ignore)
		if err != nil {
			return nil, 0, err
		}
		messages = append(messages, v)
	}
	_ = conn.write(opClose, binary.BigEndian.AppendUint16(nil, 1000))
	return messages, triggerStatus, nil
}

func acceptFor(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID)) //nolint:gosec // see the import
	return base64.StdEncoding.EncodeToString(sum[:])
}

func messageValue(op byte, payload []byte, norm *normalize.Normalizer, ignore []string) (any, error) {
	if op == opBinary {
		return map[string]any{"binary": binaryBody(payload)}, nil
	}
	body, err := norm.Body(payload, ignore)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": body}, nil
}

// closeValue is a close frame as data: the code and reason are the contract.
func closeValue(payload []byte) any {
	if len(payload) < 2 {
		return map[string]any{"close": 0, "reason": ""}
	}
	return map[string]any{"close": int(binary.BigEndian.Uint16(payload)), "reason": string(payload[2:])}
}

type wsConn struct {
	rw io.ReadWriter
}

// write sends one masked frame, as a client must.
func (c *wsConn) write(op byte, payload []byte) error {
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame := make([]byte, 0, wsMaxHeader+len(payload))
	frame = append(frame, 0x80|op)
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n <= 0xFFFF:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := c.rw.Write(frame)
	return err
}

// next returns the next message or close frame, joining fragments and answering
// pings on the way.
func (c *wsConn) next() (byte, []byte, error) {
	var (
		op      byte
		message []byte
		started bool
	)
	for {
		fin, frameOp, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch frameOp {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			return opClose, payload, nil
		case opContinue:
			if !started {
				return 0, nil, errors.New("continuation frame with nothing to continue")
			}
		case opText, opBinary:
			if started {
				return 0, nil, errors.New("new message inside a fragmented one")
			}
			op, started = frameOp, true
		default:
			return 0, nil, fmt.Errorf("unknown opcode %#x", frameOp)
		}
		if len(message)+len(payload) > maxFrameBytes {
			return 0, nil, errors.New("message over the harness's limit")
		}
		message = append(message, payload...)
		if fin {
			return op, message, nil
		}
	}
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.rw, head[:]); err != nil {
		return false, 0, nil, err
	}
	if head[1]&0x80 != 0 {
		return false, 0, nil, errors.New("a server must not mask its frames")
	}
	if head[0]&0x70 != 0 {
		return false, 0, nil, errors.New("reserved bits set, and no extension was offered")
	}
	size := uint64(head[1] & 0x7F)
	switch size {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.rw, ext[:]); err != nil {
			return false, 0, nil, err
		}
		size = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.rw, ext[:]); err != nil {
			return false, 0, nil, err
		}
		size = binary.BigEndian.Uint64(ext[:])
	}
	if size > maxFrameBytes {
		return false, 0, nil, errors.New("frame over the harness's limit")
	}
	payload = make([]byte, size)
	if _, err = io.ReadFull(c.rw, payload); err != nil {
		return false, 0, nil, err
	}
	return head[0]&0x80 != 0, head[0] & 0x0F, payload, nil
}
