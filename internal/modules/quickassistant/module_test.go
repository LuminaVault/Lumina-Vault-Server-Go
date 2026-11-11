package quickassistant

import (
	"context"
	"encoding/json"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/internal/devicerpc"
	"lumina/internal/devicetools"
	"lumina/internal/entitlement"
	"lumina/internal/httpserver"
	"lumina/internal/llm"
	"lumina/internal/ratelimit"
	"lumina/internal/vaultaccess"
	"lumina/parity/scenario"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// Ports the endpoint half of QuickAssistantConfirmTests: confirm runs the
// stored action once, from the stored arguments, on the stored vault.

var qaKeys = auth.NewKeyset(auth.Key{Kid: "t", Secret: []byte("test-secret-test-secret-test-0000")})

// confirmPhone stands in for the phone: it records every command and answers
// ok, or times out, or fails with failWith, after delay. started, when set,
// runs once the command is recorded and before the delay.
type confirmPhone struct {
	mu       sync.Mutex
	commands []devicerpc.Command
	timeout  bool
	failWith string
	delay    time.Duration
	started  func()
}

func (p *confirmPhone) Request(ctx context.Context, _ uuid.UUID, cmd devicerpc.Command) (devicerpc.Result, error) {
	p.mu.Lock()
	p.commands = append(p.commands, cmd)
	p.mu.Unlock()
	if p.started != nil {
		p.started()
	}
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return devicerpc.Result{}, ctx.Err()
		}
	}
	if p.timeout {
		return devicerpc.Result{}, devicerpc.ErrTimeout
	}
	if p.failWith != "" {
		return devicerpc.Result{ID: cmd.ID, OK: false, Error: &p.failWith}, nil
	}
	return devicerpc.Result{ID: cmd.ID, OK: true, Payload: map[string]string{}}, nil
}

func (p *confirmPhone) sent() []devicerpc.Command {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]devicerpc.Command(nil), p.commands...)
}

// world is one user with a conversation, a phone and the module mounted.
type world struct {
	g            rig
	user         uuid.UUID
	conversation uuid.UUID
	phone        *confirmPhone
	module       *Module
	e            *echo.Echo
}

type worldOptions struct {
	phone       *confirmPhone
	noDevices   bool
	enforcement bool
	user        uuid.UUID
	// forgetTimeout replaces the confirmed forget's bound when set.
	forgetTimeout time.Duration
}

func newWorld(g rig, o worldOptions) world {
	g.t.Helper()
	if o.phone == nil {
		o.phone = &confirmPhone{}
	}
	if o.user == uuid.Nil {
		o.user = g.user()
	}
	d := g.deps
	if !o.noDevices {
		d.Devices = o.phone
	}
	users := auth.PGUsers(g.pool)
	module, err := New(g.pool, d, Options{
		Identify: auth.Identify(qaKeys, users),
		Gate:     entitlement.Middleware(g.pool, entitlement.MemoryQuery, o.enforcement, nil),
		Limits:   ratelimit.NewStore(),
	})
	require.NoError(g.t, err)
	if o.forgetTimeout > 0 {
		module.forgetTimeout = o.forgetTimeout
	}
	e := httpserver.New(config.Config{}, httpserver.Deps{Auth: auth.Middleware(qaKeys, users), Modules: []httpserver.Module{module}})
	return world{g: g, user: o.user, conversation: g.conversation(o.user), phone: o.phone, module: module, e: e}
}

// runner is the assistant's runner for this world's user on vault.
func (w world) runner(vault uuid.UUID) *Runner {
	d := w.g.deps
	d.Devices = w.phone
	return NewRunner(d, w.user, vault, nil, &w.conversation, nil)
}

// proposeCalendar makes a pending calendar card the way the assistant does.
func (w world) proposeCalendar(title string) uuid.UUID {
	w.g.t.Helper()
	w.allowCalendar(true, true)
	result := w.runner(w.user).Run(context.Background(), "calendar_create", calendarArgs(title))
	require.NotNil(w.g.t, result.Action, result.ModelContent)
	return result.Action.ID.UUID
}

func (w world) proposeForget(vault, memoryID uuid.UUID) uuid.UUID {
	w.g.t.Helper()
	result := w.runner(vault).Run(context.Background(), "memory_forget", map[string]any{"memoryID": memoryID.String()})
	require.NotNil(w.g.t, result.Action, result.ModelContent)
	return result.Action.ID.UUID
}

func (w world) allowCalendar(allowed, writes bool) {
	w.g.exec(`INSERT INTO apple_consent (tenant_id, domain, allowed, allow_writes, updated_at) VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (tenant_id, domain) DO UPDATE SET allowed = EXCLUDED.allowed, allow_writes = EXCLUDED.allow_writes`,
		w.user, devicetools.DomainCalendar, allowed, writes)
}

type outcome struct {
	code       int
	resolution Resolution
	body       string
}

func (w world) resolveAs(user, id uuid.UUID, verb string, headers ...string) outcome {
	w.g.t.Helper()
	out, err := w.tryResolveAs(user, id, verb, headers...)
	require.NoError(w.g.t, err, out.body)
	return out
}

// tryResolveAs is resolveAs without assertions, safe off the test goroutine.
func (w world) tryResolveAs(user, id uuid.UUID, verb string, headers ...string) (outcome, error) {
	req := httptest.NewRequest(http.MethodPost, "/v1/assistant/actions/"+id.String()+"/"+verb, nil)
	token, err := scenario.Mint(qaKeys, user)
	if err != nil {
		return outcome{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	w.e.ServeHTTP(rec, req)
	out := outcome{code: rec.Code, body: rec.Body.String()}
	if rec.Code == http.StatusOK {
		err = json.Unmarshal(rec.Body.Bytes(), &out.resolution)
	}
	return out, err
}

func (w world) confirm(id uuid.UUID, headers ...string) outcome {
	return w.resolveAs(w.user, id, "confirm", headers...)
}

func (w world) cancel(id uuid.UUID) outcome { return w.resolveAs(w.user, id, "cancel") }

func (w world) status(id uuid.UUID) string {
	w.g.t.Helper()
	var s string
	require.NoError(w.g.t, w.g.pool.QueryRow(context.Background(), `SELECT status FROM assistant_pending_actions WHERE id = $1`, id).Scan(&s))
	return s
}

type storedMessage struct{ Role, Content string }

func (w world) messages() []storedMessage {
	w.g.t.Helper()
	rows, err := w.g.pool.Query(context.Background(),
		`SELECT role, content FROM conversation_messages WHERE conversation_id = $1 ORDER BY created_at`, w.conversation)
	require.NoError(w.g.t, err)
	defer rows.Close()
	var out []storedMessage
	for rows.Next() {
		var m storedMessage
		require.NoError(w.g.t, rows.Scan(&m.Role, &m.Content))
		out = append(out, m)
	}
	require.NoError(w.g.t, rows.Err())
	return out
}

func (w world) memoryExists(id uuid.UUID) bool {
	return w.g.count(`SELECT count(*) FROM memories WHERE id = $1`, id) == 1
}

func (w world) argsAndSummary(id uuid.UUID) (string, string) {
	w.g.t.Helper()
	var args, summary string
	require.NoError(w.g.t, w.g.pool.QueryRow(context.Background(),
		`SELECT args::text, summary FROM assistant_pending_actions WHERE id = $1`, id).Scan(&args, &summary))
	return args, summary
}

// MARK: - Confirm

func TestConfirmRunsStoredArgsOnce(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{})
	id := w.proposeCalendar("Dentist")
	jobsBefore := w.g.count(`SELECT count(*) FROM memory_index_jobs`)

	out := w.confirm(id)

	require.Equal(t, http.StatusOK, out.code, out.body)
	require.Equal(t, StatusConfirmed, out.resolution.Status)
	require.Equal(t, "Added “Dentist” to your calendar.", out.resolution.Message)
	commands := w.phone.sent()
	require.Len(t, commands, 1)
	require.Equal(t, devicerpc.KindCalendarCreate, commands[0].Kind)
	require.Equal(t, "Dentist", commands[0].Payload["title"])
	require.Equal(t, calendarStart, commands[0].Payload["start"])
	require.Equal(t, "confirmed", w.status(id))
	require.Equal(t, []storedMessage{{Role: "assistant", Content: out.resolution.Message}}, w.messages())
	require.Equal(t, jobsBefore, w.g.count(`SELECT count(*) FROM memory_index_jobs`))
}

func TestReplayDoesNothing(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{})
	id := w.proposeCalendar("Dentist")

	w.confirm(id)
	second := w.confirm(id)

	require.Equal(t, StatusAlreadyResolved, second.resolution.Status)
	require.Len(t, w.phone.sent(), 1)
}

func TestConcurrentConfirmsRunOnce(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{phone: &confirmPhone{delay: 300 * time.Millisecond}})
	id := w.proposeCalendar("Dentist")

	var wg sync.WaitGroup
	results := make([]outcome, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = w.tryResolveAs(w.user, id, "confirm")
		}()
	}
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	statuses := []string{string(results[0].resolution.Status), string(results[1].resolution.Status)}
	sort.Strings(statuses)
	require.Equal(t, []string{"alreadyResolved", "confirmed"}, statuses)
	require.Len(t, w.phone.sent(), 1)
}

func TestExpiredConfirmDoesNothing(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{})
	id := w.proposeCalendar("Dentist")
	w.g.exec(`UPDATE assistant_pending_actions SET expires_at = now() - interval '1 minute' WHERE id = $1`, id)

	out := w.confirm(id)

	require.Equal(t, StatusExpired, out.resolution.Status)
	require.Equal(t, "This request expired. Ask again if you still want it.", out.resolution.Message)
	require.Empty(t, w.phone.sent())
	require.Equal(t, "pending", w.status(id))
	require.Empty(t, w.messages())
}

func TestCancelThenConfirmDoesNothing(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{})
	id := w.proposeCalendar("Dentist")

	cancelled := w.cancel(id)
	require.Equal(t, http.StatusOK, cancelled.code, cancelled.body)
	require.Equal(t, StatusCancelled, cancelled.resolution.Status)
	confirmed := w.confirm(id)

	require.Equal(t, StatusAlreadyResolved, confirmed.resolution.Status)
	require.Empty(t, w.phone.sent())
	require.Equal(t, "cancelled", w.status(id))
	// Cancelling twice is also resolved already.
	again := w.cancel(id)
	require.Equal(t, StatusAlreadyResolved, again.resolution.Status)
	require.Equal(t, "This was already dealt with.", again.resolution.Message)
}

func TestOtherTenantGets404(t *testing.T) {
	g := newRig(t)
	w := newWorld(g, worldOptions{})
	id := w.proposeCalendar("Dentist")
	intruder := g.user()

	for _, verb := range []string{"confirm", "cancel"} {
		out := w.resolveAs(intruder, id, verb)
		require.Equal(t, http.StatusNotFound, out.code, verb)
		require.Contains(t, out.body, "action not found")
	}
	require.Equal(t, http.StatusNotFound, w.confirm(uuid.New()).code)
	require.Equal(t, http.StatusNotFound, w.cancel(uuid.New()).code)
	bad := httptest.NewRequest(http.MethodPost, "/v1/assistant/actions/not-a-uuid/confirm", nil)
	token, err := scenario.Mint(qaKeys, w.user)
	require.NoError(t, err)
	bad.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	w.e.ServeHTTP(rec, bad)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Empty(t, w.phone.sent())
	require.Equal(t, "pending", w.status(id))
	require.Empty(t, w.messages())
}

func TestConsentRevokedBeforeConfirmFails(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{})
	id := w.proposeCalendar("Dentist")
	w.allowCalendar(false, false)

	out := w.confirm(id)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, "Calendar changes are turned off, so nothing was added. Allow them in Data Access and ask again.", out.resolution.Message)
	require.Empty(t, w.phone.sent())
	require.Equal(t, "failed", w.status(id))
}

// Ruling 26: a timeout is failed, never queued, and a retry does not run it again.
func TestAFailedExecutionStaysFailedAndARetryDoesNothing(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{phone: &confirmPhone{timeout: true}})
	id := w.proposeCalendar("Dentist")

	first := w.confirm(id)
	require.Equal(t, StatusFailed, first.resolution.Status)
	require.Equal(t, "Your iPhone didn't answer, so I can't tell whether the event was added. Check Calendar before trying again.", first.resolution.Message)
	require.NotContains(t, strings.ToLower(first.resolution.Message), "queued")
	require.Equal(t, "failed", w.status(id))

	retry := w.confirm(id)
	require.Equal(t, StatusAlreadyResolved, retry.resolution.Status)
	require.Len(t, w.phone.sent(), 1)
	require.Zero(t, w.g.count(`SELECT count(*) FROM device_command_queue WHERE tenant_id = $1`, w.user))
}

// The phone answered but could not add the event, for a reason other than
// consent.
func TestAPhoneThatCannotAddTheEventFails(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{phone: &confirmPhone{failWith: "calendar is read-only"}})
	id := w.proposeCalendar("Dentist")

	out := w.confirm(id)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, "The event could not be added. Check Calendar before trying again.", out.resolution.Message)
	require.Len(t, w.phone.sent(), 1)
	require.Equal(t, "failed", w.status(id))
	require.Equal(t, []storedMessage{{Role: "assistant", Content: out.resolution.Message}}, w.messages())
}

// MARK: - Forget

func TestConfirmForgetDeletesTheMemory(t *testing.T) {
	g := newRig(t)
	w := newWorld(g, worldOptions{})
	memoryID := g.memory(w.user, "I like tea", nil, time.Now())
	id := w.proposeForget(w.user, memoryID)

	out := w.confirm(id)

	require.Equal(t, StatusConfirmed, out.resolution.Status)
	require.Equal(t, "Forgot that memory.", out.resolution.Message)
	require.Zero(t, g.memories(w.user))
	require.Equal(t, "confirmed", w.status(id))
}

func TestConfirmForgetOfAMemoryAlreadyGoneFails(t *testing.T) {
	g := newRig(t)
	w := newWorld(g, worldOptions{})
	memoryID := g.memory(w.user, "I like tea", nil, time.Now())
	id := w.proposeForget(w.user, memoryID)
	g.exec(`DELETE FROM memories WHERE id = $1`, memoryID)

	out := w.confirm(id)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, "That memory no longer exists, so nothing was forgotten.", out.resolution.Message)
	require.Equal(t, "failed", w.status(id))
}

// Ruling 25: confirm acts on the stored vault and ignores X-Vault-ID.
func TestConfirmActsOnTheStoredVaultNotTheHeader(t *testing.T) {
	g := newRig(t)
	w := newWorld(g, worldOptions{})
	other := g.user()
	shared := g.sharedVault(other, w.user, "editor")
	own := g.memory(w.user, "mine", nil, time.Now())
	theirs := g.memory(shared, "shared", nil, time.Now())
	id := w.proposeForget(w.user, own)

	out := w.confirm(id, vaultaccess.Header, shared.String())

	require.Equal(t, StatusConfirmed, out.resolution.Status)
	require.False(t, w.memoryExists(own))
	require.True(t, w.memoryExists(theirs))
}

func TestAMemberDemotedBeforeConfirmFailsWithNoEffect(t *testing.T) {
	g := newRig(t)
	owner, editor := g.user(), g.user()
	vault := g.sharedVault(owner, editor, "editor")
	memoryID := g.memory(vault, "team note", nil, time.Now())
	w := newWorld(g, worldOptions{user: editor})
	id := w.proposeForget(vault, memoryID)

	g.exec(`UPDATE vault_memberships SET role = 'viewer' WHERE vault_id = $1 AND user_id = $2`, vault, editor)
	out := w.confirm(id)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, "You no longer have access to make that change, so nothing was done.", out.resolution.Message)
	require.True(t, w.memoryExists(memoryID))
	require.Equal(t, "failed", w.status(id))
}

func TestForgetInATeamVaultDeletesAndDoesNotThrow(t *testing.T) {
	g := newRig(t)
	owner, editor := g.user(), g.user()
	vault := g.sharedVault(owner, editor, "editor")
	memoryID := g.memory(vault, "team note", nil, time.Now())
	w := newWorld(g, worldOptions{user: editor})
	id := w.proposeForget(vault, memoryID)

	out := w.confirm(id)

	require.Equal(t, StatusConfirmed, out.resolution.Status)
	require.False(t, w.memoryExists(memoryID))
	require.Equal(t, vault, g.pending(editor)[0].VaultID)
}

// MARK: - Disconnects

// Ruling 31: once claimed, a request whose context is cancelled still runs the
// action and keeps its bookkeeping.
func TestACancelledRequestStillFinishesTheRun(t *testing.T) {
	g := newRig(t)

	// The phone is still answering when the request goes away.
	for _, timeout := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		// The request goes away the moment the phone has the command, while
		// it is still answering.
		phone := &confirmPhone{timeout: timeout, delay: 200 * time.Millisecond, started: cancel}
		w := newWorld(g, worldOptions{phone: phone})
		id := w.proposeCalendar("Dentist")

		res, err := w.module.confirmAction(ctx, id, w.user)
		// Read before the cleanup cancel, so it shows the started hook cancelled
		// the request while the phone was answering.
		cancelledMidCall := ctx.Err()
		cancel()

		require.NoError(t, err)
		require.ErrorIs(t, cancelledMidCall, context.Canceled, "the request was never cancelled mid-call")
		require.NotEqual(t, StatusAlreadyResolved, res.Status)

		require.Len(t, w.phone.sent(), 1)
		want := "confirmed"
		if timeout {
			want = "failed"
		}
		require.Equal(t, want, w.status(id))
		require.Len(t, w.messages(), 1)
	}

	// Cancelled right after the claim, before anything ran.
	w := newWorld(g, worldOptions{})
	memoryID := g.memory(w.user, "I like tea", nil, time.Now())
	id := w.proposeForget(w.user, memoryID)
	ctx, cancel := context.WithCancel(context.Background())
	w.module.afterClaim = cancel

	res, err := w.module.confirmAction(ctx, id, w.user)

	require.NoError(t, err)
	require.Equal(t, StatusConfirmed, res.Status)
	require.Error(t, ctx.Err())
	require.False(t, w.memoryExists(memoryID))
	require.Equal(t, "confirmed", w.status(id))
	require.Equal(t, []storedMessage{{Role: "assistant", Content: "Forgot that memory."}}, w.messages())
}

func TestReplayTextIsTrueForEachState(t *testing.T) {
	g := newRig(t)

	confirmed := newWorld(g, worldOptions{})
	first := confirmed.proposeCalendar("Dentist")
	confirmed.confirm(first)
	require.Equal(t, Resolution{Status: StatusAlreadyResolved, Message: "This was already confirmed. If it didn't happen, ask again."},
		confirmed.confirm(first).resolution)

	cancelled := newWorld(g, worldOptions{})
	second := cancelled.proposeCalendar("Dentist")
	cancelled.cancel(second)
	require.Equal(t, Resolution{Status: StatusAlreadyResolved, Message: "This was cancelled earlier, so nothing was changed."},
		cancelled.confirm(second).resolution)

	failed := newWorld(g, worldOptions{phone: &confirmPhone{timeout: true}})
	third := failed.proposeCalendar("Dentist")
	failed.confirm(third)
	require.Equal(t, Resolution{Status: StatusAlreadyResolved, Message: "This already failed and nothing was changed. Ask again if you still want it."},
		failed.confirm(third).resolution)

	expired := newWorld(g, worldOptions{})
	fourth := expired.proposeCalendar("Dentist")
	g.exec(`UPDATE assistant_pending_actions SET expires_at = now() - interval '1 minute' WHERE id = $1`, fourth)
	require.Equal(t, Resolution{Status: StatusExpired, Message: "This request expired. Ask again if you still want it."},
		expired.confirm(fourth).resolution)
}

// MARK: - The conversation

func TestAConfirmMovesTheConversationUp(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{})
	id := w.proposeCalendar("Dentist")
	w.g.exec(`UPDATE conversations SET updated_at = now() - interval '1 day' WHERE id = $1`, w.conversation)

	w.confirm(id)

	var updated time.Time
	require.NoError(t, w.g.pool.QueryRow(context.Background(), `SELECT updated_at FROM conversations WHERE id = $1`, w.conversation).Scan(&updated))
	require.Greater(t, updated, time.Now().Add(-time.Minute))
}

func TestACancelLeavesALineInTheConversation(t *testing.T) {
	w := newWorld(newRig(t), worldOptions{})
	id := w.proposeCalendar("Dentist")
	w.g.exec(`UPDATE conversations SET updated_at = now() - interval '1 day' WHERE id = $1`, w.conversation)

	cancelled := w.cancel(id)
	w.cancel(id)

	require.Equal(t, Resolution{Status: StatusCancelled, Message: "Cancelled. Nothing was changed."}, cancelled.resolution)
	require.Equal(t, []storedMessage{{Role: "assistant", Content: "Cancelled. Nothing was changed."}}, w.messages())
	var updated time.Time
	require.NoError(t, w.g.pool.QueryRow(context.Background(), `SELECT updated_at FROM conversations WHERE id = $1`, w.conversation).Scan(&updated))
	require.Greater(t, updated, time.Now().Add(-time.Minute))
}

// Ruling 28: the appended line goes straight to the transcript.
func TestTheConfirmationLineIsNeverIndexed(t *testing.T) {
	g := newRig(t)
	w := newWorld(g, worldOptions{})
	memoryID := g.memory(w.user, "I like tea", nil, time.Now())
	forget := w.proposeForget(w.user, memoryID)
	event := w.proposeCalendar("Dentist")
	cancelLater := w.proposeCalendar("Other")

	w.confirm(forget)
	w.confirm(event)
	w.cancel(cancelLater)

	require.Len(t, w.messages(), 3)
	require.Zero(t, g.count(`SELECT count(*) FROM memory_index_jobs WHERE tenant_id = $1`, w.user))
}

func TestAResolvedActionKeepsNoArgsOrSummary(t *testing.T) {
	g := newRig(t)
	w := newWorld(g, worldOptions{})
	confirmed := w.proposeCalendar("Confirmed")
	cancelled := w.proposeCalendar("Cancelled")
	args, _ := w.argsAndSummary(confirmed)
	require.Contains(t, args, "Confirmed")

	require.Equal(t, StatusConfirmed, w.confirm(confirmed).resolution.Status)
	w.cancel(cancelled)

	// Execution read the values the claim cleared.
	require.Equal(t, "Confirmed", w.phone.sent()[0].Payload["title"])
	for _, id := range []uuid.UUID{confirmed, cancelled} {
		args, summary := w.argsAndSummary(id)
		require.Equal(t, "{}", args)
		require.Empty(t, summary)
	}

	failing := newWorld(g, worldOptions{phone: &confirmPhone{timeout: true}})
	failed := failing.proposeCalendar("Failed")
	require.Equal(t, StatusFailed, failing.confirm(failed).resolution.Status)
	require.Equal(t, "failed", failing.status(failed))
	args, summary := failing.argsAndSummary(failed)
	require.Equal(t, "{}", args)
	require.Empty(t, summary)
}

func TestALateTapOnASweptCardIs404(t *testing.T) {
	g := newRig(t)
	w := newWorld(g, worldOptions{})
	old := uuid.New()
	require.NoError(t, g.deps.Store.Insert(context.Background(), PendingAction{
		ID: old, TenantID: w.user, VaultID: w.user, ConversationID: w.conversation, Tool: "calendar_create",
		Args: calendarArgs("Old"), Summary: "s", ExpiresAt: time.Now().Add(-48 * time.Hour),
	}))

	w.proposeCalendar("Dentist")

	require.Equal(t, http.StatusNotFound, w.confirm(old).code)
	require.Empty(t, w.phone.sent())
}

// Ruling 29: resolving a card is not a chat message. A free account can do it,
// and it moves no counter.
func TestConfirmAndCancelNeverConsumeAFreeMessageOrGrant(t *testing.T) {
	g := newRig(t)
	free := g.user()
	g.exec(`UPDATE users SET tier = 'free' WHERE id = $1`, free)
	w := newWorld(g, worldOptions{user: free, enforcement: true})
	first := w.proposeCalendar("Dentist")
	second := w.proposeCalendar("Other")
	buckets := func() int {
		return g.count(`SELECT count(*) FROM workflow_spend_buckets WHERE scope_key ILIKE '%' || $1 || '%'`, strings.ToLower(free.String()))
	}
	before := buckets()

	confirmed := w.confirm(first)
	cancelled := w.cancel(second)

	require.Equal(t, http.StatusOK, confirmed.code, confirmed.body)
	require.Equal(t, StatusConfirmed, confirmed.resolution.Status)
	require.Equal(t, StatusCancelled, cancelled.resolution.Status)
	require.Equal(t, before, buckets())
	gate := llm.NewFreeLaneGate(g.pool, llm.FreeLaneLimits{PerUserDaily: 20})
	require.Equal(t, int64(20), gate.RemainingToday(context.Background(), free))

	// The entitlement gate is the chat's: an archived account is refused.
	g.exec(`UPDATE users SET tier = 'archived' WHERE id = $1`, free)
	third := w.resolveAs(free, uuid.New(), "confirm")
	require.Equal(t, http.StatusPaymentRequired, third.code)
}

func TestConfirmCalendarWithoutDevicesFailsGenerically(t *testing.T) {
	g := newRig(t)
	// The row can only come from a runner that had a device; the module has none.
	w := newWorld(g, worldOptions{noDevices: true})
	id := w.proposeCalendar("Dentist")

	out := w.confirm(id)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, "That didn't work, and nothing was changed.", out.resolution.Message)
	require.Empty(t, w.phone.sent())
	require.Equal(t, "failed", w.status(id))
	require.Len(t, w.messages(), 1)
}

// MARK: - Wiring

// R87 M1: the routes never mount without their limiter, gate or identity.
func TestNewRefusesToMountWithoutItsMiddleware(t *testing.T) {
	mw := func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	full := Options{Identify: mw, Gate: mw, Limits: ratelimit.NewStore()}
	for name, mutate := range map[string]func(*Options){
		"identify": func(o *Options) { o.Identify = nil },
		"gate":     func(o *Options) { o.Gate = nil },
		"limits":   func(o *Options) { o.Limits = nil },
	} {
		o := full
		mutate(&o)
		module, err := New(nil, Deps{}, o)
		require.Error(t, err, name)
		require.Nil(t, module, name)
	}
	module, err := New(nil, Deps{}, full)
	require.NoError(t, err)
	require.NotNil(t, module)
}

// R87 M2: a confirmed forget is bounded like any tool call. A bound already
// spent fails the forget generically and leaves the memory.
func TestAConfirmedForgetIsBounded(t *testing.T) {
	g := newRig(t)
	require.Equal(t, toolTimeout, mustNew(t, g).forgetTimeout, "the runner's per-tool bound")
	w := newWorld(g, worldOptions{forgetTimeout: time.Nanosecond})
	memoryID := g.memory(w.user, "I like tea", nil, time.Now())
	id := w.proposeForget(w.user, memoryID)

	out := w.confirm(id)

	require.Equal(t, StatusFailed, out.resolution.Status)
	require.Equal(t, "That didn't work, and nothing was changed.", out.resolution.Message)
	require.True(t, w.memoryExists(memoryID))
	require.Equal(t, "failed", w.status(id))
}

func mustNew(t *testing.T, g rig) *Module {
	t.Helper()
	mw := func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	m, err := New(g.pool, g.deps, Options{Identify: mw, Gate: mw, Limits: ratelimit.NewStore()})
	require.NoError(t, err)
	return m
}
