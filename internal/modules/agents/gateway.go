package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// gatewayClient is AgentGatewayClient: the calls the Agents page and agent rooms
// make to one Hermes gateway api_server, the caller's own. The base URL has been
// through the SSRF guard (hermesresolve). Reads are GETs; chat is the one call
// that makes the agent act.
type gatewayClient struct {
	baseURL    string
	authHeader *string
	http       *http.Client
	log        *slog.Logger
}

const (
	gatewayTimeout     = 8 * time.Second
	gatewayChatTimeout = 180 * time.Second
	gatewayBodyCap     = 4 * 1024 * 1024
)

// failureKind is AgentGatewayClient.Failure.
type failureKind int

const (
	failUnreachable failureKind = iota
	failHTTP
	failInvalidResponse
	failNotSupported // this Hermes predates /api/profiles/*
	failInvalidID    // a session id or profile name that can't be one path segment
)

type gatewayError struct {
	kind   failureKind
	status int // failHTTP only
}

func (e *gatewayError) Error() string {
	if e.kind == failHTTP {
		return fmt.Sprintf("agents gateway: http %d", e.status)
	}
	return fmt.Sprintf("agents gateway: failure %d", e.kind)
}

var (
	errUnreachable     = &gatewayError{kind: failUnreachable}
	errInvalidResponse = &gatewayError{kind: failInvalidResponse}
	errNotSupported    = &gatewayError{kind: failNotSupported}
	errInvalidID       = &gatewayError{kind: failInvalidID}
)

func httpFailure(status int) *gatewayError { return &gatewayError{kind: failHTTP, status: status} }

func isFailure(err error, kind failureKind) bool {
	var g *gatewayError
	return errors.As(err, &g) && g.kind == kind
}

// instanceInfo is AgentGatewayClient.Instance.
type instanceInfo struct {
	hostname      *string
	version       *string
	profiles      []string
	activeProfile *string // the profile this gateway runs as, which answers session chat
}

// sessionPage is AgentGatewayClient.SessionPage.
type sessionPage struct {
	profileAware bool // false when the gateway only knows its own profile
	sessions     []sessionDTO
}

// instance is GET /api/instance; errNotSupported on an older Hermes.
func (g *gatewayClient) instance(ctx context.Context) (instanceInfo, error) {
	object, err := g.getObject(ctx, "api/instance", nil, true)
	if err != nil {
		return instanceInfo{}, err
	}
	profiles := stringArray(object["profiles"])
	if profiles == nil {
		profiles = []string{}
	}
	return instanceInfo{
		hostname: stringValue(object["hostname"]), version: stringValue(object["version"]),
		profiles: profiles, activeProfile: stringValue(object["profile"]),
	}, nil
}

// chat is one agent turn in a named session, creating the session first if it
// doesn't exist. It runs as the gateway's own profile; a turn can use tools, so
// the timeout is long.
func (g *gatewayClient) chat(ctx context.Context, sessionID, title, systemMessage, message string) (string, *int, error) {
	id, err := pathSegment(sessionID)
	if err != nil {
		return "", nil, err
	}
	created, err := g.send(ctx, "api/sessions", [][2]string{{"id", sessionID}, {"title", title}}, gatewayTimeout)
	if err != nil {
		return "", nil, err
	}
	// 409: the session is already there from an earlier turn.
	if !created.isSuccess() && created.status != http.StatusConflict {
		return "", nil, httpFailure(created.status)
	}
	response, err := g.send(ctx, "api/sessions/"+id+"/chat",
		[][2]string{{"message", message}, {"system_message", systemMessage}}, gatewayChatTimeout)
	if err != nil {
		return "", nil, err
	}
	if !response.isSuccess() {
		return "", nil, httpFailure(response.status)
	}
	object, ok := response.jsonObject()
	if !ok {
		return "", nil, errInvalidResponse
	}
	msg, _ := object["message"].(map[string]any)
	text := stringValue(msg["content"])
	if text == nil {
		return "", nil, errInvalidResponse
	}
	usage, _ := object["usage"].(map[string]any)
	total := intValue(usage["total_tokens"])
	if total == nil {
		if in, out := intValue(usage["input_tokens"]), intValue(usage["output_tokens"]); in != nil && out != nil {
			sum := *in + *out
			total = &sum
		}
	}
	return *text, total, nil
}

// connectedPlatforms reads GET /health/detailed. Empty when the gateway doesn't
// say; it never fails the instance card.
func (g *gatewayClient) connectedPlatforms(ctx context.Context) []string {
	out := []string{}
	object, err := g.getObject(ctx, "health/detailed", nil, false)
	if err != nil {
		return out
	}
	platforms, ok := object["platforms"].(map[string]any)
	if !ok {
		return out
	}
	for name, value := range platforms {
		var state *string
		if m, ok := value.(map[string]any); ok {
			state = stringValue(m["state"])
		}
		if state == nil || *state == "connected" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// sessions lists every profile's sessions, falling back to the single-profile
// /api/sessions on a Hermes without /api/profiles/sessions.
func (g *gatewayClient) sessions(ctx context.Context, instanceID string, profile, source *string, limit int) (sessionPage, error) {
	query := [][2]string{{"limit", fmt.Sprint(limit)}}
	if source != nil {
		query = append(query, [2]string{"source", *source})
	}
	profileQuery := slices.Clone(query)
	if profile != nil {
		profileQuery = append(profileQuery, [2]string{"profile", *profile})
	}
	object, err := g.getObject(ctx, "api/profiles/sessions", profileQuery, true)
	if err == nil {
		return sessionPage{profileAware: true, sessions: parseSessions(object, instanceID, time.Now())}, nil
	}
	if !isFailure(err, failNotSupported) {
		return sessionPage{}, err
	}
	object, err = g.getObject(ctx, "api/sessions", query, false)
	if err != nil {
		return sessionPage{}, err
	}
	return sessionPage{profileAware: false, sessions: parseSessions(object, instanceID, time.Now())}, nil
}

// messages reads one session's messages. A nil profile reads the gateway's own
// profile, for Hermes versions that don't report profiles.
func (g *gatewayClient) messages(ctx context.Context, profile *string, sessionID string) (string, []AgentMessageDTO, error) {
	id, err := pathSegment(sessionID)
	if err != nil {
		return "", nil, err
	}
	path := "api/sessions/" + id + "/messages"
	if profile != nil {
		p, err := pathSegment(*profile)
		if err != nil {
			return "", nil, err
		}
		path = "api/profiles/" + p + "/sessions/" + id + "/messages"
	}
	object, err := g.getObject(ctx, path, nil, false)
	if err != nil {
		return "", nil, err
	}
	resolved := sessionID
	if s := stringValue(object["session_id"]); s != nil {
		resolved = *s
	}
	rows, _ := object["data"].([]any)
	out := make([]AgentMessageDTO, 0, len(rows))
	for _, r := range rows {
		// (object["data"] as? [[String: Any]]) ?? []: one non-object row drops them all.
		row, ok := r.(map[string]any)
		if !ok {
			return resolved, []AgentMessageDTO{}, nil
		}
		out = append(out, parseMessage(row))
	}
	return resolved, out, nil
}

// MARK: - Parsing

// parseSessions is AgentGatewayClient.parseSessions. A row without a string id
// is skipped.
func parseSessions(object map[string]any, instanceID string, now time.Time) []sessionDTO {
	rows, _ := object["data"].([]any)
	out := []sessionDTO{}
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			// [[String: Any]] fails as a whole for one non-object element.
			return []sessionDTO{}
		}
		id := stringValue(row["id"])
		if id == nil {
			continue
		}
		lastActive := unixDate(row["last_active"])
		if lastActive == nil {
			lastActive = unixDate(row["started_at"])
		}
		ended, hasEnded := row["ended_at"]
		open := !hasEnded || ended == nil
		recent := lastActive != nil && now.Sub(*lastActive).Seconds() < 300
		isActive := open && recent
		if b, ok := row["is_active"].(bool); ok {
			isActive = b
		}
		title := stringValue(row["title"])
		if title != nil && *title == "" {
			title = nil
		}
		if title == nil {
			title = stringValue(row["preview"])
		}
		source := "unknown"
		if s := stringValue(row["source"]); s != nil {
			source = *s
		}
		cost := doubleValue(row["actual_cost_usd"])
		if cost == nil {
			cost = doubleValue(row["estimated_cost_usd"])
		}
		var costUSD *wire.Double
		if cost != nil {
			c := wire.Double(*cost)
			costUSD = &c
		}
		out = append(out, sessionDTO{
			InstanceID: instanceID, Profile: stringValue(row["profile"]), ID: *id, Title: title, Source: source,
			StartedAt: wireTime(unixDate(row["started_at"])), LastActiveAt: wireTime(lastActive),
			MessageCount: intValue(row["message_count"]), IsActive: isActive, CostUSD: costUSD,
		})
	}
	return out
}

// parseMessage is AgentGatewayClient.parseMessage: tool calls stay text, or are
// re-encoded as JSON when they are an array or object.
func parseMessage(row map[string]any) AgentMessageDTO {
	var toolCalls *string
	if calls, ok := row["tool_calls"]; ok && calls != nil {
		switch v := calls.(type) {
		case string:
			toolCalls = &v
		case []any, map[string]any:
			if data, err := wire.MarshalFoundation(v); err == nil {
				s := string(data)
				toolCalls = &s
			}
		}
	}
	role := "unknown"
	if r := stringValue(row["role"]); r != nil {
		role = *r
	}
	return AgentMessageDTO{
		Role: role, Content: stringValue(row["content"]), ToolName: stringValue(row["tool_name"]),
		ToolCalls: toolCalls, CreatedAt: wireTime(unixDate(row["timestamp"])),
	}
}

func wireTime(t *time.Time) *wire.Time {
	if t == nil {
		return nil
	}
	return &wire.Time{Time: *t}
}

// unixDate is AgentGatewayClient.date: Hermes stores times as Unix seconds.
func unixDate(v any) *time.Time {
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	f, err := n.Float64()
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil
	}
	sec, frac := math.Modf(f)
	t := time.Unix(int64(sec), int64(math.Round(frac*1e9)))
	return &t
}

func stringValue(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// stringArray is `as? [String]`: nil unless every element is a string.
func stringArray(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// intValue is an NSNumber's `as? Int`: a JSON number that is a whole Int.
func intValue(v any) *int {
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	if i, err := n.Int64(); err == nil {
		x := int(i)
		return &x
	}
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || f < math.MinInt64 || f >= math.MaxInt64 {
		return nil
	}
	x := int(f)
	return &x
}

// doubleValue is an NSNumber's `as? Double`.
func doubleValue(v any) *float64 {
	n, ok := v.(json.Number)
	if !ok {
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil
	}
	return &f
}

// pathSegment is AgentGatewayClient.pathSegment: a caller-supplied id or profile
// name made safe as one path segment. Anything that could step out of the route
// is refused.
func pathSegment(raw string) (string, error) {
	if raw == "" || swiftstr.Count(raw) > 256 || raw == "." || raw == ".." ||
		strings.ContainsAny(raw, `/\`) || !utf8.ValidString(raw) {
		return "", errInvalidID
	}
	for _, r := range raw {
		// CharacterSet.controlCharacters: general categories Cc and Cf.
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return "", errInvalidID
		}
	}
	return percentEncode(raw, isSegmentAllowed), nil
}

// isSegmentAllowed is CharacterSet.urlPathAllowed (!$&'()*+,-./0-9:;=@A-Z_a-z~,
// as measured in internal/capture) less "/", "?", "#" and "%".
func isSegmentAllowed(c byte) bool {
	return isUnreserved(c) || strings.IndexByte("!$&'()*+,;=:@", c) >= 0
}

// isQueryItemAllowed is what URLComponents.queryItems leaves raw in a value
// (internal/modules/news, parity-measured): "+" stays, "&" and "=" are escaped.
func isQueryItemAllowed(c byte) bool {
	return isUnreserved(c) || strings.IndexByte("!$'()*+,;:@/?", c) >= 0
}

func isUnreserved(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
}

func percentEncode(s string, allowed func(byte) bool) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if allowed(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// MARK: - Transport

// gatewayResponse is HermesHTTPResponse.
type gatewayResponse struct {
	status int
	body   []byte
}

func (r gatewayResponse) isSuccess() bool { return r.status >= 200 && r.status < 300 }

// jsonObject is JSONSerialization's top-level dictionary, numbers kept as text so
// Int and Double casts can be told apart.
func (r gatewayResponse) jsonObject() (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if dec.More() {
		return nil, false
	}
	m, ok := v.(map[string]any)
	return m, ok
}

func (g *gatewayClient) base() string {
	if strings.HasSuffix(g.baseURL, "/") {
		return g.baseURL
	}
	return g.baseURL + "/"
}

// send POSTs a JSON object of strings, as JSONEncoder encodes [String: String].
func (g *gatewayClient) send(ctx context.Context, path string, fields [][2]string, timeout time.Duration) (gatewayResponse, error) {
	m := make(map[string]string, len(fields))
	for _, f := range fields {
		m[f[0]] = f[1]
	}
	body, err := wire.MarshalFoundation(m)
	if err != nil {
		return gatewayResponse{}, err
	}
	return g.execute(ctx, http.MethodPost, g.base()+path, body, timeout, path)
}

func (g *gatewayClient) getObject(ctx context.Context, path string, query [][2]string, notFoundIsUnsupported bool) (map[string]any, error) {
	url := g.base() + path
	if len(query) > 0 {
		parts := make([]string, len(query))
		for i, q := range query {
			parts[i] = percentEncode(q[0], isQueryItemAllowed) + "=" + percentEncode(q[1], isQueryItemAllowed)
		}
		url += "?" + strings.Join(parts, "&")
	}
	response, err := g.execute(ctx, http.MethodGet, url, nil, gatewayTimeout, path)
	if err != nil {
		return nil, err
	}
	// A missing route means an older Hermes; a known route answering 404 (unknown
	// profile or session) carries an error code and is a real 404.
	if response.status == http.StatusNotFound && notFoundIsUnsupported {
		object, _ := response.jsonObject()
		errObj, _ := object["error"].(map[string]any)
		if errObj["code"] == nil {
			return nil, errNotSupported
		}
	}
	if !response.isSuccess() {
		return nil, httpFailure(response.status)
	}
	object, ok := response.jsonObject()
	if !ok {
		return nil, errInvalidResponse
	}
	return object, nil
}

// execute is AsyncHTTPClientHermesHTTP on BYOHTTP's client, which never follows a
// redirect. Any failure to get a whole response, a body over the cap included,
// is errUnreachable.
func (g *gatewayClient) execute(ctx context.Context, method, url string, body []byte, timeout time.Duration, path string) (gatewayResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		g.log.DebugContext(ctx, "agents gateway request failed", "path", path, "error", err)
		return gatewayResponse{}, errUnreachable
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if g.authHeader != nil && *g.authHeader != "" {
		req.Header.Set("Authorization", *g.authHeader)
	}
	res, err := g.http.Do(req)
	if err != nil {
		g.log.DebugContext(ctx, "agents gateway request failed", "path", path, "error", err)
		return gatewayResponse{}, errUnreachable
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, gatewayBodyCap+1))
	if err != nil || len(data) > gatewayBodyCap {
		g.log.DebugContext(ctx, "agents gateway request failed", "path", path, "error", err)
		return gatewayResponse{}, errUnreachable
	}
	return gatewayResponse{status: res.StatusCode, body: data}, nil
}
