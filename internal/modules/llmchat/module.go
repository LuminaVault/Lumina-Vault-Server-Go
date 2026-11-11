// Package llmchat serves POST /v1/llm/chat: the Go port of LuminaVaultServer
// Sources/App/LLM/LLMController.swift with ChatURLPreEnricher, URLExtractor and
// ContextRouterMiddleware. One non-streaming completion through the routed service.
package llmchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"lumina/internal/achievementsink"
	"lumina/internal/apns"
	"lumina/internal/auth"
	"lumina/internal/capture"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/achievements"
	"lumina/internal/onboardlatch"
	"lumina/internal/ratelimit"
	"lumina/internal/skills"
	"lumina/internal/usagemeter"
	"lumina/internal/wire"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"
)

var errMessagesRequired = wire.NewError(http.StatusBadRequest, "messages required")

// Options are the module's dependencies. Nil optional ones behave as Swift's nil.
type Options struct {
	Identify echo.MiddlewareFunc
	// Hermes is the BYO Hermes middleware; nil without a SecretBox.
	Hermes echo.MiddlewareFunc
	// Gate is Entitlement .chat with the BYO credential exemption.
	Gate          echo.MiddlewareFunc
	Limits        *ratelimit.Store
	MaxStreams    int
	ContextRouter *skills.ContextRouter
	Chat          llm.ChatService
	Enrichment    *capture.EnrichmentService // ChatURLPreEnricher; nil disables it
	Usage         *usagemeter.Meter
	Push          *apns.Service
	Achievements  *achievementsink.Sink
	Latches       *onboardlatch.Latches
}

// Module is the route.
type Module struct {
	pool *pgxpool.Pool
	opts Options
	tier func(*echo.Context) (string, bool)
	log  *slog.Logger
}

// New builds the module.
func New(pool *pgxpool.Pool, opts Options) *Module {
	return &Module{pool: pool, opts: opts, tier: ratelimit.PGTier(pool), log: slog.Default().With("module", "llmchat")}
}

// Register mounts Swift's /v1/llm group: jwt, BYO Hermes, Entitlement .chat,
// chatByUser, the in-flight limit, then the context router.
func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	var mws []echo.MiddlewareFunc
	for _, mw := range []echo.MiddlewareFunc{
		m.opts.Identify, m.opts.Hermes, m.opts.Gate,
		ratelimit.TieredMiddleware(m.opts.Limits, ratelimit.ChatByUser, ratelimit.UserOrIPKey, m.tier),
		ratelimit.InFlightLimit(m.opts.MaxStreams, ratelimit.UserOrIPKey),
		m.contextRouter(),
	} {
		if mw != nil {
			mws = append(mws, mw)
		}
	}
	r.POST("/v1/llm/chat", m.chat, mws...)
}

// chatRequest is LuminaVaultShared's ChatRequest; stream defaults to false.
type chatRequest struct {
	Messages    []llm.CompletionMessage `json:"messages"`
	Model       *string                 `json:"model,omitempty"`
	Temperature *float64                `json:"temperature,omitempty"`
	Stream      *bool                   `json:"stream,omitempty"`
	Tools       *[]llm.ChatTool         `json:"tools,omitempty"`
	ToolChoice  json.RawMessage         `json:"tool_choice,omitempty"`
	SessionID   *string                 `json:"session_id,omitempty"`
}

type userRow struct {
	username       string
	tier           string
	tierOverride   string
	contextRouting bool
	noCN           bool
}

func (m *Module) loadUser(ctx context.Context, id uuid.UUID) (userRow, error) {
	var u userRow
	err := m.pool.QueryRow(ctx, `SELECT username, tier, tier_override, context_routing, privacy_no_cn_origin FROM users WHERE id = $1`, id).
		Scan(&u.username, &u.tier, &u.tierOverride, &u.contextRouting, &u.noCN)
	return u, err
}

func (m *Module) chat(c *echo.Context) error {
	actor, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	body, err := wire.Bind[chatRequest](c)
	if err != nil {
		return err
	}
	if len(body.Messages) == 0 {
		return errMessagesRequired
	}
	base := llm.RequestContext(c)
	user, err := m.loadUser(base, actor)
	if err != nil {
		return err
	}
	skip, _ := wire.HeaderValue(c.Request(), "X-Skip-URL-Enrichment")
	if strings.ToLower(skip) != "true" && m.opts.Enrichment != nil {
		body.Messages = m.enrich(base, body.Messages)
	}
	degraded := false
	if m.opts.Usage != nil {
		decision := m.opts.Usage.CheckBudget(base, actor, user.tier)
		switch decision.Action {
		case usagemeter.BudgetDegrade:
			model := decision.Model
			body.Model = &model
			degraded = true
		case usagemeter.BudgetDeny:
			return &llm.UsageCapExceededError{RetryAfter: decision.RetryAfter}
		}
	}
	req := llm.CompletionRequest{Messages: body.Messages, Temperature: body.Temperature, ToolChoice: body.ToolChoice}
	if body.Model != nil {
		req.Model = *body.Model
	}
	if body.Tools != nil {
		req.Tools = *body.Tools
	}
	latest := ""
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if body.Messages[i].Role == "user" {
			latest = body.Messages[i].Content
			break
		}
	}
	ctx := llm.WithValues(base, func(v *llm.Values) {
		v.CerberusScope = &llm.CerberusScope{Surface: llm.SurfaceChat}
		v.CerberusPrompt = latest
		v.CurrentUser = &llm.User{ID: actor, Tier: user.tier, TierOverride: user.tierOverride, PrivacyNoCNOrigin: user.noCN}
	})
	sessionID := ""
	if body.SessionID != nil {
		sessionID = *body.SessionID
	}
	resp, err := m.opts.Chat.Chat(ctx, ids.Upper(actor), sessionID, req)
	if err != nil {
		var chatErr *llm.ChatError
		if errors.As(err, &chatErr) {
			return wire.NewError(chatErr.Status, chatErr.Message)
		}
		return err
	}
	// Push delivery is best effort and never blocks the reply.
	if m.opts.Push != nil {
		content, username := resp.Message.Content, user.username
		go func() {
			if err := m.opts.Push.NotifyLLMReply(context.Background(), actor, username, content); err != nil {
				m.log.Warn("push notify failed", "error", err)
			}
		}()
	}
	if m.opts.Achievements != nil {
		m.opts.Achievements.Enqueue(actor, achievements.EventChatCompleted)
	}
	if m.opts.Latches != nil {
		m.opts.Latches.Latch(base, onboardlatch.FirstQuery, actor)
	}
	if degraded {
		c.Response().Header().Set("X-LuminaVault-Degraded", "true")
	}
	return wire.JSON(c, http.StatusOK, resp)
}

// MARK: - ChatURLPreEnricher

const maxURLsPerMessage = 5

// urlPattern is URLExtractor's `https?://[^\s)>\]]+`, with ICU's \s.
var urlPattern = regexp.MustCompile(`https?://[^\t\n\x{0B}\f\r\x{85}\p{Z})>\]]+`)

// ExtractURLs is URLExtractor.extract: up to five distinct http(s) URLs, trailing
// sentence punctuation stripped, as url.absoluteString.
func ExtractURLs(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range urlPattern.FindAllString(content, -1) {
		raw = strings.TrimRight(raw, ".,;:!?\"'")
		if seen[raw] {
			continue
		}
		seen[raw] = true
		u, ok := capture.ParseURL(raw)
		if !ok || u.Scheme == nil {
			continue
		}
		if s := strings.ToLower(*u.Scheme); s != "http" && s != "https" {
			continue
		}
		out = append(out, u.Absolute)
		if len(out) >= maxURLsPerMessage {
			break
		}
	}
	return out
}

func (m *Module) enrich(ctx context.Context, messages []llm.CompletionMessage) []llm.CompletionMessage {
	out := make([]llm.CompletionMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != "user" {
			out = append(out, msg)
			continue
		}
		var blocks []string
		for _, u := range ExtractURLs(msg.Content) {
			if block, ok := m.contextBlock(ctx, u); ok {
				blocks = append(blocks, block)
			}
		}
		if len(blocks) == 0 {
			out = append(out, msg)
			continue
		}
		msg.Content = strings.Join(blocks, "\n") + "\n\n" + msg.Content
		out = append(out, msg)
	}
	return out
}

func (m *Module) contextBlock(ctx context.Context, u string) (string, bool) {
	meta := m.opts.Enrichment.EnrichURL(ctx, u)
	if meta == nil {
		return "", false
	}
	block := `<context source="` + escapeAttr(u) + `">`
	add := func(tag string, v *string) {
		if v != nil && *v != "" {
			block += "\n  <" + tag + ">" + escapeXML(*v) + "</" + tag + ">"
		}
	}
	add("title", meta.Title)
	add("description", meta.Description)
	add("transcript", meta.Transcript)
	add("body", meta.Body)
	return block + "\n</context>", true
}

func escapeXML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func escapeAttr(s string) string { return strings.ReplaceAll(escapeXML(s), `"`, "&quot;") }

// MARK: - ContextRouterMiddleware

// contextRouter rewrites the body when the user's context routing flag is on and
// they're entitled; every other case forwards the body untouched.
func (m *Module) contextRouter() echo.MiddlewareFunc {
	router := m.opts.ContextRouter
	if router == nil {
		return nil
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			actor, ok := auth.Identity(c)
			if !ok {
				return next(c)
			}
			ctx := c.Request().Context()
			user, err := m.loadUser(ctx, actor)
			if err != nil || !user.contextRouting {
				return next(c)
			}
			ru := skills.RoutingUser{Tenant: actor, Username: user.username, ContextRouting: true, Tier: user.tier, TierOverride: user.tierOverride}
			if router.Entitled != nil && !router.Entitled(ru) {
				return next(c)
			}
			req := c.Request()
			raw, err := io.ReadAll(io.LimitReader(req.Body, skills.MaxRoutingBodyBytes+1))
			rest := req.Body
			if err != nil || len(raw) > skills.MaxRoutingBodyBytes {
				req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), rest))
				return next(c)
			}
			rewritten := router.Rewrite(ctx, ru, raw)
			req.Body = io.NopCloser(bytes.NewReader(rewritten))
			req.ContentLength = int64(len(rewritten))
			return next(c)
		}
	}
}

// SelectorTransport adapts the routed transport to skills.ChatTransport.
type SelectorTransport struct{ Transport *llm.Transport }

// ChatCompletions returns the routed call's raw body.
func (t SelectorTransport) ChatCompletions(ctx context.Context, payload []byte, sessionKey, sessionID string) ([]byte, error) {
	md, err := t.Transport.ChatCompletions(ctx, payload, sessionKey, sessionID)
	if err != nil {
		return nil, err
	}
	return md.Data, nil
}

// Entitled is the router's cost gate, entitlement privacyContextRouter.
func Entitled(u skills.RoutingUser) bool {
	return entitlement.Entitled(u.Tier, u.TierOverride, entitlement.PrivacyContextRouter)
}
