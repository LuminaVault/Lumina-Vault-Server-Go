package chat

import (
	"context"
	"lumina/internal/auth"
	"lumina/internal/capture"
	"lumina/internal/clientcaps"
	"lumina/internal/embedding"
	"lumina/internal/entitlement"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/modules/memoryagent"
	"lumina/internal/modules/quickassistant"
	"lumina/internal/onboardlatch"
	"lumina/internal/sse"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// userRow is the part of Swift's User the chat routes read.
type userRow struct {
	ID                uuid.UUID
	Username          string
	Tier              string
	TierOverride      string
	Timezone          string
	AutoSaveLinks     bool
	PrivacyNoCNOrigin bool
}

func (m *Module) loadUser(ctx context.Context, id uuid.UUID) (*userRow, error) {
	u := userRow{ID: id}
	err := m.pool.QueryRow(ctx, `SELECT username, tier, tier_override, timezone, auto_save_links, privacy_no_cn_origin
		FROM users WHERE id = $1`, id).Scan(&u.Username, &u.Tier, &u.TierOverride, &u.Timezone, &u.AutoSaveLinks, &u.PrivacyNoCNOrigin)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// pinnedHits are the conversation's pinned memories as search results, distance 0.
func (m *Module) pinnedHits(ctx context.Context, vault uuid.UUID, pinned []uuid.UUID) ([]memorystore.SearchResult, error) {
	var out []memorystore.SearchResult
	for _, id := range pinned {
		mem, err := m.opts.Memories.Find(ctx, vault, id)
		if err != nil {
			return nil, err
		}
		if mem == nil {
			continue
		}
		out = append(out, memorystore.SearchResult{
			ID: mem.ID, TenantID: vault, Content: mem.Content, CreatedAt: mem.CreatedAt, Distance: 0,
			Source: memorystore.ParseSourceKind(mem.OriginKind), Provider: mem.OriginProvider, Model: mem.OriginModel,
		})
	}
	return out, nil
}

// groundingHits is pinned first, then semantic hits not already pinned, five in all.
func groundingHits(pinned, semantic []memorystore.SearchResult) []memorystore.SearchResult {
	seen := map[uuid.UUID]bool{}
	for _, h := range pinned {
		seen[h.ID] = true
	}
	room := max(0, 5-len(pinned))
	out := append([]memorystore.SearchResult{}, pinned...)
	for _, h := range semantic {
		if room == 0 {
			break
		}
		if seen[h.ID] {
			continue
		}
		out = append(out, h)
		room--
	}
	return out
}

// recordMemoryRetrievals inserts one memory_retrieved row per hit; failures are try?.
func (m *Module) recordMemoryRetrievals(ctx context.Context, vault, actor uuid.UUID, count int) {
	if count <= 0 {
		return
	}
	if _, err := m.pool.Exec(ctx, `INSERT INTO analytics_events (vault_id, actor_user_id, event_name, source, dimensions)
		SELECT $1, $2, 'memory_retrieved', 'server', '{}'::jsonb FROM generate_series(1, $3::int)`, vault, actor, count); err != nil {
		m.log.WarnContext(ctx, "memory_retrieved insert failed", "error", err)
	}
}

func (m *Module) telemetry(ctx context.Context, vault uuid.UUID, source string, hits []memorystore.SearchResult, limit int) {
	if m.opts.Telemetry == nil {
		return
	}
	d := make([]float32, len(hits))
	for i, h := range hits {
		d[i] = h.Distance
	}
	m.opts.Telemetry.Record(ctx, memoryagent.TelemetrySample{Tenant: vault, SourcePath: source, Distances: d, Limit: limit})
}

// hitDTO is the chat's QueryHitDTO: id, content, distance, createdAt, no snippet or
// citation (ConversationController at fdbed9b0).
func hitDTO(h memorystore.SearchResult) HitDTO {
	dto := HitDTO{ID: wire.UUID{UUID: h.ID}, Content: h.Content, Distance: wire.Float32(h.Distance)}
	if h.CreatedAt != nil {
		dto.CreatedAt = &wire.Time{Time: *h.CreatedAt}
	}
	return dto
}

func historyMessages(rows []messageRow) []llm.ChatMessage {
	out := make([]llm.ChatMessage, len(rows))
	for i, r := range rows {
		out[i] = llm.ChatMessage{Role: r.Role, Content: r.Content}
	}
	return out
}

func (m *Module) insertMessage(ctx context.Context, r messageRow, localExecution *uuid.UUID) error {
	sources := r.SourceMemoryIDs
	if sources == nil {
		sources = []uuid.UUID{}
	}
	_, err := m.pool.Exec(ctx, `INSERT INTO conversation_messages
		(id, conversation_id, role, content, source_memory_ids, parallel_execution_id, local_execution_id, tool_call_count, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		r.ID, r.ConversationID, r.Role, r.Content, sources, r.ParallelExecutionID, localExecution, r.ToolCallCount, r.CreatedAt)
	return err
}

func (m *Module) touchConversation(ctx context.Context, id uuid.UUID) error {
	_, err := m.pool.Exec(ctx, `UPDATE conversations SET updated_at = $2 WHERE id = $1`, id, m.now())
	return err
}

// MARK: - frames

func errorFrame(err error) StreamEvent {
	e := llm.StreamErrorFor(err)
	return StreamEvent{Type: "error", Payload: e.Message, Code: e.Code, CTA: e.CTA}
}

func fallbackFrame(n llm.FailoverNotice) StreamEvent {
	from, _ := n.OriginalProvider.ToShared()
	to, _ := n.FallbackProvider.ToShared()
	return StreamEvent{Type: "fallback", Payload: fallbackDTO{from, n.OriginalModel, to, n.FallbackModel, n.ReasonCode, n.UserMessage}}
}

func cerberusFrame(e llm.CerberusEvent) (StreamEvent, bool) {
	switch {
	case e.Routing != nil:
		r := e.Routing
		routes := r.ActiveRoutes
		if routes == nil {
			routes = []llm.WireRoute{}
		}
		return StreamEvent{Type: "routing", Payload: routingDTO{
			ExecutionID: wire.UUID{UUID: r.ExecutionID}, Phase: "selected", ProfileID: wire.UUID{UUID: r.ProfileID},
			ProfileName: r.ProfileName, TaskType: r.TaskType, Strategy: r.Strategy, ActiveRoutes: routes,
			DisplayLabel: r.DisplayLabel, PromptTokens: r.PromptTokens, ContextWindowTokens: r.ContextWindowTokens,
		}}, true
	case e.Usage != nil:
		u := e.Usage
		dto := usageDTO{
			ExecutionID: wire.UUID{UUID: u.ExecutionID}, TokensIn: u.TokensIn, TokensOut: u.TokensOut,
			EstimatedCostUsdMicros: u.EstimatedCostUsdMicros, LatencyMs: u.LatencyMs, UsageEstimated: u.UsageEstimated,
		}
		if u.Provider != "" {
			p := string(u.Provider)
			dto.Provider = &p
		}
		if u.Model != "" {
			model := u.Model
			dto.Model = &model
		}
		return StreamEvent{Type: "usage", Payload: dto}, true
	}
	return StreamEvent{}, false
}

type routeBox struct {
	mu    sync.Mutex
	value *llm.ModelProvenance
}

func (b *routeBox) set(v llm.ModelProvenance) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.value = &v
}

func (b *routeBox) get() *llm.ModelProvenance {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.value
}

// MARK: - POST /v1/conversations/:id/messages/stream

func (m *Module) streamReply(c *echo.Context) error {
	actor, ok := auth.Identity(c)
	if !ok {
		return wire.ErrUnauthenticated
	}
	convID, err := conversationID(c)
	if err != nil {
		return err
	}
	access, err := m.access.Resolve(c, vaultaccess.AI)
	if err != nil {
		return err
	}
	vault := access.VaultID
	base := llm.RequestContext(c)
	conv, err := m.fetch(base, actor, convID)
	if err != nil {
		return err
	}
	body, err := wire.Bind[streamRequest](c)
	if err != nil {
		return err
	}
	// Decoded only when Quick Assistant is on: off, the field is ignored
	// whatever it holds, as Swift production ignores it.
	var quick *quickassistant.Context
	if m.opts.QuickAssistant != nil && len(body.QuickAssistant) > 0 && string(body.QuickAssistant) != "null" {
		if quick, err = wire.Decode[quickassistant.Context](body.QuickAssistant); err != nil {
			return err
		}
	}
	var attachments []Attachment
	if body.Attachments != nil {
		attachments = *body.Attachments
	}
	typed := swiftstr.TrimWhitespacesAndNewlines(body.Content)
	if typed == "" && len(attachments) == 0 {
		return errContentRequired
	}
	content := ComposeAttachments(typed, attachments)
	user, err := m.loadUser(base, actor)
	if err != nil {
		return err
	}
	var parallel llm.ParallelStrategy
	if body.MultiModel != nil && body.MultiModel.Enabled {
		if !m.opts.ParallelEnabled || entitlement.EffectiveTier(user.Tier, user.TierOverride) != "ultimate" {
			return errParallelForbidden
		}
		parallel = llm.ParallelStrategy(body.MultiModel.Strategy)
	}

	now := m.now()
	if err := m.insertMessage(base, messageRow{ID: uuid.New(), ConversationID: convID, Role: "user", Content: content, CreatedAt: now}, nil); err != nil {
		return err
	}

	// Escalation, before any grounding work. Only for clients that can skip a frame
	// type they don't know (chat.hermes_run). A Quick Assistant turn never
	// escalates: it stays on this stream so its card can show what the loop does.
	if quick == nil && m.opts.Runs != nil && clientcaps.FromRequest(c.Request()).Supports(clientcaps.ChatHermesRun) {
		mode := ModeAuto
		if body.AgentMode != nil {
			mode = string(*body.AgentMode)
		}
		if DecideEscalation(content, mode, true) {
			if ref, ok := m.startAgentRun(base, c, actor, convID, content, ids.Upper(vault)); ok {
				stream := sse.Produce(base, func(_ context.Context, yield func(StreamEvent) bool) error {
					if yield(StreamEvent{Type: "hermes_run", Payload: ref}) {
						yield(StreamEvent{Type: "done"})
					}
					return nil
				})
				return sse.ServeStream(c, stream, func(err error) any { return errorFrame(err) })
			}
		}
	}

	history, err := m.messages(base, convID)
	if err != nil {
		return err
	}
	vec, err := embedding.EmbedForQuery(base, m.opts.Embeddings, content, vault, "chat.send")
	if err != nil {
		return err
	}
	semantic, err := m.retrieve(base, vault, content, vec, 5)
	if err != nil {
		return err
	}
	m.telemetry(base, vault, "local_reply", semantic, 5)
	pinned, err := m.pinnedHits(base, vault, conv.Pinned)
	if err != nil {
		return err
	}
	hits := groundingHits(pinned, semantic)
	m.recordMemoryRetrievals(base, vault, actor, len(hits))

	// A Quick Assistant turn runs on the device's clock when it names a real
	// zone; classic chat keeps the account's, as before.
	scheduleLoc := location(user.Timezone)
	var turnLoc *time.Location
	if quick != nil {
		turnLoc = quickassistant.TurnLocation(quick.TimeZone, user.Timezone)
		scheduleLoc = turnLoc
		if scheduleLoc == nil {
			scheduleLoc = time.UTC
		}
	}
	schedule := scheduleContext(base, m.pool, actor, scheduleLoc, m.now())
	disclosure := llm.DisclosureHidden
	var pref *llm.Preference
	if m.opts.Preferences != nil {
		if got, err := m.opts.Preferences.Get(base, actor); err == nil {
			pref = got
		}
	}
	if pref != nil && pref.Mode == llm.ModeBYOK {
		disclosure = llm.DisclosureVisible
	}
	var soulText *string
	if disclosure == llm.DisclosureVisible {
		soulText = soulExcerpt(m.opts.Soul, actor, user.Username)
	}
	prior := historyMessages(history)
	if m.opts.QuickAssistant != nil {
		prior = quickassistant.MergeAdjacentAssistant(prior)
	}
	temperature := 0.4
	req := llm.ChatRequest{
		Messages:    BuildPrompt(prior, hits, schedule, soulText, disclosure == llm.DisclosureHidden),
		Model:       m.opts.DefaultModel,
		Temperature: &temperature,
	}
	t := turn{
		actor: actor, vault: vault, billing: access.BillingSponsorUserID, conv: conv, user: user,
		sessionKey: ids.Upper(vault), sessionID: ids.Upper(convID), req: req, hits: hits,
		disclosure: disclosure, parallel: parallel, content: content,
	}
	if quick != nil {
		// This prompt runs only when the loop could not, so it says actions are
		// off and sets no temperature (Haiku 5.5 refuses one).
		t.req.Messages = quickassistant.AddingSystemText(
			quickassistant.ActionsUnavailable+"\n"+quickassistant.Clock(m.now(), scheduleLoc), t.req.Messages, false)
		t.req.Temperature = nil
		t.qa = m.newQuickTurn(base, *quick, quickInputs{
			prior: prior, hits: hits, schedule: schedule, pref: pref, turnLoc: turnLoc, scheduleLoc: scheduleLoc,
		}, t)
	}
	stream := sse.Produce(base, func(ctx context.Context, yield func(StreamEvent) bool) error {
		m.runTurn(ctx, t, yield)
		if m.afterTurn != nil {
			m.afterTurn()
		}
		return nil
	})
	return sse.ServeStream(c, stream, func(err error) any { return errorFrame(err) })
}

// startAgentRun is ConversationController.startAgentRun: nil (false) on any failure,
// so the caller answers the classic way.
func (m *Module) startAgentRun(ctx context.Context, c *echo.Context, actor, conv uuid.UUID, prompt, sessionKey string) (hermesRunRef, bool) {
	run, err := m.opts.Runs.Start(ctx, RunStart{
		Tenant: actor, ConversationID: conv, Prompt: prompt, SessionKey: sessionKey,
		Authorization: c.Request().Header.Get("Authorization"), HermesProfile: c.Request().Header.Get("X-Hermes-Profile"),
	})
	if err != nil {
		m.log.WarnContext(ctx, "chat escalation failed, using classic stream", "error", err)
		return hermesRunRef{}, false
	}
	m.log.InfoContext(ctx, "chat escalated to hermes run", "run_id", ids.Upper(run.ID))
	return hermesRunRef{RunID: wire.UUID{UUID: run.ID}, SessionID: run.SessionID, AfterSeq: 0, StartedAt: wire.Time{Time: run.StartedAt}}, true
}

type turn struct {
	actor, vault, billing uuid.UUID
	conv                  *conversationRow
	user                  *userRow
	sessionKey, sessionID string
	req                   llm.ChatRequest
	hits                  []memorystore.SearchResult
	disclosure            llm.Disclosure
	parallel              llm.ParallelStrategy
	content               string
	// qa is the Quick Assistant loop to try first; nil for a classic turn.
	qa *quickTurn
}

// runTurn is the stream task of streamReply: sources, tokens (with routing, usage and
// fallback frames), persistence and side effects, link saves, follow-ups, done.
func (m *Module) runTurn(ctx context.Context, t turn, yield func(StreamEvent) bool) {
	dtos := make([]HitDTO, len(t.hits))
	sourceIDs := make([]uuid.UUID, len(t.hits))
	for i, h := range t.hits {
		dtos[i] = hitDTO(h)
		sourceIDs[i] = h.ID
		if !yield(StreamEvent{Type: "source", Payload: dtos[i]}) {
			return
		}
	}
	route := &routeBox{}
	vault, billing, conv := t.vault, t.billing, t.conv.ID
	user := &llm.User{ID: t.user.ID, Tier: t.user.Tier, TierOverride: t.user.TierOverride, PrivacyNoCNOrigin: t.user.PrivacyNoCNOrigin}
	bind := func(v *llm.Values) {
		v.AnalyticsVaultID = &vault
		v.BillingTenantID = &billing
		v.RouteOutcomeSink = route.set
		v.ForcedRoute = t.conv.routeOverride()
		v.CerberusSink = func(e llm.CerberusEvent) {
			if frame, ok := cerberusFrame(llm.ScrubCerberusEvent(e, t.disclosure)); ok {
				yield(frame)
			}
		}
		v.ParallelStrategy = t.parallel
		v.CerberusScope = &llm.CerberusScope{Surface: llm.SurfaceChat, SpaceID: t.conv.SpaceID, ConversationID: &conv}
		v.FailoverSink = func(n llm.FailoverNotice) { yield(fallbackFrame(llm.ScrubFailoverNotice(n, t.disclosure))) }
		v.CurrentUser = user
	}
	streamCtx := llm.WithValues(ctx, bind)
	if t.qa != nil {
		outcome := m.answerQuick(ctx, t, bind, yield)
		if reply := outcome.Reply; reply != nil {
			// Not ctx: a dismissed card cancels it, and a write that already ran
			// must keep its record.
			if reply.ShouldPersist {
				work := context.WithoutCancel(ctx)
				if err := m.persistTurn(work, t, assistantRecord{
					Text: reply.Text, SourceIDs: sourceIDs, ToolCalls: reply.ToolCallCount, Route: route.get(),
					Indexable: reply.Indexable(), IsAnswer: reply.IsAnswer,
				}); err != nil {
					m.log.ErrorContext(work, "assistant turn persist failed", "error", err)
				}
			}
			yield(StreamEvent{Type: "done"})
			return
		}
		// Nobody is listening, and the classic path would only reserve again.
		if ctx.Err() != nil {
			return
		}
		// Only this binding: the classic stream reuses the free-lane decision
		// the loop already paid its one grant for.
		if decided := outcome.PreDecided; decided != nil {
			streamCtx = llm.WithValues(ctx, func(v *llm.Values) {
				bind(v)
				v.PreDecidedRoute = decided
			})
		}
	}
	var buffer strings.Builder
	tokens := 0
	toolCalls := map[string]bool{}
	for chunk, err := range m.opts.Stream.ChatStream(streamCtx, t.sessionKey, t.sessionID, t.req) {
		if err != nil {
			if ctx.Err() == nil {
				m.log.ErrorContext(ctx, "conversation stream upstream failed", "error", err)
			}
			yield(errorFrame(err))
			return
		}
		if ctx.Err() != nil {
			break
		}
		if chunk.ToolCallID != "" {
			toolCalls[chunk.ToolCallID] = true
		}
		if chunk.Delta != "" {
			tokens++
			buffer.WriteString(chunk.Delta)
			if !yield(StreamEvent{Type: "token", Payload: chunk.Delta}) {
				return
			}
		}
	}
	answer := buffer.String()
	if cleaned, ok := llm.SanitizeToolOutput(answer); ok {
		answer = cleaned
	}
	if llm.IsEmptyCompletion(answer, tokens) {
		m.log.WarnContext(ctx, "conversation stream completed without assistant content")
		yield(StreamEvent{Type: "error", Payload: llm.EmptyCompletionMessage})
		return
	}

	// Persistence and side effects; failures are logged, the stream already succeeded.
	work := context.WithoutCancel(ctx)
	if err := m.persistTurn(work, t, assistantRecord{
		Text: answer, SourceIDs: sourceIDs, ToolCalls: len(toolCalls), Route: route.get(),
		Indexable: true, IsAnswer: true, ReviewComplex: true,
	}); err != nil {
		m.log.ErrorContext(work, "assistant turn persist failed", "error", err)
	}

	if m.opts.Links != nil && t.user.AutoSaveLinks {
		seen := map[string]bool{}
		note := "Auto-saved from chat " + ids.Upper(conv)
		save := func(text string, fromUser bool) {
			for _, u := range capture.ExtractURLs(text) {
				if seen[u.Normalized] {
					continue
				}
				seen[u.Normalized] = true
				captured, err := m.opts.Links.CaptureLink(work, vault, u.Raw, &note, nil)
				if err != nil {
					m.log.WarnContext(work, "auto-save-link skip", "url", u.Raw, "error", err)
					continue
				}
				yield(StreamEvent{Type: "link_saved", Payload: linkSavedDTO{
					URL: u.Raw, VaultPath: captured.RelativePath, CapturedAt: wire.Time{Time: m.now()}, FromUserMessage: fromUser,
				}})
			}
		}
		save(t.content, true)
		save(answer, false)
	}

	followUps := []string{}
	// A Quick Assistant turn sends an empty list: nobody taps a chip in the card.
	if m.opts.FollowUps != nil && t.qa == nil {
		if got := m.opts.FollowUps.Generate(ctx, t.sessionKey, t.sessionID, answer, dtos); got != nil {
			followUps = got
		}
	}
	if !yield(StreamEvent{Type: "follow_ups", Payload: followUps}) {
		return
	}
	yield(StreamEvent{Type: "done"})
}

// assistantRecord is AssistantTurnRecord: what to write for one assistant turn.
// A classic turn sets every flag; a Quick Assistant reply sets its own.
type assistantRecord struct {
	Text      string
	SourceIDs []uuid.UUID
	ToolCalls int
	Route     *llm.ModelProvenance
	// Indexable: the reply may become a memory through the output index.
	Indexable bool
	// IsAnswer: a real answer, so it counts for the first-query latch and the
	// grounded-answer metric.
	IsAnswer bool
	// ReviewComplex: the turn may start a self-improvement review. Quick
	// Assistant turns may not: that is background model work a single question
	// should not cost.
	ReviewComplex bool
}

// persistTurn writes the assistant message, the output-index job, the turn trace and
// the conversation's updated_at, then the activation side effects.
func (m *Module) persistTurn(ctx context.Context, t turn, rec assistantRecord) error {
	msgID := uuid.New()
	if err := m.insertMessage(ctx, messageRow{
		ID: msgID, ConversationID: t.conv.ID, Role: "assistant", Content: rec.Text, SourceMemoryIDs: rec.SourceIDs,
		ToolCallCount: rec.ToolCalls, CreatedAt: m.now(),
	}, nil); err != nil {
		return err
	}
	if rec.Indexable {
		output := memorystore.EnqueueOutputParams{
			Tenant: t.vault, Source: memorystore.SourceChat, SourceID: ids.Upper(msgID), ConversationMessageID: &msgID, Content: rec.Text,
		}
		if rec.Route != nil {
			output.Provider, output.Model = &rec.Route.Provider, &rec.Route.Model
		}
		if err := m.opts.Memories.EnqueueOutput(ctx, output); err != nil {
			return err
		}
	}
	if m.opts.Traces != nil {
		provider, model := llm.ProviderID("openRouter"), "unknown"
		if rec.Route != nil {
			if knownProvider(rec.Route.Provider) {
				provider = llm.ProviderID(rec.Route.Provider)
			}
			model = rec.Route.Model
		}
		count := rec.ToolCalls
		m.opts.Traces.Record(ctx, llm.TraceTurn{
			TenantID: t.vault, ConversationMessageID: &msgID, Provider: provider, Model: model, ToolCallCount: &count,
		})
	}
	if err := m.touchConversation(ctx, t.conv.ID); err != nil {
		return err
	}
	if rec.IsAnswer {
		m.opts.Analytics.Capture("chat_grounded_answer", ids.Upper(t.actor), map[string]any{
			"grounded_hits": len(rec.SourceIDs), "used_tools": rec.ToolCalls > 0,
		})
	}
	m.opts.Activity.NoteActivity(ctx, t.actor, m.now())
	if rec.ReviewComplex {
		m.opts.Activity.TriggerComplexSession(ctx, t.actor, rec.ToolCalls, false)
	}
	if rec.IsAnswer && m.opts.Latches != nil {
		m.opts.Latches.Latch(ctx, onboardlatch.FirstQuery, t.actor)
	}
	return nil
}
