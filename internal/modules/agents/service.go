package agents

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/hermesresolve"
	"lumina/internal/ids"
	"lumina/internal/modules/agents/db"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentsService's instance ids and the central agent's source.
const (
	centralID     = "central"
	byoID         = "byo"
	centralSource = "app"
	activeWindow  = 300 * time.Second
)

// resolver is hermesresolve.Resolver's Resolve, behind an interface for tests.
type resolver interface {
	Resolve(ctx context.Context, tenant uuid.UUID) (hermesresolve.Resolution, error)
}

// service is AgentsService: one list of a user's agents and their sessions,
// wherever they run.
//   - central is LuminaVault's own agent. Its sessions are the user's
//     conversations, read from the database. The shared gateway's /api/sessions
//     holds every tenant's rows, so it is never asked.
//   - byo is the user's own Hermes gateway (user_hermes_config), reached through
//     the resolver so the SSRF guard and the sealed key apply.
//
// Read-only. Every call is scoped to the caller's own account.
type service struct {
	pool     *pgxpool.Pool
	q        *db.Queries
	resolver resolver // nil when the deployment has no SecretBox or gateway URL
	http     *http.Client
	log      *slog.Logger
	now      func() time.Time
}

// sessionsFilter is AgentsService.Filter.
type sessionsFilter struct {
	instanceID, profile, source *string
	liveOnly                    bool
	limit                       int
}

// lookupError is AgentsService.LookupError.
type lookupError int

const (
	lookupUnknownInstance lookupError = iota + 1
	lookupSessionNotFound
)

func (e lookupError) Error() string {
	if e == lookupUnknownInstance {
		return "unknown_instance"
	}
	return "session_not_found"
}

func centralInstance() AgentInstanceDTO {
	return AgentInstanceDTO{
		Id: centralID, Kind: AgentInstanceDTOKindCentral, Name: "LuminaVault agent", Status: Ok,
		Profiles: []string{}, Platforms: []string{"app", "web"},
	}
}

func (s *service) instances(ctx context.Context, user uuid.UUID) []AgentInstanceDTO {
	out := []AgentInstanceDTO{centralInstance()}
	if byo := s.byoInstance(ctx, user); byo != nil {
		out = append(out, *byo)
	}
	return out
}

func (s *service) byoInstance(ctx context.Context, user uuid.UUID) *AgentInstanceDTO {
	client := s.gatewayClient(ctx, user)
	if client == nil {
		return nil
	}
	// Swift asks for the platforms alongside the instance.
	platforms := make(chan []string, 1)
	go func() { platforms <- client.connectedPlatforms(ctx) }()
	info, err := client.instance(ctx)
	switch {
	case err == nil:
		name := "Your Hermes"
		if info.hostname != nil {
			name = *info.hostname
		}
		return &AgentInstanceDTO{
			Id: byoID, Kind: AgentInstanceDTOKindByo, Name: name, Status: Ok,
			Hostname: info.hostname, Version: info.version, Profiles: info.profiles, Platforms: <-platforms,
		}
	case isFailure(err, failNotSupported):
		return &AgentInstanceDTO{
			Id: byoID, Kind: AgentInstanceDTOKindByo, Name: "Your Hermes", Status: Outdated,
			Profiles: []string{}, Platforms: <-platforms,
		}
	default:
		<-platforms
		return &AgentInstanceDTO{
			Id: byoID, Kind: AgentInstanceDTOKindByo, Name: "Your Hermes", Status: Unreachable,
			Profiles: []string{}, Platforms: []string{},
		}
	}
}

func (s *service) sessions(ctx context.Context, user uuid.UUID, f sessionsFilter) sessionsResponse {
	wantsCentral := f.instanceID == nil || *f.instanceID == centralID
	wantsByo := f.instanceID == nil || *f.instanceID == byoID

	type result struct {
		rows []sessionDTO
		err  error
	}
	central := make(chan result, 1)
	go func() {
		if !wantsCentral || !centralMatches(f) {
			central <- result{rows: []sessionDTO{}}
			return
		}
		rows, err := s.centralSessions(ctx, user, f.limit)
		central <- result{rows, err}
	}()
	byo := result{rows: []sessionDTO{}}
	if wantsByo {
		byo.rows, byo.err = s.byoSessions(ctx, user, f)
	}

	sessions := []sessionDTO{}
	errs := []AgentInstanceErrorDTO{}
	for _, r := range []struct {
		id  string
		res result
	}{{centralID, <-central}, {byoID, byo}} {
		if r.res.err != nil {
			s.log.WarnContext(ctx, "agents.sessions instance failed", "instance", r.id, "error", r.res.err)
			errs = append(errs, AgentInstanceErrorDTO{InstanceID: r.id, Message: describe(r.res.err)})
			continue
		}
		sessions = append(sessions, r.res.rows...)
	}
	if f.liveOnly {
		live := []sessionDTO{}
		for _, row := range sessions {
			if row.IsActive {
				live = append(live, row)
			}
		}
		sessions = live
	}
	// Swift's sort is stable; a missing date sorts as distantPast.
	sort.SliceStable(sessions, func(i, j int) bool {
		return lastActive(sessions[i]).After(lastActive(sessions[j]))
	})
	if len(sessions) > f.limit {
		sessions = sessions[:f.limit]
	}
	return sessionsResponse{Sessions: sessions, Errors: errs}
}

// distantPast is Date.distantPast, 0001-01-01.
var distantPast = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)

func lastActive(s sessionDTO) time.Time {
	if s.LastActiveAt == nil {
		return distantPast
	}
	return s.LastActiveAt.Time
}

func centralMatches(f sessionsFilter) bool {
	return f.profile == nil && (f.source == nil || *f.source == centralSource)
}

func (s *service) centralSessions(ctx context.Context, user uuid.UUID, limit int) ([]sessionDTO, error) {
	rows, err := s.q.CentralSessions(ctx, db.CentralSessionsParams{TenantID: user, RowLimit: int32(limit)}) //nolint:gosec // limit is clamped to 1...200
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]sessionDTO, 0, len(rows))
	for _, row := range rows {
		var title *string
		if row.Title != "" {
			title = &row.Title
		}
		count := int(row.MessageCount)
		created, last := row.CreatedAt, row.UpdatedAt
		out = append(out, sessionDTO{
			InstanceID: centralID, ID: ids.Upper(row.ID), Title: title, Source: centralSource,
			StartedAt: wireTime(&created), LastActiveAt: wireTime(&last), MessageCount: &count,
			IsActive: now.Sub(last) < activeWindow,
		})
	}
	return out, nil
}

func (s *service) byoSessions(ctx context.Context, user uuid.UUID, f sessionsFilter) ([]sessionDTO, error) {
	client := s.gatewayClient(ctx, user)
	if client == nil {
		return []sessionDTO{}, nil
	}
	page, err := client.sessions(ctx, byoID, f.profile, f.source, f.limit)
	if err != nil {
		return nil, err
	}
	// An old Hermes can't filter by profile; showing its one profile's rows under
	// a profile filter would mislabel them.
	if f.profile != nil && !page.profileAware {
		return []sessionDTO{}, nil
	}
	return page.sessions, nil
}

func (s *service) messages(ctx context.Context, user uuid.UUID, instanceID string, profile *string, sessionID string) (AgentSessionMessagesResponse, error) {
	switch instanceID {
	case centralID:
		return s.centralMessages(ctx, user, sessionID)
	case byoID:
		client := s.gatewayClient(ctx, user)
		if client == nil {
			return AgentSessionMessagesResponse{}, lookupUnknownInstance
		}
		resolved, messages, err := client.messages(ctx, profile, sessionID)
		if err != nil {
			var g *gatewayError
			if errors.As(err, &g) && g.kind == failHTTP && g.status == http.StatusNotFound {
				return AgentSessionMessagesResponse{}, lookupSessionNotFound
			}
			return AgentSessionMessagesResponse{}, err
		}
		return AgentSessionMessagesResponse{InstanceID: instanceID, Profile: profile, SessionID: resolved, Messages: messages}, nil
	default:
		return AgentSessionMessagesResponse{}, lookupUnknownInstance
	}
}

func (s *service) centralMessages(ctx context.Context, user uuid.UUID, sessionID string) (AgentSessionMessagesResponse, error) {
	// Ownership first: a conversation id alone is not proof of access.
	id, err := ids.Parse(sessionID)
	if err != nil {
		return AgentSessionMessagesResponse{}, lookupSessionNotFound
	}
	owned, err := s.q.ConversationOwned(ctx, db.ConversationOwnedParams{ID: id, TenantID: user})
	if err != nil {
		return AgentSessionMessagesResponse{}, err
	}
	if !owned {
		return AgentSessionMessagesResponse{}, lookupSessionNotFound
	}
	rows, err := s.q.ConversationMessages(ctx, id)
	if err != nil {
		return AgentSessionMessagesResponse{}, err
	}
	out := make([]AgentMessageDTO, 0, len(rows))
	for _, row := range rows {
		content, created := row.Content, row.CreatedAt
		out = append(out, AgentMessageDTO{Role: row.Role, Content: &content, CreatedAt: wireTime(&created)})
	}
	return AgentSessionMessagesResponse{InstanceID: centralID, SessionID: sessionID, Messages: out}, nil
}

// gatewayClient is the user's own gateway, or nil when they have none or it is
// unusable: a broken BYO config must not take the page down.
func (s *service) gatewayClient(ctx context.Context, user uuid.UUID) *gatewayClient {
	if s.resolver == nil {
		return nil
	}
	res, err := s.resolver.Resolve(ctx, user)
	if err != nil {
		s.log.WarnContext(ctx, "agents: byo gateway unusable", "error", err)
		return nil
	}
	if !res.IsUserOverride || res.BaseURL == nil {
		return nil
	}
	return &gatewayClient{baseURL: res.BaseURL.String(), authHeader: res.AuthHeader, http: s.http, log: s.log}
}

// describe is AgentsService.describe.
func describe(err error) string {
	var g *gatewayError
	if !errors.As(err, &g) {
		return "Sessions could not be loaded."
	}
	switch g.kind {
	case failUnreachable:
		return "Your Hermes did not answer."
	case failHTTP:
		if g.status == http.StatusUnauthorized || g.status == http.StatusForbidden {
			return "Your Hermes rejected the saved key."
		}
		return "Your Hermes answered HTTP " + strconv.Itoa(g.status) + "."
	case failInvalidResponse:
		return "Your Hermes sent a response we could not read."
	default:
		return "Sessions could not be loaded."
	}
}
