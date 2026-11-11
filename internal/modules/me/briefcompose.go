package me

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lumina/internal/modules/me/db"
	"lumina/internal/swiftstr"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BriefComposer (Wave 4): the daily brief as a short list of cards with an end.
// It runs after the daily-brief skill (SkillRunner.dispatchOutputs) and stores
// one brief per tenant per local day in briefs. Cards, in order, each skipped
// when empty: today (calendar), openLoops (commitments), toRevisit (resurfacing),
// learned (the digest count), then the skill's own markdown sections. A kind the
// user marked down moves to the bottom.

const (
	// BriefSkillName is BriefComposer.skillName.
	BriefSkillName        = "daily-brief"
	briefEndNote          = "That's it for today."
	briefFeedbackWindow   = 14 * 24 * time.Hour
	briefDemoteAtOrBelow  = -2
	briefMaxEventsListed  = 8
	briefMaxSectionLength = 1500
	// CommitmentsService.maxBriefAppearances and briefLimit.
	commitmentMaxBriefAppearances = 3
	commitmentBriefLimit          = 3
)

// ComposeBrief is BriefComposer.compose: it stores today's brief and returns how
// many cards it holds.
func (m *Module) ComposeBrief(cx context.Context, tenant uuid.UUID, skillMarkdown string, now time.Time) (int, error) {
	var u *user
	row, err := m.q.MeUser(cx, tenant)
	switch {
	case err == nil:
		u = &user{id: tenant, tier: row.Tier, timezone: row.Timezone, createdAt: row.CreatedAt}
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, err
	}
	loc := time.UTC
	tzName := "UTC"
	if u != nil {
		if l, ok := zone(u.timezone); ok {
			loc, tzName = l, u.timezone
		}
	}
	day := localDay(now, tzName)

	alreadyCounted := map[uuid.UUID]bool{}
	prev, err := m.q.BriefByDate(cx, db.BriefByDateParams{TenantID: tenant, Day: day})
	switch {
	case err == nil:
		for _, card := range decodeCards(prev.Cards) {
			for _, ref := range card.Refs {
				if ref.Kind != Commitment {
					continue
				}
				if id, err := uuid.Parse(ref.Id); err == nil && len(ref.Id) == 36 {
					alreadyCounted[id] = true
				}
			}
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, err
	}

	var cards []BriefCardDTO
	today, err := m.briefTodayCard(cx, tenant, loc, now)
	if err != nil {
		return 0, err
	}
	if today != nil {
		cards = append(cards, *today)
	}
	loops, err := m.commitmentsForBrief(cx, tenant, alreadyCounted)
	if err != nil {
		return 0, err
	}
	if card := openLoopsCard(loops, loc, now); card != nil {
		cards = append(cards, *card)
	}
	if u != nil {
		resp, err := m.resurface(cx, *u, 1, now)
		if err != nil {
			return 0, err
		}
		if len(resp.Items) > 0 {
			item := resp.Items[0]
			cards = append(cards, BriefCardDTO{
				Id: string(ToRevisit), Kind: ToRevisit, Title: "To revisit", Body: item.Snippet, Why: &item.Why,
				Refs: []BriefCardRefDTO{{Kind: Memory, Id: strings.ToUpper(item.MemoryId.String())}},
			})
		}
	}
	learned, err := m.briefLearnedCard(cx, tenant, now)
	if err != nil {
		return 0, err
	}
	if learned != nil {
		cards = append(cards, *learned)
	}
	hasToday := false
	for _, c := range cards {
		if c.Kind == Today {
			hasToday = true
		}
	}
	for _, s := range briefSections(skillMarkdown) {
		if hasToday && s.kind == Today {
			continue
		}
		cards = append(cards, sectionCard(s))
	}
	cards = uniqueCardIDs(cards)

	net, err := m.briefNetFeedback(cx, tenant, now)
	if err != nil {
		return 0, err
	}
	cards = orderCards(cards, net)
	if cards == nil {
		cards = []BriefCardDTO{}
	}

	encoded, err := json.Marshal(cards)
	if err != nil {
		return 0, err
	}
	if _, err := m.pool.Exec(cx, `INSERT INTO briefs (tenant_id, date, cards, end_note, created_at, updated_at)
		VALUES ($1, $2::date, $3::jsonb, $4, $5, $5)
		ON CONFLICT (tenant_id, date) DO UPDATE
		SET cards = EXCLUDED.cards, end_note = EXCLUDED.end_note, updated_at = EXCLUDED.updated_at`,
		tenant, day, string(encoded), briefEndNote, now); err != nil {
		return 0, err
	}
	var newlyShown []uuid.UUID
	for _, l := range loops {
		if !alreadyCounted[l.ID] {
			newlyShown = append(newlyShown, l.ID)
		}
	}
	if len(newlyShown) > 0 {
		if _, err := m.pool.Exec(cx, `UPDATE commitments SET surfaced_count = surfaced_count + 1
			WHERE tenant_id = $1 AND id = ANY($2)`, tenant, newlyShown); err != nil {
			return 0, err
		}
	}
	return len(cards), nil
}

// briefTodayCard lists the day's calendar events behind the calendar consent.
func (m *Module) briefTodayCard(cx context.Context, tenant uuid.UUID, loc *time.Location, now time.Time) (*BriefCardDTO, error) {
	var allowed bool
	if err := m.pool.QueryRow(cx, `SELECT allowed FROM apple_consent WHERE tenant_id = $1 AND domain = 'calendar'`, tenant).
		Scan(&allowed); err != nil || !allowed {
		return nil, nil //nolint:nilerr // Swift's isAllowed treats a failed read as not allowed
	}
	start := startOfDay(now, loc)
	end := addDays(start, 1, loc)
	rows, err := m.pool.Query(cx, `SELECT id, title, starts_at, all_day FROM calendar_events
		WHERE tenant_id = $1 AND status <> 'cancelled' AND starts_at < $2 AND ends_at > $3
		ORDER BY all_day DESC, starts_at ASC, id LIMIT 50`, tenant, end, start)
	if err != nil {
		return nil, err
	}
	type event struct {
		id     uuid.UUID
		title  string
		starts time.Time
		allDay bool
	}
	var events []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.id, &e.title, &e.starts, &e.allDay); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	hhmm := func(t time.Time) string { return t.In(loc).Format("15:04") }
	listed := events[:min(len(events), briefMaxEventsListed)]
	lines := make([]string, 0, len(listed)+1)
	refs := make([]BriefCardRefDTO, 0, len(listed))
	for _, e := range listed {
		when := "All day"
		if !e.allDay {
			when = hhmm(e.starts)
		}
		lines = append(lines, "- "+when+" · "+e.title)
		refs = append(refs, BriefCardRefDTO{Kind: Event, Id: strings.ToUpper(e.id.String())})
	}
	if len(events) > briefMaxEventsListed {
		lines = append(lines, fmt.Sprintf("- and %d more", len(events)-briefMaxEventsListed))
	}
	var first *event
	for i := range events {
		if !events[i].allDay && !events[i].starts.Before(now) {
			first = &events[i]
			break
		}
	}
	if first == nil {
		for i := range events {
			if !events[i].allDay {
				first = &events[i]
				break
			}
		}
	}
	why := countNoun(len(events), "event") + " on your calendar today"
	if first != nil {
		why = countNoun(len(events), "event") + " today, first at " + hhmm(first.starts)
	}
	return &BriefCardDTO{Id: string(Today), Kind: Today, Title: "Today", Body: strings.Join(lines, "\n"), Why: &why, Refs: refs}, nil
}

// commitmentsForBrief is CommitmentsService.forBrief: open loops, due first then
// oldest, each in at most three briefs (today's own count again).
func (m *Module) commitmentsForBrief(cx context.Context, tenant uuid.UUID, alreadyCounted map[uuid.UUID]bool) ([]commitmentRow, error) {
	counted := make([]uuid.UUID, 0, len(alreadyCounted))
	for id := range alreadyCounted {
		counted = append(counted, id)
	}
	rows, err := m.pool.Query(cx, `SELECT id, text, due_at, state, surfaced_count, memory_id, source_vault_file_id, created_at, done_at
		FROM commitments WHERE tenant_id = $1 AND state = 'open' AND (surfaced_count < $2 OR id = ANY($3))
		ORDER BY due_at ASC NULLS LAST, created_at ASC, id LIMIT $4`,
		tenant, commitmentMaxBriefAppearances, counted, commitmentBriefLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []commitmentRow
	for rows.Next() {
		var r commitmentRow
		if err := rows.Scan(&r.ID, &r.Text, &r.DueAt, &r.State, &r.SurfacedCount, &r.MemoryID, &r.SourceVaultFileID,
			&r.CreatedAt, &r.DoneAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// openLoopsCard is BriefComposer.openLoopsCard.
func openLoopsCard(loops []commitmentRow, loc *time.Location, now time.Time) *BriefCardDTO {
	if len(loops) == 0 {
		return nil
	}
	today := startOfDay(now, loc)
	tomorrow := addDays(today, 1, loc)
	lines := make([]string, 0, len(loops))
	refs := make([]BriefCardRefDTO, 0, len(loops))
	overdue, dueToday := 0, 0
	for _, l := range loops {
		refs = append(refs, BriefCardRefDTO{Kind: Commitment, Id: strings.ToUpper(l.ID.String())})
		if l.DueAt == nil {
			lines = append(lines, "- "+l.Text)
			continue
		}
		lines = append(lines, "- "+l.Text+" (due "+l.DueAt.In(loc).Format("Mon 2 Jan")+")")
		if l.DueAt.Before(today) {
			overdue++
		}
		if !l.DueAt.Before(today) && l.DueAt.Before(tomorrow) {
			dueToday++
		}
	}
	are := func(n int) string {
		if n == 1 {
			return "one is"
		}
		return fmt.Sprintf("%d are", n)
	}
	why := "Things you said you'd do"
	switch {
	case overdue > 0:
		why = "You said you'd do these — " + are(overdue) + " overdue"
	case dueToday > 0:
		why = "You said you'd do these — " + are(dueToday) + " due today"
	}
	return &BriefCardDTO{
		Id: string(OpenLoops), Kind: OpenLoops, Title: "Open loops", Body: strings.Join(lines, "\n"), Why: &why, Refs: refs,
	}
}

// briefLearnedCard is a count, only while the "What I learned" digest has
// something waiting.
func (m *Module) briefLearnedCard(cx context.Context, tenant uuid.UUID, now time.Time) (*BriefCardDTO, error) {
	since := now.Add(-learnedDefaultWindow)
	soul, err := m.q.LearnedSoulChanges(cx, db.LearnedSoulChangesParams{TenantID: tenant, Since: since, Now: now})
	if err != nil {
		return nil, err
	}
	mems, err := m.q.LearnedMemories(cx, db.LearnedMemoriesParams{TenantID: tenant, Since: since, Now: now})
	if err != nil {
		return nil, err
	}
	preferences, decisions := 0, 0
	for _, r := range mems {
		switch {
		case r.Kind != nil && *r.Kind == "preference":
			preferences++
		case r.Kind != nil && *r.Kind == "decision":
			decisions++
		}
	}
	body, ok := learnedPushBody(len(soul)+preferences, decisions)
	if !ok {
		return nil, nil
	}
	refs := make([]BriefCardRefDTO, 0, len(soul)+len(mems))
	for _, s := range soul {
		refs = append(refs, BriefCardRefDTO{Kind: SoulChange, Id: strings.ToUpper(s.ID.String())})
	}
	for _, r := range mems {
		refs = append(refs, BriefCardRefDTO{Kind: Memory, Id: strings.ToUpper(r.ID.String())})
	}
	why := "Picked up this week and waiting for you to confirm"
	return &BriefCardDTO{Id: string(Learned), Kind: Learned, Title: "What I learned", Body: body, Why: &why, Refs: refs}, nil
}

// learnedPushBody is MeLearnedService.pushBody(for:).
func learnedPushBody(aboutYou, decisions int) (string, bool) {
	var parts []string
	if aboutYou > 0 {
		noun := "things"
		if aboutYou == 1 {
			noun = "thing"
		}
		parts = append(parts, fmt.Sprintf("%d %s about you", aboutYou, noun))
	}
	if decisions > 0 {
		noun := "decisions"
		if decisions == 1 {
			noun = "decision"
		}
		parts = append(parts, fmt.Sprintf("%d %s", decisions, noun))
	}
	if len(parts) == 0 {
		return "", false
	}
	pronoun := "them"
	if aboutYou+decisions == 1 {
		pronoun = "it"
	}
	return strings.Join(parts, " and ") + " — keep, edit or forget " + pronoun + ".", true
}

func (m *Module) briefNetFeedback(cx context.Context, tenant uuid.UUID, now time.Time) (map[string]int, error) {
	rows, err := m.pool.Query(cx, `SELECT card_kind, SUM(CASE WHEN action = 'up' THEN 1 ELSE -1 END)::int AS net
		FROM brief_feedback WHERE tenant_id = $1 AND created_at >= $2 GROUP BY card_kind`, tenant, now.Add(-briefFeedbackWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	net := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		net[kind] += n
	}
	return net, rows.Err()
}

// orderCards is BriefComposer.order: demoted kinds keep their order, last.
func orderCards(cards []BriefCardDTO, net map[string]int) []BriefCardDTO {
	var kept, demoted []BriefCardDTO
	for _, c := range cards {
		if net[string(c.Kind)] <= briefDemoteAtOrBelow {
			demoted = append(demoted, c)
		} else {
			kept = append(kept, c)
		}
	}
	return append(kept, demoted...)
}

// uniqueCardIDs is BriefComposer.uniqueIDs: a second card of a kind is `<kind>-2`.
func uniqueCardIDs(cards []BriefCardDTO) []BriefCardDTO {
	seen := map[BriefCardKind]int{}
	out := make([]BriefCardDTO, len(cards))
	for i, c := range cards {
		seen[c.Kind]++
		if n := seen[c.Kind]; n > 1 {
			c.Id = fmt.Sprintf("%s-%d", c.Kind, n)
		}
		out[i] = c
	}
	return out
}

type briefSection struct {
	kind        BriefCardKind
	title, body string
}

// isFoundationNewline is CharacterSet.newlines, matched per scalar as
// components(separatedBy:) does.
func isFoundationNewline(r rune) bool {
	return (r >= '\n' && r <= '\r') || r == 0x85 || r == 0x2028 || r == 0x2029
}

// briefSections is BriefComposer.sections(from:).
func briefSections(markdown string) []briefSection {
	text := swiftstr.TrimWhitespacesAndNewlines(markdown)
	if text == "" {
		return nil
	}
	type entry struct {
		title string
		lines []string
	}
	var found []entry
	var current *entry
	sawLabel := false
	for _, line := range splitFunc(text, isFoundationNewline) {
		if title, rest, ok := briefLabel(line); ok {
			if current != nil {
				found = append(found, *current)
			}
			current = &entry{title: title}
			if rest != "" {
				current.lines = []string{rest}
			}
			sawLabel = true
		} else if current != nil {
			current.lines = append(current.lines, line)
		}
	}
	if current != nil {
		found = append(found, *current)
	}
	if !sawLabel {
		return []briefSection{{kind: OnYourMind, title: "On your mind", body: clipSection(text)}}
	}
	var out []briefSection
	for _, e := range found {
		body := swiftstr.TrimWhitespacesAndNewlines(strings.Join(e.lines, "\n"))
		if body == "" {
			continue
		}
		out = append(out, briefSection{kind: kindForHeading(e.title), title: e.title, body: clipSection(body)})
	}
	return out
}

func splitFunc(s string, sep func(rune) bool) []string {
	var out []string
	start := 0
	for i, r := range s {
		if sep(r) {
			out = append(out, s[start:i])
			start = i + len(string(r))
		}
	}
	return append(out, s[start:])
}

func trimSet(s, set string) string {
	return strings.TrimFunc(s, func(r rune) bool { return strings.ContainsRune(set, r) })
}

func trimSetAndWhitespace(s, set string) string {
	return strings.TrimFunc(s, func(r rune) bool { return strings.ContainsRune(set, r) || swiftstr.IsWhitespace(r) })
}

// briefLabel is BriefComposer.label(in:): `## Title`, `**Title**`,
// `**Title:** rest`, `**Title** — rest`.
func briefLabel(raw string) (title, rest string, ok bool) {
	line := swiftstr.TrimWhitespaces(raw)
	if strings.HasPrefix(line, "#") {
		t := swiftstr.TrimWhitespaces(strings.TrimLeft(line, "#"))
		cleaned := trimSet(strings.ReplaceAll(t, "**", ""), ": ")
		if cleaned == "" {
			return "", "", false
		}
		return cleaned, "", true
	}
	if !strings.HasPrefix(line, "**") {
		return "", "", false
	}
	afterOpen := line[2:]
	closeAt := strings.Index(afterOpen, "**")
	if closeAt < 0 {
		return "", "", false
	}
	inner, after := afterOpen[:closeAt], afterOpen[closeAt+2:]
	title = trimSetAndWhitespace(inner, ": ")
	if title == "" || swiftstr.Count(title) > 40 {
		return "", "", false
	}
	rest = trimSetAndWhitespace(after, ":—–- ")
	titleEndsInColon := strings.HasSuffix(swiftstr.TrimWhitespaces(inner), ":")
	restStartsLikeLabel := true
	if chars := swiftstr.Characters(swiftstr.TrimWhitespaces(after)); len(chars) > 0 {
		restStartsLikeLabel = chars[0] == ":" || chars[0] == "—" || chars[0] == "–" || chars[0] == "-"
	}
	if rest != "" && !titleEndsInColon && !restStartsLikeLabel {
		return "", "", false
	}
	return title, rest, true
}

// kindForHeading is BriefComposer.kind(forHeading:).
func kindForHeading(heading string) BriefCardKind {
	h := strings.ToLower(heading)
	switch {
	case strings.Contains(h, "weather") || strings.Contains(h, "forecast"):
		return Weather
	case strings.Contains(h, "inbox") || strings.Contains(h, "mail"):
		return Inbox
	case strings.Contains(h, "on your mind") || strings.Contains(h, "mind"):
		return OnYourMind
	case h == "today" || strings.Contains(h, "schedule") || strings.Contains(h, "calendar"):
		return Today
	}
	return Other
}

func sectionCard(s briefSection) BriefCardDTO {
	why := "From your morning brief"
	switch s.kind {
	case Weather:
		why = "For planning your day"
	case Inbox:
		why = "Messages that look like they need you"
	case OnYourMind:
		why = "From your recent notes"
	case Today:
		why = "On your calendar today"
	}
	return BriefCardDTO{Id: string(s.kind), Kind: s.kind, Title: s.title, Body: s.body, Why: &why, Refs: []BriefCardRefDTO{}}
}

func clipSection(text string) string {
	if swiftstr.Count(text) > briefMaxSectionLength {
		return swiftstr.Prefix(text, briefMaxSectionLength-1) + "…"
	}
	return text
}

func countNoun(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
