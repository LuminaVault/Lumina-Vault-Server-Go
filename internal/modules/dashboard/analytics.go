package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/auth"
	"lumina/internal/modules/dashboard/db"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v5"
)

const (
	scopeActive   = "active"
	scopePersonal = "personal"
)

// parseScope is AnalyticsController.scope: unknown → active.
func parseScope(raw string) string {
	if raw == scopePersonal {
		return raw
	}
	return scopeActive
}

// resolveScope is AnalyticsController.resolve: personal ignores X-Vault-ID.
func (m *Module) resolveScope(c *echo.Context, scope string) (vaultaccess.Access, error) {
	if scope == scopePersonal {
		user, ok := auth.Identity(c)
		if !ok {
			return vaultaccess.Access{}, wire.ErrUnauthenticated
		}
		return m.vaults.ResolveVault(requestContext(c), user, user, vaultaccess.Read)
	}
	return m.vaults.Resolve(c, vaultaccess.Read)
}

type retrievalWindow struct {
	events, hits int
	meanTop      *float64
}

func (m *Module) retrievalWindow(ctx context.Context, tenant uuid.UUID, from, to time.Time) (retrievalWindow, error) {
	var w retrievalWindow
	err := m.pool.QueryRow(ctx, sqlRetrievalWindow, pgx.NamedArgs{"tenant_id": tenant, "range_start": from, "range_end": to}).
		Scan(&w.events, &w.hits, &w.meanTop)
	return w, err
}

// retrievalHealth is AnalyticsController.retrievalHealth.
func (m *Module) retrievalHealth(c *echo.Context) error {
	ctx, tenant := requestContext(c), auth.UserID(c)
	now := time.Now()
	windowStart := now.Add(-7 * 24 * time.Hour)
	current, err := m.retrievalWindow(ctx, tenant, windowStart, now)
	if err != nil {
		return err
	}
	prior, err := m.retrievalWindow(ctx, tenant, now.Add(-14*24*time.Hour), windowStart)
	if err != nil {
		return err
	}
	leaks, err := orZero(m.q.LatestLeakCount(ctx, tenant))
	if err != nil {
		return err
	}
	out := RetrievalHealthResponse{EventsCount: current.events, LeakCount: int(leaks), Trend: "steady"}
	if current.meanTop != nil {
		v := swiftDouble(*current.meanTop)
		out.MeanTopDistance = &v
	}
	if current.events > 0 {
		rate := float64(current.hits) / float64(current.events)
		v := swiftDouble(rate)
		out.HitRate = &v
		// ±2 percentage points is steady.
		if prior.events > 0 {
			priorRate := float64(prior.hits) / float64(prior.events)
			switch {
			case rate > priorRate+0.02:
				out.Trend = "improving"
			case rate < priorRate-0.02:
				out.Trend = "declining"
			}
		}
	}
	return wire.JSON(c, http.StatusOK, out)
}

// usageSummary is AnalyticsController.usageSummary (always the 30-day range).
func (m *Module) usageSummary(c *echo.Context) error {
	ctx, tenant := requestContext(c), auth.UserID(c)
	now := time.Now()
	start := analyticsPeriodStart("30d", now)
	var tin, tout, sessions, cost int64
	err := m.pool.QueryRow(ctx, sqlUsageTotals, pgx.NamedArgs{"tenant_id": tenant, "since": start}).
		Scan(&tin, &tout, &sessions, &cost)
	if err != nil {
		return err
	}
	// EmbeddingUsage.yearMonth: "YYYY-MM" in UTC.
	embedding, err := orZero(m.q.EmbeddingTokens(ctx, db.EmbeddingTokensParams{TenantID: tenant, YearMonth: now.UTC().Format("2006-01")}))
	if err != nil {
		return err
	}
	voice, err := m.voiceUsage(ctx, tenant, start)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, UsageSummaryResponse{
		LLMTokensIn: int(tin), LLMTokensOut: int(tout), EmbeddingTokens: int(embedding), SessionsCount: int(sessions),
		EstimatedCostCents: centsFromMicros(cost), Voice: voice,
		PeriodStart: wire.Time{Time: start}, PeriodEnd: wire.Time{Time: now},
	})
}

// voiceUsage is AnalyticsController.voiceUsage: nil when there are no rows.
// Totals sum the raw columns, not the rounded per-channel values.
func (m *Module) voiceUsage(ctx context.Context, tenant uuid.UUID, since time.Time) (*VoiceUsageSummaryDTO, error) {
	rows, err := m.pool.Query(ctx, sqlVoiceUsage, pgx.NamedArgs{"tenant_id": tenant, "since": since})
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := VoiceUsageSummaryDTO{ByChannel: []VoiceChannelUsageDTO{}}
	var duration, real, imputed int64
	for rows.Next() {
		var (
			channel                  string
			calls, failed, ms, r, im int64
		)
		if err := rows.Scan(&channel, &calls, &failed, &ms, &r, &im); err != nil {
			return nil, err
		}
		out.ByChannel = append(out.ByChannel, VoiceChannelUsageDTO{
			Channel: channel, Calls: int(calls), FailedCalls: int(failed), AudioMinutes: swiftDouble(audioMinutes(ms)),
			RealCostCents: centsFromMicros(r), ImputedCostCents: centsFromMicros(im),
		})
		out.Calls += int(calls)
		out.FailedCalls += int(failed)
		duration, real, imputed = duration+ms, real+r, imputed+im
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.ByChannel) == 0 {
		return nil, nil //nolint:nilnil // nil is the "no voice in use" answer
	}
	out.AudioMinutes = swiftDouble(audioMinutes(duration))
	out.RealCostCents, out.ImputedCostCents = centsFromMicros(real), centsFromMicros(imputed)
	return &out, nil
}

func (m *Module) summary(ctx context.Context, vault, user uuid.UUID, personal bool, since time.Time) (AnalyticsSummaryDTO, error) {
	var personalSessions, teamSessions, requests, tin, tout, captures, retrievals, cost int64
	err := m.pool.QueryRow(ctx, sqlAnalyticsSummary, pgx.NamedArgs{"user_id": user, "vault_id": vault, "since": since}).
		Scan(&personalSessions, &teamSessions, &requests, &tin, &tout, &captures, &retrievals, &cost)
	if err != nil {
		return AnalyticsSummaryDTO{}, err
	}
	sessions := teamSessions
	if personal {
		sessions = personalSessions
	}
	return AnalyticsSummaryDTO{
		Sessions: int(sessions), AIRequests: int(requests), TokensIn: int(tin), TokensOut: int(tout),
		Captures: int(captures), Retrievals: int(retrievals), EstimatedCostUsdMicros: cost,
	}, nil
}

func (m *Module) daily(ctx context.Context, vault, user uuid.UUID, personal bool, since time.Time, days int) ([]AnalyticsDailyPointDTO, error) {
	rows, err := m.pool.Query(ctx, sqlAnalyticsDaily, pgx.NamedArgs{
		"user_id": user, "vault_id": vault, "personal": personal, "since": since, "row_limit": days,
	})
	if err != nil {
		return nil, err
	}
	points, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AnalyticsDailyPointDTO, error) {
		var (
			day                                              time.Time
			sessions, requests, tokens, captures, retrievals int64
			cost                                             int64
		)
		err := r.Scan(&day, &sessions, &requests, &tokens, &captures, &retrievals, &cost)
		return AnalyticsDailyPointDTO{
			Date: wire.Time{Time: day}, Sessions: int(sessions), AIRequests: int(requests), Tokens: int(tokens),
			Captures: int(captures), Retrievals: int(retrievals), EstimatedCostUsdMicros: cost,
		}, err
	})
	return nonNil(points), err
}

func percent(v float64) int { return int(math.Round(v * 100)) }

func (m *Module) memoryHealth(ctx context.Context, vault uuid.UUID, now time.Time) (MemoryHealthDTO, error) {
	var (
		total, stale, never, unorganized, pending  int64
		freshnessV, engagementV, organizationV, rv float64
	)
	err := m.pool.QueryRow(ctx, sqlMemoryHealth, pgx.NamedArgs{"vault_id": vault, "now": now}).
		Scan(&total, &stale, &never, &unorganized, &pending, &freshnessV, &engagementV, &organizationV, &rv)
	if err != nil {
		return MemoryHealthDTO{}, err
	}
	freshness, engagement, organization, review := percent(freshnessV), percent(engagementV), percent(organizationV), percent(rv)
	score := 0
	if total != 0 {
		score = weightedHealthScore(freshness, engagement, organization, review)
	}
	return MemoryHealthDTO{
		Score: score, TotalMemories: int(total), StaleCount: int(stale), NeverRetrievedCount: int(never),
		UnorganizedCount: int(unorganized), PendingReviewCount: int(pending),
		Components: []MemoryHealthComponentDTO{
			{Key: "freshness", Title: "Freshness", Score: freshness, Weight: 35},
			{Key: "engagement", Title: "Engagement", Score: engagement, Weight: 25},
			{Key: "organization", Title: "Organization", Score: organization, Weight: 20},
			{Key: "review", Title: "Review readiness", Score: review, Weight: 20},
		},
	}, nil
}

// recommendations is AnalyticsController.recommendations: hidden ids are removed
// before the cap of five.
func (m *Module) recommendations(ctx context.Context, vault, user uuid.UUID, h MemoryHealthDTO, since, now time.Time) ([]AnalyticsRecommendationDTO, error) {
	values := []AnalyticsRecommendationDTO{}
	if h.StaleCount > 0 {
		values = append(values, AnalyticsRecommendationDTO{
			ID: "memory-review-overdue", Title: "Review older memories",
			Detail:   strconv.Itoa(h.StaleCount) + " memories have not been revisited in 30 days.",
			Severity: "important", ActionTitle: "Open review queue", DeepLink: "/memories?filter=review-overdue",
		})
	}
	if h.PendingReviewCount > 0 {
		values = append(values, AnalyticsRecommendationDTO{
			ID: "memory-moderation-pending", Title: "Resolve pending memories",
			Detail:   strconv.Itoa(h.PendingReviewCount) + " compiled memories are waiting for approval.",
			Severity: "attention", ActionTitle: "Review pending", DeepLink: "/memories?reviewState=pending",
		})
	}
	if h.UnorganizedCount > 0 {
		values = append(values, AnalyticsRecommendationDTO{
			ID: "memory-organization", Title: "Organize loose memories",
			Detail:   strconv.Itoa(h.UnorganizedCount) + " memories have no tags, Space, or source.",
			Severity: "attention", ActionTitle: "Organize memories", DeepLink: "/memories?filter=unorganized",
		})
	}
	if h.NeverRetrievedCount >= 5 {
		values = append(values, AnalyticsRecommendationDTO{
			ID: "memory-never-retrieved", Title: "Check unused knowledge",
			Detail:   strconv.Itoa(h.NeverRetrievedCount) + " memories have never appeared in retrieval.",
			Severity: "info", ActionTitle: "Inspect unused", DeepLink: "/memories?filter=unused",
		})
	}
	var requests, failures, latency int64
	err := m.pool.QueryRow(ctx, sqlModelReliability, pgx.NamedArgs{"vault_id": vault, "since": since}).Scan(&requests, &failures, &latency)
	if err != nil {
		return nil, err
	}
	if requests >= 10 && (float64(failures)/float64(requests) >= 0.10 || latency >= 10000) {
		values = append(values, AnalyticsRecommendationDTO{
			ID: "model-reliability", Title: "Review model routing",
			Detail:   "Recent AI runs are slower or less reliable than expected.",
			Severity: "attention", ActionTitle: "Compare models", DeepLink: "/analytics#models",
		})
	}
	hidden, err := m.q.HiddenRecommendations(ctx, db.HiddenRecommendationsParams{VaultID: vault, UserID: user, Now: timestamptz(now)})
	if err != nil {
		return nil, err
	}
	hiddenIDs := make(map[string]bool, len(hidden))
	for _, id := range hidden {
		hiddenIDs[id] = true
	}
	out := make([]AnalyticsRecommendationDTO, 0, len(values))
	for _, v := range values {
		if !hiddenIDs[v.ID] && len(out) < 5 {
			out = append(out, v)
		}
	}
	return out, nil
}

// overview is AnalyticsController.overview.
func (m *Module) overview(c *echo.Context) error {
	ctx := requestContext(c)
	rng, scope := parseRange(wire.Query(c, "range")), parseScope(wire.Query(c, "scope"))
	access, err := m.resolveScope(c, scope)
	if err != nil {
		return err
	}
	user := auth.UserID(c)
	now := time.Now()
	start := analyticsPeriodStart(rng, now)
	summary, err := m.summary(ctx, access.VaultID, user, access.IsPersonal, start)
	if err != nil {
		return err
	}
	daily, err := m.daily(ctx, access.VaultID, user, access.IsPersonal, start, rangeDays(rng))
	if err != nil {
		return err
	}
	health, err := m.memoryHealth(ctx, access.VaultID, now)
	if err != nil {
		return err
	}
	recs, err := m.recommendations(ctx, access.VaultID, user, health, start, now)
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, AnalyticsOverviewResponse{
		Scope: scope, VaultID: wire.UUID{UUID: access.VaultID}, Range: rng,
		PeriodStart: wire.Time{Time: start}, PeriodEnd: wire.Time{Time: now},
		Summary: summary, Daily: daily, MemoryHealth: health, Recommendations: recs,
	})
}

// models is AnalyticsController.models. Identity stays hidden unless the tenant
// is BYOK (ModelDisclosure.forBrainMode); a missing preference row means managed.
func (m *Module) models(c *echo.Context) error {
	ctx := requestContext(c)
	rng, scope := parseRange(wire.Query(c, "range")), parseScope(wire.Query(c, "scope"))
	access, err := m.resolveScope(c, scope)
	if err != nil {
		return err
	}
	pref, err := orZero(m.q.LLMPreference(ctx, auth.UserID(c)))
	if err != nil {
		return err
	}
	rows, err := m.pool.Query(ctx, sqlModelEffectiveness, pgx.NamedArgs{
		"hide": pref.Mode != "byok", "generic_provider": genericProviderName, "generic_model": genericModelID,
		"vault_id": access.VaultID, "since": analyticsPeriodStart(rng, time.Now()),
	})
	if err != nil {
		return err
	}
	models, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ModelEffectivenessDTO, error) {
		var (
			provider, model                                                      string
			requests, successes, fallbacks, latency, p95, tokens, cost, pos, neg int64
		)
		if err := r.Scan(&provider, &model, &requests, &successes, &fallbacks, &latency, &p95, &tokens, &cost, &pos, &neg); err != nil {
			return ModelEffectivenessDTO{}, err
		}
		denominator := math.Max(1, float64(requests))
		dto := ModelEffectivenessDTO{
			Provider: provider, Model: model, Requests: int(requests),
			SuccessRate:      swiftDouble(float64(successes) / denominator),
			FallbackRate:     swiftDouble(float64(fallbacks) / denominator),
			AverageLatencyMs: int(latency), P95LatencyMs: int(p95), Tokens: int(tokens), EstimatedCostUsdMicros: cost,
			PositiveFeedback: int(pos), NegativeFeedback: int(neg),
		}
		if pos+neg > 0 {
			rate := swiftDouble(float64(pos) / float64(pos+neg))
			dto.SatisfactionRate = &rate
		}
		return dto, nil
	})
	if err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, ModelEffectivenessResponse{Range: rng, Models: nonNil(models)})
}

// team is AnalyticsController.team. Members are for vault admins only.
func (m *Module) team(c *echo.Context) error {
	ctx := requestContext(c)
	rng := parseRange(wire.Query(c, "range"))
	access, err := m.vaults.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	if access.IsPersonal || access.TeamID == nil {
		return wire.NewError(http.StatusUnprocessableEntity, "team analytics requires a shared vault")
	}
	start := analyticsPeriodStart(rng, time.Now())
	summary, err := m.summary(ctx, access.VaultID, auth.UserID(c), false, start)
	if err != nil {
		return err
	}
	out := TeamAnalyticsResponse{VaultID: wire.UUID{UUID: access.VaultID}, Range: rng, Summary: summary}
	if access.CanAdmin() {
		rows, err := m.pool.Query(ctx, sqlTeamMembers, pgx.NamedArgs{"vault_id": access.VaultID, "team_id": *access.TeamID, "since": start})
		if err != nil {
			return err
		}
		members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TeamMemberAnalyticsDTO, error) {
			var (
				id                                           uuid.UUID
				name                                         string
				captures, retrievals, requests, tokens, cost int64
			)
			err := r.Scan(&id, &name, &captures, &retrievals, &requests, &tokens, &cost)
			return TeamMemberAnalyticsDTO{
				ID: wire.UUID{UUID: id}, DisplayName: name, Captures: int(captures), Retrievals: int(retrievals),
				AIRequests: int(requests), Tokens: int(tokens), EstimatedCostUsdMicros: cost,
			}, err
		})
		if err != nil {
			return err
		}
		members = nonNil(members)
		out.Members = &members
	}
	return wire.JSON(c, http.StatusOK, out)
}

func badRequest(msg string) error { return wire.NewError(http.StatusBadRequest, msg) }

var accepted = AnalyticsMutationResponse{Accepted: true}

// recordEvent is AnalyticsController.recordEvent. Order matters on the wire:
// body decode, idempotency key, vault and identity, then the dimensions check.
func (m *Module) recordEvent(c *echo.Context) error {
	body, err := wire.Bind[analyticsEventRequest](c)
	if err != nil {
		return err
	}
	if body.IdempotencyKey != nil && !validIdempotencyKey(*body.IdempotencyKey) {
		return badRequest("invalid idempotencyKey")
	}
	access, err := m.vaults.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	dimensions := "{}"
	switch body.Name {
	case eventDashboardViewed, eventRangeChanged:
		if body.Range != nil {
			dimensions = `{"range":"` + string(*body.Range) + `"}`
		}
	default:
		if body.RecommendationID == nil || !validRecommendationID(*body.RecommendationID) {
			return badRequest("recommendationId required")
		}
		dimensions = `{"recommendation_id":"` + *body.RecommendationID + `"}`
	}
	ctx, user := requestContext(c), auth.UserID(c)
	if err := m.q.InsertAnalyticsEvent(ctx, db.InsertAnalyticsEventParams{
		VaultID: access.VaultID, ActorUserID: user, EventName: string(body.Name), Source: string(body.Source),
		Dimensions: []byte(dimensions), IdempotencyKey: body.IdempotencyKey,
	}); err != nil {
		return err
	}
	// The rollup bumps even when the idempotency key deduped the event.
	if err := m.q.BumpAnalyticsRollup(ctx, db.BumpAnalyticsRollupParams{
		VaultID: access.VaultID, ActorUserID: user, Metric: string(body.Name),
	}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, accepted)
}

// recordModelFeedback is AnalyticsController.recordModelFeedback.
func (m *Module) recordModelFeedback(c *echo.Context) error {
	body, err := wire.Bind[modelFeedbackRequest](c)
	if err != nil {
		return err
	}
	if !validModelIdentity(body.Provider) || !validModelIdentity(body.Model) {
		return badRequest("invalid provider or model")
	}
	if body.IdempotencyKey != nil && !validIdempotencyKey(*body.IdempotencyKey) {
		return badRequest("invalid idempotencyKey")
	}
	access, err := m.vaults.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	dimensions, err := json.Marshal(map[string]string{"provider": body.Provider, "model": body.Model})
	if err != nil {
		return err
	}
	name := "model_feedback_negative"
	if body.Rating == "positive" {
		name = "model_feedback_positive"
	}
	if err := m.q.InsertAnalyticsEvent(requestContext(c), db.InsertAnalyticsEventParams{
		VaultID: access.VaultID, ActorUserID: auth.UserID(c), EventName: name, Source: "server",
		Dimensions: dimensions, IdempotencyKey: body.IdempotencyKey,
	}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, accepted)
}

// updateRecommendation is AnalyticsController.updateRecommendation.
func (m *Module) updateRecommendation(c *echo.Context) error {
	body, err := wire.Bind[recommendationStateRequest](c)
	if err != nil {
		return err
	}
	if !validRecommendationID(body.RecommendationID) {
		return badRequest("invalid recommendationId")
	}
	access, err := m.vaults.Resolve(c, vaultaccess.Read)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var dismissed, snoozed pgtype.Timestamptz
	switch body.Disposition {
	case dispositionDismiss:
		dismissed = timestamptz(now)
	case dispositionSnooze7:
		snoozed = timestamptz(now.AddDate(0, 0, 7))
	case dispositionSnooze30:
		snoozed = timestamptz(now.AddDate(0, 0, 30))
	default:
		return errors.New("dashboard: unreachable disposition")
	}
	if err := m.q.UpsertRecommendationState(requestContext(c), db.UpsertRecommendationStateParams{
		VaultID: access.VaultID, UserID: auth.UserID(c), RecommendationID: body.RecommendationID,
		DismissedAt: dismissed, SnoozedUntil: snoozed,
	}); err != nil {
		return err
	}
	return wire.JSON(c, http.StatusOK, accepted)
}
