package agents

import (
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// AgentRoomTurnPolicy: who speaks next in a room, and when a chain of agent turns
// must end. Pure, so the loop guard is testable without agents or a database.
const (
	maxAgentTurns   = 6  // agent replies allowed per message the user posts
	firstTurnWindow = 20 // messages a member sees on its first turn in a room
	maxBodyLength   = 8000
	maxMembers      = 8
	maxTitleLength  = 120
	messagePage     = 200
	roomTokenBudget = 200_000
)

// policyMember is AgentRoomTurnPolicy.Member.
type policyMember struct {
	id          uuid.UUID
	handle      string
	respondMode respondMode
}

// mentions are the members named as @handle in text, in order of first mention.
// A handle must end at a non-handle character: @ops doesn't match inside @opsbot.
func mentions(text string, members []policyMember) []uuid.UUID {
	lowered := swiftLower(text)
	type hit struct {
		id  uuid.UUID
		pos int
	}
	var hits []hit
	for _, m := range members {
		needle := "@" + swiftLower(m.handle)
		from := 0
		for {
			i := strings.Index(lowered[from:], needle)
			if i < 0 {
				break
			}
			start := from + i
			next := start + len(needle)
			// Foundation's search matches whole Characters: a needle that ends inside
			// one (a combining mark after it) is no match there.
			if next == len(lowered) || (atCharacterBoundary(lowered, next) && !isHandleCharacter(lowered[next:next+swiftstr.CharacterLen(lowered[next:])])) {
				hits = append(hits, hit{m.id, start})
				break
			}
			from = next
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].pos < hits[j].pos })
	out := make([]uuid.UUID, len(hits))
	for i, h := range hits {
		out[i] = h.id
	}
	return out
}

// atCharacterBoundary reports whether byte offset i of s starts a Character.
func atCharacterBoundary(s string, i int) bool {
	for at := 0; at < len(s); {
		if at == i {
			return true
		}
		if at > i {
			return false
		}
		at += swiftstr.CharacterLen(s[at:])
	}
	return i == len(s)
}

// firstResponders answer a message the user just posted: whoever it names or,
// when it names nobody, the members set to answer every message.
func firstResponders(text string, members []policyMember) []uuid.UUID {
	if named := mentions(text, members); len(named) > 0 {
		return named
	}
	out := []uuid.UUID{}
	for _, m := range members {
		if m.respondMode == modeEveryMessage {
			out = append(out, m.id)
		}
	}
	return out
}

// enqueueHandovers adds the members an agent's reply hands over to. The speaker
// never queues itself, and nobody is queued twice.
func enqueueHandovers(reply string, speaker uuid.UUID, members []policyMember, queue []uuid.UUID) []uuid.UUID {
	for _, id := range mentions(reply, members) {
		if id != speaker && !containsID(queue, id) {
			queue = append(queue, id)
		}
	}
	return queue
}

func containsID(ids []uuid.UUID, id uuid.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// isHandleCharacter is Character.isLetter || isNumber || "_" || "-".
func isHandleCharacter(c string) bool {
	return c == "_" || c == "-" || swiftstr.IsLetterOrNumber(c)
}

// swiftLower is String.lowercased(): per-scalar full lowercase mapping. The one
// unconditional SpecialCasing entry, U+0130, becomes "i" plus U+0307, where
// unicode.ToLower gives a bare "i".
func swiftLower(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0x130 {
			b.WriteString("i̇")
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// handleFrom is AgentRoomTurnPolicy.handle(from:): ^[a-z0-9][a-z0-9_-]{0,30}$ from
// a display name or slug.
func handleFrom(raw string) string {
	var out []byte
	for _, c := range swiftstr.Characters(swiftLower(raw)) {
		switch {
		case len(c) == 1 && isHandleByte(c[0]):
			out = append(out, c[0])
		case (c == " " || c == ".") && len(out) > 0 && out[len(out)-1] != '-':
			out = append(out, '-')
		}
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	for len(out) > 0 && (out[0] == '-' || out[0] == '_') {
		out = out[1:]
	}
	if len(out) == 0 {
		return "agent"
	}
	if len(out) > 31 {
		out = out[:31]
	}
	return string(out)
}

func isHandleByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// isValidHandle is AgentRoomTurnPolicy.isValidHandle.
func isValidHandle(h string) bool {
	if h == "" || len(h) > 31 {
		return false
	}
	if c := h[0]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
		return false
	}
	for i := range len(h) {
		if !isHandleByte(h[i]) {
			return false
		}
	}
	return true
}

// seat is one member resolveMembers accepted.
type seat struct {
	instanceID  string
	profile     *string
	handle      string
	displayName string
	respondMode respondMode
}

var (
	errUnknownMember   = wire.NewError(http.StatusBadRequest, "unknown_member")
	errDuplicateMember = wire.NewError(http.StatusBadRequest, "duplicate_member")
	errInvalidHandle   = wire.NewError(http.StatusBadRequest, "invalid_handle")
	errDuplicateHandle = wire.NewError(http.StatusBadRequest, "duplicate_handle")
)

// resolveMembers is AgentRoomsController.resolveMembers: requested members checked
// against what the user can actually seat.
func resolveMembers(requested []memberRequest, available []candidate) ([]seat, error) {
	handles := map[string]bool{}
	seatedByo := false
	out := make([]seat, 0, len(requested))
	for _, req := range requested {
		var cand *candidate
		for i := range available {
			if equalOptional(&available[i].InstanceID, &req.InstanceID) && equalOptional(available[i].Profile, req.Profile) {
				cand = &available[i]
				break
			}
		}
		if cand == nil {
			return nil, errUnknownMember
		}
		if cand.InstanceID == byoID {
			// One linked gateway answers as one profile: a second seat would be the
			// same agent twice.
			if seatedByo {
				return nil, errDuplicateMember
			}
			seatedByo = true
		}
		handle := cand.SuggestedHandle
		if req.Handle != nil {
			if h := swiftLower(swiftstr.TrimWhitespaces(*req.Handle)); h != "" {
				handle = h
			}
		}
		if !isValidHandle(handle) {
			return nil, errInvalidHandle
		}
		if handles[handle] {
			return nil, errDuplicateHandle
		}
		handles[handle] = true
		name := ""
		if req.DisplayName != nil {
			name = swiftstr.TrimWhitespacesAndNewlines(*req.DisplayName)
		}
		if name == "" {
			name = cand.DisplayName
		} else {
			name = swiftstr.Prefix(name, 60)
		}
		mode := modeMention
		if req.RespondMode != nil {
			mode = *req.RespondMode
		}
		out = append(out, seat{instanceID: cand.InstanceID, profile: cand.Profile, handle: handle, displayName: name, respondMode: mode})
	}
	return out, nil
}

// equalOptional is Swift's == on two String?: canonical equivalence, nil only
// equal to nil.
func equalOptional(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return swiftstr.CanonicalKey(*a) == swiftstr.CanonicalKey(*b)
}
