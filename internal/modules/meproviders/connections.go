package meproviders

import (
	"context"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/llm"
	"lumina/internal/modules/meproviders/db"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// connectionSummary is ConnectionSummaryDTO. Nil optionals stay out.
type connectionSummary struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	Title         string          `json:"title"`
	Subtitle      *string         `json:"subtitle,omitempty"`
	Health        string          `json:"health"`
	ProviderID    *llm.ProviderID `json:"providerID,omitempty"`
	GatewayID     *string         `json:"gatewayID,omitempty"`
	LastCheckedAt *wire.Time      `json:"lastCheckedAt,omitempty"`
	StatusDetail  *string         `json:"statusDetail,omitempty"`
	ActionHint    *string         `json:"actionHint,omitempty"`
}

type connectionsSummaryResponse struct {
	Connections []connectionSummary `json:"connections"`
	CheckedAt   *wire.Time          `json:"checkedAt,omitempty"`
}

// connectionTestResult is ConnectionTestResultDTO.
type connectionTestResult struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Title        string    `json:"title"`
	Health       string    `json:"health"`
	OK           bool      `json:"ok"`
	CheckedAt    wire.Time `json:"checkedAt"`
	StatusDetail *string   `json:"statusDetail,omitempty"`
	ErrorCode    *string   `json:"errorCode,omitempty"`
	ErrorMessage *string   `json:"errorMessage,omitempty"`
}

type connectionsTestAllResponse struct {
	Results   []connectionTestResult `json:"results"`
	CheckedAt wire.Time              `json:"checkedAt"`
}

// diagnosticEvent is ConnectionDiagnosticEventDTO.
type diagnosticEvent struct {
	ID              wire.UUID `json:"id"`
	OccurredAt      wire.Time `json:"occurredAt"`
	Kind            string    `json:"kind"`
	ConnectionID    *string   `json:"connectionID,omitempty"`
	ConnectionTitle *string   `json:"connectionTitle,omitempty"`
	Severity        string    `json:"severity"`
	Message         string    `json:"message"`
	Code            *string   `json:"code,omitempty"`
}

// diagnosticEventsResponse is ConnectionDiagnosticEventsResponse; nextCursor is
// always nil and so never encoded.
type diagnosticEventsResponse struct {
	Events []diagnosticEvent `json:"events"`
}

// ConnectionHealth values.
const (
	healthConnected  = "connected"
	healthNeedsSetup = "needs_setup"
	healthDegraded   = "degraded"
	healthError      = "error"
)

// connectionKinds is ConnectionKind's raw values.
var connectionKinds = map[string]bool{
	"llm_provider": true, "hermes_server": true, "hermes_gateway": true, "linked_account": true,
	"calendar": true, "nous": true, "plugin": true, "server": true,
}

// defaultStatusDetail is ConnectionsController.defaultStatusDetail.
func defaultStatusDetail(health string) string {
	switch health {
	case healthConnected:
		return "Connected"
	case healthNeedsSetup:
		return "Not configured"
	case healthDegraded:
		return "Configured but not verified"
	case healthError:
		return "Needs attention"
	case "testing":
		return "Testing"
	}
	return "Status unknown"
}

// severityFor is ConnectionsController.severity(for:).
func severityFor(health string) string {
	switch health {
	case healthConnected, healthNeedsSetup, "testing":
		return "info"
	case healthError:
		return "error"
	}
	return "warning"
}

// healthOf is ConnectionsController.health: a failure newer than the last
// verification (or with none) is an error.
func healthOf(configured bool, verifiedAt, lastFailureAt *time.Time) string {
	if !configured {
		return healthNeedsSetup
	}
	if lastFailureAt != nil && (verifiedAt == nil || lastFailureAt.After(*verifiedAt)) {
		return healthError
	}
	if verifiedAt != nil {
		return healthConnected
	}
	return healthDegraded
}

// grantsCalendarEvents is GoogleCalendarOAuthClient.grants(scope,
// calendarEventsScope): the full calendar scope implies the events one.
func grantsCalendarEvents(scope string) bool {
	for f := range strings.SplitSeq(scope, " ") {
		if f == "https://www.googleapis.com/auth/calendar.events" || f == "https://www.googleapis.com/auth/calendar" {
			return true
		}
	}
	return false
}

func orDefault(s *string, health string) *string {
	if s != nil {
		return s
	}
	return str(defaultStatusDetail(health))
}

func firstTime(a, b *time.Time) *time.Time {
	if a != nil {
		return a
	}
	return b
}

// buildSummaries is ConnectionsController.buildSummaries.
func (m *Module) buildSummaries(ctx context.Context, tenant uuid.UUID) ([]connectionSummary, error) {
	providerRows, err := m.q.ListCredentials(ctx, tenant)
	if err != nil {
		return nil, err
	}
	gatewayRows, err := m.q.ListGateways(ctx, tenant)
	if err != nil {
		return nil, err
	}
	hermes, err := m.q.GetHermesConfig(ctx, tenant)
	hasHermes := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	calendar, err := m.q.GetGoogleCalendarAccount(ctx, tenant)
	hasCalendar := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	container, err := m.q.GetHermesContainer(ctx, tenant)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	plugins, err := m.q.ListPluginInstalls(ctx, tenant)
	if err != nil {
		return nil, err
	}

	now := wire.Time{Time: m.now()}
	out := []connectionSummary{{
		ID: "server:api", Kind: "server", Title: "LuminaVault API", Subtitle: str("Current authenticated backend"),
		Health: healthConnected, LastCheckedAt: &now, StatusDetail: str("Reachable"), ActionHint: str("open_server_settings"),
	}}

	if !hasHermes {
		out = append(out, connectionSummary{
			ID: "hermes:managed", Kind: "hermes_server", Title: "Hermes Server", Subtitle: str("Managed default"),
			Health: healthConnected, StatusDetail: str("Using managed Hermes"), ActionHint: str("configure_hermes"),
		})
	} else {
		h := healthOf(true, hermes.VerifiedAt, nil)
		title := "Hermes Server"
		if hermes.Name != nil {
			title = *hermes.Name
		}
		out = append(out, connectionSummary{
			ID: "hermes:byo", Kind: "hermes_server", Title: title, Subtitle: str(hermes.BaseUrl),
			Health: h, LastCheckedAt: wireTime(hermes.VerifiedAt), StatusDetail: str(defaultStatusDetail(h)), ActionHint: str("configure_hermes"),
		})
	}

	// A Google grant made by "Connect Gmail" alone has no Calendar scope.
	if !hasCalendar || !grantsCalendarEvents(calendar.Scope) {
		out = append(out, connectionSummary{
			ID: "calendar:google", Kind: "calendar", Title: "Google Calendar", Subtitle: str("Schedule context"),
			Health: healthNeedsSetup, StatusDetail: str("Not connected"), ActionHint: str("connect_account"),
		})
	} else {
		h := healthConnected
		if calendar.Status == "needs_reauth" {
			h = healthError
		}
		out = append(out, connectionSummary{
			ID: "calendar:google", Kind: "calendar", Title: "Google Calendar", Subtitle: calendar.AccountEmail,
			Health: h, LastCheckedAt: wireTime(calendar.LastSyncedAt), StatusDetail: orDefault(calendar.LastFailureCode, h), ActionHint: str("connect_account"),
		})
	}

	linked := func(id, kind, title, subtitle string, at *time.Time) connectionSummary {
		s := connectionSummary{
			ID: id, Kind: kind, Title: title, Subtitle: str(subtitle), Health: healthNeedsSetup,
			LastCheckedAt: wireTime(at), StatusDetail: str("Not connected"), ActionHint: str("connect_account"),
		}
		if at != nil {
			s.Health, s.StatusDetail = healthConnected, str("Connected")
		}
		return s
	}
	out = append(out,
		linked("linked:xai", "linked_account", "Grok", "xAI OAuth account", container.XaiConnectedAt),
		linked("nous:portal", "nous", "Nous Portal", "Personal Nous subscription", container.NousConnectedAt))

	// Dictionary(uniquingKeysWith: { current, _ in current }) keeps the first row.
	byProvider := map[string]db.ListCredentialsRow{}
	for _, r := range providerRows {
		if _, ok := byProvider[r.Provider]; !ok {
			byProvider[r.Provider] = r
		}
	}
	for _, id := range providerIDs {
		r, ok := byProvider[string(id)]
		out = append(out, providerConnection(id, r, ok))
	}

	byGateway := map[string]db.ListGatewaysRow{}
	for _, r := range gatewayRows {
		if _, ok := byGateway[r.GatewayID]; !ok {
			byGateway[r.GatewayID] = r
		}
	}
	for _, g := range gateways {
		r, ok := byGateway[g.id]
		out = append(out, gatewayConnection(g, r, ok))
	}

	for _, p := range plugins {
		s := connectionSummary{
			ID: "plugin:" + p.PluginSlug, Kind: "plugin", Title: p.PluginSlug, Subtitle: str("Plugin disabled"),
			Health: healthDegraded, LastCheckedAt: wireTime(p.LastSyncAt), StatusDetail: str(p.Status), ActionHint: str("open_plugin"),
		}
		if p.Status == "enabled" {
			s.Subtitle, s.Health = str("Plugin enabled"), healthConnected
		}
		out = append(out, s)
	}
	return out, nil
}

// providerConnection is ConnectionsController.providerConnection.
func providerConnection(id llm.ProviderID, r db.ListCredentialsRow, found bool) connectionSummary {
	kind := defaultKind(id)
	if found && validCredentialKind(r.CredentialKind) {
		kind = r.CredentialKind
	}
	has := found && (r.Ciphertext != nil || r.BaseUrl != nil || kind == string(kindOAuth))
	var verifiedAt, failedAt *time.Time
	var code *string
	if found {
		verifiedAt, failedAt, code = r.VerifiedAt, r.LastFailureAt, r.LastFailureCode
	}
	h := healthOf(has, verifiedAt, failedAt)

	var subtitle string
	switch {
	case found && r.Label != nil && *r.Label != "":
		subtitle = *r.Label
	case found && r.BaseUrl != nil && *r.BaseUrl != "":
		subtitle = *r.BaseUrl
	case kind == string(kindAPIKey):
		subtitle = "API key"
	case kind == string(kindOAuth):
		subtitle = "OAuth account"
	case id == "ollama":
		subtitle = "Local host URL"
	default:
		subtitle = "OpenAI-compatible URL"
	}
	pid := id
	return connectionSummary{
		ID: "provider:" + string(id), Kind: "llm_provider", Title: providerDisplayName(id), Subtitle: &subtitle,
		Health: h, ProviderID: &pid, LastCheckedAt: wireTime(firstTime(verifiedAt, failedAt)),
		StatusDetail: orDefault(code, h), ActionHint: str("configure_provider"),
	}
}

// gatewayConnection is ConnectionsController.gatewayConnection. An unknown
// stored status reads as not configured.
func gatewayConnection(g gateway, r db.ListGatewaysRow, found bool) connectionSummary {
	status := "not_configured"
	if found {
		switch r.Status {
		case "configured", "verified", "error":
			status = r.Status
		}
	}
	var verifiedAt, failedAt *time.Time
	var code *string
	if found {
		verifiedAt, failedAt, code = r.VerifiedAt, r.LastFailureAt, r.LastFailureCode
	}
	var h string
	switch status {
	case "not_configured":
		h = healthNeedsSetup
	case "verified":
		h = healthConnected
	case "configured":
		h = healthOf(true, verifiedAt, failedAt)
	default:
		h = healthError
	}
	gid := g.id
	return connectionSummary{
		ID: "gateway:" + g.id, Kind: "hermes_gateway", Title: g.name, Subtitle: str(g.description),
		Health: h, GatewayID: &gid, LastCheckedAt: wireTime(firstTime(verifiedAt, failedAt)),
		StatusDetail: orDefault(code, h), ActionHint: str("configure_gateway"),
	}
}

func (m *Module) summary(c *echo.Context) error {
	connections, err := m.buildSummaries(c.Request().Context(), auth.UserID(c))
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, connectionsSummaryResponse{Connections: connections, CheckedAt: &wire.Time{Time: m.now()}})
}

func (m *Module) testAll(c *echo.Context) error {
	tenant := auth.UserID(c)
	ctx := c.Request().Context()
	checkedAt := m.now()
	summaries, err := m.buildSummaries(ctx, tenant)
	if err != nil {
		return err
	}
	results := make([]connectionTestResult, len(summaries))
	for i, s := range summaries {
		r := connectionTestResult{
			ID: s.ID, Kind: s.Kind, Title: s.Title, Health: s.Health, OK: s.Health == healthConnected,
			CheckedAt: wire.Time{Time: checkedAt}, StatusDetail: orDefault(s.StatusDetail, s.Health),
		}
		if s.Health == healthError {
			r.ErrorCode = str("connection_error")
			r.ErrorMessage = s.StatusDetail
			if r.ErrorMessage == nil {
				r.ErrorMessage = str("Connection failed.")
			}
		}
		results[i] = r
	}
	for _, r := range results {
		if err := m.q.InsertDiagnosticEvent(ctx, db.InsertDiagnosticEventParams{
			ID: uuid.New(), TenantID: tenant, OccurredAt: checkedAt, Kind: r.Kind,
			ConnectionID: str(r.ID), ConnectionTitle: str(r.Title), Severity: severityFor(r.Health),
			Message: r.Title + ": " + *orDefault(r.StatusDetail, r.Health), Code: r.ErrorCode, CreatedAt: &checkedAt,
		}); err != nil {
			return err
		}
	}
	return wire.JSON(c, http.StatusOK, connectionsTestAllResponse{Results: results, CheckedAt: wire.Time{Time: checkedAt}})
}

// Events limits: ConnectionsController.defaultEventsLimit and maxEventsLimit.
const (
	defaultEventsLimit = 30
	maxEventsLimit     = 100
)

func (m *Module) events(c *echo.Context) error {
	limit := defaultEventsLimit
	if n, ok := wire.QueryInt(c, "limit"); ok {
		limit = max(1, min(n, maxEventsLimit))
	}
	rows, err := m.q.ListDiagnosticEvents(c.Request().Context(), db.ListDiagnosticEventsParams{TenantID: auth.UserID(c), Limit: int32(limit)})
	if err != nil {
		return err
	}
	out := []diagnosticEvent{}
	for _, r := range rows {
		if !connectionKinds[r.Kind] || (r.Severity != "info" && r.Severity != "warning" && r.Severity != "error") {
			continue
		}
		out = append(out, diagnosticEvent{
			ID: wire.UUID{UUID: r.ID}, OccurredAt: wire.Time{Time: r.OccurredAt}, Kind: r.Kind,
			ConnectionID: r.ConnectionID, ConnectionTitle: r.ConnectionTitle, Severity: r.Severity,
			Message: r.Message, Code: r.Code,
		})
	}
	return wire.JSON(c, http.StatusOK, diagnosticEventsResponse{Events: out})
}
