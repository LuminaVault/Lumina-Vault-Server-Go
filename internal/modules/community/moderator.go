package community

import (
	"context"
	"regexp"
	"strings"
	"unicode"
)

// Verdict is CommunityModerationVerdict: what the check before publishing said.
type Verdict struct {
	Kind     VerdictKind
	Category string // set when Kind is Rejected
}

type VerdictKind int

const (
	// Unavailable: the check could not run. The post is held pending, visible
	// to its author only, and re-checked later. Never publish unchecked, never
	// lose a member's post.
	Unavailable VerdictKind = iota
	Allowed
	Rejected
)

// Moderator is CommunityModerating.
type Moderator interface {
	Review(ctx context.Context, title *string, body string) Verdict
}

// FilterOnly is the production moderator until LLM routing is ported (wave 5).
// It runs Swift's deterministic pre-filter and has no model to ask, so whatever
// passes the filter is Unavailable: held pending for DailyPostWorker's sweep,
// which stays in Swift and asks the managed chain. See the PR's gap note: Swift
// would have published an allowed post immediately.
type FilterOnly struct{}

func (FilterOnly) Review(_ context.Context, title *string, body string) Verdict {
	text := body
	if title != nil {
		text = *title + "\n\n" + body
	}
	if category := FilterCheck(text); category != "" {
		return Verdict{Kind: Rejected, Category: category}
	}
	return Verdict{Kind: Unavailable}
}

const maxLinks = 3

var (
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// ICU's \d is any Nd digit and its \s is [\t\n\f\r\p{Z}]; RE2's are ASCII.
	phoneRe = regexp.MustCompile(`\+?\(?\p{Nd}[\p{Nd}\t\n\f\r\p{Z}().-]{7,}\p{Nd}`)
	linkRe  = regexp.MustCompile(`(?i)https?://|www\.`)
)

// FilterCheck is CommunityContentFilter.check: the rejection category for
// personal data (doxxing) or link spam, or "" when the text passes.
func FilterCheck(text string) string {
	if emailRe.MatchString(text) || containsPhoneNumber(text) {
		return "personal_data"
	}
	if len(linkRe.FindAllStringIndex(text, -1)) > maxLinks {
		return "spam"
	}
	return ""
}

// Nine or more digits in one run, allowing the usual separators. Dates
// (2026-10-19, eight digits) and counts (10,000) stay under it.
func containsPhoneNumber(text string) bool {
	for _, m := range phoneRe.FindAllString(text, -1) {
		digits := strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Nd, r) {
				return r
			}
			return -1
		}, m)
		if len([]rune(digits)) >= 9 {
			return true
		}
	}
	return false
}
