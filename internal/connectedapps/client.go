package connectedapps

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds one whole call (initialize, notify, request).
	DefaultTimeout = 15 * time.Second
	// DefaultMaxBytes caps how much of a response body is read.
	DefaultMaxBytes = 64 << 10

	protocolVersion = "2025-06-18"
	clientVersion   = "1"
	truncatedMarker = "\n[truncated]"
)

// ErrUnauthorized is returned when the remote app answers 401.
var ErrUnauthorized = errors.New("connectedapps: unauthorized")

// RemoteTool is one tool as listed by a remote MCP server.
type RemoteTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Validator checks an outbound URL before any request (ssrfguard.Guard satisfies it).
type Validator interface {
	Validate(ctx context.Context, raw string) (*url.URL, error)
}

// Client speaks MCP Streamable HTTP (JSON-RPC 2.0) to a remote app on behalf
// of one user's bearer token. It never logs.
type Client struct {
	HTTP     *http.Client
	Guard    Validator
	Timeout  time.Duration
	MaxBytes int64
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// ListTools returns the tools the remote server advertises.
func (c *Client) ListTools(ctx context.Context, endpoint, bearer string) ([]RemoteTool, error) {
	res, _, err := c.run(ctx, endpoint, bearer, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, fmt.Errorf("connectedapps: bad tools/list result: %w", err)
	}
	tools := make([]RemoteTool, 0, len(out.Tools))
	for _, t := range out.Tools {
		tools = append(tools, RemoteTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return tools, nil
}

// CallTool invokes a tool and returns its concatenated text content. A tool
// that reports isError returns its text with a nil error.
func (c *Client) CallTool(ctx context.Context, endpoint, bearer, name string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	res, truncated, err := c.run(ctx, endpoint, bearer, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var text string
	if truncated {
		// The body was cut at the cap, so the JSON is incomplete: recover the
		// text prefix leniently.
		text = partialText(res)
	} else {
		var out struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(res, &out); err != nil {
			return "", fmt.Errorf("connectedapps: bad tools/call result: %w", err)
		}
		var sb strings.Builder
		for _, p := range out.Content {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		text = sb.String()
	}
	return capText(text, c.maxBytes(), truncated), nil
}

func (c *Client) maxBytes() int64 {
	if c.MaxBytes > 0 {
		return c.MaxBytes
	}
	return DefaultMaxBytes
}

func capText(s string, max int64, force bool) string {
	if !force && int64(len(s)) <= max {
		return s
	}
	keep := int(max) - len(truncatedMarker)
	if keep < 0 {
		keep = 0
	}
	if keep > len(s) {
		keep = len(s)
	}
	// Linear: drop invalid bytes (an incomplete rune at the cut, or junk from
	// a hostile app) without discarding the content after them.
	return strings.ToValidUTF8(s[:keep], "") + truncatedMarker
}

// run performs initialize, notifications/initialized and then the real request.
// It returns the matching response's result; truncated reports that the body hit the cap.
func (c *Client) run(ctx context.Context, endpoint, bearer, method string, params any) (json.RawMessage, bool, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, trunc, err := c.run1(ctx, endpoint, bearer, method, params)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, false, errors.New("connectedapps: request timed out")
	}
	return res, trunc, err
}

func (c *Client) run1(ctx context.Context, endpoint, bearer, method string, params any) (json.RawMessage, bool, error) {
	initParams := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "LuminaVault", "version": clientVersion},
	}
	hdr, _, _, err := c.post(ctx, endpoint, bearer, "", 1, "initialize", initParams)
	if err != nil {
		return nil, false, err
	}
	session := hdr.Get("Mcp-Session-Id")
	if _, _, _, err := c.post(ctx, endpoint, bearer, session, 0, "notifications/initialized", nil); err != nil {
		return nil, false, err
	}
	_, res, trunc, err := c.post(ctx, endpoint, bearer, session, 2, method, params)
	return res, trunc, err
}

// post sends one JSON-RPC message. id 0 means a notification (no id, no result expected).
func (c *Client) post(ctx context.Context, endpoint, bearer, session string, id int, method string, params any) (http.Header, json.RawMessage, bool, error) {
	if c.Guard == nil {
		return nil, nil, false, errors.New("connectedapps: no URL guard configured")
	}
	if c.HTTP == nil {
		// Fail closed: never fall back to http.DefaultClient (no dial-time SSRF guard).
		return nil, nil, false, errors.New("connectedapps: no HTTP client configured")
	}
	u, err := c.Guard.Validate(ctx, endpoint)
	if err != nil {
		return nil, nil, false, fmt.Errorf("connectedapps: endpoint rejected: %w", err)
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != 0 {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, nil, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, false, errors.New("connectedapps: bad request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, false, ctx.Err()
		}
		// Do not wrap err: url.Error text is safe but keep it terse.
		return nil, nil, false, errors.New("connectedapps: request failed")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, nil, false, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, false, fmt.Errorf("connectedapps: %s returned HTTP %d", method, resp.StatusCode)
	}
	if id == 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.Header, nil, false, nil
	}

	max := c.maxBytes()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, false, ctx.Err()
		}
		return nil, nil, false, errors.New("connectedapps: read failed")
	}
	truncated := int64(len(raw)) > max
	if truncated {
		raw = raw[:max]
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt == "text/event-stream" {
		raw = sseData(raw, id, truncated)
	}
	if truncated {
		if method != "tools/call" {
			return nil, nil, false, errors.New("connectedapps: response too large")
		}
		return resp.Header, raw, true, nil
	}
	if raw == nil {
		return nil, nil, false, errors.New("connectedapps: no response in stream")
	}
	var r rpcResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, nil, false, errors.New("connectedapps: malformed JSON-RPC response")
	}
	if r.Error != nil {
		return nil, nil, false, fmt.Errorf("connectedapps: %s failed: %s (code %d)", method, r.Error.Message, r.Error.Code)
	}
	return resp.Header, r.Result, false, nil
}

// sseData returns the data payload of the event whose JSON-RPC id matches.
// When truncated, the final event may be cut, so the first data payload that
// is not a complete non-matching message is returned as-is.
func sseData(raw []byte, id int, truncated bool) []byte {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), len(raw)+1)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := []byte(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		var probe struct {
			ID *int `json:"id"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil {
			if truncated {
				return payload // cut mid-event: best effort
			}
			continue
		}
		if probe.ID != nil && *probe.ID == id {
			return payload
		}
	}
	return nil
}

type jsonFrame struct {
	obj       bool
	expectKey bool
	key       string
}

// partialText recovers the text of result.content[] items of type "text" from
// a possibly cut-off response body, in order. Items outside result.content
// (structuredContent, resource.text, ...) are ignored. The final cut string,
// if it is a text value, is recovered as a decoded prefix. Garbage yields "".
func partialText(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var stack []jsonFrame
	var out strings.Builder
	var itemText strings.Builder
	itemType, hasText := "", false
	// isItem: stack is {root obj key "result", result obj key "content", array, item obj}.
	isItem := func() bool {
		return len(stack) == 4 && stack[0].obj && stack[0].key == "result" &&
			stack[1].obj && stack[1].key == "content" && !stack[2].obj && stack[3].obj
	}
	flush := func() {
		if hasText && (itemType == "text" || itemType == "") {
			out.WriteString(itemText.String())
		}
		itemText.Reset()
		itemType, hasText = "", false
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			{
				// A cut string value: recover its prefix when it is an item's text.
				if len(stack) == 4 && isItem() && stack[3].key == "text" && !stack[3].expectKey {
					rest := raw[dec.InputOffset():]
					if q := bytes.IndexByte(rest, '"'); q >= 0 && itemType != "image" {
						itemText.Reset()
						itemText.WriteString(decodePartialString(rest[q+1:]))
						hasText = true
					}
				}
				if len(stack) >= 4 && isItem() {
					flush()
				}
			}
			return out.String()
		}
		// Shape: stack frames mirror open containers. Position of this token:
		var top *jsonFrame
		if n := len(stack); n > 0 {
			top = &stack[n-1]
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				if top != nil && top.obj {
					top.expectKey = false
				}
				stack = append(stack, jsonFrame{obj: v == '{', expectKey: v == '{'})
			default:
				if isItem() {
					flush()
				}
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1].obj {
					stack[n-1].expectKey = true
				}
			}
		case string:
			if top != nil && top.obj && top.expectKey {
				top.key, top.expectKey = v, false
			} else {
				if isItem() && top.key == "text" {
					itemText.WriteString(v)
					hasText = true
				} else if isItem() && top.key == "type" {
					itemType = v
				}
				if top != nil && top.obj {
					top.expectKey = true
				}
			}
		default:
			if top != nil && top.obj {
				top.expectKey = true
			}
		}
	}
}

func decodePartialString(rest []byte) string {
	var sb strings.Builder
	for k := 0; k < len(rest); k++ {
		ch := rest[k]
		switch {
		case ch == '"':
			return sb.String()
		case ch == '\\' && k+1 < len(rest):
			k++
			switch rest[k] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case 'b', 'f':
			case 'u':
				if k+4 < len(rest) {
					var r rune
					if _, err := fmt.Sscanf(string(rest[k+1:k+5]), "%04x", &r); err == nil {
						sb.WriteRune(r)
					}
					k += 4
				}
			default:
				sb.WriteByte(rest[k])
			}
		case ch == '\\':
			// lone trailing backslash: cut mid-escape
		default:
			sb.WriteByte(ch)
		}
	}
	return sb.String()
}
