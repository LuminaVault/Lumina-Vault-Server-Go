package marketplace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/auth"
	"lumina/internal/modules/marketplace/db"
	"lumina/internal/wire"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// Runner is PluginRunnerClienting: runs a WASM module in the plugin runner.
type Runner interface {
	Execute(ctx context.Context, module []byte, toolName string, permissions []PluginPermission, input map[string]string) (output map[string]string, fuel int, err error)
}

// httpRunner is PluginRunnerClient: POST <base>/v1/execute with a bearer token.
type httpRunner struct {
	baseURL, token string
	client         *http.Client
}

// NewRunner builds the runner client. With no token every call is 503
// plugin_runner_disabled, as in Swift.
func NewRunner(baseURL, token string) Runner {
	return httpRunner{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{Timeout: 32 * time.Second}}
}

func (r httpRunner) Execute(ctx context.Context, module []byte, toolName string, permissions []PluginPermission, input map[string]string) (map[string]string, int, error) {
	if r.token == "" {
		return nil, 0, wire.NewError(http.StatusServiceUnavailable, "plugin_runner_disabled")
	}
	body, err := json.Marshal(struct {
		ModuleBase64 []byte             `json:"moduleBase64"`
		ToolName     string             `json:"toolName"`
		Permissions  []PluginPermission `json:"permissions"`
		Input        map[string]string  `json:"input"`
		Fuel         int                `json:"fuel"`
	}{module, toolName, permissions, input, 10_000_000})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/execute", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req) //nolint:gosec // the runner URL is operator configuration
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		slog.WarnContext(ctx, "plugin runner rejected execution", "status", resp.StatusCode)
		return nil, 0, wire.NewError(http.StatusBadGateway, "plugin_runner_rejected")
	}
	var out struct {
		Output       *map[string]string `json:"output"`
		FuelConsumed *int               `json:"fuelConsumed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, err
	}
	if out.Output == nil || out.FuelConsumed == nil {
		return nil, 0, errors.New("plugin runner: incomplete response")
	}
	return *out.Output, *out.FuelConsumed, nil
}

func (m *Module) runTool(c *echo.Context) error {
	req, err := wire.Bind[toolRunRequest](c)
	if err != nil {
		return err
	}
	slug, toolName := c.Param("slug"), c.Param("toolName")
	tenant, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	size := 0
	for k, v := range req.Input {
		size += len(k) + len(v)
	}
	if size > maxToolInput {
		return wire.NewError(http.StatusRequestEntityTooLarge, "plugin_input_too_large")
	}
	ctx := c.Request().Context()
	install, version, err := m.runnableVersion(ctx, tenant, slug)
	if err != nil {
		return err
	}
	artifactKey := *version.ArtifactKey
	if !safeArtifactKey(artifactKey) {
		return errArtifactKey
	}
	data, ok := m.readArtifact(artifactKey)
	if !ok {
		return wire.NewError(http.StatusServiceUnavailable, "plugin_artifact_unavailable")
	}
	if version.ArtifactSha256 == nil || sha256Hex(data) != *version.ArtifactSha256 {
		return errArtifactDigest
	}
	if version.ArtifactSignature == nil || !VerifyArtifactSignature(*version.ArtifactSignature, artifactKey, *version.ArtifactSha256, m.opts.ArtifactSigningKey) {
		return wire.NewError(http.StatusUnprocessableEntity, "plugin_artifact_signature_mismatch")
	}
	manifest, err := ParseManifest(version)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(manifest.Tools, func(t MarketplaceToolManifest) bool { return t.Name == toolName }) {
		return wire.NewError(http.StatusNotFound, "plugin_tool_not_found")
	}
	granted := []PluginPermission{}
	for _, p := range install.GrantedPermissions {
		if PluginPermission(p).Valid() {
			granted = append(granted, PluginPermission(p))
		}
	}

	runID := uuid.New()
	if err := m.q.InsertExecution(ctx, db.InsertExecutionParams{
		ID: runID, TenantID: tenant, InstallID: install.ID, VersionID: version.ID, ToolName: toolName, CreatedAt: new(m.now()),
	}); err != nil {
		return err
	}
	started := time.Now()
	output, fuel, runErr := m.executeWithCapabilities(ctx, data, toolName, tenant, slug, granted, version.NetworkHosts, req.Input)
	// Swift keeps whole seconds only: components.seconds * 1000.
	duration := int64(time.Since(started)/time.Second) * 1000
	if runErr != nil {
		_ = m.q.FinishExecution(ctx, db.FinishExecutionParams{Status: "failed", ErrorCode: new("runner_failed"), DurationMs: &duration, ID: runID})
		return runErr
	}
	if err := m.q.FinishExecution(ctx, db.FinishExecutionParams{Status: "succeeded", DurationMs: &duration, ID: runID}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, PluginToolRunResponse{RunId: wire.UUID{UUID: runID}, Output: output, FuelConsumed: fuel})
}

func (m *Module) runnableVersion(ctx context.Context, tenant uuid.UUID, slug string) (db.PluginInstall, db.MarketplaceVersion, error) {
	errRequired := wire.NewError(http.StatusConflict, "installed_wasm_version_required")
	install, err := m.q.EnabledInstall(ctx, db.EnabledInstallParams{TenantID: tenant, PluginSlug: slug})
	if errors.Is(err, pgx.ErrNoRows) {
		return install, db.MarketplaceVersion{}, errRequired
	}
	if err != nil {
		return install, db.MarketplaceVersion{}, err
	}
	if install.MarketplaceVersionID == nil {
		return install, db.MarketplaceVersion{}, errRequired
	}
	version, err := m.q.VersionByID(ctx, *install.MarketplaceVersionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return install, version, errRequired
	}
	if err != nil {
		return install, version, err
	}
	if version.Status != string(MarketplaceVersionStatusApproved) || version.RuntimeKind != string(Wasm) || version.ArtifactKey == nil {
		return install, version, errRequired
	}
	return install, version, nil
}

// executeWithCapabilities runs the module, serving its capability requests
// between rounds. Reserved keys in the client's input are stripped, so a client
// cannot spoof broker results.
func (m *Module) executeWithCapabilities(ctx context.Context, module []byte, toolName string, tenant uuid.UUID, slug string, permissions []PluginPermission, hosts []string, input map[string]string) (map[string]string, int, error) {
	original := maps.Clone(input)
	if original == nil {
		original = map[string]string{}
	}
	delete(original, "_capabilityRequests")
	delete(original, "_capabilityResults")
	delete(original, "_capabilityRound")
	roundInput := original
	total := 0
	for round := 0; round <= maxCapRounds; round++ {
		output, fuel, err := m.runner.Execute(ctx, module, toolName, permissions, roundInput)
		if err != nil {
			return nil, 0, err
		}
		total += fuel
		encoded, ok := output["_capabilityRequests"]
		if !ok {
			return output, total, nil
		}
		requests, ok := parseCapabilityRequests(encoded)
		if !ok {
			return nil, 0, wire.NewError(http.StatusUnprocessableEntity, "invalid_capability_requests")
		}
		if round >= maxCapRounds {
			break
		}
		results := m.broker.Execute(ctx, requests, tenant, slug, permissions, hosts)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(results); err != nil {
			return nil, 0, err
		}
		if buf.Len()-1 > maxToolInput {
			return nil, 0, wire.NewError(http.StatusUnprocessableEntity, "capability_results_too_large")
		}
		roundInput = maps.Clone(original)
		roundInput["_capabilityResults"] = strings.TrimSuffix(buf.String(), "\n")
		roundInput["_capabilityRound"] = strconv.Itoa(round + 1)
	}
	return nil, 0, wire.NewError(http.StatusUnprocessableEntity, "capability_round_limit_exceeded")
}

// parseCapabilityRequests needs a non-empty array with every key present.
func parseCapabilityRequests(encoded string) ([]CapabilityRequest, bool) {
	var raw []struct {
		ID        *string            `json:"id"`
		Operation *string            `json:"operation"`
		Arguments *map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(encoded), &raw); err != nil || len(raw) == 0 {
		return nil, false
	}
	out := make([]CapabilityRequest, len(raw))
	for i, r := range raw {
		if r.ID == nil || r.Operation == nil || r.Arguments == nil {
			return nil, false
		}
		out[i] = CapabilityRequest{ID: *r.ID, Operation: *r.Operation, Arguments: *r.Arguments}
	}
	return out, true
}
