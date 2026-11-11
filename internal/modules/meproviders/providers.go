package meproviders

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"lumina/internal/auth"
	"lumina/internal/capture"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/meproviders/db"
	"lumina/internal/wire"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// The /test failure codes: ProvidersController.TestError.
const (
	errNoCredential        = "no_credential"
	errUnsupportedProvider = "unsupported_provider"
	errCreditExhausted     = "credit_exhausted"
	errRateLimit           = "rate_limit"
	errUpstreamError       = "upstream_error"
	errUpstreamRejected    = "upstream_rejected"
	errNetwork             = "network"
)

// probeTimeout is the URLRequest timeoutInterval of /test and /models.
const probeTimeout = 8 * time.Second

// parseProvider is ProvidersController.parseProvider: an unknown id is a 404.
func parseProvider(c *echo.Context) (llm.ProviderID, llm.ProviderKind, error) {
	raw := c.Param("provider")
	kind, ok := llm.FromShared(llm.ProviderID(raw))
	if !ok {
		return "", "", wire.NewError(http.StatusNotFound, errUnsupportedProvider)
	}
	return llm.ProviderID(raw), kind, nil
}

func (m *Module) invalidate(tenant uuid.UUID, kind llm.ProviderKind) {
	m.opts.Stack.Credentials.Invalidate(tenant, kind)
}

// recordSuccess is UserCredentialStore.recordSuccess: a no-op without a row.
func (m *Module) recordSuccess(ctx context.Context, tenant uuid.UUID, kind llm.ProviderKind) error {
	now := m.now()
	n, err := m.q.RecordSuccess(ctx, db.RecordSuccessParams{TenantID: tenant, Provider: string(kind), VerifiedAt: &now})
	if err != nil {
		return err
	}
	if n > 0 {
		m.invalidate(tenant, kind)
	}
	return nil
}

// recordFailure is UserCredentialStore.recordFailure. Callers ignore its error (try?).
func (m *Module) recordFailure(ctx context.Context, tenant uuid.UUID, kind llm.ProviderKind, code string) {
	now := m.now()
	n, err := m.q.RecordFailure(ctx, db.RecordFailureParams{TenantID: tenant, Provider: string(kind), LastFailureAt: &now, LastFailureCode: &code})
	if err == nil && n > 0 {
		m.invalidate(tenant, kind)
	}
}

// MARK: - catalog and list

func (m *Module) catalog(c *echo.Context) error {
	reg := m.opts.Stack.Registry
	return wire.JSON(c, http.StatusOK, catalogResponse{Providers: catalogAll(func(id llm.ProviderID) bool {
		kind, _ := llm.FromShared(id)
		return reg != nil && reg.IsEnabled(kind)
	})})
}

func (m *Module) list(c *echo.Context) error {
	tenant := auth.UserID(c)
	rows, err := m.q.ListCredentials(c.Request().Context(), tenant)
	if err != nil {
		return err
	}
	byProvider := map[string]db.ListCredentialsRow{}
	for _, r := range rows {
		byProvider[r.Provider] = r
	}
	out := make([]credentialDTO, len(providerIDs))
	for i, id := range providerIDs {
		dto := credentialDTO{Provider: id, Kind: defaultKind(id)}
		if r, ok := byProvider[string(id)]; ok {
			if validCredentialKind(r.CredentialKind) {
				dto.Kind = r.CredentialKind
			}
			dto.HasCredential = r.Ciphertext != nil || r.BaseUrl != nil
			dto.BaseURL, dto.Label = r.BaseUrl, r.Label
			dto.VerifiedAt, dto.LastFailureAt, dto.LastFailureCode = wireTime(r.VerifiedAt), wireTime(r.LastFailureAt), r.LastFailureCode
			dto.SupportsTools, dto.ToolsProbedAt = r.SupportsTools, wireTime(r.ToolsProbedAt)
		}
		// oauth marker rows (the SuperGrok link) carry no secret but authorize use.
		dto.HasCredential = dto.HasCredential || dto.Kind == string(kindOAuth)
		out[i] = dto
	}
	return wire.JSON(c, http.StatusOK, credentialsListResponse{Providers: out})
}

// MARK: - PUT and DELETE

func (m *Module) put(c *echo.Context) error {
	tenant := auth.UserID(c)
	id, kind, err := parseProvider(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[credentialPutRequest](c)
	if err != nil {
		return err
	}
	switch body.Kind {
	case kindAPIKey:
		if body.APIKey == nil || *body.APIKey == "" {
			return wire.NewError(http.StatusBadRequest, "api_key_required")
		}
	case kindHostURL:
		if body.BaseURL == nil || *body.BaseURL == "" {
			return wire.NewError(http.StatusBadRequest, "base_url_required")
		}
		if _, ok := capture.ParseURL(*body.BaseURL); !ok {
			return wire.NewError(http.StatusBadRequest, "invalid_base_url")
		}
	}
	ctx := c.Request().Context()
	if err := m.upsert(ctx, tenant, kind, body); err != nil {
		m.log.ErrorContext(ctx, "provider credential upsert failed", "error", err)
		return wire.NewError(http.StatusInternalServerError, "credential_save_failed")
	}
	dto := credentialDTO{Provider: id, Kind: string(body.Kind), HasCredential: body.Kind == kindOAuth}
	row, err := m.q.GetCredential(ctx, db.GetCredentialParams{TenantID: tenant, Provider: string(kind)})
	switch {
	case err == nil:
		dto.HasCredential = dto.HasCredential || row.Ciphertext != nil || row.BaseUrl != nil
		dto.BaseURL, dto.Label = row.BaseUrl, row.Label
		dto.VerifiedAt, dto.LastFailureAt, dto.LastFailureCode = wireTime(row.VerifiedAt), wireTime(row.LastFailureAt), row.LastFailureCode
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	return wire.JSON(c, http.StatusOK, dto)
}

// upsert is UserCredentialStore.upsert: oauth clears the secret, a non-empty key
// seals a new one, an empty key clears it, and an absent key keeps it. Base URL
// and label are overwritten and the verification state reset.
func (m *Module) upsert(ctx context.Context, tenant uuid.UUID, kind llm.ProviderKind, body *credentialPutRequest) error {
	existing, err := m.q.GetCredential(ctx, db.GetCredentialParams{TenantID: tenant, Provider: string(kind)})
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var ct, nonce []byte
	if found {
		ct, nonce = existing.Ciphertext, existing.Nonce
	}
	switch {
	case body.Kind == kindOAuth:
		ct, nonce = nil, nil
	case body.APIKey != nil && *body.APIKey != "":
		sealed, err := m.opts.Box.Seal(*body.APIKey, tenant)
		if err != nil {
			return err
		}
		ct, nonce = sealed.Ciphertext, sealed.Nonce
	case body.APIKey != nil:
		ct, nonce = nil, nil
	}
	now := m.now()
	if found {
		err = m.q.UpdateCredential(ctx, db.UpdateCredentialParams{
			ID: existing.ID, CredentialKind: string(body.Kind), Ciphertext: ct, Nonce: nonce,
			BaseUrl: body.BaseURL, Label: body.Label, UpdatedAt: &now,
		})
	} else {
		err = m.q.InsertCredential(ctx, db.InsertCredentialParams{
			ID: uuid.New(), TenantID: tenant, Provider: string(kind), CredentialKind: string(body.Kind),
			Ciphertext: ct, Nonce: nonce, BaseUrl: body.BaseURL, Label: body.Label, CreatedAt: &now,
		})
	}
	if err != nil {
		return err
	}
	m.invalidate(tenant, kind)
	return nil
}

func (m *Module) delete(c *echo.Context) error {
	tenant := auth.UserID(c)
	_, kind, err := parseProvider(c)
	if err != nil {
		return err
	}
	if err := m.q.DeleteCredential(c.Request().Context(), db.DeleteCredentialParams{TenantID: tenant, Provider: string(kind)}); err != nil {
		return err
	}
	m.invalidate(tenant, kind)
	return c.NoContent(http.StatusNoContent)
}

// MARK: - POST /test

// pingModels is ProvidersController.pingPayload's model per provider.
var pingModels = map[llm.ProviderID]string{
	"xai": "grok-4", "nvidia": "meta/llama-3.1-8b-instruct", "anthropic": "claude-sonnet-4-6",
	"openai": "gpt-4o-mini", "openRouter": "openrouter/auto", "ollama": "llama3.1",
	"gemini": "gemini-2.5-flash", "nous": "stepfun/step-3.7-flash:free", "custom": "",
}

// pingPayload is ProvidersController.pingPayload: a one-token completion.
func pingPayload(id llm.ProviderID) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      pingModels[id],
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	return b
}

// buildAdapter is ProvidersController.buildAdapter: a one-off adapter spending
// exactly the resolved credential, with no store behind it.
func (m *Module) buildAdapter(kind llm.ProviderKind, resolved *llm.ResolvedCredential) llm.Adapter {
	key := ""
	if resolved.APIKey != nil {
		key = *resolved.APIKey
	}
	base := resolved.BaseURL
	switch kind {
	case llm.Anthropic:
		if base == nil {
			base, _ = url.Parse("https://api.anthropic.com")
		}
		return &llm.AnthropicAdapter{APIKey: key, BaseURL: base, Client: m.client}
	case llm.Ollama:
		if base == nil {
			base, _ = url.Parse("http://localhost:11434")
		}
		return &llm.OllamaAdapter{DefaultBaseURL: base, Client: m.client}
	case llm.Gemini:
		return &llm.GeminiContentsAdapter{APIKey: key, Client: m.client}
	}
	if base == nil {
		base = llm.DefaultBaseURL(kind)
	}
	return &llm.OpenAICompatibleAdapter{ProviderKind: kind, APIKey: key, BaseURL: base, Client: m.client}
}

// stableCode is ProvidersController.stableCode.
func stableCode(e *llm.ProviderError) string {
	switch e.Kind {
	case llm.KindCreditExhausted:
		return errCreditExhausted
	case llm.KindTransient:
		if e.Status == http.StatusTooManyRequests {
			return errRateLimit
		}
		return errUpstreamError
	case llm.KindNetwork:
		return errNetwork
	}
	return errUpstreamRejected
}

func (m *Module) test(c *echo.Context) error {
	tenant := auth.UserID(c)
	id, kind, err := parseProvider(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	resolved, err := m.opts.Stack.Credentials.Credential(ctx, kind, tenant)
	if err != nil {
		return err
	}
	if resolved == nil {
		return wire.NewError(http.StatusBadRequest, errNoCredential)
	}

	// A generic endpoint has no known model to ping: GET <base>/v1/models proves
	// reachability and auth.
	if id == "custom" {
		if resolved.BaseURL == nil {
			return wire.NewError(http.StatusBadRequest, errNoCredential)
		}
		status, ok := m.get(ctx, resolved.BaseURL.JoinPath("v1", "models").String(), bearer(resolved.APIKey), nil)
		if !ok {
			return wire.NewError(http.StatusBadGateway, errNetwork)
		}
		if status < 200 || status >= 300 {
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				m.recordFailure(ctx, tenant, kind, errUpstreamRejected)
				return wire.NewError(http.StatusBadGateway, errUpstreamRejected)
			}
			return wire.NewError(http.StatusBadGateway, errUpstreamError)
		}
		if err := m.recordSuccess(ctx, tenant, kind); err != nil {
			return err
		}
		return wire.JSON(c, http.StatusOK, testResponse{VerifiedAt: wire.Time{Time: m.now()}})
	}

	adapter := m.buildAdapter(kind, resolved)
	if _, err := adapter.ChatCompletions(ctx, pingPayload(id), tenant.String(), ""); err != nil {
		var perr *llm.ProviderError
		if errors.As(err, &perr) {
			// Only an auth rejection is evidence about the key.
			if perr.IsCredentialRejection() {
				m.recordFailure(ctx, tenant, kind, perr.ReasonCode())
			}
			return wire.NewError(http.StatusBadGateway, stableCode(perr))
		}
		return wire.NewError(http.StatusBadGateway, errNetwork)
	}
	if err := m.recordSuccess(ctx, tenant, kind); err != nil {
		return wire.NewError(http.StatusBadGateway, errNetwork)
	}
	model := pingModels[id]
	m.probeToolSupport(tenant, kind, adapter, model)
	resp := testResponse{VerifiedAt: wire.Time{Time: m.now()}}
	if model != "" {
		resp.Model = &model
	}
	return wire.JSON(c, http.StatusOK, resp)
}

// probeToolSupport is ProvidersController.probeToolSupport: detached, so the
// verdict never delays or fails the /test response.
func (m *Module) probeToolSupport(tenant uuid.UUID, kind llm.ProviderKind, adapter llm.Adapter, model string) {
	if model == "" {
		return
	}
	m.probes.Add(1)
	go func() {
		defer m.probes.Done()
		ctx := m.probeCtx
		var stored *bool
		if resp, err := adapter.ChatCompletions(ctx, probePayload(model, probeNonce()), tenant.String(), ""); err == nil {
			switch probeVerdict(resp.Data) {
			case verdictSupported:
				t := true
				stored = &t
			case verdictNotSupported:
				f := false
				stored = &f
			}
		} else {
			m.log.Debug("tool probe could not reach a verdict", "provider", kind, "error", err)
		}
		now := m.now()
		n, err := m.q.RecordToolSupport(ctx, db.RecordToolSupportParams{TenantID: tenant, Provider: string(kind), SupportsTools: stored, ToolsProbedAt: &now})
		if err != nil {
			m.log.Debug("could not store tool probe verdict", "provider", kind, "error", err)
			return
		}
		if n > 0 {
			m.invalidate(tenant, kind)
		}
	}()
}

// MARK: - GET /models

func bearer(key *string) map[string]string {
	if key == nil || *key == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + *key}
}

// get issues a probe GET. ok is false when no HTTP response came back.
func (m *Module) get(ctx context.Context, rawURL string, headers map[string]string, body *[]byte) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, false
	}
	if body != nil {
		*body = data
	}
	return resp.StatusCode, true
}

// openAIListing is the set with a standard OpenAI /v1/models listing.
var openAIListing = map[llm.ProviderKind]bool{
	llm.XAI: true, llm.Nvidia: true, llm.OpenAI: true, llm.OpenRouter: true, llm.Nous: true, llm.Custom: true,
}

func (m *Module) models(c *echo.Context) error {
	tenant := auth.UserID(c)
	id, kind, err := parseProvider(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	fallback := func() error {
		catalog := llm.ModelsFor(id)
		out := make([]modelInfoDTO, len(catalog))
		for i, info := range catalog {
			out[i] = modelInfoDTO{ID: info.ID, DisplayName: info.DisplayName, Tier: string(info.Tier)}
			if info.ContextWindow != 0 {
				cw := info.ContextWindow
				out[i].ContextWindow = &cw
			}
		}
		return wire.JSON(c, http.StatusOK, modelsResponse{Provider: id, Models: out, FetchedLive: false})
	}

	// A failed lookup reads as no credential (try? ... ?? nil).
	resolved, _ := m.opts.Stack.Credentials.Credential(ctx, kind, tenant)
	var key *string
	if resolved != nil {
		key = resolved.APIKey
	}
	var target string
	headers := map[string]string{}
	parse := parseModels
	switch {
	case openAIListing[kind]:
		base := llm.DefaultBaseURL(kind)
		if resolved != nil && resolved.BaseURL != nil {
			base = resolved.BaseURL
		}
		target = base.JoinPath("v1", "models").String()
		headers = bearer(key)
		if headers == nil {
			headers = map[string]string{}
		}
		if kind == llm.OpenRouter {
			headers["HTTP-Referer"] = "https://luminavault.app"
			headers["X-Title"] = "LuminaVault"
		}
	case kind == llm.Anthropic:
		if key == nil || *key == "" {
			return fallback()
		}
		target = "https://api.anthropic.com/v1/models?limit=100"
		headers["x-api-key"] = *key
		headers["anthropic-version"] = llm.AnthropicAPIVersion
	case kind == llm.Gemini:
		if key == nil || *key == "" {
			return fallback()
		}
		target = "https://generativelanguage.googleapis.com/v1beta/models?pageSize=200&key=" + url.QueryEscape(*key)
		parse = parseGeminiModels
	case kind == llm.Ollama:
		if resolved == nil || resolved.BaseURL == nil {
			return fallback()
		}
		target = resolved.BaseURL.JoinPath("api", "tags").String()
		parse = parseOllamaModels
	default:
		return fallback()
	}
	var data []byte
	status, ok := m.get(ctx, target, headers, &data)
	if !ok || status < 200 || status >= 300 {
		return fallback()
	}
	parsed, ok := parse(data)
	if !ok || len(parsed) == 0 {
		return fallback()
	}
	return wire.JSON(c, http.StatusOK, modelsResponse{Provider: id, Models: parsed, FetchedLive: true})
}

// liveModel is LLMModelInfo(id:displayName:): no context window, tier balanced.
func liveModel(id, name string) modelInfoDTO {
	return modelInfoDTO{ID: id, DisplayName: name, Tier: "balanced"}
}

func jsonList(data []byte, key string) ([]map[string]any, bool) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return nil, false
	}
	raw, ok := root[key].([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		obj, ok := e.(map[string]any)
		if !ok {
			// `as? [[String: Any]]` fails on any non-object element.
			return nil, false
		}
		out = append(out, obj)
	}
	return out, true
}

// parseModels is ProvidersController.parseModels: data[].id, labelled by name,
// else display_name, else the id. An empty name does not fall through to
// display_name: it falls to the id.
func parseModels(data []byte) ([]modelInfoDTO, bool) {
	list, ok := jsonList(data, "data")
	if !ok {
		return nil, false
	}
	out := []modelInfoDTO{}
	for _, e := range list {
		id, _ := e["id"].(string)
		if id == "" {
			continue
		}
		name, ok := e["name"].(string)
		if !ok {
			name, _ = e["display_name"].(string)
		}
		if name == "" {
			name = id
		}
		out = append(out, liveModel(id, name))
	}
	return out, true
}

// parseGeminiModels is ProvidersController.parseGeminiModels.
func parseGeminiModels(data []byte) ([]modelInfoDTO, bool) {
	list, ok := jsonList(data, "models")
	if !ok {
		return nil, false
	}
	out := []modelInfoDTO{}
	for _, e := range list {
		id, _ := e["name"].(string)
		if id == "" {
			continue
		}
		if methods, ok := stringArray(e["supportedGenerationMethods"]); ok && !contains(methods, "generateContent") {
			continue
		}
		if len(id) >= len("models/") && id[:len("models/")] == "models/" {
			id = id[len("models/"):]
		}
		label, _ := e["displayName"].(string)
		if label == "" {
			label = id
		}
		out = append(out, liveModel(id, label))
	}
	return out, true
}

// parseOllamaModels is ProvidersController.parseOllamaModels.
func parseOllamaModels(data []byte) ([]modelInfoDTO, bool) {
	list, ok := jsonList(data, "models")
	if !ok {
		return nil, false
	}
	out := []modelInfoDTO{}
	for _, e := range list {
		if id, _ := e["name"].(string); id != "" {
			out = append(out, liveModel(id, id))
		}
	}
	return out, true
}

// stringArray is `as? [String]`: every element must be a string.
func stringArray(v any) ([]string, bool) {
	raw, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(raw))
	for i, e := range raw {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

// MARK: - Pool

func (m *Module) listPool(c *echo.Context) error {
	tenant := auth.UserID(c)
	id, kind, err := parseProvider(c)
	if err != nil {
		return err
	}
	rows, err := m.q.ListPoolKeys(c.Request().Context(), db.ListPoolKeysParams{TenantID: tenant, Provider: string(kind)})
	if err != nil {
		return err
	}
	keys := make([]poolKeyDTO, len(rows))
	for i, r := range rows {
		keys[i] = poolKeyDTO{ID: wire.UUID{UUID: r.ID}, Label: r.Label, CreatedAt: wireTime(r.CreatedAt)}
	}
	return wire.JSON(c, http.StatusOK, poolListResponse{Provider: id, Keys: keys})
}

func (m *Module) addPool(c *echo.Context) error {
	tenant := auth.UserID(c)
	_, kind, err := parseProvider(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[poolAddRequest](c)
	if err != nil {
		return err
	}
	if body.APIKey == "" {
		return wire.NewError(http.StatusBadRequest, "api_key_required")
	}
	sealed, err := m.opts.Box.Seal(body.APIKey, tenant)
	if err != nil {
		return err
	}
	now := m.now()
	row := db.InsertPoolKeyParams{
		ID: uuid.New(), TenantID: tenant, Provider: string(kind),
		Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce, Label: body.Label, CreatedAt: &now,
	}
	if err := m.q.InsertPoolKey(c.Request().Context(), row); err != nil {
		return err
	}
	m.invalidate(tenant, kind)
	return wire.JSON(c, http.StatusOK, poolKeyDTO{ID: wire.UUID{UUID: row.ID}, Label: row.Label, CreatedAt: wireTime(&now)})
}

func (m *Module) deletePool(c *echo.Context) error {
	tenant := auth.UserID(c)
	_, kind, err := parseProvider(c)
	if err != nil {
		return err
	}
	keyID, err := ids.Parse(c.Param("keyID"))
	if err != nil {
		return wire.NewError(http.StatusBadRequest, "invalid_key_id")
	}
	if err := m.q.DeletePoolKey(c.Request().Context(), db.DeletePoolKeyParams{TenantID: tenant, Provider: string(kind), ID: keyID}); err != nil {
		return err
	}
	m.invalidate(tenant, kind)
	return c.NoContent(http.StatusNoContent)
}
