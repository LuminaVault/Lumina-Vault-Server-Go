// Package llmadmin serves GET /admin/llm/health: the Go port of LuminaVaultServer
// Sources/App/LLM/Routing/LLMHealthController.swift and the healthCheck() of
// OpenAICompatibleAdapter it fans out to.
package llmadmin

//go:generate sh -c "cd ../../.. && go tool oapi-codegen -config internal/modules/llmadmin/oapi-codegen.yaml api/openapi.yaml"

import (
	"bytes"
	"context"
	"crypto/subtle"
	"io"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/wire"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
)

var (
	errAdminDisabled = wire.NewError(http.StatusNotFound, "admin disabled")
	errAdminToken    = wire.NewError(http.StatusUnauthorized, "admin token invalid")
)

// healthTimeout is healthCheck(timeout:)'s default.
const healthTimeout = 3 * time.Second

// pingPayload is OpenAICompatibleAdapter.pingPayload.
var pingPayload = []byte(`{"max_tokens":1,"messages":[{"content":"ping","role":"user"}],"model":"ping"}`)

// Module is the /admin group behind AdminTokenMiddleware.
type Module struct {
	registry   *llm.Registry
	adminToken string
	client     *http.Client
	now        func() time.Time
}

// New wires the route. client nil means a default client.
func New(registry *llm.Registry, adminToken string, client *http.Client) *Module {
	if client == nil {
		client = &http.Client{}
	}
	return &Module{registry: registry, adminToken: adminToken, client: client, now: time.Now}
}

func (m *Module) Register(r httpserver.Router, _ echo.MiddlewareFunc) {
	r.GET("/admin/llm/health", m.health, m.requireAdminToken)
}

// requireAdminToken is AdminTokenMiddleware: X-Admin-Token against ADMIN_TOKEN.
// An empty token rejects everything.
func (m *Module) requireAdminToken(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if m.adminToken == "" {
			return errAdminDisabled
		}
		got := c.Request().Header.Get("X-Admin-Token")
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(m.adminToken)) != 1 {
			return errAdminToken
		}
		return next(c)
	}
}

// healthLine is LLMProviderHealthLine.
type healthLine struct {
	Kind      string  `json:"kind"`
	Region    string  `json:"region"`
	OK        bool    `json:"ok"`
	LatencyMs int     `json:"latency_ms"`
	Error     *string `json:"error,omitempty"`
}

type healthResponse struct {
	CheckedAt wire.Time    `json:"checked_at"`
	Providers []healthLine `json:"providers"`
}

// region is ProviderKind.region: the Chinese-hosted upstreams are "cn".
func region(kind llm.ProviderKind) string {
	switch kind {
	case llm.Deepseek, llm.DeepseekDirect, llm.Kimi:
		return "cn"
	}
	return "us"
}

func (m *Module) health(c *echo.Context) error {
	var compatible []*llm.OpenAICompatibleAdapter
	if m.registry != nil {
		for _, kind := range m.registry.Registered() {
			if a, ok := m.registry.Adapter(kind); ok {
				if oa, ok := a.(*llm.OpenAICompatibleAdapter); ok {
					compatible = append(compatible, oa)
				}
			}
		}
	}
	lines := make([]healthLine, len(compatible))
	var wg sync.WaitGroup
	for i, a := range compatible {
		wg.Go(func() { lines[i] = m.check(c.Request().Context(), a) })
	}
	wg.Wait()
	sort.Slice(lines, func(i, j int) bool { return lines[i].Kind < lines[j].Kind })
	return wire.JSON(c, http.StatusOK, healthResponse{CheckedAt: wire.Time{Time: m.now()}, Providers: lines})
}

// check is OpenAICompatibleAdapter.healthCheck: any HTTP status is reachable.
// The error text is Go's, not Foundation's String(describing:).
func (m *Module) check(ctx context.Context, a *llm.OpenAICompatibleAdapter) healthLine {
	kind := a.Kind()
	line := healthLine{Kind: string(kind), Region: region(kind)}
	base := a.BaseURL
	if base == nil {
		base = llm.DefaultBaseURL(kind)
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, llm.Endpoint(kind, base), bytes.NewReader(pingPayload))
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
		req.Header.Set("Content-Type", "application/json")
		var resp *http.Response
		resp, err = m.client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			line.OK = resp.StatusCode > 0
		}
	}
	line.LatencyMs = max(0, int(time.Since(start).Milliseconds()))
	if err != nil {
		msg := err.Error()
		line.Error = &msg
	}
	return line
}
