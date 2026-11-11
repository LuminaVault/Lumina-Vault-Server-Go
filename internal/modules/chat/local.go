package chat

import (
	"context"
	"encoding/json"
	"errors"
	"lumina/internal/embedding"
	"lumina/internal/ids"
	"lumina/internal/memorystore"
	"lumina/internal/onboardlatch"
	"lumina/internal/swiftstr"
	"lumina/internal/vaultaccess"
	"lumina/internal/wire"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
)

// Hybrid execution: the client runs the model itself (on device or a local endpoint)
// against a prompt the server prepared, then commits the reply.

const localExecutionTTL = 15 * time.Minute

type executionRow struct {
	ID          uuid.UUID
	UserMessage *uuid.UUID
	SourceIDs   []uuid.UUID
	Committed   *uuid.UUID
	ExpiresAt   time.Time
}

func (m *Module) findExecution(ctx context.Context, tenant, id, conv uuid.UUID) (*executionRow, error) {
	var e executionRow
	err := m.pool.QueryRow(ctx, `SELECT id, user_message_id, source_ids, committed_message_id, expires_at
		FROM prepared_local_executions WHERE tenant_id = $1 AND id = $2 AND conversation_id = $3 LIMIT 1`, tenant, id, conv).
		Scan(&e.ID, &e.UserMessage, &e.SourceIDs, &e.Committed, &e.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &e, err
}

// dropExecution is cancel's delete: the user message when there is one (its
// cascade takes the execution), otherwise the execution itself.
func (m *Module) dropExecution(ctx context.Context, e executionRow) error {
	if e.UserMessage != nil {
		tag, err := m.pool.Exec(ctx, `DELETE FROM conversation_messages WHERE id = $1`, *e.UserMessage)
		if err != nil {
			return err
		}
		if tag.RowsAffected() > 0 {
			return nil
		}
	}
	_, err := m.pool.Exec(ctx, `DELETE FROM prepared_local_executions WHERE id = $1`, e.ID)
	return err
}

func (m *Module) removeExpiredLocalExecutions(ctx context.Context, tenant uuid.UUID) error {
	rows, err := m.pool.Query(ctx, `SELECT id, user_message_id, source_ids, committed_message_id, expires_at
		FROM prepared_local_executions WHERE tenant_id = $1 AND expires_at < $2 AND committed_message_id IS NULL`, tenant, m.now())
	if err != nil {
		return err
	}
	var expired []executionRow
	for rows.Next() {
		var e executionRow
		if err := rows.Scan(&e.ID, &e.UserMessage, &e.SourceIDs, &e.Committed, &e.ExpiresAt); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range expired {
		if err := m.dropExecution(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

func (m *Module) prepareLocalReply(c *echo.Context) error {
	actor, err := requireUser(c)
	if err != nil {
		return err
	}
	access, err := m.access.Resolve(c, vaultaccess.AI)
	if err != nil {
		return err
	}
	vault := access.VaultID
	convID, err := conversationID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	conv, err := m.fetch(ctx, actor, convID)
	if err != nil {
		return err
	}
	body, err := wire.Bind[prepareRequest](c)
	if err != nil {
		return err
	}
	content := swiftstr.TrimWhitespacesAndNewlines(body.Content)
	if content == "" {
		return errContentRequired
	}
	if err := m.removeExpiredLocalExecutions(ctx, actor); err != nil {
		return err
	}
	userMsg := uuid.New()
	if err := m.insertMessage(ctx, messageRow{ID: userMsg, ConversationID: convID, Role: "user", Content: content, CreatedAt: m.now()}, nil); err != nil {
		return err
	}
	history, err := m.messages(ctx, convID)
	if err != nil {
		return err
	}
	vec, err := embedding.EmbedForQuery(ctx, m.opts.Embeddings, content, vault, "chat.localReply")
	if err != nil {
		return err
	}
	semantic, err := m.retrieve(ctx, vault, content, vec, 5)
	if err != nil {
		return err
	}
	m.telemetry(ctx, vault, "local_reply", semantic, 5)
	pinned, err := m.pinnedHits(ctx, vault, conv.Pinned)
	if err != nil {
		return err
	}
	hits := groundingHits(pinned, semantic)
	m.recordMemoryRetrievals(ctx, vault, actor, len(hits))
	user, err := m.loadUser(ctx, actor)
	if err != nil {
		return err
	}
	schedule := scheduleContext(ctx, m.pool, actor, location(user.Timezone), m.now())
	prompt := BuildPrompt(historyMessages(history), hits, schedule, soulExcerpt(m.opts.Soul, actor, user.Username), false)
	messages := make([]ChatMessageDTO, len(prompt))
	for i, p := range prompt {
		messages[i] = ChatMessageDTO{Role: p.Role, Content: p.Content}
	}
	sourceIDs := make([]uuid.UUID, len(hits))
	sources := make([]HitDTO, len(hits))
	for i, h := range hits {
		sourceIDs[i] = h.ID
		sources[i] = hitDTO(h)
	}
	now := m.now()
	expires := now.Add(localExecutionTTL)
	encoded, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	execID := uuid.New()
	if _, err := m.pool.Exec(ctx, `INSERT INTO prepared_local_executions
		(id, tenant_id, conversation_id, user_message_id, messages, source_ids, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, execID, actor, convID, userMsg, encoded, sourceIDs, expires, now); err != nil {
		return err
	}
	tools := []localToolDTO{}
	if m.opts.LocalExecutionToolBrokerEnabled {
		tools = append(tools, localToolDTO{Name: "memory_search", Description: "Search the caller's LuminaVault memories for read-only grounding context."})
	}
	return wire.JSON(c, http.StatusOK, prepareResponse{
		ExecutionID: wire.UUID{UUID: execID}, Messages: messages, Sources: sources,
		ExpiresAt: wire.Time{Time: expires}, AllowedTools: tools,
	})
}

func (m *Module) invokeLocalTool(c *echo.Context) error {
	actor, err := requireUser(c)
	if err != nil {
		return err
	}
	access, err := m.access.Resolve(c, vaultaccess.AI)
	if err != nil {
		return err
	}
	vault := access.VaultID
	convID, err := conversationID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	execID, ok := parseID(c, "executionID")
	if !ok {
		return errExecNotFound
	}
	exec, err := m.findExecution(ctx, actor, execID, convID)
	if err != nil {
		return err
	}
	if exec == nil {
		return errExecNotFound
	}
	if exec.Committed != nil {
		return errExecCommitted
	}
	if !exec.ExpiresAt.After(m.now()) {
		return errExecExpired
	}
	body, err := wire.Bind[toolRequest](c)
	if err != nil {
		return err
	}
	query := swiftstr.TrimWhitespacesAndNewlines(body.Query)
	if query == "" {
		return errToolQuery
	}
	limit := 5
	if body.Limit != nil {
		limit = *body.Limit
	}
	limit = max(1, min(limit, 10))
	vec, err := embedding.EmbedForQuery(ctx, m.opts.Embeddings, query, vault, "chat.localTool")
	if err != nil {
		return err
	}
	hits, err := m.retrieve(ctx, vault, query, vec, limit)
	if err != nil {
		return err
	}
	m.telemetry(ctx, vault, "agentic_search", hits, limit)
	sources := make([]HitDTO, len(hits))
	texts := make([]string, len(hits))
	for i, h := range hits {
		sources[i] = hitDTO(h)
		texts[i] = h.Content
	}
	content := "No relevant memories found."
	if len(hits) > 0 {
		content = strings.Join(texts, "\n\n")
	}
	return wire.JSON(c, http.StatusOK, toolResponse{Name: "memory_search", Content: content, Sources: sources})
}

func (m *Module) commitLocalReply(c *echo.Context) error {
	actor, err := requireUser(c)
	if err != nil {
		return err
	}
	access, err := m.access.Resolve(c, vaultaccess.AI)
	if err != nil {
		return err
	}
	vault := access.VaultID
	convID, err := conversationID(c)
	if err != nil {
		return err
	}
	body, err := wire.Bind[commitRequest](c)
	if err != nil {
		return err
	}
	content := swiftstr.TrimWhitespacesAndNewlines(body.Content)
	if content == "" {
		return errContentRequired
	}
	ctx := c.Request().Context()
	exec, err := m.findExecution(ctx, actor, body.ExecutionID.UUID, convID)
	if err != nil {
		return err
	}
	if exec == nil {
		return errExecNotFound
	}
	if exec.Committed != nil {
		if existing, err := m.findMessage(ctx, *exec.Committed); err != nil {
			return err
		} else if existing != nil {
			return wire.JSON(c, http.StatusOK, commitResponse{Message: messageDTO(*existing, nil), FollowUps: []string{}})
		}
	}
	if !exec.ExpiresAt.After(m.now()) {
		return errExecExpired
	}
	if _, err := m.fetch(ctx, actor, convID); err != nil {
		return err
	}
	execID := body.ExecutionID.UUID
	msg := messageRow{ID: uuid.New(), ConversationID: convID, Role: "assistant", Content: content, SourceMemoryIDs: exec.SourceIDs, CreatedAt: m.now()}
	if err := m.insertMessage(ctx, msg, &execID); err != nil {
		var existing *messageRow
		if e, ferr := scanMessage(m.pool.QueryRow(ctx, `SELECT `+messageColumns+` FROM conversation_messages WHERE local_execution_id = $1 LIMIT 1`, execID)); ferr == nil {
			existing = e
		}
		if existing == nil {
			return err
		}
		_, _ = m.pool.Exec(ctx, `UPDATE prepared_local_executions SET committed_message_id = $2 WHERE id = $1`, execID, existing.ID)
		return wire.JSON(c, http.StatusOK, commitResponse{Message: messageDTO(*existing, nil), FollowUps: []string{}})
	}
	if _, err := m.pool.Exec(ctx, `UPDATE prepared_local_executions SET committed_message_id = $2 WHERE id = $1`, execID, msg.ID); err != nil {
		return err
	}
	provider := string(body.Location) + ":" + body.Provider
	model := body.Model
	if err := m.opts.Memories.EnqueueOutput(ctx, memorystore.EnqueueOutputParams{
		Tenant: vault, Source: memorystore.SourceChat, SourceID: ids.Upper(msg.ID), ConversationMessageID: &msg.ID,
		Content: content, Provider: &provider, Model: &model,
	}); err != nil {
		return err
	}
	if err := m.touchConversation(ctx, convID); err != nil {
		return err
	}
	if m.opts.Latches != nil {
		m.opts.Latches.Latch(ctx, onboardlatch.FirstQuery, actor)
	}
	return wire.JSON(c, http.StatusOK, commitResponse{Message: messageDTO(msg, nil), FollowUps: []string{}})
}

func (m *Module) findMessage(ctx context.Context, id uuid.UUID) (*messageRow, error) {
	r, err := scanMessage(m.pool.QueryRow(ctx, `SELECT `+messageColumns+` FROM conversation_messages WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (m *Module) cancelLocalReply(c *echo.Context) error {
	actor, err := requireUser(c)
	if err != nil {
		return err
	}
	convID, err := conversationID(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	execID, ok := parseID(c, "executionID")
	if !ok {
		return errExecNotFound
	}
	exec, err := m.findExecution(ctx, actor, execID, convID)
	if err != nil {
		return err
	}
	if exec == nil {
		return errExecNotFound
	}
	if exec.Committed != nil {
		return errExecCommitted
	}
	if err := m.dropExecution(ctx, *exec); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
