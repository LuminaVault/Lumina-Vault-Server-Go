package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"lumina/internal/wire"
	"math"
	"sort"
)

// rpcError is MCPError: a protocol-level failure, answered as a JSON-RPC error.
// A tool that merely fails returns a normal result with isError instead.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

func parseError(m string) *rpcError     { return &rpcError{-32700, m} }
func methodNotFound(m string) *rpcError { return &rpcError{-32601, m} }
func invalidParams(m string) *rpcError  { return &rpcError{-32602, m} }
func internalError(m string) *rpcError  { return &rpcError{-32603, m} }

// request is MCPRequest. Values are Swift's JSONValue: decoded with float64
// numbers, so an id of 1e2 comes back as 100, as Swift's Double round-trip does.
type request struct {
	id     any // nil when absent or null: a notification
	method string
	params any
}

// decodeRequest is JSONDecoder().decode(MCPRequest.self, from:): any failure
// is the caller's parse error.
func decodeRequest(body []byte) (*request, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var top map[string]any
	if err := dec.Decode(&top); err != nil || top == nil {
		return nil, errors.New("not an object")
	}
	if dec.More() {
		return nil, errors.New("trailing data")
	}
	if v, ok := top["jsonrpc"]; ok && v != nil {
		if _, isString := v.(string); !isString {
			return nil, errors.New("jsonrpc is not a string")
		}
	}
	method, ok := top["method"].(string)
	if !ok {
		return nil, errors.New("method missing or not a string")
	}
	return &request{id: top["id"], method: method, params: top["params"]}, nil
}

// response is MCPResponse: nil id, result and error are left out.
type response struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

func success(id, result any) response {
	return response{JSONRPC: "2.0", ID: swiftValue(id), Result: swiftValue(result)}
}

func failure(id any, err *rpcError) response {
	return response{JSONRPC: "2.0", ID: swiftValue(id), Error: err}
}

// swiftValue rewrites a decoded JSON tree so it encodes as Swift's JSONValue
// does: every number is a Double. Ints written by tools stay ints.
func swiftValue(v any) any {
	switch x := v.(type) {
	case float64:
		return wire.Float64(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = swiftValue(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = swiftValue(e)
		}
		return out
	default:
		return v
	}
}

// toolContent is MCPController.toolContent: a structured payload plus its
// text rendering. Swift renders the text with a dictionary whose key order
// varies per process, so the text is not contract; parity ignores it.
func toolContent(result map[string]any) map[string]any {
	isError, _ := result["isError"].(bool)
	text := "{}"
	if b, err := wire.MarshalFoundation(swiftValue(result)); err == nil {
		text = string(b)
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": text}},
		"structuredContent": result,
		"isError":           isError,
	}
}

// Argument accessors: JSONValue's typed views.

func argString(args map[string]any, key string) (string, bool) {
	s, ok := args[key].(string)
	return s, ok
}

func argStringPtr(args map[string]any, key string) *string {
	if s, ok := args[key].(string); ok {
		return &s
	}
	return nil
}

// argInt is JSONValue.intValue: Int(Double), truncating; non-numbers are nil.
func argInt(args map[string]any, key string) (int, bool) {
	f, ok := args[key].(float64)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int(f), true
}

func argIntPtr(args map[string]any, key string) *int {
	if n, ok := argInt(args, key); ok {
		return &n
	}
	return nil
}

func intOr(args map[string]any, key string, def int) int {
	if n, ok := argInt(args, key); ok {
		return n
	}
	return def
}

// unknownArgument is the first argument name, sorted, the tool's schema doesn't list.
func unknownArgument(t tool, args map[string]any) (string, bool) {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := t.properties[k]; !ok {
			return k, true
		}
	}
	return "", false
}
