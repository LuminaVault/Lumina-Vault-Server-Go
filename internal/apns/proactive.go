package apns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/swiftstr"
	"strings"
	"time"
	_ "time/tzdata"

	// users.timezone is resolved in-process; the runtime image has no zoneinfo

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// HermieSystemKey is conversations.system_key of the thread proactive messages
	// land in. It's found by this key, not by title, so renaming it starts no second one.
	HermieSystemKey = "hermie"
	// HermieTitle is the title that thread is created with.
	HermieTitle = "Hermie"

	// BriefDeepLink is the daily brief's push deep link.
	BriefDeepLink = "luminavault://brief"
	// BriefBudgetKind is the proactive_push_log.kind the brief claims under.
	BriefBudgetKind = "brief"

	briefActionCategory = "brief"
	briefThreadID       = "brief"
	briefTitle          = "Your daily brief"

	originProactive = "proactive"
	roleAssistant   = "assistant"
	levelPassive    = "passive"
)

// Delivered names the message a proactive delivery wrote.
type Delivered struct {
	ConversationID uuid.UUID
	MessageID      uuid.UUID
}

// BriefPush is ProactiveChatDelivery.BriefPush: whether the brief's push went out,
// and if not, why.
type BriefPush string

const (
	BriefPushSent        BriefPush = "sent"
	BriefPushOptedOut    BriefPush = "opted_out"
	BriefPushNoDevice    BriefPush = "no_device"
	BriefPushBudgetSpent BriefPush = "budget_spent"
)

// ProactiveDelivery is ProactiveChatDelivery, the assistant speaking first: a skill
// with a chat_message output lands in the tenant's Hermie thread as an assistant
// message with origin proactive, then pushes a chat notification deep-linking to it.
type ProactiveDelivery struct {
	pool *pgxpool.Pool
	apns *Service
	now  func() time.Time
}

// NewProactiveDelivery builds it. A nil or disabled apns still writes the messages.
func NewProactiveDelivery(pool *pgxpool.Pool, apns *Service) *ProactiveDelivery {
	return &ProactiveDelivery{pool: pool, apns: apns, now: time.Now}
}

// DeepLink is the push payload's deepLink for a conversation.
func DeepLink(conversation uuid.UUID) string {
	return "luminavault://chat/" + ids.Upper(conversation)
}

// BriefLine is "Your brief is ready — 4 cards".
func BriefLine(cardCount int) string {
	noun := "cards"
	if cardCount == 1 {
		noun = "card"
	}
	return fmt.Sprintf("Your brief is ready — %d %s", cardCount, noun)
}

// Deliver appends content to the Hermie thread and notifies. The message is the
// point: a failed push is logged and doesn't fail the delivery.
func (d *ProactiveDelivery) Deliver(ctx context.Context, tenant uuid.UUID, content, sourceLabel string) (Delivered, error) {
	body := swiftstr.TrimWhitespacesAndNewlines(content)
	delivered, err := d.appendMessage(ctx, tenant, body, sourceLabel)
	if err != nil {
		return Delivered{}, err
	}
	subtitle := sourceLabel
	err = d.apns.Notify(ctx, tenant, Message{
		Title:    HermieTitle,
		Subtitle: &subtitle,
		Body:     Preview(body),
		Category: CategoryChat,
		Payload:  proactivePayload(delivered, sourceLabel, DeepLink(delivered.ConversationID)),
	})
	if err != nil {
		slog.WarnContext(ctx, "proactive chat push failed", "tenant", ids.Upper(tenant), "source", sourceLabel, "error", err)
	}
	return delivered, nil
}

// DeliverBrief is the daily brief's chat message: one line linking to the cards. Its
// push is passive with action category "brief", and spends the day's proactive
// budget slot. The base category stays chat, so the chat opt-out gates it. Opted out
// or no device leaves the budget alone; a slot already spent means the message lands
// and the push doesn't. With APNs disabled the slot is still spent and the result is
// sent, as in Swift.
func (d *ProactiveDelivery) DeliverBrief(ctx context.Context, tenant uuid.UUID, cardCount int, sourceLabel string, now time.Time) (BriefPush, error) {
	line := BriefLine(cardCount)
	delivered, err := d.appendMessage(ctx, tenant, "["+line+"]("+BriefDeepLink+")", sourceLabel)
	if err != nil {
		return "", err
	}
	suppressed, err := isSuppressed(ctx, d.pool, CategoryChat, tenant)
	if err != nil {
		return "", err
	}
	if suppressed {
		return BriefPushOptedOut, nil
	}
	var devices int
	if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM device_tokens WHERE tenant_id = $1`, tenant).Scan(&devices); err != nil {
		return "", err
	}
	if devices == 0 {
		return BriefPushNoDevice, nil
	}
	timezone := "UTC"
	err = d.pool.QueryRow(ctx, `SELECT timezone FROM users WHERE id = $1`, tenant).Scan(&timezone)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	won, err := ClaimProactivePush(ctx, d.pool, tenant, BriefBudgetKind, timezone, now)
	if err != nil {
		return "", err
	}
	if !won {
		slog.InfoContext(ctx, "brief push dropped: today's proactive push already spent", "tenant", ids.Upper(tenant))
		return BriefPushBudgetSpent, nil
	}
	err = d.apns.Notify(ctx, tenant, Message{
		Title:             briefTitle,
		Body:              line,
		Category:          CategoryChat,
		Payload:           proactivePayload(delivered, sourceLabel, BriefDeepLink),
		InterruptionLevel: levelPassive,
		ThreadID:          briefThreadID,
		ActionCategory:    briefActionCategory,
	})
	if err != nil {
		slog.WarnContext(ctx, "brief push failed", "tenant", ids.Upper(tenant), "error", err)
	}
	return BriefPushSent, nil
}

func proactivePayload(d Delivered, sourceLabel, deepLink string) map[string]string {
	return map[string]string{
		"conversationID": ids.Upper(d.ConversationID),
		"messageID":      ids.Upper(d.MessageID),
		"origin":         originProactive,
		"sourceLabel":    sourceLabel,
		"deepLink":       deepLink,
	}
}

// appendMessage writes the assistant message and bumps the thread to the top of the
// list. Like Swift, it uses no transaction.
func (d *ProactiveDelivery) appendMessage(ctx context.Context, tenant uuid.UUID, body, sourceLabel string) (Delivered, error) {
	conversation, err := d.HermieConversation(ctx, tenant)
	if err != nil {
		return Delivered{}, err
	}
	message := uuid.New()
	_, err = d.pool.Exec(ctx, `INSERT INTO conversation_messages
		(id, conversation_id, role, content, source_memory_ids, tool_call_count, origin, source_label, created_at)
		VALUES ($1, $2, $3, $4, $5, 0, $6, $7, $8)`,
		message, conversation, roleAssistant, body, []uuid.UUID{}, originProactive, sourceLabel, d.now())
	if err != nil {
		return Delivered{}, err
	}
	if _, err := d.pool.Exec(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, d.now(), conversation); err != nil {
		return Delivered{}, err
	}
	return Delivered{ConversationID: conversation, MessageID: message}, nil
}

// HermieConversation finds the tenant's Hermie thread, creating it on first use. A
// delivery that loses the insert race on the unique (tenant_id, system_key) index
// re-reads the winner's row.
func (d *ProactiveDelivery) HermieConversation(ctx context.Context, tenant uuid.UUID) (uuid.UUID, error) {
	id, err := d.findHermie(ctx, tenant)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	id = uuid.New()
	now := d.now()
	_, err = d.pool.Exec(ctx, `INSERT INTO conversations (id, tenant_id, title, pinned_memory_ids, system_key, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, id, tenant, HermieTitle, []uuid.UUID{}, HermieSystemKey, now)
	if err == nil {
		return id, nil
	}
	if winner, findErr := d.findHermie(ctx, tenant); findErr == nil {
		return winner, nil
	}
	return uuid.Nil, err
}

func (d *ProactiveDelivery) findHermie(ctx context.Context, tenant uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `SELECT id FROM conversations WHERE tenant_id = $1 AND system_key = $2 LIMIT 1`, tenant, HermieSystemKey).Scan(&id)
	return id, err
}

const (
	previewCharacters = 140
	previewMarkers    = "#*-> "
)

// Preview is the lock screen text: the first line left non-empty after stripping
// spaces and markdown heading and bullet markers, at most 140 Characters (139 and an
// ellipsis). Only the ends of the line are stripped, so "**Busy.** Yes" keeps its
// inner asterisks.
func Preview(content string) string {
	for _, line := range strings.FieldsFunc(content, isNewlineScalar) {
		line = strings.Trim(swiftstr.TrimWhitespaces(line), previewMarkers)
		if line == "" {
			continue
		}
		if swiftstr.CharacterCount(line) > previewCharacters {
			return swiftstr.CharacterPrefix(line, previewCharacters-1) + "…"
		}
		return line
	}
	return ""
}

// isNewlineScalar is Character.isNewline. Scalars do for splitting: nothing combines
// with a newline, and the one two-scalar newline, CRLF, only yields an empty field.
func isNewlineScalar(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// ClaimProactivePush is ProactivePushBudget.claim: at most one proactive push per
// tenant per local day, enforced by proactive_push_log's primary key. true means the
// caller won and should send. timezone is users.timezone; an unknown one counts as UTC.
func ClaimProactivePush(ctx context.Context, pool *pgxpool.Pool, tenant uuid.UUID, kind, timezone string, now time.Time) (bool, error) {
	var won string
	err := pool.QueryRow(ctx, `INSERT INTO proactive_push_log (tenant_id, day, kind, sent_at)
		VALUES ($1, $2::date, $3, $4)
		ON CONFLICT (tenant_id, day) DO NOTHING
		RETURNING kind`, tenant, LocalDay(now, timezone), kind, now).Scan(&won)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// LocalDay is ProactivePushBudget.localDay: yyyy-MM-dd of now in timezone.
func LocalDay(now time.Time, timezone string) string {
	loc := time.UTC
	if timezone != "" && timezone != "Local" {
		if l, err := time.LoadLocation(timezone); err == nil {
			loc = l
		}
	}
	return now.In(loc).Format(time.DateOnly)
}
