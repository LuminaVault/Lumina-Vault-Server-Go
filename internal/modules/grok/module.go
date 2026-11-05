// Package grok serves /v1/grok: the Go port of LuminaVaultServer Sources/App/Grok
// (GrokController, HermesGrokProxy). It forwards to the tenant's own Hermes container,
// whose xai-oauth session is what reaches xAI. This server holds no xAI credential: the
// bearer it sends is the container's API_SERVER_KEY.
package grok

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/grok/oapi-codegen.yaml api/openapi.yaml"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/hermescontainers"
	"lumina/internal/httpserver"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
)

const (
	defaultChatModel   = "grok-4.3"
	xSearchModel       = "grok-4.20-reasoning"
	upstreamTimeout    = 60 * time.Second
	maxUpstreamBody    = 8 << 20 // response.body.collect(upTo: 8 * 1024 * 1024)
	pathChatCompletion = "/v1/chat/completions"
	pathResponses      = "/v1/responses"
)

var (
	errNotConnected = wire.NewError(http.StatusConflict, "xai_not_connected")
	errTTSDisabled  = wire.NewError(http.StatusNotImplemented, "tts_coming_soon")
	errNeedsQuery   = wire.NewError(http.StatusBadRequest, "query required")
	errNeedsImages  = wire.NewError(http.StatusBadRequest, "imageURLs required")
	errDecodeFailed = wire.NewError(http.StatusBadGateway, "grok_upstream_decode_failed")
	errNoAnswer     = wire.NewError(http.StatusBadGateway, "grok_upstream_missing_answer")
)

// Module is the grok route group. Swift mounts it behind jwtAuthenticator and
// PremiumGuardMiddleware, with no rate limit, and writes nothing to the database.
type Module struct {
	containers *hermescontainers.Manager
	guard      echo.MiddlewareFunc
	client     *http.Client
	log        *slog.Logger
}

// New builds the module. guard is entitlement.PremiumGuardMiddleware. A nil client
// means the default transport. Either way post follows redirects itself.
func New(containers *hermescontainers.Manager, guard echo.MiddlewareFunc, client *http.Client) *Module {
	c := http.Client{}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Module{containers: containers, guard: guard, client: &c, log: slog.Default().With("module", "grok")}
}

func (m *Module) Register(r httpserver.Router, authMW echo.MiddlewareFunc) {
	mw := []echo.MiddlewareFunc{authMW, m.guard}
	r.POST("/v1/grok/chat", m.chat, mw...)
	r.POST("/v1/grok/x-search", m.xSearch, mw...)
	r.POST("/v1/grok/vision", m.vision, mw...)
	r.POST("/v1/grok/tts", m.tts, mw...)
}

func (m *Module) chat(c *echo.Context) error {
	req, err := wire.Bind[chatRequest](c)
	if err != nil {
		return err
	}
	h, err := m.resolveHandle(c)
	if err != nil {
		return err
	}
	model := defaultChatModel
	if req.Model != nil {
		model = *req.Model
	}
	var reply chatCompletion
	err = m.post(c.Request().Context(), h, pathChatCompletion, upstreamChat{Model: model, Messages: req.Messages, MaxTokens: req.MaxTokens}, &reply)
	if err != nil {
		return err
	}
	if len(reply.Choices) == 0 {
		return errNoAnswer
	}
	if reply.Model != nil {
		model = *reply.Model
	}
	return wire.JSON(c, http.StatusOK, GrokChatResponse{Answer: reply.Choices[0].Message.Content, Model: model, Usage: reply.Usage})
}

func (m *Module) xSearch(c *echo.Context) error {
	req, err := wire.Bind[xSearchRequest](c)
	if err != nil {
		return err
	}
	if req.Query == "" {
		return errNeedsQuery
	}
	h, err := m.resolveHandle(c)
	if err != nil {
		return err
	}
	body := upstreamResponses{
		Model: xSearchModel,
		Input: req.Query,
		Tools: []upstreamTool{{Type: "x_search", XSearch: upstreamXSearch{
			AllowedXHandles:          req.AllowedXHandles,
			ExcludedXHandles:         req.ExcludedXHandles,
			FromDate:                 req.FromDate,
			ToDate:                   req.ToDate,
			EnableImageUnderstanding: req.EnableImageUnderstanding,
			EnableVideoUnderstanding: req.EnableVideoUnderstanding,
		}}},
	}
	var reply responsesReply
	if err := m.post(c.Request().Context(), h, pathResponses, body, &reply); err != nil {
		return err
	}
	out := GrokXSearchResponse{Citations: reply.Citations, Model: xSearchModel}
	if reply.Output != nil {
		out.Answer = *reply.Output
	}
	if out.Citations == nil {
		out.Citations = []GrokXSearchCitation{}
	}
	if reply.Model != nil {
		out.Model = *reply.Model
	}
	return wire.JSON(c, http.StatusOK, out)
}

func (m *Module) vision(c *echo.Context) error {
	req, err := wire.Bind[visionRequest](c)
	if err != nil {
		return err
	}
	if len(req.ImageURLs) == 0 {
		return errNeedsImages
	}
	h, err := m.resolveHandle(c)
	if err != nil {
		return err
	}
	parts := make([]visionPart, 0, len(req.ImageURLs)+1)
	parts = append(parts, visionPart{Type: "text", Text: &req.Prompt})
	for _, url := range req.ImageURLs {
		parts = append(parts, visionPart{Type: "image_url", ImageURL: &visionImage{URL: url}})
	}
	body := upstreamVision{Model: defaultChatModel, Messages: []visionMessage{{Role: "user", Content: parts}}}
	var reply chatCompletion
	if err := m.post(c.Request().Context(), h, pathChatCompletion, body, &reply); err != nil {
		return err
	}
	if len(reply.Choices) == 0 {
		return errNoAnswer
	}
	model := defaultChatModel
	if reply.Model != nil {
		model = *reply.Model
	}
	return wire.JSON(c, http.StatusOK, GrokVisionResponse{Answer: reply.Choices[0].Message.Content, Model: model})
}

// tts is always 501: both of Swift's branches throw, and it never reads the body.
func (m *Module) tts(*echo.Context) error {
	return errTTSDisabled
}

// resolveHandle is GrokController.resolveHandle: no row and no xAI marker are both a
// 409 that sends iOS back to Linked Accounts. A key that won't open is a plain error.
func (m *Module) resolveHandle(c *echo.Context) (*hermescontainers.Handle, error) {
	tenant, ok := auth.Identity(c)
	if !ok {
		return nil, wire.ErrUnauthenticated
	}
	h, err := m.containers.Handle(c.Request().Context(), tenant)
	if err != nil {
		return nil, err
	}
	if h == nil || h.XaiConnectedAt == nil {
		return nil, errNotConnected
	}
	return h, nil
}

// post is HermesGrokProxy.post. Only a non-2xx status and an undecodable body are
// grok_upstream_*: mapProxyError passes every other failure through, so a dead
// connection is a 500, a broken HTTP reply is errBrokenReply, and an over-limit body
// is Hummingbird's bare 413.
func (m *Module) post(ctx context.Context, h *hermescontainers.Handle, path string, body, reply any) error {
	payload, err := wire.MarshalFoundation(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout) // one deadline for every hop
	defer cancel()
	resp, err := m.execute(ctx, h, path, payload)
	if err != nil {
		return parserOr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody+1))
	if err != nil {
		return parserOr(err)
	}
	if len(raw) > maxUpstreamBody {
		return wire.ErrBodyTooLarge
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		m.log.WarnContext(ctx, "hermes grok proxy non-2xx", "status", resp.StatusCode, "container", h.ContainerName, "path", path)
		return wire.NewError(http.StatusBadGateway, fmt.Sprintf("grok_upstream_%d", resp.StatusCode))
	}
	if err := decodeUpstream(raw, reply); err != nil {
		m.log.ErrorContext(ctx, "hermes grok proxy decode failed", "error", err, "container", h.ContainerName, "path", path)
		return errDecodeFailed
	}
	return nil
}

// decodeUpstream is the proxy's JSONDecoder: Foundation's rules, through wire.Decode,
// on keys put through .convertFromSnakeCase.
func decodeUpstream(raw []byte, dst any) error {
	if !json.Valid(raw) {
		return wire.Decode(raw, dst) // names the failure
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 1.0 and big integers reach wire.Decode as written
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return err
	}
	converted, err := json.Marshal(camelKeys(tree))
	if err != nil {
		return err
	}
	return wire.Decode(converted, dst)
}

func camelKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[fromSnakeCase(k)] = camelKeys(e)
		}
		return out
	case []any:
		for i, e := range t {
			t[i] = camelKeys(e)
		}
	}
	return v
}

// fromSnakeCase is Foundation's JSONDecoder.KeyDecodingStrategy.convertFromSnakeCase:
// leading and trailing underscores stay, a key with one word is left alone, and
// otherwise the first word is lowercased and the rest capitalized.
func fromSnakeCase(key string) string {
	trimmed := strings.Trim(key, "_")
	if trimmed == "" {
		return key
	}
	words := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '_' })
	if len(words) == 1 {
		return key
	}
	start := strings.Index(key, trimmed)
	var b strings.Builder
	b.WriteString(key[:start])
	b.WriteString(strings.ToLower(words[0]))
	for _, w := range words[1:] {
		lower := strings.ToLower(w)
		b.WriteString(strings.ToUpper(lower[:1]) + lower[1:])
	}
	b.WriteString(key[start+len(trimmed):])
	return b.String()
}
