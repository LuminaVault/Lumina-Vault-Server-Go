package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"lumina/internal/embedding"
	"lumina/internal/modules/calendar/db"
	"lumina/internal/swiftstr"
	"lumina/internal/wire"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

const (
	// DefaultBriefMaxDistance is MeetingBriefService.defaultMaxDistance: cosine
	// similarity at least 0.60, tighter than "connects to".
	DefaultBriefMaxDistance = 0.40
	maxBriefMemories        = 5
	// candidateLimit is what is asked of the search before the floor and the cut.
	candidateLimit  = 10
	maxNotesInQuery = 600
	// chunkArmDepth is limit * MemoryChunkRepository.armDepthMultiplier.
	chunkArmDepth = candidateLimit * 4
)

// briefService is MeetingBriefService for GET /v1/calendar/events/{id}/brief.
// The push half (MeetingBriefScheduler) stays in Swift.
type briefService struct {
	q           *db.Queries
	embeddings  embedding.Service
	maxDistance float32
}

type briefEvent struct {
	id        uuid.UUID
	title     string
	notes     *string
	location  *string
	startsAt  time.Time
	endsAt    time.Time
	attendees []CalendarAttendeeDTO
}

// briefMatch is one note past the floor, and the stored form in
// meeting_briefs.memories.
type briefMatch struct {
	id         uuid.UUID
	similarity float64
}

// searchHit is the part of MemorySearchResult the floor reads.
type searchHit struct {
	id       uuid.UUID
	distance float32
}

// attendeesFromJSON is MeetingBriefService.attendees(fromJSON:) for what
// Fluent decodes: empty text is a NULL column. Swift fails the whole request
// when a stored value does not decode.
func attendeesFromJSON(raw string) ([]CalendarAttendeeDTO, error) {
	if raw == "" {
		return []CalendarAttendeeDTO{}, nil
	}
	var stored struct {
		List *[]googleAttendee `json:"list"`
	}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, err
	}
	if stored.List == nil {
		return nil, errors.New("calendar_events.attendees has no list")
	}
	out := make([]CalendarAttendeeDTO, len(*stored.List))
	for i, a := range *stored.List {
		out[i] = CalendarAttendeeDTO{Name: a.DisplayName, Email: a.Email}
	}
	return out, nil
}

// brief is MeetingBriefService.brief: the notes the push promised when there
// was one, else matched now. ok is false when the event is not the tenant's.
func (b *briefService) brief(ctx context.Context, tenant, eventID uuid.UUID) (resp MeetingBriefResponse, ok bool, err error) {
	row, err := b.q.BriefEvent(ctx, db.BriefEventParams{TenantID: tenant, ID: eventID})
	if errors.Is(err, pgx.ErrNoRows) {
		return resp, false, nil
	}
	if err != nil {
		return resp, false, err
	}
	attendees, err := attendeesFromJSON(row.Attendees)
	if err != nil {
		return resp, false, err
	}
	ev := briefEvent{id: row.ID, title: row.Title, notes: row.Notes, location: row.Location, startsAt: row.StartsAt, endsAt: row.EndsAt, attendees: attendees}

	user, err := b.q.UserForBrief(ctx, tenant)
	var selfEmail *string
	switch {
	case err == nil:
		selfEmail = &user.Email
	case !errors.Is(err, pgx.ErrNoRows):
		return resp, false, err
	}
	matches, err := b.stored(ctx, tenant, eventID)
	if err != nil {
		return resp, false, err
	}
	if len(matches) == 0 {
		if matches, err = b.match(ctx, tenant, ev, selfEmail); err != nil {
			return resp, false, err
		}
	}
	idList := make([]uuid.UUID, len(matches))
	for i, m := range matches {
		idList[i] = m.id
	}
	rows := map[uuid.UUID]db.BriefMemoriesRow{}
	if len(idList) > 0 {
		list, err := b.q.BriefMemories(ctx, db.BriefMemoriesParams{TenantID: tenant, Ids: idList})
		if err != nil {
			return resp, false, err
		}
		for _, r := range list {
			rows[r.ID] = r
		}
	}
	// A note rejected or deleted since the push is no longer shown.
	memories := make([]MeetingBriefMemoryDTO, 0, len(matches))
	contents := make([]string, 0, len(matches))
	for _, m := range matches {
		r, ok := rows[m.id]
		if !ok {
			continue
		}
		created := time.Unix(0, 0).UTC()
		if r.CreatedAt != nil {
			created = *r.CreatedAt
		}
		memories = append(memories, MeetingBriefMemoryDTO{
			Id: wire.UUID{UUID: m.id}, Snippet: relatedSnippet(r.Content), Kind: r.Kind,
			CreatedAt: wire.Time{Time: created}, Similarity: m.similarity,
		})
		contents = append(contents, r.Content)
	}
	return MeetingBriefResponse{
		Event: MeetingBriefEventDTO{
			Id: wire.UUID{UUID: ev.id}, Title: ev.title, Location: ev.location,
			StartsAt: wire.Time{Time: ev.startsAt}, EndsAt: wire.Time{Time: ev.endsAt}, Attendees: ev.attendees,
		},
		Memories: memories,
		Why:      whyLine(len(memories), mentionedNames(ev.attendees, selfEmail, contents)),
	}, true, nil
}

// stored reads meeting_briefs.memories. A value that does not decode as a
// whole counts as no brief (try?), which falls through to a live match.
func (b *briefService) stored(ctx context.Context, tenant, eventID uuid.UUID) ([]briefMatch, error) {
	raw, err := b.q.StoredBrief(ctx, db.StoredBriefParams{TenantID: tenant, EventID: eventID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID         *wire.UUID `json:"id"`
		Similarity *float64   `json:"similarity"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, nil //nolint:nilerr // Swift: try? decode
	}
	out := make([]briefMatch, len(list))
	for i, m := range list {
		if m.ID == nil || m.Similarity == nil {
			return nil, nil
		}
		out[i] = briefMatch{id: m.ID.UUID, similarity: *m.Similarity}
	}
	return out, nil
}

// match embeds the event once and keeps the notes within the floor. A failed
// embed fails the request: this path has no lexical fallback in Swift.
func (b *briefService) match(ctx context.Context, tenant uuid.UUID, ev briefEvent, selfEmail *string) ([]briefMatch, error) {
	query := queryText(ev, selfEmail)
	if query == "" {
		return nil, nil
	}
	vector, err := b.embeddings.Embed(ctx, query, tenant)
	if err != nil {
		return nil, err
	}
	hits, err := b.search(ctx, tenant, query, embedding.FormatVector(vector))
	if err != nil {
		return nil, err
	}
	return floorHits(hits, b.maxDistance), nil
}

// search is HybridMemorySearch.search with countHits false: the chunk arm
// (degraded to empty on failure) merged with the document arm, chunk hits
// first, one per memory, at most candidateLimit.
func (b *briefService) search(ctx context.Context, tenant uuid.UUID, query, vec string) ([]searchHit, error) {
	var chunks []searchHit
	rows, err := b.q.SearchChunks(ctx, db.SearchChunksParams{Query: query, Vec: vec, TenantID: tenant, Depth: chunkArmDepth, Lim: candidateLimit})
	if err != nil {
		slog.ErrorContext(ctx, "memory.search.chunkArmFailed", "tenant", tenant, "error", err)
	}
	for _, r := range rows {
		chunks = append(chunks, searchHit{id: r.MemoryID, distance: float32(r.Distance)})
	}
	docs, err := b.q.SearchDocuments(ctx, db.SearchDocumentsParams{Vec: vec, TenantID: tenant, Lim: candidateLimit * 3})
	if err != nil {
		return nil, err
	}
	return mergeHits(chunks, balanced(docs, candidateLimit), candidateLimit), nil
}

// balanced is MemoryRepository.balanced: at most 40% prior model answers,
// backfilled from whatever is left, nearest first.
func balanced(rows []db.SearchDocumentsRow, limit int) []searchHit {
	outputLimit := max(1, int(float64(limit)*0.4))
	factLimit := max(0, limit-outputLimit)
	selected := make([]db.SearchDocumentsRow, 0, limit)
	chosen := map[uuid.UUID]bool{}
	facts, outputs := 0, 0
	for _, r := range rows {
		isOutput := r.OriginKind == "chat" || r.OriginKind == "query"
		if !isOutput && facts < factLimit {
			facts++
			selected = append(selected, r)
			chosen[r.ID] = true
		}
	}
	for _, r := range rows {
		isOutput := r.OriginKind == "chat" || r.OriginKind == "query"
		if isOutput && outputs < outputLimit {
			outputs++
			selected = append(selected, r)
			chosen[r.ID] = true
		}
	}
	room := limit - len(selected)
	for _, r := range rows {
		if room <= 0 {
			break
		}
		if !chosen[r.ID] {
			selected = append(selected, r)
			room--
		}
	}
	sort.SliceStable(selected, func(i, j int) bool { return float32(selected[i].Distance) < float32(selected[j].Distance) })
	out := make([]searchHit, len(selected))
	for i, r := range selected {
		out[i] = searchHit{id: r.ID, distance: float32(r.Distance)}
	}
	return out
}

// mergeHits is HybridMemorySearch.merge. A chunk hit wins over a document hit
// for the same memory even when it is further away. A lexical-only chunk
// (distance 1.0) therefore hides a document hit that would have passed the floor.
func mergeHits(chunks, docs []searchHit, limit int) []searchHit {
	seen := map[uuid.UUID]bool{}
	out := make([]searchHit, 0, limit)
	for _, list := range [][]searchHit{chunks, docs} {
		for _, h := range list {
			if len(out) == limit {
				return out
			}
			if !seen[h.id] {
				seen[h.id] = true
				out = append(out, h)
			}
		}
	}
	return out
}

// floorHits is MeetingBriefService.floor: within maxDistance, one per memory
// (the nearest), nearest first, at most five.
func floorHits(hits []searchHit, maxDistance float32) []briefMatch {
	best := map[uuid.UUID]float32{}
	for _, h := range hits {
		if h.distance > maxDistance {
			continue
		}
		if d, ok := best[h.id]; !ok || h.distance < d {
			best[h.id] = h.distance
		}
	}
	list := make([]searchHit, 0, len(best))
	for id, d := range best {
		list = append(list, searchHit{id, d})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].distance == list[j].distance {
			return strings.ToUpper(list[i].id.String()) < strings.ToUpper(list[j].id.String())
		}
		return list[i].distance < list[j].distance
	})
	if len(list) > maxBriefMemories {
		list = list[:maxBriefMemories]
	}
	out := make([]briefMatch, len(list))
	for i, h := range list {
		out[i] = briefMatch{h.id, max(0, min(1, 1-float64(h.distance)))}
	}
	return out
}

// queryText is MeetingBriefService.queryText: title, attendee names, then
// notes, one line each, blanks dropped.
func queryText(ev briefEvent, selfEmail *string) string {
	var names []string
	for _, p := range people(ev.attendees, selfEmail) {
		names = append(names, p.full)
	}
	notes := ""
	if ev.notes != nil {
		notes = swiftstr.Prefix(flatten(*ev.notes), maxNotesInQuery)
	}
	var parts []string
	for _, s := range []string{flatten(ev.title), strings.Join(names, ", "), notes} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

type person struct{ full, first string }

// people is MeetingBriefService.people: named attendees other than the user,
// de-duplicated.
func people(attendees []CalendarAttendeeDTO, selfEmail *string) []person {
	me := ""
	if selfEmail != nil {
		me = strings.ToLower(*selfEmail)
	}
	seen := map[string]bool{}
	var out []person
	for _, a := range attendees {
		if a.Email != nil && selfEmail != nil && strings.ToLower(*a.Email) == me {
			continue
		}
		name := ""
		if a.Name != nil {
			name = *a.Name
		}
		full := flatten(name)
		if full == "" || seen[fold(full)] {
			continue
		}
		seen[fold(full)] = true
		first, _, _ := strings.Cut(full, " ")
		out = append(out, person{full, first})
	}
	return out
}

type mention struct {
	name  string
	count int
}

// mentionedNames counts, per attendee, the notes naming them: most first, ties
// in invite order, shown by first name.
func mentionedNames(attendees []CalendarAttendeeDTO, selfEmail *string, contents []string) []mention {
	folded := make([]string, len(contents))
	for i, c := range contents {
		folded[i] = fold(c)
	}
	var out []mention
	for _, p := range people(attendees, selfEmail) {
		var needles []string
		for _, n := range []string{fold(p.full), fold(p.first)} {
			if swiftstr.Count(n) >= 2 {
				needles = append(needles, n)
			}
		}
		count := 0
		for _, content := range folded {
			for _, n := range needles {
				if containsWord(n, content) {
					count++
					break
				}
			}
		}
		if count > 0 {
			out = append(out, mention{p.first, count})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].count > out[j].count })
	return out
}

// whyLine is MeetingBriefService.why.
func whyLine(count int, mentioned []mention) string {
	if count == 0 {
		return "Nothing in your notes matches this meeting yet"
	}
	if len(mentioned) > 0 {
		return "You mentioned " + mentioned[0].name + " in " + notesCount(mentioned[0].count)
	}
	if count == 1 {
		return "1 note matches this meeting"
	}
	return strconv.Itoa(count) + " notes match this meeting"
}

func notesCount(n int) string {
	if n == 1 {
		return "1 note"
	}
	return strconv.Itoa(n) + " notes"
}

func flatten(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// fold is text.folding(options: [.caseInsensitive, .diacriticInsensitive]).
func fold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		out = s
	}
	return strings.ToLower(out)
}

// containsWord reports needle in haystack with no letter or digit either side.
func containsWord(needle, haystack string) bool {
	if needle == "" {
		return false
	}
	start := 0
	for start <= len(haystack) {
		i := strings.Index(haystack[start:], needle)
		if i < 0 {
			return false
		}
		i += start
		before, _ := utf8.DecodeLastRuneInString(haystack[:i])
		after, _ := utf8.DecodeRuneInString(haystack[i+len(needle):])
		if !isWordRune(haystack[:i], before) && !isWordRune(haystack[i+len(needle):], after) {
			return true
		}
		_, size := utf8.DecodeRuneInString(haystack[i:])
		start = i + size
	}
	return false
}

func isWordRune(side string, r rune) bool {
	return side != "" && (unicode.IsLetter(r) || unicode.IsNumber(r))
}

// relatedSnippet is MemoryController.relatedSnippet: on one line, at most 160
// characters (159 + "…").
func relatedSnippet(content string) string {
	flat := flatten(content)
	if swiftstr.Count(flat) <= 160 {
		return flat
	}
	return swiftstr.Prefix(flat, 159) + "…"
}
