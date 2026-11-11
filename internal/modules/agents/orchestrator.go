package agents

import (
	"context"
	"errors"
	"log/slog"
	"lumina/internal/ids"
	"lumina/internal/llm"
	"lumina/internal/modules/agents/db"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Room message authors (AgentRoomAuthorKind) and chain ends (AgentRoomChainEnd).
const (
	authorHuman  = "human"
	authorAgent  = "agent"
	authorSystem = "system"
)

// speaker is AgentRoomSpeaker: asks one member of a room for its reply. Behind an
// interface so the orchestrator's tests don't call real agents.
type speaker interface {
	reply(ctx context.Context, user uuid.UUID, room db.AgentRoom, member db.AgentRoomMember, systemMessage, message string) (string, *int, error)
}

// chatter is llm.RoutedService's Chat, the RoutedHermesLLMService Swift builds once.
type chatter interface {
	Chat(ctx context.Context, sessionKey, sessionID string, req llm.CompletionRequest) (*llm.ChatResponse, error)
}

// speakerFailure is LiveAgentRoomSpeaker.Failure.
type speakerFailure int

const (
	errNoGateway speakerFailure = iota + 1
	errUnknownInstanceSpeaker
)

func (e speakerFailure) Error() string {
	if e == errNoGateway {
		return "noGateway"
	}
	return "unknownInstance"
}

// liveSpeaker is LiveAgentRoomSpeaker: LuminaVault's own agent as a persona, or
// the user's own Hermes.
type liveSpeaker struct {
	q      *db.Queries
	llm    chatter
	agents *service
}

func (s liveSpeaker) reply(ctx context.Context, user uuid.UUID, room db.AgentRoom, member db.AgentRoomMember, systemMessage, message string) (string, *int, error) {
	// One Hermes session per member per room, so each agent keeps the room's
	// thread and shows up once on the Agents page.
	sessionID := "room-" + room.ID.String() + "-" + member.ID.String()
	switch member.InstanceID {
	case centralID:
		slug := "default"
		if member.Profile != nil {
			slug = *member.Profile
		}
		// The same key the chat path composes in HermesProfileMiddleware.
		key := "pending-" + ids.Upper(user) + ":" + slug
		hermes, err := s.q.HermesProfileID(context.WithoutCancel(ctx), user)
		switch {
		case err == nil:
			key = hermes + ":" + slug
		case !errors.Is(err, pgx.ErrNoRows):
			return "", nil, err
		}
		if s.llm == nil {
			return "", nil, errors.New("agents: no LLM service")
		}
		res, err := s.llm.Chat(ctx, key, sessionID, llm.CompletionRequest{Messages: []llm.CompletionMessage{
			{Role: "system", Content: systemMessage},
			{Role: "user", Content: message},
		}})
		if err != nil {
			return "", nil, err
		}
		var tokens *int
		if res.Raw.Usage != nil {
			tokens = res.Raw.Usage.TotalTokens
		}
		return res.Message.Content, tokens, nil
	case byoID:
		client := s.agents.gatewayClient(ctx, user)
		if client == nil {
			return "", nil, errNoGateway
		}
		return client.chat(ctx, sessionID, "Room: "+room.Title, systemMessage, message)
	default:
		return "", nil, errUnknownInstanceSpeaker
	}
}

// runRegistry is AgentRoomRunRegistry: one running chain per room. Process-local,
// as Swift's: the API runs as one replica.
type runRegistry struct {
	mu      sync.Mutex
	running map[uuid.UUID]bool
}

func newRunRegistry() *runRegistry { return &runRegistry{running: map[uuid.UUID]bool{}} }

func (r *runRegistry) begin(id uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running[id] {
		return false
	}
	r.running[id] = true
	return true
}

func (r *runRegistry) end(id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, id)
}

func (r *runRegistry) isRunning(id uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[id]
}

// orchestrator is AgentRoomOrchestrator: runs one chain of agent turns after the
// user posts in a room. The loop guard, all of it here and nowhere else:
//   - at most maxAgentTurns agent replies per post;
//   - the same agent never answers twice in a row;
//   - the room's token budget, checked before every call;
//   - Stop, re-read from the database between turns;
//   - one chain per room at a time (runRegistry).
type orchestrator struct {
	q        *db.Queries
	speaker  speaker
	registry *runRegistry
	log      *slog.Logger
	now      func() time.Time
}

var (
	errAlreadyRunning = errors.New("alreadyRunning")
	errEmptyMessage   = errors.New("emptyMessage")
)

// post posts body as the user and runs the chain, reporting each step through
// emit. It returns why the chain ended.
//
// ctx ending is Swift's task cancellation: the loop stops before the next turn
// and the call in flight is cancelled. Database work runs without it, as Fluent
// queries in a cancelled task still complete.
func (o *orchestrator) post(ctx context.Context, user uuid.UUID, room db.AgentRoom, body string, emit func(AgentRoomStreamEvent)) (AgentRoomStreamEventReason, error) {
	text := swiftstr.TrimWhitespacesAndNewlines(body)
	if text == "" || swiftstr.Count(text) > maxBodyLength {
		return "", errEmptyMessage
	}
	if !o.registry.begin(room.ID) {
		return "", errAlreadyRunning
	}
	defer o.registry.end(room.ID)
	return o.runChain(ctx, user, room.ID, text, emit)
}

func (o *orchestrator) runChain(ctx context.Context, user, roomID uuid.UUID, text string, emit func(AgentRoomStreamEvent)) (AgentRoomStreamEventReason, error) {
	dbx := context.WithoutCancel(ctx)
	chainStart := o.now()
	members, err := o.q.RoomMembers(dbx, roomID)
	if err != nil {
		return "", err
	}
	policy := make([]policyMember, len(members))
	byID := make(map[uuid.UUID]db.AgentRoomMember, len(members))
	for i, m := range members {
		policy[i] = policyMember{id: m.ID, handle: m.Handle, respondMode: storedMode(m.RespondMode)}
		byID[m.ID] = m
	}

	saved, err := o.save(dbx, roomID, authorHuman, nil, text, nil)
	if err != nil {
		return "", err
	}
	emit(AgentRoomStreamEvent{Kind: Message, Message: &saved})

	queue := firstResponders(text, policy)
	var lastSpeaker *uuid.UUID
	turns := 0
	for len(queue) > 0 {
		if ctx.Err() != nil {
			return Stopped, nil
		}
		current, err := o.q.FindRoom(dbx, roomID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Stopped, nil
		}
		if err != nil {
			return "", err
		}
		if current.StopRequestedAt != nil && !current.StopRequestedAt.Before(chainStart) {
			return o.end(dbx, Stopped, roomID, "Stopped.", emit)
		}
		if turns >= maxAgentTurns {
			return o.end(dbx, TurnCap, roomID,
				"Paused after "+strconv.Itoa(maxAgentTurns)+" agent replies. Mention an agent to carry on.", emit)
		}
		if current.SpentTokens >= current.TokenBudget {
			return o.end(dbx, Budget, roomID, "This room's token budget is spent.", emit)
		}

		memberID := queue[0]
		queue = queue[1:]
		member, ok := byID[memberID]
		if (lastSpeaker != nil && memberID == *lastSpeaker) || !ok {
			continue
		}

		mid := wire.UUID{UUID: memberID}
		emit(AgentRoomStreamEvent{Kind: Thinking, MemberID: &mid})
		turns++
		lastSpeaker = &memberID
		reply, err := o.turn(ctx, user, current, member, members, byID)
		if err != nil {
			o.log.WarnContext(ctx, "agent room turn failed", "room", ids.Upper(roomID), "member", ids.Upper(memberID), "error", err)
			note, err := o.save(dbx, roomID, authorSystem, &memberID, "@"+member.Handle+" could not answer: "+describeTurnError(err), nil)
			if err != nil {
				return "", err
			}
			emit(AgentRoomStreamEvent{Kind: Message, Message: &note, MemberID: &mid})
			continue
		}
		emit(AgentRoomStreamEvent{Kind: Message, Message: &reply.saved, MemberID: &mid})
		queue = enqueueHandovers(reply.text, memberID, policy, queue)
	}
	return Idle, nil
}

type turnResult struct {
	text  string
	saved AgentRoomMessageDTO
}

// turn is the do block of one agent turn: everything in it that fails becomes the
// "could not answer" note.
func (o *orchestrator) turn(ctx context.Context, user uuid.UUID, current db.AgentRoom, member db.AgentRoomMember, members []db.AgentRoomMember, byID map[uuid.UUID]db.AgentRoomMember) (turnResult, error) {
	dbx := context.WithoutCancel(ctx)
	message, err := o.newMessages(dbx, member.ID, current.ID, byID)
	if err != nil {
		return turnResult{}, err
	}
	reply, tokens, err := o.speaker.reply(ctx, user, current, member, preamble(current, member, members), message)
	if err != nil {
		return turnResult{}, err
	}
	spent := max(1, (swiftstr.Count(message)+swiftstr.Count(reply))/4)
	if tokens != nil {
		spent = *tokens
	}
	if err := o.q.SetSpentTokens(dbx, db.SetSpentTokensParams{ID: current.ID, SpentTokens: current.SpentTokens + int64(spent), UpdatedAt: ptr(o.now())}); err != nil {
		return turnResult{}, err
	}
	n := int64(spent)
	saved, err := o.save(dbx, current.ID, authorAgent, &member.ID, reply, &n)
	if err != nil {
		return turnResult{}, err
	}
	return turnResult{text: reply, saved: saved}, nil
}

// newMessages is what a member hasn't seen yet: everything since its own last
// reply, or the recent window on its first turn.
func (o *orchestrator) newMessages(ctx context.Context, memberID, roomID uuid.UUID, byID map[uuid.UUID]db.AgentRoomMember) (string, error) {
	last, err := o.q.LastOwnReply(ctx, db.LastOwnReplyParams{RoomID: roomID, MemberID: &memberID})
	if err != nil {
		return "", err
	}
	var rows []db.MessagesAfterRow
	if len(last) > 0 && last[0] != nil {
		rows, err = o.q.MessagesAfter(ctx, db.MessagesAfterParams{RoomID: roomID, Since: last[0]})
	} else {
		var recent []db.RecentMessagesRow
		recent, err = o.q.RecentMessages(ctx, roomID)
		for _, r := range recent {
			rows = append(rows, db.MessagesAfterRow(r))
		}
	}
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		switch storedAuthor(row.AuthorKind) {
		case authorHuman:
			parts = append(parts, "User: "+row.Body)
		case authorAgent:
			handle := "agent"
			if row.MemberID != nil {
				if m, ok := byID[*row.MemberID]; ok {
					handle = m.Handle
				}
			}
			parts = append(parts, "@"+handle+": "+row.Body)
		default:
			parts = append(parts, "(room: "+row.Body+")")
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// preamble is AgentRoomOrchestrator.preamble, the system message for one turn.
func preamble(room db.AgentRoom, speaker db.AgentRoomMember, members []db.AgentRoomMember) string {
	var others []string
	for _, m := range members {
		if m.ID != speaker.ID {
			others = append(others, "@"+m.Handle+" ("+m.DisplayName+")")
		}
	}
	with := ""
	if len(others) > 0 {
		with = " and these agents: " + strings.Join(others, ", ")
	}
	return "You are " + speaker.DisplayName + ", @" + speaker.Handle + `, in a LuminaVault room called "` + room.Title + `" ` +
		"with the user" + with + ". " +
		"The message below is what was said in the room since you last spoke. " +
		"Reply as yourself only, and keep it short. Do not write lines for the user or the other agents. " +
		"To hand the conversation to another agent, write their @handle; otherwise do not mention them."
}

func (o *orchestrator) save(ctx context.Context, roomID uuid.UUID, kind string, memberID *uuid.UUID, body string, tokens *int64) (AgentRoomMessageDTO, error) {
	row := db.AgentRoomMessage{
		ID: uuid.New(), RoomID: roomID, AuthorKind: kind, MemberID: memberID, Body: body, Tokens: tokens, CreatedAt: ptr(o.now()),
	}
	if err := o.q.InsertMessage(ctx, db.InsertMessageParams(row)); err != nil {
		return AgentRoomMessageDTO{}, err
	}
	return messageDTO(row), nil
}

func (o *orchestrator) end(ctx context.Context, reason AgentRoomStreamEventReason, roomID uuid.UUID, note string, emit func(AgentRoomStreamEvent)) (AgentRoomStreamEventReason, error) {
	saved, err := o.save(ctx, roomID, authorSystem, nil, note, nil)
	if err != nil {
		return "", err
	}
	emit(AgentRoomStreamEvent{Kind: Message, Message: &saved})
	return reason, nil
}

// describeTurnError is AgentRoomOrchestrator.describe.
func describeTurnError(err error) string {
	if errors.Is(err, errNoGateway) {
		return "your Hermes is no longer linked"
	}
	var g *gatewayError
	if errors.As(err, &g) {
		switch g.kind {
		case failUnreachable:
			return "it did not answer"
		case failHTTP:
			return "it answered HTTP " + strconv.Itoa(g.status)
		}
	}
	return "the call failed"
}

// storedMode is AgentRoomRespondMode(rawValue:) ?? .mention.
func storedMode(raw string) respondMode {
	if raw == string(modeEveryMessage) {
		return modeEveryMessage
	}
	return modeMention
}

// storedAuthor is AgentRoomAuthorKind(rawValue:) ?? .system.
func storedAuthor(raw string) string {
	switch raw {
	case authorHuman, authorAgent:
		return raw
	default:
		return authorSystem
	}
}

func messageDTO(row db.AgentRoomMessage) AgentRoomMessageDTO {
	out := AgentRoomMessageDTO{
		Id: wire.UUID{UUID: row.ID}, AuthorKind: AgentRoomAuthorKind(storedAuthor(row.AuthorKind)), Body: row.Body,
		CreatedAt: wireTime(row.CreatedAt),
	}
	if row.MemberID != nil {
		out.MemberID = &wire.UUID{UUID: *row.MemberID}
	}
	if row.Tokens != nil {
		n := int(*row.Tokens)
		out.Tokens = &n
	}
	return out
}

func ptr[T any](v T) *T { return &v }
