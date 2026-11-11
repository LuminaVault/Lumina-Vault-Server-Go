package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/swiftstr"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Outcome statuses, as HealthCorrelationRunResponse spells them.
const (
	StatusSaved                = "saved"
	StatusInsufficientHistory  = "skipped_insufficient_history"
	StatusAlreadyRan           = "skipped_already_ran"
	StatusNoRecentEvents       = "skipped_no_recent_events"
	StatusNoSynthesis          = "skipped_no_synthesis"
	defaultMinHealthHistoryDay = 30
	defaultLookbackDays        = 7
	defaultRunIntervalDays     = 7
)

// Outcome is HealthCorrelationOutcome.
type Outcome struct {
	Status   string
	MemoryID *uuid.UUID
}

// Correlation is HealthCorrelationService with HealthCorrelationJob: a weekly LLM
// pass over a tenant's last 7 days of health events and memories, saved as a memory
// tagged correlation and weekly. A NO_CORRELATION reply is saved like any other
// text; only an empty one is a skip, as in Swift.
type Correlation struct {
	Pool         *pgxpool.Pool
	Transport    llm.ChatTransport
	Embeddings   embedding.Service
	Memories     *memorystore.Repository
	DefaultModel string
	// Zero means Swift's defaults: 30, 7 and 7.
	MinHealthHistoryDays, LookbackDays, RunIntervalDays int
}

func orDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// CorrelationSweep is HealthCorrelationSweepSummary.
type CorrelationSweep struct {
	UsersScanned               int      `json:"usersScanned"`
	MemoriesCreated            int      `json:"memoriesCreated"`
	SkippedInsufficientHistory int      `json:"skippedInsufficientHistory"`
	SkippedAlreadyRan          int      `json:"skippedAlreadyRan"`
	SkippedNoEvents            int      `json:"skippedNoEvents"`
	SkippedNoSynthesis         int      `json:"skippedNoSynthesis"`
	Failures                   []string `json:"failures"`
}

// RunForAllUsers is HealthCorrelationJob.runForAllUsers.
func (s *Correlation) RunForAllUsers(ctx context.Context, now time.Time) (CorrelationSweep, error) {
	users, err := listUsers(ctx, s.Pool)
	if err != nil {
		return CorrelationSweep{}, err
	}
	sum := CorrelationSweep{UsersScanned: len(users), Failures: []string{}}
	for _, u := range users {
		out, err := s.Correlate(ctx, u.id, now)
		if err != nil {
			sum.Failures = append(sum.Failures, fmt.Sprintf("%s: %v", u.username, err))
			slog.WarnContext(ctx, "health-correlation failed", "username", u.username, "error", err)
			continue
		}
		switch out.Status {
		case StatusSaved:
			sum.MemoriesCreated++
		case StatusInsufficientHistory:
			sum.SkippedInsufficientHistory++
		case StatusAlreadyRan:
			sum.SkippedAlreadyRan++
		case StatusNoRecentEvents:
			sum.SkippedNoEvents++
		case StatusNoSynthesis:
			sum.SkippedNoSynthesis++
		}
	}
	slog.InfoContext(ctx, "health-correlation sweep", "scanned", len(users), "created", sum.MemoriesCreated,
		"failures", len(sum.Failures))
	return sum, nil
}

// HealthEvent is the subset of health_events the prompt reads.
type HealthEvent struct {
	EventType    string
	ValueNumeric *float64
	ValueText    *string
	Unit         *string
	RecordedAt   time.Time
}

// PromptMemory is the subset of memories the prompt reads.
type PromptMemory struct {
	Content   string
	CreatedAt *time.Time
}

// Correlate is HealthCorrelationService.correlate(user:now:).
func (s *Correlation) Correlate(ctx context.Context, tenant uuid.UUID, now time.Time) (Outcome, error) {
	// 1) Idempotency: a correlation memory already written this week.
	var recent bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT id FROM memories
		WHERE tenant_id = $1 AND created_at >= $2 AND 'correlation' = ANY(tags)
		LIMIT 1)`, tenant, now.Add(-days(orDefault(s.RunIntervalDays, defaultRunIntervalDays)))).Scan(&recent)
	if err != nil {
		return Outcome{}, err
	}
	if recent {
		return Outcome{Status: StatusAlreadyRan}, nil
	}
	// 2) Less than MinHealthHistoryDays of HealthKit data.
	var oldest time.Time
	err = s.Pool.QueryRow(ctx, `SELECT recorded_at FROM health_events WHERE tenant_id = $1 ORDER BY recorded_at ASC LIMIT 1`, tenant).Scan(&oldest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Outcome{Status: StatusInsufficientHistory}, nil
	}
	if err != nil {
		return Outcome{}, err
	}
	if oldest.After(now.Add(-days(orDefault(s.MinHealthHistoryDays, defaultMinHealthHistoryDay)))) {
		return Outcome{Status: StatusInsufficientHistory}, nil
	}
	// 3) The rolling window.
	windowStart := now.Add(-days(orDefault(s.LookbackDays, defaultLookbackDays)))
	events, err := s.events(ctx, tenant, windowStart)
	if err != nil {
		return Outcome{}, err
	}
	if len(events) == 0 {
		return Outcome{Status: StatusNoRecentEvents}, nil
	}
	memories, err := s.memories(ctx, tenant, windowStart)
	if err != nil {
		return Outcome{}, err
	}
	// 4) Ask Hermes.
	synthesis, err := s.callHermes(ctx, BuildCorrelationPrompt(events, memories, now), ids.Upper(tenant))
	if err != nil {
		return Outcome{}, err
	}
	trimmed := swiftstr.TrimWhitespacesAndNewlines(synthesis)
	if trimmed == "" {
		return Outcome{Status: StatusNoSynthesis}, nil
	}
	// 5) A tagged memory, kept without a vector if the embedder is down: the
	// synthesis was paid for.
	vec, err := embedding.EmbedForWrite(ctx, s.Embeddings, trimmed, tenant, "health.correlation")
	if err != nil {
		return Outcome{}, err
	}
	m, err := s.Memories.Create(ctx, memorystore.CreateParams{
		Tenant: tenant, Content: trimmed, Embedding: vec, Tags: []string{"correlation", "weekly"},
	})
	if err != nil {
		return Outcome{}, err
	}
	slog.InfoContext(ctx, "health correlation saved", "tenant", tenant, "memory", m.ID,
		"eventCount", len(events), "memoryCount", len(memories))
	id := m.ID
	return Outcome{Status: StatusSaved, MemoryID: &id}, nil
}

func (s *Correlation) events(ctx context.Context, tenant uuid.UUID, since time.Time) ([]HealthEvent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT event_type, value_numeric, value_text, unit, recorded_at
		FROM health_events WHERE tenant_id = $1 AND recorded_at >= $2 ORDER BY recorded_at ASC`, tenant, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (HealthEvent, error) {
		var e HealthEvent
		err := r.Scan(&e.EventType, &e.ValueNumeric, &e.ValueText, &e.Unit, &e.RecordedAt)
		return e, err
	})
}

func (s *Correlation) memories(ctx context.Context, tenant uuid.UUID, since time.Time) ([]PromptMemory, error) {
	rows, err := s.Pool.Query(ctx, `SELECT content, created_at
		FROM memories WHERE tenant_id = $1 AND created_at >= $2 ORDER BY created_at ASC`, tenant, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PromptMemory, error) {
		var m PromptMemory
		err := r.Scan(&m.Content, &m.CreatedAt)
		return m, err
	})
}

const correlationSystemPrompt = "You are Hermes, a private second brain. Your job is to find concrete\n" +
	"correlations between the user's last 7 days of health metrics and\n" +
	"their captured notes / thoughts.\n" +
	"\n" +
	"Output rules:\n" +
	"* Single tight paragraph followed by a `- ` bullet list of 1-5\n" +
	"  concrete correlations. No headers, no preamble, no apologies.\n" +
	"* Each bullet pairs a health observation with a note observation\n" +
	"  using the format: `When X happened in the metrics, Y appeared in\n" +
	"  the notes.`\n" +
	"* Only assert a correlation if both sides actually appear in the\n" +
	"  input data. Do NOT invent metrics or notes.\n" +
	"* If there is no plausible correlation, respond with EXACTLY the\n" +
	"  single line `NO_CORRELATION` — the caller treats it as a skip."

type correlationBody struct {
	Model       string            `json:"model"`
	Messages    []llm.ChatMessage `json:"messages"`
	Temperature float64           `json:"temperature"`
	Stream      bool              `json:"stream"`
}

// callHermes sends one non-streaming completion and returns the first choice's
// content, "" when it's null or there is no choice. A body that doesn't decode as
// {choices:[{message:{role,content?}}]} is an error, as Swift's decode throws.
func (s *Correlation) callHermes(ctx context.Context, prompt, sessionKey string) (string, error) {
	payload, err := json.Marshal(correlationBody{
		Model: s.DefaultModel,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: correlationSystemPrompt},
			{Role: "user", Content: prompt},
		},
		Temperature: 0.3,
	})
	if err != nil {
		return "", err
	}
	raw, err := s.Transport.ChatCompletions(ctx, payload, sessionKey, "")
	if err != nil {
		return "", err
	}
	return decodeFirstContent(raw.Data)
}

var errBadCompletion = errors.New("health correlation: completion body did not decode")

func decodeFirstContent(data []byte) (string, error) {
	var body struct {
		Choices *[]struct {
			Message *struct {
				Role    *string `json:"role"`
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Choices == nil {
		return "", errBadCompletion
	}
	for _, c := range *body.Choices {
		if c.Message == nil || c.Message.Role == nil {
			return "", errBadCompletion
		}
	}
	if len(*body.Choices) == 0 || (*body.Choices)[0].Message.Content == nil {
		return "", nil
	}
	return *(*body.Choices)[0].Message.Content, nil
}

// iso is ISO8601DateFormatter with .withInternetDateTime: UTC, whole seconds.
func iso(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// BuildCorrelationPrompt is buildUserPrompt(events:memories:now:). Each memory line
// is its first 280 Characters with newlines flattened.
func BuildCorrelationPrompt(events []HealthEvent, memories []PromptMemory, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Window ending %s (last 7 days).\n\n", iso(now))
	fmt.Fprintf(&b, "## Health events (%d)\n", len(events))
	for _, ev := range events {
		value := "?"
		switch {
		case ev.ValueNumeric != nil:
			value = swiftDouble(*ev.ValueNumeric)
			if ev.Unit != nil {
				value += " " + *ev.Unit
			}
		case ev.ValueText != nil:
			value = *ev.ValueText
		}
		fmt.Fprintf(&b, "- [%s] %s: %s\n", iso(ev.RecordedAt), ev.EventType, value)
	}
	fmt.Fprintf(&b, "\n## Memories (%d)\n", len(memories))
	for _, m := range memories {
		stamp := "?"
		if m.CreatedAt != nil {
			stamp = iso(*m.CreatedAt)
		}
		fmt.Fprintf(&b, "- [%s] %s\n", stamp, strings.ReplaceAll(swiftstr.Prefix(m.Content, 280), "\n", " "))
	}
	b.WriteString("\nFind correlations. Respond per system instructions.")
	return b.String()
}
