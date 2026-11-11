package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/jobs"
	"lumina/internal/llm"
	"lumina/internal/swiftstr"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SynthesisWorker is SynthesisWorker (HER-37 Slice D): hourly, in UTC, it
// writes the this_week synthesis (Sunday 02:00), up to MaxPatterns patterns
// (03:00) and contradictions read from the knowledge graph (04:00) as insights
// rows, for every user. Off unless SYNTHESIS_WORKER_ENABLED=true.
type SynthesisWorker struct {
	Pool         *pgxpool.Pool
	Transport    llm.ChatTransport
	DefaultModel string
	Logger       *slog.Logger
	// MaxPatterns is maxPatternsPerRun (3); MemorySample is memorySampleSize (30).
	MaxPatterns  int
	MemorySample int
}

// NewSynthesisWorker fills Swift's defaults.
func NewSynthesisWorker(pool *pgxpool.Pool, transport llm.ChatTransport, model string, logger *slog.Logger) *SynthesisWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &SynthesisWorker{Pool: pool, Transport: transport, DefaultModel: model, Logger: logger, MaxPatterns: 3, MemorySample: 30}
}

// Job is Swift's run loop: tick at boot, then every hour.
func (w *SynthesisWorker) Job() jobs.Job {
	return jobs.Job{Name: "SynthesisWorker", Schedule: jobs.Every(time.Hour, 0), Run: func(ctx context.Context) error {
		if _, err := w.Tick(ctx, time.Now()); err != nil {
			w.Logger.Warn(fmt.Sprintf("synthesis.worker tick error: %v", err))
			return err
		}
		return nil
	}}
}

// SynthesisCounts is what one tick inserted.
type SynthesisCounts struct{ Weekly, Patterns, Contradictions int }

// Tick runs whichever pass the UTC hour calls for.
func (w *SynthesisWorker) Tick(ctx context.Context, now time.Time) (SynthesisCounts, error) {
	var out SynthesisCounts
	var err error
	hour, weekday := HourComponent(now), WeekdayComponent(now)
	if hour == 2 && weekday == 1 {
		if out.Weekly, err = w.forAllUsers(ctx, "synthesis.weekly", func(tenant uuid.UUID) (int, error) {
			ok, err := w.RunWeeklyJob(ctx, tenant, ids.Upper(tenant), WeeklyWindow(now))
			if ok {
				return 1, err
			}
			return 0, err
		}); err != nil {
			return out, err
		}
	}
	if hour == 3 {
		if out.Patterns, err = w.forAllUsers(ctx, "synthesis.patterns", func(tenant uuid.UUID) (int, error) {
			return w.RunPatternJob(ctx, tenant, ids.Upper(tenant), now)
		}); err != nil {
			return out, err
		}
	}
	if hour == 4 {
		if out.Contradictions, err = w.forAllUsers(ctx, "synthesis.contradictions", func(tenant uuid.UUID) (int, error) {
			return w.RunContradictionJob(ctx, tenant, now)
		}); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (w *SynthesisWorker) forAllUsers(ctx context.Context, label string, run func(uuid.UUID) (int, error)) (int, error) {
	rows, err := w.Pool.Query(ctx, `SELECT id FROM users`)
	if err != nil {
		return 0, err
	}
	users, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	total := 0
	for _, tenant := range users {
		n, err := run(tenant)
		if err != nil {
			w.Logger.Warn(fmt.Sprintf("%s tenant=%s error: %v", label, ids.Upper(tenant), err))
			continue
		}
		total += n
	}
	return total, nil
}

// SynthesisPeriod is SynthesisWorker.Period.
type SynthesisPeriod struct{ Start, End time.Time }

// WeeklyWindow is the 7 days ending at the UTC start of now's day.
func WeeklyWindow(now time.Time) SynthesisPeriod {
	u := now.UTC()
	end := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return SynthesisPeriod{Start: end.AddDate(0, 0, -7), End: end}
}

// HourComponent is the UTC hour.
func HourComponent(t time.Time) int { return t.UTC().Hour() }

// WeekdayComponent is the Gregorian weekday in UTC, Sunday = 1.
func WeekdayComponent(t time.Time) int { return int(t.UTC().Weekday()) + 1 }

// InsightInput is one insights row to write.
type InsightInput struct {
	Tenant                 uuid.UUID
	Section                string
	Headline, Summary      string
	Sources                []uuid.UUID
	PeriodStart, PeriodEnd *time.Time
	KnowledgeEdge          *uuid.UUID
}

// SaveInsight is Insight.save: created_at is the app clock, as Fluent's
// @Timestamp(on: .create) sets it.
func SaveInsight(ctx context.Context, pool *pgxpool.Pool, in InsightInput) (uuid.UUID, error) {
	id := uuid.New()
	sources := in.Sources
	if sources == nil {
		sources = []uuid.UUID{}
	}
	_, err := pool.Exec(ctx, `
INSERT INTO insights (id, tenant_id, section, headline, summary, source_memory_ids, period_start, period_end, created_at, knowledge_edge_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		id, in.Tenant, in.Section, in.Headline, in.Summary, sources, in.PeriodStart, in.PeriodEnd, time.Now(), in.KnowledgeEdge)
	return id, err
}

type synthMemory struct {
	id      uuid.UUID
	content string
}

func (w *SynthesisWorker) loadMemories(ctx context.Context, tenant uuid.UUID, since time.Time) ([]synthMemory, error) {
	rows, err := w.Pool.Query(ctx, `
SELECT id, content FROM memories
WHERE tenant_id = $1 AND created_at >= $2
ORDER BY created_at DESC
LIMIT $3`, tenant, since, w.MemorySample)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (synthMemory, error) {
		var m synthMemory
		return m, r.Scan(&m.id, &m.content)
	})
}

func memoryIDs(rows []synthMemory) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.id
	}
	return out
}

// RunWeeklyJob writes the this_week row for window unless one exists. It is
// false when the row existed, there were no memories, or the model gave nothing.
func (w *SynthesisWorker) RunWeeklyJob(ctx context.Context, tenant uuid.UUID, sessionKey string, window SynthesisPeriod) (bool, error) {
	var exists bool
	if err := w.Pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM insights WHERE tenant_id = $1 AND section = 'this_week' AND period_start = $2 AND period_end = $3)`,
		tenant, window.Start, window.End).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	rows, err := w.loadMemories(ctx, tenant, window.Start)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	content, ok := w.call(ctx, sessionKey, weeklyPrompt(rows, window))
	if !ok {
		return false, nil
	}
	synth, ok := ParseSynthesis(content)
	if !ok {
		return false, nil
	}
	_, err = SaveInsight(ctx, w.Pool, InsightInput{
		Tenant: tenant, Section: "this_week", Headline: synth.Headline, Summary: synth.Summary,
		Sources: memoryIDs(rows), PeriodStart: &window.Start, PeriodEnd: &window.End,
	})
	return err == nil, err
}

func (w *SynthesisWorker) recentlyWritten(ctx context.Context, tenant uuid.UUID, section string, now time.Time) (bool, error) {
	var createdAt time.Time
	err := w.Pool.QueryRow(ctx, `SELECT created_at FROM insights WHERE tenant_id = $1 AND section = $2 ORDER BY created_at DESC LIMIT 1`, tenant, section).Scan(&createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return createdAt.After(now.Add(-24 * time.Hour)), nil
}

// RunPatternJob writes up to MaxPatterns pattern rows, at most once a day.
func (w *SynthesisWorker) RunPatternJob(ctx context.Context, tenant uuid.UUID, sessionKey string, now time.Time) (int, error) {
	if recent, err := w.recentlyWritten(ctx, tenant, "patterns", now); err != nil || recent {
		return 0, err
	}
	rows, err := w.loadMemories(ctx, tenant, now.Add(-7*24*time.Hour))
	if err != nil || len(rows) < 3 {
		return 0, err
	}
	content, ok := w.call(ctx, sessionKey, patternsPrompt(rows, w.MaxPatterns))
	if !ok {
		return 0, nil
	}
	patterns, ok := ParsePatterns(content)
	if !ok || len(patterns) == 0 {
		return 0, nil
	}
	inserted := 0
	for _, p := range patterns[:min(len(patterns), w.MaxPatterns)] {
		if _, err := SaveInsight(ctx, w.Pool, InsightInput{
			Tenant: tenant, Section: "patterns", Headline: p.Headline, Summary: p.Summary, Sources: memoryIDs(rows),
		}); err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}

// RunContradictionJob turns up to MaxPatterns unreported contradicts edges of the
// knowledge graph into rows, at most once a day. It makes no model call.
func (w *SynthesisWorker) RunContradictionJob(ctx context.Context, tenant uuid.UUID, now time.Time) (int, error) {
	if recent, err := w.recentlyWritten(ctx, tenant, "contradictions", now); err != nil || recent {
		return 0, err
	}
	rows, err := w.Pool.Query(ctx, `
SELECT e.id AS edge_id,
       left_node.label AS left_label,
       right_node.label AS right_label,
       COALESCE(e.rationale, 'Evidence-backed claims use opposing positions.') AS rationale,
       ARRAY_AGG(DISTINCT evidence.memory_id) AS memory_ids
FROM knowledge_edges e
JOIN knowledge_nodes left_node ON left_node.id = e.from_node_id
JOIN knowledge_nodes right_node ON right_node.id = e.to_node_id
JOIN knowledge_evidence evidence ON evidence.edge_id = e.id
WHERE e.tenant_id = $1
  AND e.predicate = 'contradicts'
  AND e.state IN ('suggested', 'confirmed')
  AND NOT EXISTS (SELECT 1 FROM insights i WHERE i.knowledge_edge_id = e.id)
GROUP BY e.id, left_node.label, right_node.label, e.rationale, e.confidence
ORDER BY e.confidence DESC, e.updated_at DESC
LIMIT $2`, tenant, w.MaxPatterns)
	if err != nil {
		return 0, err
	}
	type contradiction struct {
		edge                   uuid.UUID
		left, right, rationale string
		memories               []uuid.UUID
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (contradiction, error) {
		var c contradiction
		return c, r.Scan(&c.edge, &c.left, &c.right, &c.rationale, &c.memories)
	})
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, c := range found {
		edge := c.edge
		if _, err := SaveInsight(ctx, w.Pool, InsightInput{
			Tenant: tenant, Section: "contradictions",
			Headline: swiftstr.CharacterPrefix(c.left+" ↔ "+c.right, 60), Summary: c.rationale,
			Sources: c.memories, KnowledgeEdge: &edge,
		}); err != nil {
			return inserted, err
		}
		inserted++
	}
	return inserted, nil
}

// call sends one json_object request and returns the assistant content. Any
// failure is nil, as Swift's try? is.
func (w *SynthesisWorker) call(ctx context.Context, sessionKey string, messages []llm.ChatMessage) ([]byte, bool) {
	type responseFormat struct {
		Type string `json:"type"`
	}
	// Swift's temperature is kept; the Anthropic adapter drops it.
	payload, err := json.Marshal(struct {
		Model          string            `json:"model"`
		Messages       []llm.ChatMessage `json:"messages"`
		Temperature    float64           `json:"temperature"`
		ResponseFormat responseFormat    `json:"response_format"`
		Stream         bool              `json:"stream"`
	}{w.DefaultModel, messages, 0.5, responseFormat{"json_object"}, false})
	if err != nil {
		return nil, false
	}
	response, err := w.Transport.ChatCompletions(ctx, payload, sessionKey, "")
	if err != nil {
		return nil, false
	}
	return response.Data, true
}

// Synthesised is one headline and summary.
type Synthesised struct{ Headline, Summary string }

func swiftDate(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 +0000") }

func renderMemoryList(rows []synthMemory) string {
	lines := make([]string, len(rows))
	for i, m := range rows {
		lines[i] = fmt.Sprintf("[%d] %s", i+1, swiftstr.TrimWhitespacesAndNewlines(swiftstr.CharacterPrefix(m.content, 180)))
	}
	return strings.Join(lines, "\n")
}

// weeklyPrompt is SynthesisWorker.weeklyPrompt.
func weeklyPrompt(rows []synthMemory, window SynthesisPeriod) []llm.ChatMessage {
	return []llm.ChatMessage{
		{Role: "system", Content: "You are Lumina synthesising the user's week. You are concise, grounded, and never invent detail."},
		{Role: "user", Content: "Synthesise the user's week (" + swiftDate(window.Start) + "…" + swiftDate(window.End) + `) from these memories. Reply ONLY with JSON: {"headline": "...", "summary": "..."}
- headline ≤ 60 chars, in the user's voice.
- summary ≤ 600 chars, 1-3 sentences. Cite memories by bracket number.
- Do NOT invent facts.
Memories:
` + renderMemoryList(rows)},
	}
}

// patternsPrompt is SynthesisWorker.patternsPrompt.
func patternsPrompt(rows []synthMemory, maxPatterns int) []llm.ChatMessage {
	return []llm.ChatMessage{
		{Role: "system", Content: "You spot non-obvious patterns. You never invent or exaggerate; absence of a pattern is a valid answer."},
		{Role: "user", Content: fmt.Sprintf("Spot up to %d surprising cross-cutting patterns.", maxPatterns) + ` Each must connect ≥2 memories from different topics. Reply ONLY with JSON: {"patterns": [{"headline": "...", "summary": "..."}, ...]}
- Each headline ≤ 60 chars.
- Each summary ≤ 400 chars.
- If no real pattern exists, return {"patterns": []}.
Memories:
` + renderMemoryList(rows)},
	}
}

// envelopeContent is Swift's Envelope decode: choices[0].message.content as a
// string, which must be present.
func envelopeContent(response []byte) (string, bool) {
	var envelope struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(response, &envelope) != nil || len(envelope.Choices) == 0 || envelope.Choices[0].Message.Content == nil {
		return "", false
	}
	return *envelope.Choices[0].Message.Content, true
}

type synthItem struct {
	Headline *string `json:"headline"`
	Summary  *string `json:"summary"`
}

func (s synthItem) clean() (Synthesised, bool) {
	if s.Headline == nil || s.Summary == nil {
		return Synthesised{}, false
	}
	h, sum := swiftstr.TrimWhitespacesAndNewlines(*s.Headline), swiftstr.TrimWhitespacesAndNewlines(*s.Summary)
	return Synthesised{h, sum}, h != "" && sum != ""
}

// ParseSynthesis reads {"headline","summary"}; both must be non-empty.
func ParseSynthesis(response []byte) (Synthesised, bool) {
	content, ok := envelopeContent(response)
	if !ok {
		return Synthesised{}, false
	}
	var item synthItem
	if json.Unmarshal([]byte(content), &item) != nil {
		return Synthesised{}, false
	}
	return item.clean()
}

func parseList(response []byte, key string) ([]Synthesised, bool) {
	content, ok := envelopeContent(response)
	if !ok {
		return nil, false
	}
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &body) != nil || body[key] == nil {
		return nil, false
	}
	var items []synthItem
	if json.Unmarshal(body[key], &items) != nil {
		return nil, false
	}
	out := []Synthesised{}
	for _, item := range items {
		// Swift's Decodable fails the whole list on an entry missing a field.
		if item.Headline == nil || item.Summary == nil {
			return nil, false
		}
		if s, ok := item.clean(); ok {
			out = append(out, s)
		}
	}
	return out, true
}

// ParsePatterns reads {"patterns": [...]}, dropping empty entries.
func ParsePatterns(response []byte) ([]Synthesised, bool) { return parseList(response, "patterns") }

// ParseContradictions reads {"contradictions": [...]}, dropping empty entries.
// Swift's worker no longer calls it (contradictions come from the graph); the
// parser is kept with its test.
func ParseContradictions(response []byte) ([]Synthesised, bool) {
	return parseList(response, "contradictions")
}
