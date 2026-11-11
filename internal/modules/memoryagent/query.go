package memoryagent

import (
	"context"
	"encoding/json"
	"log/slog"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/wire"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StreamEvent is QueryStreamEvent on the wire: "type" discriminates, and an errorDetail
// carries code and cta beside the string payload.
type StreamEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"payload,omitempty"`
	Code    string `json:"code,omitempty"`
	// CTA is omitzero: an errorDetail always carries "cta" ([] when it has no
	// actions, from llm.StreamErrorFor), and a plain error (nil) never does.
	CTA []string `json:"cta,omitzero"`
}

// fallbackDTO is ProviderFallbackNoticeDTO.
type fallbackDTO struct {
	OriginalProvider llm.ProviderID `json:"originalProvider"`
	OriginalModel    string         `json:"originalModel"`
	FallbackProvider llm.ProviderID `json:"fallbackProvider"`
	FallbackModel    string         `json:"fallbackModel"`
	ReasonCode       string         `json:"reasonCode"`
	UserMessage      string         `json:"userMessage"`
}

func fallbackEvent(n llm.FailoverNotice) StreamEvent {
	from, _ := n.OriginalProvider.ToShared()
	to, _ := n.FallbackProvider.ToShared()
	return StreamEvent{Type: "fallback", Payload: fallbackDTO{from, n.OriginalModel, to, n.FallbackModel, n.ReasonCode, n.UserMessage}}
}

func errorEvent(err error) StreamEvent {
	e := llm.StreamErrorFor(err)
	return StreamEvent{Type: "error", Payload: e.Message, Code: e.Code, CTA: e.CTA}
}

func hitDTO(h memorystore.SearchResult) HitDTO {
	dto := HitDTO{ID: wire.UUID{UUID: h.ID}, Content: h.Content, Distance: wire.Float32(h.Distance), Snippet: h.Snippet}
	if h.CreatedAt != nil {
		dto.CreatedAt = &wire.Time{Time: *h.CreatedAt}
	}
	if c := h.Citation; c != nil {
		dto.Citation = &CitationDTO{
			ChunkID: c.ChunkID, DocumentID: c.DocumentID, Path: c.Path,
			HeadingPath: c.HeadingPath, StartLine: c.StartLine, EndLine: c.EndLine,
		}
		if dto.Citation.HeadingPath == nil {
			dto.Citation.HeadingPath = []string{}
		}
	}
	return dto
}

// BuildQueryPrompt is QueryController.buildPrompt: the system prompt that grounds the
// streaming reply in retrieved memories.
func BuildQueryPrompt(query string, hits []memorystore.SearchResult, identityGuard bool) []llm.ChatMessage {
	context := "(no relevant memories were found)"
	if len(hits) > 0 {
		lines := make([]string, len(hits))
		for i, hit := range hits {
			provenance := hit.Source
			if hit.Provider != nil && hit.Model != nil {
				if identityGuard {
					provenance = "prior assistant output"
				} else {
					provenance = "prior model output from " + *hit.Provider + "/" + *hit.Model
				}
			}
			locator := ""
			if hit.Citation != nil {
				locator = " [" + hit.Citation.DisplayTrail() + "]"
			}
			lines[i] = "[" + strconv.Itoa(i+1) + "] [" + provenance + "]" + locator + " " + hit.Content
		}
		context = strings.Join(lines, "\n\n")
	}
	guard := ""
	if identityGuard {
		guard = "\n" + llm.ModelIdentityGuard
	}
	system := `You are Lumina, the user's second-brain assistant. Use the
retrieved memories below to ground your answer. Cite by their
bracket number when relevant (e.g. "[1]"). Keep the reply concise
and conversational. If the memories do not cover the question,
say so plainly rather than inventing detail.

Some memories carry a source locator in a second bracket — the file,
heading trail, and line range they were read from. Never invent a
locator, and never cite one that is not listed below.

Retrieved memories:
` + context + `

Prior model outputs are drafts, not authoritative user facts. Prefer
direct user memories when sources disagree and state uncertainty.
` + guard
	return []llm.ChatMessage{{Role: "system", Content: system}, {Role: "user", Content: query}}
}

// routeOutcome is QueryRouteOutcomeBox.
type routeOutcome struct {
	mu    sync.Mutex
	value *llm.ModelProvenance
}

func (b *routeOutcome) set(v llm.ModelProvenance) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.value = &v
}

func (b *routeOutcome) get() *llm.ModelProvenance {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value
}

// streamQuery is the producer half of queryStream. It emits sources, tokens (with any
// failover notices), the output-index row, follow-ups, then done. Nothing here can
// change the status: failures are events.
func (m *Module) streamQuery(ctx context.Context, tenant uuid.UUID, sessionKey, sessionID string, user *llm.User, billing uuid.UUID, req llm.ChatRequest, hits []HitDTO, emit func(StreamEvent) bool) {
	for _, dto := range hits {
		if !emit(StreamEvent{Type: "source", Payload: dto}) {
			return
		}
	}
	route := &routeOutcome{}
	var emitMu sync.Mutex
	locked := func(e StreamEvent) bool {
		emitMu.Lock()
		defer emitMu.Unlock()
		return emit(e)
	}
	vault := tenant
	streamCtx := llm.WithValues(ctx, func(v *llm.Values) {
		v.AnalyticsVaultID = &vault
		v.BillingTenantID = &billing
		v.RouteOutcomeSink = route.set
		v.FailoverSink = func(n llm.FailoverNotice) { locked(fallbackEvent(n)) }
		v.CurrentUser = user
	})
	var buffer strings.Builder
	tokens := 0
	for chunk, err := range m.opts.Stream.ChatStream(streamCtx, sessionKey, sessionID, req) {
		if err != nil {
			slog.ErrorContext(ctx, "query stream upstream failed", "error", err)
			locked(errorEvent(err))
			return
		}
		if ctx.Err() != nil {
			break
		}
		if chunk.Delta != "" {
			tokens++
			buffer.WriteString(chunk.Delta)
			if !locked(StreamEvent{Type: "token", Payload: chunk.Delta}) {
				return
			}
		}
	}
	answer := buffer.String()
	if llm.IsEmptyCompletion(answer, tokens) {
		slog.WarnContext(ctx, "query stream completed without assistant content")
		locked(StreamEvent{Type: "error", Payload: llm.EmptyCompletionMessage})
		return
	}
	output := memorystore.EnqueueOutputParams{Tenant: tenant, Source: memorystore.SourceQuery, SourceID: uuidString(), Content: answer}
	if r := route.get(); r != nil {
		output.Provider, output.Model = &r.Provider, &r.Model
	}
	if err := m.opts.Memories.EnqueueOutput(ctx, output); err != nil {
		slog.WarnContext(ctx, "query output indexing enqueue failed", "error", err)
	}
	followUps := m.opts.FollowUps.Generate(ctx, sessionKey, sessionID, answer, hits)
	if !locked(StreamEvent{Type: "follow_ups", Payload: followUps}) {
		return
	}
	locked(StreamEvent{Type: "done"})
}

func uuidString() string { return strings.ToUpper(uuid.NewString()) }

// streamHeaders are SSEStreamResponse's.
func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// frame writes one SSE record: `data: <json>\n\n`.
func frame(w http.ResponseWriter, rc *http.ResponseController, event any) bool {
	data, err := json.Marshal(event)
	if err != nil {
		return false
	}
	if _, err := w.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// PGTelemetry is the direct insert standing in for RetrievalTelemetryWorker's inbox:
// the sink is insert-only and additive, so a failure is logged and nothing else happens.
type PGTelemetry struct {
	Pool *pgxpool.Pool
}

// Record inserts one sample. It runs detached, as the inbox did, with a bound Swift's
// worker doesn't need.
func (t PGTelemetry) Record(ctx context.Context, s TelemetrySample) {
	var top, mean *float64
	if len(s.Distances) > 0 {
		lowest, sum := float64(s.Distances[0]), 0.0
		for _, d := range s.Distances {
			lowest = min(lowest, float64(d))
			sum += float64(d)
		}
		avg := sum / float64(len(s.Distances))
		top, mean = &lowest, &avg
	}
	go func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := t.Pool.Exec(ctx, `
			INSERT INTO retrieval_telemetry_events
				(tenant_id, source_path, space_id, hit_count, zero_hit, top_distance, mean_distance, limit_requested)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			s.Tenant, s.SourcePath, s.SpaceID, len(s.Distances), len(s.Distances) == 0, top, mean, s.Limit)
		if err != nil {
			slog.WarnContext(ctx, "retrieval telemetry insert failed", "error", err)
		}
	}(context.WithoutCancel(ctx))
}
