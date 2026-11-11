// Package apns is the part of Swift's APNSNotificationService
// (Sources/App/Services/APNSNotificationService.swift) waves 4 and 5 need: the
// achievement, ingestion, LLM reply, nudge and digest pushes, device token lookup,
// the per-tenant category opt-outs and the reaping of dead tokens. It also holds
// ProactiveChatDelivery and ProactivePushBudget (proactive.go). The other notify*
// surfaces (workflow, approvals) move with their callers and can call Notify.
//
// When APNS_ENABLED isn't "true", the key file is missing or unreadable, or
// APNS_BUNDLE_ID is empty, every call is a no-op that succeeds, as in Swift.
// Sending goes through the Sender interface: tests use a stub or a local stub
// server, and never reach Apple.
package apns

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/modules/devices"
	"lumina/internal/swiftstr"
	"lumina/internal/telemetry"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/token"
)

// Category is APNSPushCategory; the values go on the wire.
type Category string

const (
	CategoryChat          Category = "chat"
	CategoryNudge         Category = "nudge"
	CategoryDigest        Category = "digest"
	CategoryAchievement   Category = "achievement"
	CategoryReminder      Category = "reminder"
	CategoryCron          Category = "cron"
	CategoryIngestion     Category = "ingestion"
	CategoryWorkflow      Category = "workflow"
	CategoryApproval      Category = "approval"
	CategoryRunCompleted  Category = "runCompleted"
	CategoryDeviceCommand Category = "deviceCommand"
	CategoryMeeting       Category = "meeting"
)

// Message is one alert for every device of a tenant.
type Message struct {
	Title    string
	Subtitle *string
	Body     string
	Category Category
	// Payload holds custom top-level keys. Its own "category" wins over the
	// default, and it can't replace "aps".
	Payload map[string]string
	// InterruptionLevel is aps.interruption-level; empty omits it.
	InterruptionLevel string
	// ThreadID is aps.thread-id; empty omits it.
	ThreadID string
	// ActionCategory replaces Category in aps.category only, for an actionable
	// variant. Opt-outs and the payload's "category" key still use Category.
	ActionCategory string
}

// Delivery is what a Sender needs for one device.
type Delivery struct {
	DeviceToken string
	Topic       string
	Message     Message
}

// Sender delivers one push. A dead token is reported as a *PushError.
type Sender interface {
	Send(ctx context.Context, d Delivery) error
}

// PushError is APNs refusing a push.
type PushError struct {
	Status int
	Reason string
}

func (e *PushError) Error() string { return "apns: " + e.Reason }

// deadTokenReasons mean "stop using this token forever".
var deadTokenReasons = map[string]bool{
	"BadDeviceToken":         true,
	"Unregistered":           true,
	"DeviceTokenNotForTopic": true,
	"MissingDeviceToken":     true,
}

// ShouldReap reports that err means the device token is permanently dead.
// Anything else is transient, and the row survives.
func ShouldReap(err error) bool {
	var pe *PushError
	return errors.As(err, &pe) && deadTokenReasons[pe.Reason]
}

// Config is the apns.* settings.
type Config struct {
	// Enabled is APNS_ENABLED; only "true", in any case, turns it on.
	Enabled        string
	BundleID       string
	TeamID         string
	KeyID          string
	PrivateKeyPath string
	// Environment is APNS_ENVIRONMENT; anything but "production", in any case,
	// is the sandbox.
	Environment string
}

// Service fans pushes out. A nil *Service does nothing.
type Service struct {
	pool     *pgxpool.Pool
	bundleID string
	sender   Sender
	now      func() time.Time
}

// New builds the service. It always returns one; without usable settings it
// just has no sender. A key that can't be loaded logs an error and doesn't stop
// the server.
func New(pool *pgxpool.Pool, cfg Config) *Service {
	s := &Service{pool: pool, bundleID: cfg.BundleID, now: time.Now}
	if !strings.EqualFold(cfg.Enabled, "true") || cfg.PrivateKeyPath == "" {
		return s
	}
	sender, err := newLiveSender(cfg)
	if err != nil {
		slog.Error("APNS client init failed", "error", err)
		return s
	}
	s.sender = sender
	return s
}

// NewWithSender injects the sender, bypassing the Enabled gate.
func NewWithSender(pool *pgxpool.Pool, bundleID string, sender Sender) *Service {
	return &Service{pool: pool, bundleID: bundleID, sender: sender, now: time.Now}
}

// NotifyAchievement is the push for one newly unlocked sub-achievement.
func (s *Service) NotifyAchievement(ctx context.Context, tenant uuid.UUID, _ /* key */, label string) error {
	return s.Notify(ctx, tenant, Message{
		Title:    "Achievement unlocked",
		Subtitle: &label,
		Body:     "You evolved one step closer to your true form.",
		Category: CategoryAchievement,
	})
}

// NotifyIngestion is the push for an item reaching a final state. The Swift worker
// still sends these (MultimodalIngestionService.notifyTerminalFailure); it moves
// here with the worker.
func (s *Service) NotifyIngestion(ctx context.Context, tenant, batchID, itemID uuid.UUID, completed bool, fileName *string) error {
	title, body, state := "Capture needs attention", "Open LuminaVault to review the ingestion status.", "failed"
	if completed {
		title, body, state = "Capture ready", "Hermes finished processing and linked the new memory.", "completed"
	}
	return s.Notify(ctx, tenant, Message{
		Title:    title,
		Subtitle: fileName,
		Body:     body,
		Category: CategoryIngestion,
		Payload:  map[string]string{"batchID": ids.Upper(batchID), "itemID": ids.Upper(itemID), "state": state},
	})
}

// LLMReplyPreviewCharacters is how much of a reply notifyLLMReply puts in the push.
const LLMReplyPreviewCharacters = 140

// NotifyLLMReply is the push for a finished chat reply; content is
// response.message.content.
func (s *Service) NotifyLLMReply(ctx context.Context, tenant uuid.UUID, username, content string) error {
	return s.Notify(ctx, tenant, Message{
		Title:    "LuminaVault reply ready",
		Subtitle: &username,
		Body:     swiftstr.CharacterPrefix(content, LLMReplyPreviewCharacters),
		Category: CategoryChat,
	})
}

// NotifyNudge is the push for a skill's apns_nudge output.
func (s *Service) NotifyNudge(ctx context.Context, tenant uuid.UUID, username, body string) error {
	return s.Notify(ctx, tenant, Message{
		Title:    "Hermes noticed something",
		Subtitle: &username,
		Body:     body,
		Category: CategoryNudge,
	})
}

// NotifyDigest is the push for a skill's apns_digest output.
func (s *Service) NotifyDigest(ctx context.Context, tenant uuid.UUID, username, body string) error {
	return s.Notify(ctx, tenant, Message{
		Title:    "Your daily brief",
		Subtitle: &username,
		Body:     body,
		Category: CategoryDigest,
	})
}

// Notify pushes to every device token of tenant, each under its own app's topic.
// It returns an error only for the opt-out and token lookups: a failed device is
// logged and the rest still get theirs. A dead token's row is deleted.
func (s *Service) Notify(ctx context.Context, tenant uuid.UUID, m Message) error {
	if s == nil || s.sender == nil || s.bundleID == "" {
		return nil
	}
	suppressed, err := isSuppressed(ctx, s.pool, m.Category, tenant)
	if err != nil {
		return err
	}
	if suppressed {
		slog.DebugContext(ctx, "apns suppressed: tenant opted out", "tenant", ids.Upper(tenant), "category", string(m.Category))
		return nil
	}
	tokens, err := s.deviceTokens(ctx, tenant)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		slog.DebugContext(ctx, "apns skipped: no device tokens", "tenant", ids.Upper(tenant))
		return nil
	}
	for _, row := range tokens {
		prefix := swiftstr.Prefix(row.token, 8)
		err := s.sender.Send(ctx, Delivery{DeviceToken: row.token, Topic: devices.Topic(row.bundleID, s.bundleID), Message: m})
		if err == nil {
			now := s.now()
			_, err = s.pool.Exec(ctx, `UPDATE device_tokens SET last_seen_at = $1, updated_at = $1 WHERE id = $2`, now, row.id)
		}
		if err != nil && m.Category == CategoryIngestion {
			telemetry.Global.Ingestion.APNSFailures.Add(ctx, 1)
		}
		switch {
		case err == nil:
			slog.DebugContext(ctx, "apns delivered", "token", prefix, "category", string(m.Category))
		case ShouldReap(err):
			if _, delErr := s.pool.Exec(ctx, `DELETE FROM device_tokens WHERE id = $1`, row.id); delErr != nil {
				slog.DebugContext(ctx, "apns reap failed", "token", prefix, "error", delErr)
			}
			slog.InfoContext(ctx, "apns reaped dead token", "token", prefix, "error", err)
		default:
			slog.WarnContext(ctx, "apns delivery failed", "token", prefix, "error", err)
		}
	}
	return nil
}

type deviceToken struct {
	id       uuid.UUID
	token    string
	bundleID *string
}

func (s *Service) deviceTokens(ctx context.Context, tenant uuid.UUID) ([]deviceToken, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, token, bundle_id FROM device_tokens WHERE tenant_id = $1`, tenant)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (deviceToken, error) {
		var d deviceToken
		err := r.Scan(&d.id, &d.token, &d.bundleID)
		return d, err
	})
}

// isSuppressed is isCategorySuppressed: high-signal categories are never gated.
// With no prefs row everything is on, except the one opt-in category.
func isSuppressed(ctx context.Context, pool *pgxpool.Pool, category Category, tenant uuid.UUID) (bool, error) {
	var column string
	switch category {
	case CategoryChat:
		column = "chat_enabled"
	case CategoryNudge:
		column = "nudge_enabled"
	case CategoryDigest:
		column = "digest_enabled"
	case CategoryApproval:
		column = "approval_enabled"
	case CategoryRunCompleted:
		column = "run_completed_enabled"
	case CategoryMeeting:
		column = "meeting_brief_enabled"
	default:
		return false, nil
	}
	var enabled bool
	err := pool.QueryRow(ctx, `SELECT `+column+` FROM apns_category_prefs WHERE tenant_id = $1`, tenant).Scan(&enabled) //nolint:gosec // column comes from the switch above
	if errors.Is(err, pgx.ErrNoRows) {
		return category == CategoryMeeting, nil
	}
	if err != nil {
		return false, err
	}
	return !enabled, nil
}

// MARK: - Live sender

// liveSender sends through sideshow/apns2 with a token (.p8) client.
type liveSender struct {
	client *apns2.Client
}

func newLiveSender(cfg Config) (*liveSender, error) {
	key, err := token.AuthKeyFromFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	client := apns2.NewTokenClient(&token.Token{AuthKey: key, KeyID: cfg.KeyID, TeamID: cfg.TeamID})
	if strings.EqualFold(cfg.Environment, "production") {
		client = client.Production()
	} else {
		client = client.Development()
	}
	client.HTTPClient.Transport = expireNow{client.HTTPClient.Transport}
	return &liveSender{client: client}, nil
}

// expireNow adds apns-expiration: 0, which Swift sends (APNSNotificationExpiration
// .none) and which tells APNs not to store the push for an offline device.
// apns2 can't send it: it writes the header only for times after the epoch, and
// without the header APNs stores the push and delivers it later.
type expireNow struct {
	next http.RoundTripper
}

func (t expireNow) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("apns-expiration", "0")
	if t.next == nil {
		return http.DefaultTransport.RoundTrip(r)
	}
	return t.next.RoundTrip(r)
}

func (l *liveSender) Send(ctx context.Context, d Delivery) error {
	resp, err := l.client.PushWithContext(ctx, &apns2.Notification{
		DeviceToken: d.DeviceToken,
		Topic:       d.Topic,
		Priority:    apns2.PriorityHigh,
		PushType:    apns2.PushTypeAlert,
		Payload:     payload(d.Message),
	})
	if err != nil {
		return err
	}
	if !resp.Sent() {
		return &PushError{Status: resp.StatusCode, Reason: resp.Reason}
	}
	return nil
}

// payload is the JSON body: the custom keys and "aps", as APNSAlertNotification
// encodes them.
func payload(m Message) map[string]any {
	alert := map[string]any{"title": m.Title, "body": m.Body}
	if m.Subtitle != nil {
		alert["subtitle"] = *m.Subtitle
	}
	aps := map[string]any{"alert": alert, "sound": "default", "category": string(m.Category)}
	if m.ActionCategory != "" {
		aps["category"] = m.ActionCategory
	}
	if m.ThreadID != "" {
		aps["thread-id"] = m.ThreadID
	}
	if m.InterruptionLevel != "" {
		aps["interruption-level"] = m.InterruptionLevel
	}
	out := make(map[string]any, len(m.Payload)+2)
	out["category"] = string(m.Category)
	for k, v := range m.Payload {
		out[k] = v
	}
	out["aps"] = aps
	return out
}
