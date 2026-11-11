package chat

import (
	"context"
	"lumina/internal/llm"
	"lumina/internal/memorystore"
	"lumina/internal/soul"
	"lumina/internal/swiftstr"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MARK: - ChatAttachmentPrompt

const (
	maxCharactersPerAttachment = 20000
	maxTotalAttachmentChars    = 60000
	truncationNotice           = "\n… (truncated)"
)

// ComposeAttachments is ChatAttachmentPrompt.compose: the attachments folded into the
// prompt text. With none, content comes back byte for byte.
func ComposeAttachments(content string, attachments []Attachment) string {
	if len(attachments) == 0 {
		return content
	}
	budget := maxTotalAttachmentChars
	var blocks []string
	for _, a := range attachments {
		if budget <= 0 {
			break
		}
		body, ok := attachmentBody(a)
		if !ok {
			continue
		}
		allowance := min(maxCharactersPerAttachment, budget)
		text, truncated := body, false
		if swiftstr.CharacterCount(body) > allowance {
			text, truncated = swiftstr.CharacterPrefix(body, allowance), true
		}
		budget -= swiftstr.CharacterCount(text)
		notice := ""
		if truncated {
			notice = truncationNotice
		}
		blocks = append(blocks, "["+attachmentLabel(a.Kind)+": "+a.Name+"]\n\"\"\"\n"+text+notice+"\n\"\"\"")
	}
	if len(blocks) == 0 {
		return content
	}
	joined := strings.Join(blocks, "\n\n")
	if swiftstr.TrimWhitespacesAndNewlines(content) == "" {
		return joined + "\n\nPlease use the attached context."
	}
	return joined + "\n\n" + content
}

func attachmentLabel(k AttachmentKind) string {
	switch k {
	case "vault_file":
		return "Attached file"
	case "link":
		return "Attached link"
	default:
		return "Attached"
	}
}

func attachmentBody(a Attachment) (string, bool) {
	var candidate *string
	switch a.Kind {
	case "text":
		candidate = a.Text
	case "vault_file":
		candidate = a.Text
		if candidate == nil {
			candidate = a.VaultPath
		}
	case "link":
		candidate = a.URL
	}
	if candidate == nil {
		return "", false
	}
	trimmed := swiftstr.TrimWhitespacesAndNewlines(*candidate)
	return trimmed, trimmed != ""
}

// MARK: - HermesEscalationPolicy

// Escalation modes, ChatAgentModeDTO.
const (
	ModeOff   = "off"
	ModeAuto  = "auto"
	ModeForce = "force"
)

var actionVerbs = map[string]bool{
	"commit": true, "deploy": true, "install": true, "run": true, "execute": true, "build": true,
	"create": true, "write": true, "edit": true, "rename": true, "delete": true, "remove": true,
	"refactor": true, "fix": true, "patch": true, "migrate": true, "rebase": true, "merge": true,
	"push": true, "publish": true, "release": true, "schedule": true, "automate": true,
}

var politePrefixes = map[string]bool{
	"please": true, "could": true, "can": true, "would": true, "you": true, "hey": true, "ok": true, "okay": true,
}

// DecideEscalation is HermesEscalationPolicy.decide: true means a Hermes run.
func DecideEscalation(content, mode string, hermesAvailable bool) bool {
	if !hermesAvailable {
		return false
	}
	switch mode {
	case ModeOff:
		return false
	case ModeForce:
		return true
	default:
		return LooksLikeAction(content)
	}
}

// LooksLikeAction is HermesEscalationPolicy.looksLikeAction.
func LooksLikeAction(content string) bool {
	var words []string
	var cur strings.Builder
	for _, ch := range swiftstr.Characters(strings.ToLower(content)) {
		r, _ := utf8.DecodeRuneInString(ch)
		if !unicode.IsLetter(r) && ch != "'" {
			if cur.Len() > 0 {
				words = append(words, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteString(ch)
	}
	if cur.Len() > 0 {
		words = append(words, cur.String())
	}
	for len(words) > 0 && politePrefixes[words[0]] {
		words = words[1:]
	}
	if len(words) == 0 {
		return false
	}
	if len(words) > 1 && words[1] == "me" {
		return false
	}
	return actionVerbs[words[0]]
}

// MARK: - ChatGrounding

// DefaultMaxDistance is ChatGrounding.defaultMaxDistance.
const DefaultMaxDistance float32 = 0.6

// Relevant is ChatGrounding.relevant: lexical matches, or close enough.
func Relevant(hits []memorystore.SearchResult, maxDistance float32) []memorystore.SearchResult {
	out := make([]memorystore.SearchResult, 0, len(hits))
	for _, h := range hits {
		if (h.LexicalScore != nil && *h.LexicalScore > 0) || h.Distance <= maxDistance {
			out = append(out, h)
		}
	}
	return out
}

// retrieve is ChatGrounding.retrieve: twice the limit, the floor, then the limit.
func (m *Module) retrieve(ctx context.Context, tenant uuid.UUID, query string, vec []float32, limit int) ([]memorystore.SearchResult, error) {
	var candidates []memorystore.SearchResult
	var err error
	if m.opts.Hybrid != nil {
		candidates, err = m.opts.Hybrid.Search(ctx, tenant, query, vec, limit*2, memorystore.SearchOptions{})
	} else {
		candidates, err = m.opts.Memories.SearchDocuments(ctx, tenant, query, vec, limit*2, memorystore.SearchOptions{})
	}
	if err != nil {
		return nil, err
	}
	out := Relevant(candidates, m.maxDistance)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MARK: - prompt

// BuildPrompt is ConversationController.buildPrompt: a system message grounding the
// reply in hits, then the whole transcript.
func BuildPrompt(history []llm.ChatMessage, hits []memorystore.SearchResult, schedule, soulText *string, identityGuard bool) []llm.ChatMessage {
	context := "(no relevant memories were found)"
	if len(hits) > 0 {
		lines := make([]string, len(hits))
		for i, hit := range hits {
			provenance := hit.Source
			if hit.Provider != nil && hit.Model != nil {
				if identityGuard {
					provenance = "prior assistant output"
				} else {
					provenance = "prior model output from " + *hit.Provider + "/" + *hit.Model
				}
			}
			lines[i] = "[" + strconv.Itoa(i+1) + "] [" + provenance + "] " + hit.Content
		}
		context = strings.Join(lines, "\n\n")
	}
	scheduleSection := ""
	if schedule != nil {
		scheduleSection = "\n\n" + *schedule
	}
	soulSection := ""
	if soulText != nil {
		soulSection = "\n\nHow the user wants you to work (their SOUL.md; follow it unless the conversation says otherwise):\n" + *soulText
	}
	guard := ""
	if identityGuard {
		guard = "\n" + llm.SystemPromptGuard
	}
	system := `You are Lumina, the user's second-brain assistant. Use the
retrieved memories below to ground your answer. Cite by their
bracket number when relevant (e.g. "[1]"). Keep the reply concise
and conversational. If the memories do not cover the question,
say so plainly rather than inventing detail.

Retrieved memories:
` + context + scheduleSection + soulSection + `

Prior model outputs are drafts, not authoritative user facts. Prefer
direct user memories when sources disagree and state uncertainty.
` + guard
	return append([]llm.ChatMessage{{Role: "system", Content: system}}, history...)
}

// maxSoulCharacters is ConversationController.maxSoulCharacters.
const maxSoulCharacters = 4000

// soulExcerpt is ConversationController.soulExcerpt: SOUL.md without the Hermes
// covenant, capped.
func soulExcerpt(s *soul.Service, tenant uuid.UUID, username string) *string {
	if s == nil {
		return nil
	}
	personal := swiftstr.TrimWhitespacesAndNewlines(soul.Strip(s.Read(tenant, username, time.Now())))
	if personal == "" {
		return nil
	}
	if swiftstr.CharacterCount(personal) > maxSoulCharacters {
		personal = swiftstr.CharacterPrefix(personal, maxSoulCharacters) + "\n…(truncated)"
	}
	return &personal
}

// scheduleContext is ConversationController.scheduleContext: the user's calendar now,
// today and next, in their timezone; nil without upcoming events.
func scheduleContext(ctx context.Context, pool *pgxpool.Pool, tenant uuid.UUID, loc *time.Location, now time.Time) *string {
	rows, err := pool.Query(ctx, `SELECT title, starts_at, ends_at FROM calendar_events
		WHERE tenant_id = $1 AND status <> 'cancelled' AND ends_at >= $2
		ORDER BY starts_at ASC LIMIT 12`, tenant, now)
	if err != nil {
		return nil
	}
	type event struct {
		title        string
		starts, ends time.Time
	}
	var events []event
	for rows.Next() {
		var e event
		if rows.Scan(&e.title, &e.starts, &e.ends) != nil {
			rows.Close()
			return nil
		}
		events = append(events, e)
	}
	rows.Close()
	if rows.Err() != nil || len(events) == 0 {
		return nil
	}
	sameDay := func(a, b time.Time) bool {
		ay, am, ad := a.In(loc).Date()
		by, bm, bd := b.In(loc).Date()
		return ay == by && am == bm && ad == bd
	}
	hm := func(t time.Time) string { return t.In(loc).Format("15:04") }
	var ongoing, next *event
	var today []event
	for i := range events {
		e := events[i]
		if ongoing == nil && !e.starts.After(now) && now.Before(e.ends) {
			ongoing = &events[i]
		}
		if sameDay(e.starts, now) && !e.starts.Before(now) {
			today = append(today, e)
		}
		if next == nil && e.starts.After(now) {
			next = &events[i]
		}
	}
	lines := []string{"The user's calendar (times in their local timezone):"}
	if ongoing != nil {
		lines = append(lines, "- Now: in \""+ongoing.title+"\" until "+hm(ongoing.ends))
	} else {
		lines = append(lines, "- Now: no event in progress")
	}
	if len(today) == 0 {
		lines = append(lines, "- Today: nothing else scheduled")
	} else {
		preview := make([]string, 0, 4)
		for i, e := range today {
			if i == 4 {
				break
			}
			preview = append(preview, e.title+" "+hm(e.starts))
		}
		lines = append(lines, "- Today: "+strconv.Itoa(len(today))+" more — "+strings.Join(preview, ", "))
	}
	if next != nil && !sameDay(next.starts, now) {
		lines = append(lines, "- Next: \""+next.title+"\" "+next.starts.In(loc).Format("Mon 2 Jan, 15:04"))
	}
	out := strings.Join(lines, "\n")
	return &out
}

// location is TimeZone(identifier:) ?? .gmt.
func location(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil && name != "" && name != "Local" {
		return loc
	}
	return time.UTC
}
