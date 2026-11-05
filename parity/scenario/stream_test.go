package scenario

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"lumina/parity/normalize"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var tokens = &Coalesce{Type: "token", Field: "payload"}

func tokenFrame(text string) string {
	b, _ := json.Marshal(map[string]string{"type": "token", "payload": text})
	return "data: " + string(b) + "\n\n"
}

func coalesced(in string, limit int) (any, error) {
	return readFrames(strings.NewReader(in), limit, tokens, normalize.New(), nil)
}

func TestCoalesceJoinsTokenRuns(t *testing.T) {
	const done = "data: {\"type\":\"done\"}\n\n"
	a := sseServer(t, tokenFrame("hel"), tokenFrame("lo "), tokenFrame("world"), done)
	b := sseServer(t, tokenFrame("hello world"), ": keep-alive\n\n", done)
	coalesceStep := func(n int) Step {
		st := framesStep(n)
		st.Coalesce = tokens
		return st
	}

	t.Run("chunk boundaries aren't differences", func(t *testing.T) {
		require.Empty(t, diffOf(t, scenarioOf(coalesceStep(2)), target(a.URL), target(b.URL)))
	})
	t.Run("without the option they are", func(t *testing.T) {
		require.NotEmpty(t, diffOf(t, scenarioOf(framesStep(4)), target(a.URL), target(b.URL)))
	})
	t.Run("different text is a difference", func(t *testing.T) {
		c := sseServer(t, tokenFrame("hello"), tokenFrame(" there"), done)
		diffs := diffOf(t, scenarioOf(coalesceStep(2)), target(a.URL), target(c.URL))
		require.Len(t, diffs, 1)
		require.Equal(t, "$[0].data.payload", diffs[0].Field)
	})
	t.Run("the limit waits for the run to end, whatever the chunking", func(t *testing.T) {
		require.Empty(t, diffOf(t, scenarioOf(coalesceStep(1)), target(a.URL), target(b.URL)))
	})
	t.Run("ignore reaches the joined text", func(t *testing.T) {
		c := sseServer(t, tokenFrame("other"), done)
		st := coalesceStep(2)
		st.Ignore = []string{"$.payload"}
		require.Empty(t, diffOf(t, scenarioOf(st), target(a.URL), target(c.URL)))
	})
}

func TestCoalesceKeepsWhatIsContract(t *testing.T) {
	for name, c := range map[string]struct {
		in   string
		want string
	}{
		"other frames split runs": {
			tokenFrame("a") + tokenFrame("b") + "data: {\"type\":\"source\"}\n\n" + tokenFrame("c") + "data: {\"type\":\"done\"}\n\n",
			`[{"data":{"payload":"ab","type":"token"},"fields":"data: "},{"data":{"type":"source"},"fields":"data: "},{"data":{"payload":"c","type":"token"},"fields":"data: "},{"data":{"type":"done"},"fields":"data: "}]`,
		},
		"another field in the JSON starts a new run": {
			tokenFrame("a") + "data: {\"type\":\"token\",\"payload\":\"b\",\"model\":\"m\"}\n\n",
			`[{"data":{"payload":"a","type":"token"},"fields":"data: "},{"data":{"model":"m","payload":"b","type":"token"},"fields":"data: "}]`,
		},
		"a different wire spelling starts a new run": {
			tokenFrame("a") + "data:{\"type\":\"token\",\"payload\":\"b\"}\n\n",
			`[{"data":{"payload":"a","type":"token"},"fields":"data: "},{"data":{"payload":"b","type":"token"},"fields":"data:"}]`,
		},
		"a named event starts a new run": {
			tokenFrame("a") + "event: x\n" + tokenFrame("b"),
			`[{"data":{"payload":"a","type":"token"},"fields":"data: "},{"data":{"payload":"b","type":"token"},"event":"x","fields":"event: ,data: "}]`,
		},
		"a token with no text field isn't one": {
			"data: {\"type\":\"token\"}\n\n" + tokenFrame("a"),
			`[{"data":{"type":"token"},"fields":"data: "},{"data":{"payload":"a","type":"token"},"fields":"data: "}]`,
		},
		"text frames pass through": {
			"data: not json\n\n" + tokenFrame("a"),
			`[{"data":"not json","fields":"data: "},{"data":{"payload":"a","type":"token"},"fields":"data: "}]`,
		},
		"a run cut by the end of the stream still counts": {
			tokenFrame("a") + tokenFrame("b"),
			`[{"data":{"payload":"ab","type":"token"},"fields":"data: "}]`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := coalesced(c.in, 10)
			require.NoError(t, err)
			raw, err := json.Marshal(got)
			require.NoError(t, err)
			require.JSONEq(t, c.want, string(raw))
		})
	}
}

// Normalizing after the join is what masks a value split across chunks.
func TestCoalesceNormalizesTheJoinedText(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	got, err := coalesced(tokenFrame(id[:10])+tokenFrame(id[10:]), 1)
	require.NoError(t, err)
	require.Equal(t, "<uuid#1:nocase>", got.([]any)[0].(map[string]any)["data"].(map[string]any)["payload"])
}

func TestCoalesceStopsAtTheLimit(t *testing.T) {
	const later = "data: {\"type\":\"later\",\"id\":\"11111111-1111-4111-8111-111111111111\"}\n\n"
	norm := normalize.New()
	got, err := readFrames(strings.NewReader(tokenFrame("a")+later+later), 1, tokens, norm, nil)
	require.NoError(t, err)
	require.Len(t, got, 1, "the frame that ended the run isn't compared")
	require.Equal(t, "<uuid#1:nocase>", norm.Value("22222222-2222-4222-8222-222222222222"), "and isn't numbered either")

	got, err = coalesced(tokenFrame("a")+"data:{\"type\":\"token\",\"payload\":\"b\"}\n\n", 1)
	require.NoError(t, err)
	require.Len(t, got, 1, "a new run past the limit is dropped")
}

// wsScript is what a test server does with an accepted socket.
type wsScript func(t *testing.T, rw *bufio.ReadWriter)

// wsServer accepts a WebSocket with the standard library and runs script.
func wsServer(t *testing.T, script wsScript) string {
	t.Helper()
	return serve(t, wsHandler(t, script)).URL
}

func wsHandler(t *testing.T, script wsScript) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Unauthorized"}}`)
			return
		}
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Version") != "13" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
			acceptFor(r.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n")
		_ = rw.Flush()
		script(t, rw)
	}
}

func serverFrame(fin bool, op byte, payload []byte) []byte {
	first := op
	if fin {
		first |= 0x80
	}
	out := []byte{first}
	switch n := len(payload); {
	case n < 126:
		out = append(out, byte(n))
	case n <= 0xFFFF:
		out = append(out, 126)
		out = binary.BigEndian.AppendUint16(out, uint16(n))
	default:
		out = append(out, 127)
		out = binary.BigEndian.AppendUint64(out, uint64(n))
	}
	return append(out, payload...)
}

func serverSend(rw *bufio.ReadWriter, frames ...[]byte) {
	for _, f := range frames {
		_, _ = rw.Write(f)
	}
	_ = rw.Flush()
}

// readClient reads one frame and checks it's masked, as RFC 6455 says a client's
// must be. A client that has gone reads as a close. It runs in the server's
// goroutine, so it can't use require.
func readClient(t *testing.T, r io.Reader) (byte, string) {
	t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return opClose, ""
	}
	if head[1]&0x80 == 0 {
		t.Error("client frame isn't masked")
	}
	size := int(head[1] & 0x7F)
	if size == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return opClose, ""
		}
		size = int(binary.BigEndian.Uint16(ext[:]))
	}
	var mask [4]byte
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return opClose, ""
	}
	if _, err := io.ReadFull(r, payload); err != nil {
		return opClose, ""
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return head[0] & 0x0F, string(payload)
}

func wsStep(frames int, send ...string) Step {
	return Step{
		Name: "ws", As: "alice",
		Request: Request{Method: http.MethodGet, Path: "/v1/ws"},
		WS:      &WS{Send: send},
		Frames:  frames,
	}
}

func waitForClose(t *testing.T, rw *bufio.ReadWriter) {
	for {
		if op, _ := readClient(t, rw); op == opClose {
			return
		}
	}
}

// echoUntilClose is ConnectionManager's shape: every text message comes back.
func echoUntilClose(t *testing.T, rw *bufio.ReadWriter) {
	for {
		op, msg := readClient(t, rw)
		if op == opClose {
			return
		}
		serverSend(rw, serverFrame(true, opText, []byte(msg)))
	}
}

func TestWSComparesMessages(t *testing.T) {
	const idA, idB = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	send := func(id string) string { return `{"type":"memorySaved","id":"` + id + `"}` }
	a := wsServer(t, echoUntilClose)

	t.Run("the same frames with random ids", func(t *testing.T) {
		b := wsServer(t, func(_ *testing.T, rw *bufio.ReadWriter) {
			serverSend(rw, serverFrame(true, opText, []byte(send(idB))))
		})
		require.Empty(t, diffOf(t, scenarioOf(wsStep(1, send(idA))), target(a), target(b)))
	})
	t.Run("a frame differs", func(t *testing.T) {
		b := wsServer(t, func(_ *testing.T, rw *bufio.ReadWriter) {
			serverSend(rw, serverFrame(true, opText, []byte(`{"type":"other","id":"`+idB+`"}`)))
		})
		diffs := diffOf(t, scenarioOf(wsStep(1, send(idA))), target(a), target(b))
		require.Len(t, diffs, 1)
		require.Equal(t, "$[0].text.type", diffs[0].Field)
	})
	t.Run("a frame one server doesn't relay shows at the timeout", func(t *testing.T) {
		silent := wsServer(t, waitForClose)
		st := wsStep(1, send(idA))
		st.FramesTimeout = 200 * time.Millisecond
		diffs := diffOf(t, scenarioOf(st), target(a), target(silent))
		require.Equal(t, "$.length", diffs[0].Field)
	})
	t.Run("a refused upgrade compares as a response", func(t *testing.T) {
		st := wsStep(1, send(idA))
		st.As = ""
		tr, err := Run(context.Background(), scenarioOf(st), target(a))
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, tr.Steps[0].Status)
		require.Equal(t, map[string]any{"error": map[string]any{"message": "Unauthorized"}}, tr.Steps[0].Body)
	})
	t.Run("handshake headers compare when asked", func(t *testing.T) {
		st := wsStep(1, send(idA))
		st.Headers = []string{"Upgrade"}
		tr, err := Run(context.Background(), scenarioOf(st), target(a))
		require.NoError(t, err)
		require.Equal(t, http.StatusSwitchingProtocols, tr.Steps[0].Status)
		require.Equal(t, "websocket", tr.Steps[0].Headers["Upgrade"])
	})
	t.Run("vars reach what's sent", func(t *testing.T) {
		var seen atomic.Value
		b := wsServer(t, func(t *testing.T, rw *bufio.ReadWriter) {
			_, msg := readClient(t, rw)
			seen.Store(msg)
			serverSend(rw, serverFrame(true, opText, []byte(msg)))
		})
		st := wsStep(1, `{"type":"x","id":"{{id}}"}`)
		res, err := runStep(context.Background(), scenarioOf(st), st, target(b), http.DefaultClient, normalize.New(), map[string]string{"id": "abc"})
		require.NoError(t, err)
		require.Equal(t, http.StatusSwitchingProtocols, res.Status)
		require.Equal(t, `{"type":"x","id":"abc"}`, seen.Load())
	})
}

func TestWSFraming(t *testing.T) {
	long := strings.Repeat("x", 70000)
	medium := strings.Repeat("y", 300)
	var pong atomic.Value
	srv := wsServer(t, func(t *testing.T, rw *bufio.ReadWriter) {
		serverSend(rw,
			serverFrame(true, opPing, []byte("are you there")),
			serverFrame(false, opText, []byte(`{"type":`)),
			serverFrame(true, opPing, []byte("mid-message")),
			serverFrame(false, opContinue, []byte(`"split",`)),
			serverFrame(true, opContinue, []byte(`"n":1}`)),
			serverFrame(true, opBinary, []byte{1, 2, 3}),
			serverFrame(true, opText, []byte(medium)),
			serverFrame(true, opText, []byte(long)),
			serverFrame(true, opClose, append(binary.BigEndian.AppendUint16(nil, 1009), "too big"...)),
		)
		if op, payload := readClient(t, rw); op == opPong {
			pong.Store(payload)
		}
		waitForClose(t, rw)
	})
	tr, err := Run(context.Background(), scenarioOf(wsStep(10)), target(srv))
	require.NoError(t, err)
	got := tr.Steps[0].Body.([]any)
	require.Len(t, got, 5, "the close frame ends the read")
	require.Equal(t, map[string]any{"text": map[string]any{"type": "split", "n": json.Number("1")}}, got[0])
	require.Contains(t, got[1], "binary")
	require.Equal(t, map[string]any{"text": medium}, got[2])
	require.Equal(t, map[string]any{"text": long}, got[3])
	require.Equal(t, map[string]any{"close": 1009, "reason": "too big"}, got[4])
	// The server's goroutine reads the pong after the client has finished reading.
	require.Eventually(t, func() bool { return pong.Load() == "are you there" }, 2*time.Second, 5*time.Millisecond, "a ping is answered with its payload")
}

func TestWSRefusesBadServerFrames(t *testing.T) {
	for name, frame := range map[string][]byte{
		"masked":        {0x81, 0x81, 1, 2, 3, 4, 5},
		"reserved bits": {0xC1, 0x01, 'x'},
		"bad opcode":    {0x83, 0x01, 'x'},
		"stray cont":    {0x80, 0x01, 'x'},
		"oversized":     append([]byte{0x81, 127}, binary.BigEndian.AppendUint64(nil, uint64(maxFrameBytes+1))...),
	} {
		t.Run(name, func(t *testing.T) {
			srv := wsServer(t, func(_ *testing.T, rw *bufio.ReadWriter) { serverSend(rw, frame) })
			st := wsStep(1)
			st.FramesTimeout = time.Second
			tr, err := Run(context.Background(), scenarioOf(st), target(srv))
			require.NoError(t, err, "a read that stops isn't a run error")
			require.Empty(t, tr.Steps[0].Body)
		})
	}
}

func TestWSTrigger(t *testing.T) {
	sockets := make(chan *bufio.ReadWriter, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ws", wsHandler(t, func(t *testing.T, rw *bufio.ReadWriter) {
		sockets <- rw
		waitForClose(t, rw)
	}))
	mux.HandleFunc("POST /trigger", func(w http.ResponseWriter, _ *http.Request) {
		serverSend(<-sockets, serverFrame(true, opText, []byte(`{"type":"memorySaved"}`)))
		w.WriteHeader(http.StatusAccepted)
	})
	srv := serve(t, mux.ServeHTTP)

	st := wsStep(1)
	st.Trigger = &Trigger{Request: Request{Method: http.MethodPost, Path: "/trigger"}}
	tr, err := Run(context.Background(), scenarioOf(st), target(srv.URL))
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, tr.Steps[0].TriggerStatus)
	require.Equal(t, []any{map[string]any{"text": map[string]any{"type": "memorySaved"}}}, tr.Steps[0].Body)
}

func TestWSSmokeSendsNoTrigger(t *testing.T) {
	srv := wsServer(t, waitForClose)
	smoke := target(srv)
	smoke.Token = "x"
	st := wsStep(1)
	st.FramesTimeout = 200 * time.Millisecond
	st.Trigger = &Trigger{Request: Request{Method: http.MethodPost, Path: "/nope"}}
	tr, err := Run(context.Background(), scenarioOf(st), smoke)
	require.NoError(t, err)
	require.Zero(t, tr.Steps[0].TriggerStatus)
}

func TestWSCanConnectWithoutReading(t *testing.T) {
	srv := wsServer(t, waitForClose)
	tr, err := Run(context.Background(), scenarioOf(wsStep(0)), target(srv))
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, tr.Steps[0].Status)
	require.Empty(t, tr.Steps[0].Body)
}

// A client timeout hides the upgraded writer, so the step works under the default client.
func TestWSWorksUnderTheDefaultClient(t *testing.T) {
	srv := wsServer(t, echoUntilClose)
	tgt := target(srv)
	tgt.Client = &http.Client{Timeout: 5 * time.Second}
	tr, err := Run(context.Background(), scenarioOf(wsStep(1, `{"type":"x"}`)), tgt)
	require.NoError(t, err)
	require.Len(t, tr.Steps[0].Body, 1)
}

func TestValidateCoalesceAndWS(t *testing.T) {
	get := Request{Method: http.MethodGet, Path: "/x"}
	for name, st := range map[string]Step{
		"coalesce without frames": {Request: get, Stream: true, Coalesce: tokens},
		"coalesce without type":   {Request: get, Stream: true, Frames: 1, Coalesce: &Coalesce{Field: "payload"}},
		"coalesce without field":  {Request: get, Stream: true, Frames: 1, Coalesce: &Coalesce{Type: "token"}},
		"coalesce on a socket":    {Request: get, WS: &WS{}, Frames: 1, Coalesce: tokens},
		"ws and stream":           {Request: get, WS: &WS{}, Stream: true},
		"ws and binary":           {Request: get, WS: &WS{}, Binary: true},
		"ws and zip":              {Request: get, WS: &WS{}, Zip: true},
		"ws and capture":          {Request: get, WS: &WS{}, Capture: map[string]string{"a": "$.a"}},
		"timeout on a bare ws":    {Request: get, WS: &WS{}, FramesTimeout: time.Second},
	} {
		require.Error(t, st.validate(), name)
	}
	for name, st := range map[string]Step{
		"coalesce":          {Request: get, Stream: true, Frames: 1, Coalesce: tokens},
		"ws":                {Request: get, WS: &WS{}},
		"ws with messages":  {Request: get, WS: &WS{Send: []string{"x"}}, Frames: 1, FramesTimeout: time.Second},
		"ws with a trigger": {Request: get, WS: &WS{}, Frames: 1, Trigger: &Trigger{Request: get}},
	} {
		require.NoError(t, st.validate(), name)
	}
}
