package apns

import (
	"context"
	"lumina/internal/dbtest"
	"lumina/internal/ids"
	"lumina/internal/migrations"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type harness struct {
	ctx  context.Context
	t    *testing.T
	pool *pgxpool.Pool
}

func (h harness) tenant(timezone string) uuid.UUID {
	h.t.Helper()
	id := uuid.New()
	_, err := h.pool.Exec(h.ctx, `INSERT INTO users (id, email, password_hash, username, timezone, created_at, updated_at)
		VALUES ($1, $2, 'x', $3, $4, now(), now())`, id, id.String()+"@t", id.String(), timezone)
	require.NoError(h.t, err)
	// A tenant is a vault: proactive_push_log.tenant_id references vaults(id).
	_, err = h.pool.Exec(h.ctx, `INSERT INTO vaults (id, personal_owner_user_id, name, created_at, updated_at) VALUES ($1, $1, 'v', now(), now())`, id)
	require.NoError(h.t, err)
	return id
}

func (h harness) token(tenant uuid.UUID) string {
	h.t.Helper()
	token := "tok-" + uuid.NewString()
	_, err := h.pool.Exec(h.ctx, `INSERT INTO device_tokens (id, tenant_id, token, platform, created_at, updated_at)
		VALUES ($1, $2, $3, 'ios', now(), now())`, uuid.New(), tenant, token)
	require.NoError(h.t, err)
	return token
}

func (h harness) tokens(tenant uuid.UUID) []string {
	h.t.Helper()
	rows, err := h.pool.Query(h.ctx, `SELECT token FROM device_tokens WHERE tenant_id = $1`, tenant)
	require.NoError(h.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var token string
		require.NoError(h.t, rows.Scan(&token))
		out = append(out, token)
	}
	return out
}

func (h harness) hermieMessages(tenant uuid.UUID) []string {
	h.t.Helper()
	rows, err := h.pool.Query(h.ctx, `SELECT m.content FROM conversation_messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE c.tenant_id = $1 AND c.system_key = 'hermie' ORDER BY m.created_at`, tenant)
	require.NoError(h.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var content string
		require.NoError(h.t, rows.Scan(&content))
		out = append(out, content)
	}
	return out
}

func TestProactive(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.Postgres(t)
	require.NoError(t, migrations.Up(ctx, pool, migrations.SQL()))
	h := harness{ctx: ctx, t: t, pool: pool}

	// Swift APNSNotificationServiceTests, the four over these three pushes.
	t.Run("notify LLM reply fans out to every registered token", func(t *testing.T) {
		tenant := h.tenant("UTC")
		a, b, c := h.token(tenant), h.token(tenant), h.token(tenant)
		sender := &stubSender{}
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyLLMReply(ctx, tenant, "me", "Hello there"))
		got := make([]string, 0, 3)
		for _, d := range sender.sent {
			got = append(got, d.DeviceToken)
			assert.Equal(t, bundle, d.Topic)
			assert.Equal(t, CategoryChat, d.Message.Category)
			assert.Equal(t, "LuminaVault reply ready", d.Message.Title)
			assert.Equal(t, "me", *d.Message.Subtitle)
			assert.Equal(t, "Hello there", d.Message.Body)
		}
		assert.ElementsMatch(t, []string{a, b, c}, got)
	})
	t.Run("the reply preview is the first 140 Characters", func(t *testing.T) {
		tenant := h.tenant("UTC")
		h.token(tenant)
		sender := &stubSender{}
		long := strings.Repeat("é", 200)
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyLLMReply(ctx, tenant, "me", long))
		assert.Equal(t, strings.Repeat("é", 140), sender.sent[0].Message.Body)
	})
	t.Run("dead tokens are reaped on bad device token error", func(t *testing.T) {
		tenant := h.tenant("UTC")
		alive, dead := h.token(tenant), h.token(tenant)
		sender := &stubSender{fail: map[string]error{dead: &PushError{Status: 400, Reason: "BadDeviceToken"}}}
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyNudge(ctx, tenant, "me", "test nudge"))
		assert.Equal(t, []string{alive}, h.tokens(tenant))
		require.Len(t, sender.sent, 1)
		m := sender.sent[0].Message
		assert.Equal(t, "Hermes noticed something", m.Title)
		assert.Equal(t, "me", *m.Subtitle)
		assert.Equal(t, "test nudge", m.Body)
		assert.Equal(t, CategoryNudge, m.Category)
	})
	t.Run("transient errors do not reap the token", func(t *testing.T) {
		tenant := h.tenant("UTC")
		token := h.token(tenant)
		sender := &stubSender{fail: map[string]error{token: &PushError{Status: 500, Reason: "InternalServerError"}}}
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyDigest(ctx, tenant, "me", "today"))
		assert.Equal(t, []string{token}, h.tokens(tenant))
	})
	t.Run("the digest push", func(t *testing.T) {
		tenant := h.tenant("UTC")
		h.token(tenant)
		sender := &stubSender{}
		require.NoError(t, NewWithSender(pool, bundle, sender).NotifyDigest(ctx, tenant, "me", "today"))
		m := sender.sent[0].Message
		assert.Equal(t, "Your daily brief", m.Title)
		assert.Equal(t, "me", *m.Subtitle)
		assert.Equal(t, CategoryDigest, m.Category)
	})
	t.Run("disabled service is no op", func(t *testing.T) {
		tenant := h.tenant("UTC")
		h.token(tenant)
		s := New(pool, Config{Enabled: "false"})
		require.NoError(t, s.NotifyLLMReply(ctx, tenant, "me", "x"))
		assert.Len(t, h.tokens(tenant), 1)
	})

	// Swift MuseProactiveIntegrationTests, "a proactive message lands in one Hermie
	// thread and pushes a chat deep link".
	t.Run("a proactive message lands in one Hermie thread and pushes a chat deep link", func(t *testing.T) {
		tenant := h.tenant("UTC")
		h.token(tenant)
		sender := &stubSender{}
		delivery := NewProactiveDelivery(pool, NewWithSender(pool, bundle, sender))

		first, err := delivery.Deliver(ctx, tenant, " \n## Good morning\nDry week ahead.\n", "daily-brief")
		require.NoError(t, err)

		var title, systemKey string
		var owner uuid.UUID
		var pinned []uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `SELECT title, system_key, tenant_id, pinned_memory_ids FROM conversations WHERE id = $1`,
			first.ConversationID).Scan(&title, &systemKey, &owner, &pinned))
		assert.Equal(t, "Hermie", title)
		assert.Equal(t, "hermie", systemKey)
		assert.Equal(t, tenant, owner)
		assert.Empty(t, pinned)

		var role, content, origin, label string
		var sources []uuid.UUID
		var toolCalls int
		require.NoError(t, pool.QueryRow(ctx, `SELECT role, content, origin, source_label, source_memory_ids, tool_call_count
			FROM conversation_messages WHERE id = $1`, first.MessageID).Scan(&role, &content, &origin, &label, &sources, &toolCalls))
		assert.Equal(t, "assistant", role)
		assert.Equal(t, "proactive", origin)
		assert.Equal(t, "daily-brief", label)
		assert.Equal(t, "## Good morning\nDry week ahead.", content)
		assert.Empty(t, sources)
		assert.Zero(t, toolCalls)

		require.Len(t, sender.sent, 1)
		m := sender.sent[0].Message
		assert.Equal(t, CategoryChat, m.Category)
		assert.Equal(t, "Hermie", m.Title)
		assert.Equal(t, "daily-brief", *m.Subtitle)
		assert.Equal(t, "Good morning", m.Body)
		assert.Equal(t, map[string]string{
			"conversationID": ids.Upper(first.ConversationID),
			"messageID":      ids.Upper(first.MessageID),
			"origin":         "proactive",
			"sourceLabel":    "daily-brief",
			"deepLink":       "luminavault://chat/" + ids.Upper(first.ConversationID),
		}, m.Payload)

		// Renamed by the user: the next delivery still finds it, and bumps it.
		_, err = pool.Exec(ctx, `UPDATE conversations SET title = 'My mornings', updated_at = now() - interval '1 hour' WHERE id = $1`, first.ConversationID)
		require.NoError(t, err)
		second, err := delivery.Deliver(ctx, tenant, "Rain Thursday.", "job-weather")
		require.NoError(t, err)
		assert.Equal(t, first.ConversationID, second.ConversationID)
		var threads int
		var bumped bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), bool_and(updated_at > now() - interval '1 minute') FROM conversations WHERE tenant_id = $1`,
			tenant).Scan(&threads, &bumped))
		assert.Equal(t, 1, threads)
		assert.True(t, bumped)
	})
	t.Run("a failed push doesn't fail the delivery, and no sender still writes", func(t *testing.T) {
		tenant := h.tenant("UTC")
		delivery := NewProactiveDelivery(pool, New(pool, Config{}))
		_, err := delivery.Deliver(ctx, tenant, "one", "s")
		require.NoError(t, err)
		_, err = NewProactiveDelivery(pool, nil).Deliver(ctx, tenant, "two", "s")
		require.NoError(t, err)
		assert.Equal(t, []string{"one", "two"}, h.hermieMessages(tenant))
	})
	t.Run("concurrent first deliveries share one thread", func(t *testing.T) {
		tenant := h.tenant("UTC")
		delivery := NewProactiveDelivery(pool, nil)
		const n = 8
		found := make(chan uuid.UUID, n)
		for range n {
			go func() {
				id, err := delivery.HermieConversation(ctx, tenant)
				assert.NoError(t, err)
				found <- id
			}()
		}
		first := <-found
		for range n - 1 {
			assert.Equal(t, first, <-found)
		}
	})

	// Swift MeBriefTests: the delivery half of the three brief cases. Composing the
	// cards and the runner come with 5E.
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	t.Run("the daily brief lands as a one-line chat message and one quiet push", func(t *testing.T) {
		tenant := h.tenant("UTC")
		h.token(tenant)
		sender := &stubSender{}
		delivery := NewProactiveDelivery(pool, NewWithSender(pool, bundle, sender))

		got, err := delivery.DeliverBrief(ctx, tenant, 2, "daily-brief", now)
		require.NoError(t, err)
		assert.Equal(t, BriefPushSent, got)
		assert.Equal(t, []string{"[Your brief is ready — 2 cards](luminavault://brief)"}, h.hermieMessages(tenant))
		require.Len(t, sender.sent, 1)
		m := sender.sent[0].Message
		assert.Equal(t, "Your brief is ready — 2 cards", m.Body)
		assert.Equal(t, "Your daily brief", m.Title)
		assert.Nil(t, m.Subtitle)
		assert.Equal(t, "luminavault://brief", m.Payload["deepLink"])
		assert.Equal(t, "proactive", m.Payload["origin"])
		assert.Equal(t, "daily-brief", m.Payload["sourceLabel"])
		assert.Equal(t, "passive", m.InterruptionLevel)
		assert.Equal(t, "brief", m.ActionCategory)
		assert.Equal(t, "brief", m.ThreadID)
		assert.Equal(t, CategoryChat, m.Category)

		// A second run the same day: the message lands, the push is already spent.
		got, err = delivery.DeliverBrief(ctx, tenant, 2, "daily-brief", now.Add(time.Hour))
		require.NoError(t, err)
		assert.Equal(t, BriefPushBudgetSpent, got)
		assert.Len(t, sender.sent, 1)
		assert.Len(t, h.hermieMessages(tenant), 2)
	})
	t.Run("the brief push is dropped when the learned digest already used today's slot", func(t *testing.T) {
		tenant := h.tenant("UTC")
		h.token(tenant)
		won, err := ClaimProactivePush(ctx, pool, tenant, "learned", "UTC", now)
		require.NoError(t, err)
		require.True(t, won)
		sender := &stubSender{}
		got, err := NewProactiveDelivery(pool, NewWithSender(pool, bundle, sender)).DeliverBrief(ctx, tenant, 1, "daily-brief", now)
		require.NoError(t, err)
		assert.Equal(t, BriefPushBudgetSpent, got)
		assert.Empty(t, sender.sent)
		assert.Equal(t, []string{"[Your brief is ready — 1 card](luminavault://brief)"}, h.hermieMessages(tenant))
		var kinds []string
		rows, err := pool.Query(ctx, `SELECT kind FROM proactive_push_log WHERE tenant_id = $1`, tenant)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var kind string
			require.NoError(t, rows.Scan(&kind))
			kinds = append(kinds, kind)
		}
		assert.Equal(t, []string{"learned"}, kinds)
	})
	t.Run("the brief push leaves the budget alone when chat pushes are off", func(t *testing.T) {
		tenant := h.tenant("UTC")
		h.token(tenant)
		_, err := pool.Exec(ctx, `INSERT INTO apns_category_prefs (tenant_id, chat_enabled) VALUES ($1, FALSE)`, tenant)
		require.NoError(t, err)
		sender := &stubSender{}
		got, err := NewProactiveDelivery(pool, NewWithSender(pool, bundle, sender)).DeliverBrief(ctx, tenant, 3, "daily-brief", now)
		require.NoError(t, err)
		assert.Equal(t, BriefPushOptedOut, got)
		assert.Empty(t, sender.sent)
		won, err := ClaimProactivePush(ctx, pool, tenant, "learned", "UTC", now)
		require.NoError(t, err)
		assert.True(t, won, "the day's slot is still free")
	})
	t.Run("no device leaves the budget alone", func(t *testing.T) {
		tenant := h.tenant("UTC")
		got, err := NewProactiveDelivery(pool, NewWithSender(pool, bundle, &stubSender{})).DeliverBrief(ctx, tenant, 3, "s", now)
		require.NoError(t, err)
		assert.Equal(t, BriefPushNoDevice, got)
		won, err := ClaimProactivePush(ctx, pool, tenant, "learned", "UTC", now)
		require.NoError(t, err)
		assert.True(t, won)
	})
	t.Run("the brief claims the user's local day", func(t *testing.T) {
		tenant := h.tenant("America/Los_Angeles")
		h.token(tenant)
		delivery := NewProactiveDelivery(pool, NewWithSender(pool, bundle, &stubSender{}))
		_, err := delivery.DeliverBrief(ctx, tenant, 1, "s", time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		var day string
		require.NoError(t, pool.QueryRow(ctx, `SELECT day::text FROM proactive_push_log WHERE tenant_id = $1`, tenant).Scan(&day))
		assert.Equal(t, "2026-10-16", day)
	})

	// Swift APNSPushRoutingTests, the two budget cases.
	t.Run("one proactive push per tenant per local day", func(t *testing.T) {
		lisbon, other := h.tenant("Europe/Lisbon"), h.tenant("UTC")
		morning := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
		evening := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
		nextDay := time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)
		for _, tc := range []struct {
			tenant         uuid.UUID
			kind, timezone string
			at             time.Time
			want           bool
		}{
			{lisbon, "learned_digest", "Europe/Lisbon", morning, true},
			{lisbon, "daily_brief", "Europe/Lisbon", evening, false},
			{other, "daily_brief", "UTC", evening, true},
			{lisbon, "daily_brief", "Europe/Lisbon", nextDay, true},
		} {
			won, err := ClaimProactivePush(ctx, pool, tc.tenant, tc.kind, tc.timezone, tc.at)
			require.NoError(t, err)
			assert.Equal(t, tc.want, won)
		}
	})
	t.Run("the budget day is the user's local day", func(t *testing.T) {
		tenant := h.tenant("America/Los_Angeles")
		late := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC)
		afterUTCMidnight := time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)
		won, err := ClaimProactivePush(ctx, pool, tenant, "learned_digest", "America/Los_Angeles", late)
		require.NoError(t, err)
		assert.True(t, won)
		won, err = ClaimProactivePush(ctx, pool, tenant, "learned_digest", "America/Los_Angeles", afterUTCMidnight)
		require.NoError(t, err)
		assert.False(t, won)
		assert.Equal(t, "2026-10-16", LocalDay(afterUTCMidnight, "America/Los_Angeles"))
		assert.Equal(t, "2026-10-17", LocalDay(afterUTCMidnight, "Not/AZone"))
		assert.Equal(t, "2026-10-17", LocalDay(afterUTCMidnight, ""))
		assert.Equal(t, "2026-10-17", LocalDay(afterUTCMidnight, "Local"))
	})
}

// Swift MuseProactiveManifestTests, "the lock screen preview is the first plain line".
func TestPreview(t *testing.T) {
	assert.Equal(t, "Good morning", Preview("## Good morning\n\n- Dry week ahead"))
	assert.Equal(t, "Busy day.** Three meetings.", Preview("\n\n**Busy day.** Three meetings."))
	long := Preview(strings.Repeat("a", 300))
	assert.Equal(t, strings.Repeat("a", 139)+"…", long)
	assert.Equal(t, "luminavault://chat/6F9619FF-8B86-D011-B42D-00C04FC964FF", DeepLink(uuid.MustParse("6F9619FF-8B86-D011-B42D-00C04FC964FF")))

	assert.Equal(t, strings.Repeat("a", 140), Preview(strings.Repeat("a", 140)), "exactly 140 isn't cut")
	assert.Equal(t, strings.Repeat("é", 139)+"…", Preview(strings.Repeat("é", 141)), "Characters, not bytes")
	assert.Empty(t, Preview(""))
	assert.Empty(t, Preview("\n \n###\n> - *\n"))
	assert.Equal(t, "second", Preview("###\r\n\t# second\r\nthird"))
	assert.Equal(t, "a", Preview("a b"))
	assert.Equal(t, "x", Preview("\t  > x"))
	assert.Equal(t, "\tx", Preview("#\tx"), "markers come off after whitespace, so an inner tab stays")
}

func TestBriefLine(t *testing.T) {
	assert.Equal(t, "Your brief is ready — 1 card", BriefLine(1))
	assert.Equal(t, "Your brief is ready — 4 cards", BriefLine(4))
	assert.Equal(t, "Your brief is ready — 0 cards", BriefLine(0))
}
