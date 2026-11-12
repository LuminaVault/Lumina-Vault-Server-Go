package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/embedding"
	"lumina/internal/entitlement"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"lumina/internal/telemetry"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func prefixRunes(s string, n int) string { return swiftstr.Prefix(s, n) }

// swiftInt64 is Int64(String): optional sign, decimal digits, in range.
func swiftInt64(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

func swiftInt(s string, fallback int) int {
	if n, ok := swiftInt64(s); ok {
		return int(n)
	}
	return fallback
}

func configOr(node WorkflowNodeDTO, fallback string, keys ...string) string {
	for _, k := range keys {
		if v, ok := node.Configuration[k]; ok {
			return v
		}
	}
	return fallback
}

type engineUser struct {
	tier, override, username string
	privacyNoCN              bool
}

func (e *Engine) user(ctx context.Context, tenant uuid.UUID) (engineUser, bool, error) {
	var u engineUser
	err := e.m.pool.QueryRow(ctx, `SELECT tier, tier_override, username, privacy_no_cn_origin FROM users WHERE id = $1`, tenant).
		Scan(&u.tier, &u.override, &u.username, &u.privacyNoCN)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, false, nil
	}
	return u, err == nil, err
}

func (e *Engine) executeNode(ctx context.Context, node WorkflowNodeDTO, run claimedRun, c StringMap) (StringMap, error) {
	switch node.Kind {
	case Template, Output:
		return StringMap{"text": interpolate(configOr(node, "", "value", "template"), c)}, nil
	case Condition:
		lhs := interpolate(configOr(node, "", "left"), c)
		rhs := interpolate(configOr(node, "", "right"), c)
		matches := lhs == rhs
		if op, ok := node.Configuration["operator"]; ok && op == "notEquals" {
			matches = lhs != rhs
		}
		result := strconv.FormatBool(matches)
		return StringMap{"result": result, "text": result}, nil
	case Llm:
		return e.executeLLM(ctx, node, run, c)
	case Skill:
		name, ok := node.Configuration["skillName"]
		if !ok || e.d.Skills == nil {
			return nil, errExecutorUnavailable("skill")
		}
		u, found, err := e.user(ctx, run.tenant)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, errExecutorUnavailable("skill")
		}
		markdown, model, found, err := e.d.Skills.RunWorkflowSkill(ctx, run.tenant, u.tier, u.username, name,
			interpolate(configOr(node, "", "input"), c))
		if !found && err == nil {
			return nil, errExecutorUnavailable("skill")
		}
		if err != nil {
			return nil, err
		}
		modelUsed := ""
		if model != nil {
			modelUsed = *model
		}
		return StringMap{"text": markdown, "model": modelUsed}, nil
	case MemorySearch:
		query := interpolate(configOr(node, "", "query", "value"), c)
		vec, err := embedding.EmbedForQuery(ctx, e.d.Embeddings, query, run.tenant, "workflow.memorySearch")
		if err != nil {
			return nil, err
		}
		hits, err := e.d.Memories.SearchDocuments(ctx, run.tenant, query, vec,
			min(20, swiftInt(configOr(node, "5", "limit"), 5)), memorystore.SearchOptions{})
		if err != nil {
			return nil, err
		}
		parts := make([]string, len(hits))
		for i, h := range hits {
			parts[i] = h.Content
		}
		return StringMap{"text": strings.Join(parts, "\n\n")}, nil
	case MemoryWrite:
		text := interpolate(configOr(node, "", "content", "value"), c)
		vec, err := embedding.EmbedForWrite(ctx, e.d.Embeddings, text, run.tenant, "workflow.memoryWrite")
		if err != nil {
			return nil, err
		}
		memory, err := e.d.Memories.Create(ctx, memorystore.CreateParams{Tenant: run.tenant, Content: text, Embedding: vec})
		if err != nil {
			return nil, err
		}
		return StringMap{"text": text, "memoryID": ids.Upper(memory.ID)}, nil
	case ForEach:
		return executeForEach(node, c)
	case WhileLoop:
		return executeWhile(node, c)
	default: // parallel, trigger, approval
		return StringMap{}, nil
	}
}

func boundedIterations(node WorkflowNodeDTO) int {
	return min(max(swiftInt(configOr(node, "20", "maxIterations"), 20), 1), maxIterations)
}

func executeForEach(node WorkflowNodeDTO, c StringMap) (StringMap, error) {
	items := parseItems(interpolate(configOr(node, "", "items"), c))
	limit := boundedIterations(node)
	if len(items) > limit {
		return nil, errIterationLimit(limit)
	}
	template := configOr(node, "{{item}}", "template")
	outputs := make([]string, len(items))
	for i, item := range items {
		iteration := copyMap(c)
		iteration["item"], iteration["index"] = item, strconv.Itoa(i)
		outputs[i] = interpolate(template, iteration)
	}
	return StringMap{"text": strings.Join(outputs, configOr(node, "\n", "separator")), "count": strconv.Itoa(len(outputs))}, nil
}

func executeWhile(node WorkflowNodeDTO, c StringMap) (StringMap, error) {
	limit := boundedIterations(node)
	target := configOr(node, "true", "equals")
	condition := configOr(node, "false", "condition")
	template := configOr(node, "{{previous}}", "template")
	previous := ""
	var outputs []string
	for i := range limit {
		iteration := copyMap(c)
		iteration["iteration"], iteration["previous"] = strconv.Itoa(i), previous
		if interpolate(condition, iteration) != target {
			return StringMap{"text": strings.Join(outputs, "\n"), "count": strconv.Itoa(len(outputs))}, nil
		}
		previous = interpolate(template, iteration)
		outputs = append(outputs, previous)
	}
	return nil, errIterationLimit(limit)
}

// parseItems reads a JSON string array, else splits on commas and newlines.
func parseItems(raw string) []string {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err == nil && values != nil {
		return values
	}
	var out []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == 0x85 || r == 0x2028 || r == 0x2029
	}) {
		if t := strings.Trim(part, " \t"); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// routeCapture is WorkflowRouteCapture: the last route the transport reported.
type routeCapture struct {
	mu    sync.Mutex
	route *llm.ModelProvenance
}

func (r *routeCapture) record(p llm.ModelProvenance) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.route = &p
}

func (r *routeCapture) value() *llm.ModelProvenance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.route
}

func (e *Engine) executeLLM(ctx context.Context, node WorkflowNodeDTO, run claimedRun, c StringMap) (StringMap, error) {
	u, found, err := e.user(ctx, run.tenant)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errExecutorUnavailable("user")
	}
	tier := entitlement.EffectiveTier(u.tier, u.override)
	promptLimit, maxTokens := 24000, 512
	if tier == tierUltimate {
		promptLimit, maxTokens = 64000, 2048
	}
	prompt := swiftstr.Prefix(interpolate(configOr(node, "", "prompt", "value"), c), promptLimit)
	model := e.d.DefaultModel
	if configured, ok := node.Configuration["model"]; ok && configured != "auto" {
		model = configured
	}
	workflowID := ids.Upper(run.workflow)
	mode := llm.ModeManaged
	if e.d.Profiles != nil {
		profile, err := e.d.Profiles.Resolve(ctx, run.tenant, nil, "", workflowID)
		if err != nil {
			return nil, err
		}
		mode = profile.BrainMode()
	}
	var reservation *spendReservation
	if mode == llm.ModeManaged {
		r, err := e.m.reserveManagedCall(ctx, run.tenant, run.id, tier)
		var denied spendDenied
		if errors.As(err, &denied) {
			return nil, pausedError(denied)
		}
		if err != nil {
			return nil, err
		}
		reservation = &r
	}
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
		"stream":     false,
	})
	if err != nil {
		return nil, err
	}
	capture := &routeCapture{}
	freeRoute := &llm.WireRoute{Provider: llm.ProviderID(llm.OpenRouter), Model: llm.FreeLaneOpenRouterModel}
	sessionID := ""
	if run.conversation != nil {
		sessionID = ids.Upper(*run.conversation)
	}
	send := func(forced *llm.WireRoute) (llm.Metadata, error) {
		routed := llm.WithValues(ctx, func(v *llm.Values) {
			v.CurrentUser = &llm.User{ID: run.tenant, Tier: u.tier, TierOverride: u.override, PrivacyNoCNOrigin: u.privacyNoCN}
			v.CerberusScope = &llm.CerberusScope{Surface: llm.SurfaceWorkflow, WorkflowID: workflowID, ConversationID: run.conversation}
			v.CredentialMode = mode
			v.ForcedRoute = forced
			v.RouteOutcomeSink = capture.record
		})
		return e.d.Transport.ChatCompletions(routed, payload, ids.Upper(run.tenant), sessionID)
	}
	useFree := false
	var fallbackReason *WorkflowPauseReason
	response, err := send(nil)
	if err != nil {
		if reservation != nil {
			e.m.reconcileSpend(ctx, *reservation, 0)
			reservation = nil
		}
		if mode != llm.ModeManaged {
			return nil, err
		}
		useFree = true
		reason := ProviderUnavailable
		fallbackReason = &reason
		telemetry.Global.Workflow.FreeFallbacks.Add(ctx, 1)
		nodeID := node.Id.UUID
		message := "Managed provider unavailable; retrying on a backup route."
		e.m.appendEventData(ctx, run.tenant, run.id, NodeOutput, &nodeID, &message,
			map[string]string{"fallback": "free_lane", "reason": string(ProviderUnavailable)})
		if response, err = send(freeRoute); err != nil {
			return nil, pausedError{ProviderUnavailable}
		}
	}
	var body map[string]any
	text, ok := "", false
	if json.Unmarshal(response.Data, &body) == nil {
		if choices, isList := body["choices"].([]any); isList && len(choices) > 0 {
			if first, isObj := choices[0].(map[string]any); isObj {
				if message, isObj := first["message"].(map[string]any); isObj {
					text, ok = message["content"].(string)
				}
			}
		}
	}
	if !ok {
		if reservation != nil {
			e.m.reconcileSpend(ctx, *reservation, 0)
		}
		return nil, errInvalidLLMResponse
	}
	usage, _ := body["usage"].(map[string]any)
	tokensIn, tokensOut := jsonInteger(usage["prompt_tokens"]), jsonInteger(usage["completion_tokens"])
	route := capture.value()
	selectedModel := model
	if m, isString := body["model"].(string); isString {
		selectedModel = m
	} else if route != nil {
		selectedModel = route.Model
	}
	provider := ""
	switch {
	case route != nil:
		provider = route.Provider
	case useFree:
		provider = string(llm.OpenRouter)
	}
	var actual int64
	if mode == llm.ModeManaged && !useFree {
		actual = actualCostUsdMicros(usage, provider, selectedModel, tokensIn, tokensOut)
	}
	if reservation != nil {
		e.m.reconcileSpend(ctx, *reservation, actual)
	}
	output := StringMap{
		"text":                 text,
		"provider":             provider,
		"model":                selectedModel,
		"tokensIn":             strconv.Itoa(tokensIn),
		"tokensOut":            strconv.Itoa(tokensOut),
		"managedCostUsdMicros": strconv.FormatInt(actual, 10),
	}
	if fallbackReason != nil {
		output["freeFallbackReason"] = string(*fallbackReason)
	}
	return output, nil
}

// jsonInteger is WorkflowEngine.integer: a JSON number (truncated) or a decimal
// string; anything else is 0.
func jsonInteger(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case string:
		return swiftInt(n, 0)
	default:
		return 0
	}
}

// actualCostUsdMicros prefers the provider's reported usage.cost (dollars), else
// prices the tokens from the router catalog; an unknown route costs 0.
func actualCostUsdMicros(usage map[string]any, provider, model string, tokensIn, tokensOut int) int64 {
	switch cost := usage["cost"].(type) {
	case float64:
		return max(0, int64(math.Round(cost*1_000_000)))
	case string:
		if f, err := strconv.ParseFloat(cost, 64); err == nil {
			return max(0, int64(math.Round(f*1_000_000)))
		}
	}
	entry, ok := llm.CatalogEntry(llm.ProviderID(provider), model)
	if !ok {
		return 0
	}
	return int64(tokensIn)*entry.InputPerMillionUsdMicros/1_000_000 + int64(tokensOut)*entry.OutputPerMillionUsdMicros/1_000_000
}

// appendEventData is append(..., data:).
func (m *Module) appendEventData(ctx context.Context, tenant, run uuid.UUID, kind WorkflowRunEventKind, node *uuid.UUID, message *string, data map[string]string) {
	encoded, err := json.Marshal(data)
	if err != nil || data == nil {
		encoded = []byte("{}")
	}
	if _, err := m.pool.Exec(ctx, `INSERT INTO workflow_run_events (tenant_id, run_id, kind, node_id, message, data)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)`, tenant, run, string(kind), node, message, string(encoded)); err != nil {
		telemetry.Global.Workflow.EventWriteFailures.Add(ctx, 1)
		m.log().Error("workflow event append failed", "run_id", ids.Upper(run), "kind", string(kind), "error", err.Error())
	}
}
