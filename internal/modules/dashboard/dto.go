package dashboard

import (
	"encoding/json"
	"lumina/internal/wire"
	"math"
	"strconv"
)

// Hand-written from LuminaVaultShared 5.32.0 (the version Swift's Package.resolved
// pins), not generated: every Double here must print the way Foundation prints
// it (swiftDouble), which oapi-codegen's models cannot do. Nil optionals are
// omitted, as Swift's synthesized encoding does.

// swiftDouble marshals like Foundation's JSONEncoder: Double.description with a
// trailing ".0" dropped. Below 0.0001 and above 2^53 that is exponential with at
// least two exponent digits ("1e-05"), where encoding/json would print 0.00001.
type swiftDouble float64

func (d swiftDouble) MarshalJSON() ([]byte, error) {
	f := float64(d)
	if abs := math.Abs(f); abs != 0 && (abs < 1e-4 || abs > 1<<53) {
		return strconv.AppendFloat(nil, f, 'e', -1, 64), nil
	}
	return strconv.AppendFloat(nil, f, 'f', -1, 64), nil
}

// TaskDTO is LuminaVaultShared TaskDTO.
type TaskDTO struct {
	ID             wire.UUID  `json:"id"`
	Kind           string     `json:"kind"`
	Label          string     `json:"label"`
	State          string     `json:"state"`
	StartedAt      *wire.Time `json:"startedAt,omitempty"`
	ElapsedSeconds *int       `json:"elapsedSeconds,omitempty"`
	Error          *string    `json:"error,omitempty"`
}

type TaskListResponse struct {
	Tasks []TaskDTO `json:"tasks"`
}

// InsightDTO is LuminaVaultShared InsightDTO.
type InsightDTO struct {
	ID              wire.UUID   `json:"id"`
	Headline        string      `json:"headline"`
	Summary         string      `json:"summary"`
	Section         string      `json:"section"`
	CreatedAt       wire.Time   `json:"createdAt"`
	SourceMemoryIDs []wire.UUID `json:"sourceMemoryIDs"`
	Dismissed       bool        `json:"dismissed"`
	PeriodStart     *wire.Time  `json:"periodStart,omitempty"`
	PeriodEnd       *wire.Time  `json:"periodEnd,omitempty"`
}

type InsightListResponse struct {
	Insights []InsightDTO `json:"insights"`
}

type DashboardStatsResponse struct {
	MemoriesToday int        `json:"memoriesToday"`
	MemoriesTotal int        `json:"memoriesTotal"`
	LastCompileAt *wire.Time `json:"lastCompileAt,omitempty"`
}

type DashboardProfileResponse struct {
	SkillsCount   int `json:"skillsCount"`
	JobsCount     int `json:"jobsCount"`
	SessionsCount int `json:"sessionsCount"`
	BadgesEarned  int `json:"badgesEarned"`
	PowerLevel    int `json:"powerLevel"`
	PowerXP       int `json:"powerXP"`
}

type PeriodStats struct {
	Done              int `json:"done"`
	Captures          int `json:"captures"`
	SkillRuns         int `json:"skillRuns"`
	Tokens            int `json:"tokens"`
	PreviousDone      int `json:"previousDone"`
	PreviousCaptures  int `json:"previousCaptures"`
	PreviousSkillRuns int `json:"previousSkillRuns"`
	PreviousTokens    int `json:"previousTokens"`
}

type SeriesPoint struct {
	At    wire.Time `json:"at"`
	Value int       `json:"value"`
}

type PeriodMix struct {
	Captures int `json:"captures"`
	Jobs     int `json:"jobs"`
	Skills   int `json:"skills"`
	Chats    int `json:"chats"`
}

// CronJobDTO is LuminaVaultShared DashboardCronJobDTO: one Hermes cron job in
// the home preview (see Module.UseCronPreview). nil fields are left out.
type CronJobDTO struct {
	ID       string  `json:"id"`
	Name     *string `json:"name,omitempty"`
	Schedule *string `json:"schedule,omitempty"`
	LastRun  *string `json:"lastRun,omitempty"`
	Status   *string `json:"status,omitempty"`
}

type GraphPreviewNodeDTO struct {
	ID       wire.UUID   `json:"id"`
	Label    string      `json:"label"`
	X        swiftDouble `json:"x"`
	Y        swiftDouble `json:"y"`
	Z        swiftDouble `json:"z"`
	Activity swiftDouble `json:"activity"`
	Kind     string      `json:"kind"`
}

// HomeSummaryResponse is LuminaVaultShared HomeSummaryResponse.
type HomeSummaryResponse struct {
	SkillsCount       int                   `json:"skillsCount"`
	JobsCount         int                   `json:"jobsCount"`
	RemindersCount    int                   `json:"remindersCount"`
	TodosCount        int                   `json:"todosCount"`
	ProjectsCount     int                   `json:"projectsCount"`
	InsightsCount     int                   `json:"insightsCount"`
	ActiveProfileName *string               `json:"activeProfileName,omitempty"`
	ActiveProfileSlug *string               `json:"activeProfileSlug,omitempty"`
	PrimaryProvider   string                `json:"primaryProvider"`
	PrimaryModel      string                `json:"primaryModel"`
	AgentOnline       bool                  `json:"agentOnline"`
	MemoriesToday     int                   `json:"memoriesToday"`
	MemoriesTotal     int                   `json:"memoriesTotal"`
	SessionsCount     int                   `json:"sessionsCount"`
	ActiveJobsCount   int                   `json:"activeJobsCount"`
	ActiveJobs        []TaskDTO             `json:"activeJobs"`
	Skills            []string              `json:"skills"`
	PowerLevel        int                   `json:"powerLevel"`
	PowerXP           int                   `json:"powerXP"`
	BadgesEarned      int                   `json:"badgesEarned"`
	StreakDays        int                   `json:"streakDays"`
	GraphPreview      []GraphPreviewNodeDTO `json:"graphPreview,omitempty"`
	CronJobsCount     int                   `json:"cronJobsCount"`
	CronJobs          []CronJobDTO          `json:"cronJobs"`
	ToolsCount        int                   `json:"toolsCount"`
	Tools             []string              `json:"tools"`
	Period            string                `json:"period"`
	PeriodStats       PeriodStats           `json:"periodStats"`
	PeriodSeries      []SeriesPoint         `json:"periodSeries"`
	PeriodMix         PeriodMix             `json:"periodMix"`
}

type ActivityFeedItemDTO struct {
	ID         wire.UUID `json:"id"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Subtitle   *string   `json:"subtitle,omitempty"`
	OccurredAt wire.Time `json:"occurredAt"`
}

type ActivityFeedResponse struct {
	Items []ActivityFeedItemDTO `json:"items"`
}

type VoiceChannelUsageDTO struct {
	Channel          string      `json:"channel"`
	Calls            int         `json:"calls"`
	FailedCalls      int         `json:"failedCalls"`
	AudioMinutes     swiftDouble `json:"audioMinutes"`
	RealCostCents    int         `json:"realCostCents"`
	ImputedCostCents int         `json:"imputedCostCents"`
}

type VoiceUsageSummaryDTO struct {
	Calls            int                    `json:"calls"`
	FailedCalls      int                    `json:"failedCalls"`
	AudioMinutes     swiftDouble            `json:"audioMinutes"`
	RealCostCents    int                    `json:"realCostCents"`
	ImputedCostCents int                    `json:"imputedCostCents"`
	ByChannel        []VoiceChannelUsageDTO `json:"byChannel"`
}

type UsageSummaryResponse struct {
	LLMTokensIn        int                   `json:"llmTokensIn"`
	LLMTokensOut       int                   `json:"llmTokensOut"`
	EmbeddingTokens    int                   `json:"embeddingTokens"`
	SessionsCount      int                   `json:"sessionsCount"`
	EstimatedCostCents int                   `json:"estimatedCostCents"`
	Voice              *VoiceUsageSummaryDTO `json:"voice,omitempty"`
	PeriodStart        wire.Time             `json:"periodStart"`
	PeriodEnd          wire.Time             `json:"periodEnd"`
}

type AnalyticsDailyPointDTO struct {
	Date                   wire.Time `json:"date"`
	Sessions               int       `json:"sessions"`
	AIRequests             int       `json:"aiRequests"`
	Tokens                 int       `json:"tokens"`
	Captures               int       `json:"captures"`
	Retrievals             int       `json:"retrievals"`
	EstimatedCostUsdMicros int64     `json:"estimatedCostUsdMicros"`
}

type AnalyticsSummaryDTO struct {
	Sessions               int   `json:"sessions"`
	AIRequests             int   `json:"aiRequests"`
	TokensIn               int   `json:"tokensIn"`
	TokensOut              int   `json:"tokensOut"`
	Captures               int   `json:"captures"`
	Retrievals             int   `json:"retrievals"`
	EstimatedCostUsdMicros int64 `json:"estimatedCostUsdMicros"`
}

type MemoryHealthComponentDTO struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Score  int    `json:"score"`
	Weight int    `json:"weight"`
}

type MemoryHealthDTO struct {
	Score               int                        `json:"score"`
	TotalMemories       int                        `json:"totalMemories"`
	StaleCount          int                        `json:"staleCount"`
	NeverRetrievedCount int                        `json:"neverRetrievedCount"`
	UnorganizedCount    int                        `json:"unorganizedCount"`
	PendingReviewCount  int                        `json:"pendingReviewCount"`
	Components          []MemoryHealthComponentDTO `json:"components"`
}

type AnalyticsRecommendationDTO struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Severity    string `json:"severity"`
	ActionTitle string `json:"actionTitle"`
	DeepLink    string `json:"deepLink"`
}

type AnalyticsOverviewResponse struct {
	Scope           string                       `json:"scope"`
	VaultID         wire.UUID                    `json:"vaultId"`
	Range           string                       `json:"range"`
	PeriodStart     wire.Time                    `json:"periodStart"`
	PeriodEnd       wire.Time                    `json:"periodEnd"`
	Summary         AnalyticsSummaryDTO          `json:"summary"`
	Daily           []AnalyticsDailyPointDTO     `json:"daily"`
	MemoryHealth    MemoryHealthDTO              `json:"memoryHealth"`
	Recommendations []AnalyticsRecommendationDTO `json:"recommendations"`
}

type ModelEffectivenessDTO struct {
	Provider               string       `json:"provider"`
	Model                  string       `json:"model"`
	Requests               int          `json:"requests"`
	SuccessRate            swiftDouble  `json:"successRate"`
	FallbackRate           swiftDouble  `json:"fallbackRate"`
	AverageLatencyMs       int          `json:"averageLatencyMs"`
	P95LatencyMs           int          `json:"p95LatencyMs"`
	Tokens                 int          `json:"tokens"`
	EstimatedCostUsdMicros int64        `json:"estimatedCostUsdMicros"`
	PositiveFeedback       int          `json:"positiveFeedback"`
	NegativeFeedback       int          `json:"negativeFeedback"`
	SatisfactionRate       *swiftDouble `json:"satisfactionRate,omitempty"`
}

type ModelEffectivenessResponse struct {
	Range  string                  `json:"range"`
	Models []ModelEffectivenessDTO `json:"models"`
}

type TeamMemberAnalyticsDTO struct {
	ID                     wire.UUID `json:"id"`
	DisplayName            string    `json:"displayName"`
	Captures               int       `json:"captures"`
	Retrievals             int       `json:"retrievals"`
	AIRequests             int       `json:"aiRequests"`
	Tokens                 int       `json:"tokens"`
	EstimatedCostUsdMicros int64     `json:"estimatedCostUsdMicros"`
}

type TeamAnalyticsResponse struct {
	VaultID wire.UUID                 `json:"vaultId"`
	Range   string                    `json:"range"`
	Summary AnalyticsSummaryDTO       `json:"summary"`
	Members *[]TeamMemberAnalyticsDTO `json:"members,omitempty"`
}

type AnalyticsMutationResponse struct {
	Accepted bool `json:"accepted"`
}

type RetrievalHealthResponse struct {
	HitRate         *swiftDouble `json:"hitRate,omitempty"`
	MeanTopDistance *swiftDouble `json:"meanTopDistance,omitempty"`
	EventsCount     int          `json:"eventsCount"`
	LeakCount       int          `json:"leakCount"`
	Trend           string       `json:"trend"`
}

// Request enums. A value outside the Swift enum is Foundation's dataCorrupted
// error, naming the Swift type.

func decodeEnum(b []byte, swiftType string, allowed ...string) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", err
	}
	for _, a := range allowed {
		if s == a {
			return s, nil
		}
	}
	return "", &wire.CorruptedError{Message: "Cannot initialize " + swiftType + " from invalid String value " + s}
}

type eventName string

func (e *eventName) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "AnalyticsClientEventName", eventDashboardViewed, eventRangeChanged,
		"analytics_recommendation_opened", "analytics_recommendation_dismissed", "analytics_recommendation_snoozed")
	*e = eventName(s)
	return err
}

const (
	eventDashboardViewed = "analytics_dashboard_viewed"
	eventRangeChanged    = "analytics_range_changed"
)

type eventSource string

func (e *eventSource) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "AnalyticsEventSource", "ios", "web")
	*e = eventSource(s)
	return err
}

type analyticsRange string

func (r *analyticsRange) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "AnalyticsRange", "7d", "30d", "90d")
	*r = analyticsRange(s)
	return err
}

type feedbackRating string

func (r *feedbackRating) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "ModelFeedbackRating", "positive", "negative")
	*r = feedbackRating(s)
	return err
}

type disposition string

const (
	dispositionDismiss  = "dismiss"
	dispositionSnooze7  = "snooze7"
	dispositionSnooze30 = "snooze30"
)

func (d *disposition) UnmarshalJSON(b []byte) error {
	s, err := decodeEnum(b, "AnalyticsRecommendationDisposition", dispositionDismiss, dispositionSnooze7, dispositionSnooze30)
	*d = disposition(s)
	return err
}

// analyticsEventRequest is AnalyticsEventRequest. Field order is decode order.
type analyticsEventRequest struct {
	Name             eventName       `json:"name"`
	Source           eventSource     `json:"source"`
	Range            *analyticsRange `json:"range"`
	RecommendationID *string         `json:"recommendationId"`
	IdempotencyKey   *string         `json:"idempotencyKey"`
}

type modelFeedbackRequest struct {
	Provider       string         `json:"provider"`
	Model          string         `json:"model"`
	Rating         feedbackRating `json:"rating"`
	IdempotencyKey *string        `json:"idempotencyKey"`
}

type recommendationStateRequest struct {
	RecommendationID string      `json:"recommendationId"`
	Disposition      disposition `json:"disposition"`
}
